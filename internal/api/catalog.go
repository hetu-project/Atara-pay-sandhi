package api

import (
	"net/http"

	"github.com/advaita/atara-pay/internal/domain/condition"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/advaita/atara-pay/internal/store"
)

func (h *Handler) Assets(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"assets": money.Cryptos()})
}

func (h *Handler) Fiats(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"corridors": money.Corridors()})
}

// Chain 报出前端自己发交易要用的网络与合约地址。
//
// 为什么由接口发而不是写在前端：合约换一次地址，写死的前端就会把钱
// approve 给一个旧合约，而且要等到锁币那一刻才发现。
//
// 读的是库里 chain_deployments 那一行，不是当场问链或读环境变量。那一行
// 是后端启动时按自己实际在用的配置写进去的，所以「前端看到的地址」与
// 「后端真正在说话的合约」是同一份。环境变量只是部署输出的落点，不该是
// 对外的答案——进程之间、机器之间它可能不一样。
//
// 查不到记录说明这一版跑的是 mock 链：那条链上没有合约可调，回一份空的，
// 前端据此知道不发交易，而不是拿着空地址去调用。
func (h *Handler) Chain(w http.ResponseWriter, r *http.Request) {
	d, err := h.St.ChainDeployment(r.Context(), h.Cfg.Chain.Network)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if d == nil {
		ok(w, store.ChainDeployment{
			Impl: "mock", Network: "mock", Tokens: map[string]store.ChainToken{},
		})
		return
	}
	ok(w, d)
}

// Conditions 把条件原子的定义与联动选项发给前端，
// 免得「换数据源要重置指标」这种规则在两端各写一份。
func (h *Handler) Conditions(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{
		"max": condition.Max,
		"atoms": []map[string]any{
			{"type": condition.Approve, "label": "Approval",
				"params": []map[string]any{{"key": "who", "control": "pick", "options": condition.Approvers()}}},
			{"type": condition.Evidence, "label": "Evidence",
				"params": []map[string]any{{"key": "proof", "control": "pick", "options": condition.ProofTypes()}}},
			{"type": condition.Data, "label": "API data",
				"params": []map[string]any{
					{"key": "src", "control": "pick", "options": keys(condition.DataMetrics())},
					{"key": "metric", "control": "pick", "depends_on": "src", "options_by": condition.DataMetrics()},
					{"key": "target", "control": "text", "placeholder": "target — e.g. ≥ 1,000"},
				}},
			{"type": condition.Time, "label": "Time",
				"params": []map[string]any{{"key": "date", "control": "date"}}},
		},
		// 兜底不是条件的一部分：它是条件没成立时的处置。
		"fallback": map[string]any{"default_days": 14,
			"note": "Unresolved after this window goes to human review — this is not one of the release conditions."},
	})
}

func (h *Handler) Intents(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"intents": []string{
		"Supplier balance", "Delivery acceptance", "Rent", "Payroll",
		"Service subscription", "API usage",
	}})
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for _, k := range []string{"Ad platform API", "Logistics API", "Payment gateway API", "On-chain oracle"} {
		if _, ok := m[k]; ok {
			out = append(out, k)
		}
	}
	return out
}
