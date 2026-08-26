package api

import (
	"net/http"

	"github.com/advaita/atara-pay/internal/domain/condition"
	"github.com/advaita/atara-pay/internal/money"
)

func (h *Handler) Assets(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"assets": money.Cryptos()})
}

func (h *Handler) Fiats(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"corridors": money.Corridors()})
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
