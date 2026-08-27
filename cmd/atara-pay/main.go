package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/advaita/atara-pay/internal/agent/mockagent"
	"github.com/advaita/atara-pay/internal/api"
	"github.com/advaita/atara-pay/internal/app"
	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/config"
	"github.com/advaita/atara-pay/internal/scheduler"
	"github.com/advaita/atara-pay/internal/store"
)

func main() {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer st.Close()
	if err := st.Seed(ctx); err != nil {
		log.Fatalf("seed: %v", err)
	}

	ag := mockagent.New() // ATARA_AGENT_IMPL=http 时在这里换实现，路由与 DTO 不动
	svc := app.New(st, ag, cfg, auth.NewConfirmations())
	go scheduler.New(svc).Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.New(st, svc, cfg).Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("atara-pay listening on %s · db=%s · agent=%s · demo-timing=%v",
			cfg.Addr, cfg.DBPath, cfg.AgentImpl, cfg.DemoTiming)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	log.Println("atara-pay stopped")
}
