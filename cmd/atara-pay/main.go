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
	"github.com/shopspring/decimal"
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
	funder, ok := ch.(store.Funder)
	if !ok {
		log.Fatalf("chain %s cannot seed", chainLabel)
	}
	// 真链上不灌演示余额。
	//
	// 那些余额是「凭空发币」，链上没有这回事：币不是我们发的就一路 revert，
	// 是我们发的就等于每次重启白铸一遍。而且每一笔都要等一次 RPC 往返——
	// 实测接 BSC 测试网时，光是灌种子就三分钟还没走完，而 make fresh-chain
	// 每次都会重来一遍。
	//
	// 地址派生还得用真链的格式（EVM 是 0x，TRON 是 T 开头），所以只把
	// 动钱的三个方法短路掉，DeriveAddress 照旧走真实现。
	if cfg.ChainImpl != "mock" {
		log.Printf("真链：种子数据只建账户与挂单，不灌余额、不锁币、不签额度。" +
			"演示做市方的挂单会显示可成交量 0。")
		funder = readOnlyFunder{funder}
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
//
// 地址从哪儿来：环境变量给了就用环境变量（那是刚部署完的输出），没给就读
// 库里上一次那条记录。**用哪一份，最后都写回库**——接口发给前端的就是库里
// 那一份，两边不可能不一致。这一点很要紧：库里写着 A、后端却在跟 B 说话，
// 是那种一直不报错、直到钱进了错的合约才发现的问题。
func openChain(ctx context.Context, cfg config.Config, st *store.Store) (chain.Chain, string, error) {
	switch cfg.ChainImpl {
	case "evm":
		cc := cfg.Chain
		if cc.Escrow == "" {
			if d, err := st.ChainDeployment(ctx, cc.Network); err == nil && d != nil {
				log.Printf("链地址取自库里 %s 那条记录（%s 更新）", d.Network,
					d.UpdatedAt.Format("2006-01-02 15:04"))
				cc.Escrow, cc.Spending = d.Escrow, d.Spending
				if cc.RPCURL == "" {
					cc.RPCURL = d.RPCURL
				}
				if t, ok := d.Tokens["USDT"]; ok && cc.USDT == "" {
					cc.USDT = t.Address
				}
				if t, ok := d.Tokens["USDC"]; ok && cc.USDC == "" {
					cc.USDC = t.Address
				}
			}
		}
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
		// 把实际在用的这一份写回库。前端读的就是这一行——写回来，
		// 库里那份就永远等于后端此刻真正在用的那份。
		info := ch.Info(ctx)
		if err := st.SaveChainDeployment(ctx, store.ChainDeployment{
			Network: info.Network, ChainID: info.ChainID, Impl: info.Impl,
			RPCURL: info.RPCURL, Explorer: info.Explorer,
			Escrow: info.Escrow, Spending: info.Spending,
			Tokens: toStoreTokens(info.Tokens),
		}); err != nil {
			// 记不上不该挡住启动，但前端会拿不到地址——说清楚是这个原因。
			log.Printf("链部署记录写库失败（前端会读不到合约地址）：%v", err)
		}
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

// readOnlyFunder 把种子的三个「动钱」动作变成空操作，只留地址派生。
type readOnlyFunder struct{ store.Funder }

func (readOnlyFunder) Credit(context.Context, string, string, decimal.Decimal) error {
	return nil
}
func (readOnlyFunder) LockListing(context.Context, string, string, string, decimal.Decimal) (string, error) {
	return "", nil
}
func (readOnlyFunder) GrantAllowance(context.Context, chain.AllowanceGrant) (string, error) {
	return "", nil
}

func toStoreTokens(in map[string]chain.Token) map[string]store.ChainToken {
	out := make(map[string]store.ChainToken, len(in))
	for k, v := range in {
		out[k] = store.ChainToken{Address: v.Address, Decimals: v.Decimals}
	}
	return out
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
