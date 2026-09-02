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

	"fmt"
	"github.com/advaita/atara-pay/internal/agent/mockagent"
	"github.com/advaita/atara-pay/internal/api"
	"github.com/advaita/atara-pay/internal/app"
	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/chain"
	"github.com/advaita/atara-pay/internal/chain/evmchain"
	"github.com/advaita/atara-pay/internal/chain/mockchain"
	"github.com/advaita/atara-pay/internal/config"
	"github.com/advaita/atara-pay/internal/scheduler"
	"github.com/advaita/atara-pay/internal/store"
	"sort"
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
	ch, chainLabel, err := openChain(ctx, cfg, st)
	if err != nil {
		log.Fatalf("chain: %v", err)
	}
	// Funder 刻意不在 chain.Chain 里——真链上没有「凭空记一笔余额」。
	// 两个实现都能灌种子：mock 直接记账，evm 铸测试币（真网上会失败，那是对的）。
	funder, ok := ch.(store.Funder)
	if !ok {
		log.Fatalf("chain %s cannot seed", chainLabel)
	}
	if err := st.Seed(ctx, funder); err != nil {
		log.Fatalf("seed: %v", err)
	}
	svc := app.New(st, ag, ch, cfg, auth.NewConfirmations(st))
	go scheduler.New(svc).Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.New(st, svc, cfg).Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("atara-pay listening on %s · db=%s · agent=%s · chain=%s · custody=self · demo-timing=%v",
			cfg.Addr, cfg.DBPath, cfg.AgentImpl, chainLabel, cfg.DemoTiming)
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

// openChain 按配置选链实现。
//
// evm 那一支缺参数就报错退出，不悄悄退回 mock：一个以为在跟真链说话
// 却其实在跟内存 mock 说话的后端，比起不来更危险。
func openChain(ctx context.Context, cfg config.Config, st *store.Store) (chain.Chain, string, error) {
	switch cfg.ChainImpl {
	case "evm":
		cc := cfg.Chain
		for name, v := range map[string]string{
			"ATARA_ESCROW_ADDR": cc.Escrow,
			"ATARA_SIGNER_KEY":  cc.SignerKey,
			"ATARA_TOKEN_USDT":  cc.USDT,
		} {
			if v == "" {
				return nil, "", fmt.Errorf("ATARA_CHAIN_IMPL=evm needs %s", name)
			}
		}
		tokens := map[string]string{"USDT": cc.USDT}
		if cc.USDC != "" {
			tokens["USDC"] = cc.USDC
		}
		ch, err := evmchain.New(ctx, evmchain.Config{
			RPCURL: cc.RPCURL, EscrowAddr: cc.Escrow, SpendingAddr: cc.Spending,
			SignerKeyHex: cc.SignerKey,
			Tokens:       tokens, Network: cc.Network, ExplorerBase: cc.ExplorerBase,
		})
		if err != nil {
			return nil, "", err
		}
		log.Printf("chain=evm · rpc=%s · escrow=%s · spending=%s · signer=%s · tokens=%v",
			cc.RPCURL, cc.Escrow, orDash(cc.Spending), ch.SignerAddress(), keysOf(tokens))
		if cc.Spending == "" {
			log.Printf("ATARA_SPENDING_ADDR 未配：额度只有平台侧记录，链上没有真实授权。")
		}
		log.Printf("单签名方、阈值 1：这把私钥丢了，合约里的钱就能被放走。" +
			"上真钱之前必须换成多签名方、阈值 >= 2。")
		return ch, "evm(" + cc.Network + ")", nil
	case "mock":
		ch, err := mockchain.New(ctx, st.DB(), mockchain.DemoTiming())
		if err != nil {
			return nil, "", err
		}
		return ch, "mock", nil
	}
	return nil, "", fmt.Errorf("unknown ATARA_CHAIN_IMPL %q (want mock or evm)", cfg.ChainImpl)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
