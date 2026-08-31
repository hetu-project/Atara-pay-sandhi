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
	"github.com/advaita/atara-pay/internal/chain/mockchain"
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
	// 两处外部依赖，两个可插拔实现：
	//   agent —— 解析 / 风控共识 / 放行共识，接真模型时换这里
	//   chain —— 托管合约、入金检测、确认数、额度签发，接 loka-chain 时换这里
	// 其余全部是真实实现。
	ag := mockagent.New()
	ch, err := mockchain.New(ctx, st.DB(), mockchain.DemoTiming())
	if err != nil {
		log.Fatalf("chain: %v", err)
	}
	if err := st.Seed(ctx, ch); err != nil {
		log.Fatalf("seed: %v", err)
	}
	svc := app.New(st, ag, ch, cfg, auth.NewConfirmations())
	go scheduler.New(svc).Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.New(st, svc, cfg).Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("atara-pay listening on %s · db=%s · agent=%s · chain=mock · custody=self · demo-timing=%v",
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
