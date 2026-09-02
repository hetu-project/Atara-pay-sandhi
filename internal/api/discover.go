package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/httpx"
)

// marketJSON 是 Discover 的一个纵向。三个纵向照抄前端 DISCOVER 常量，
// 只有 OTC 上线，另两个是 Coming。
//
// 与资产目录一样不入库：它们随版本走，不随数据走——一个纵向什么时候上线
// 是发版决定的，不是运行时能改的。
type marketJSON struct {
	Key  string      `json:"key"`
	Name string      `json:"name"`
	Live bool        `json:"live"`
	Desc string      `json:"desc,omitempty"`
	Map  [][2]string `json:"map,omitempty"`
}

var markets = []marketJSON{
	{Key: "otc", Name: "OTC pool", Live: true},
	{Key: "api", Name: "Compute & APIs", Live: false,
		Desc: "Where agents buy inference, data or compute — settled per call or per unit.",
		Map: [][2]string{
			{"Counterparty", "Providers — unfamiliar, high frequency"},
			{"Condition", "Call succeeds and returns"},
			{"Evidence", "API callback · usage reconciliation"},
		}},
	{Key: "shop", Name: "Merchants", Live: false,
		Desc: "Goods and services with an acceptance condition — receive first, pay after.",
		Map: [][2]string{
			{"Counterparty", "Merchants — unfamiliar, one-off"},
			{"Condition", "Signed for, no dispute in window"},
			{"Evidence", "Logistics API · 7-day auto-release"},
		}},
}

func (h *Handler) DiscoverMarkets(w http.ResponseWriter, _ *http.Request) {
	ok(w, map[string]any{"markets": markets})
}

// taskJSON 对齐前端 TKST 的三个键。Tasks 是订单的投影，不是独立实体——
// 前端的注释写得很清楚：「每笔交易开单即入列，状态跟着 advance() 走」。
// 所以后端不建表，从 orders 现算。
type taskJSON struct {
	ID       string    `json:"id"`
	OrderRef string    `json:"order_ref"`
	Title    string    `json:"title"`
	State    string    `json:"state"` // run | you | done
	At       time.Time `json:"at"`
}

// 标题取前端 OSTATE 每个阶段的第二个元素，两边措辞保持一致。
var phaseTitles = map[order.Phase]string{
	order.PhasePay:    "Send the transfer",
	order.PhaseVerify: "Verify their receipt",
	order.PhaseWait:   "Waiting on their transfer",
	order.PhaseLock:   "Locking into escrow",
	order.PhaseRel:    "Releasing to them",
}

func (h *Handler) Tasks(w http.ResponseWriter, r *http.Request) {
	me := h.actorID(r)
	orders, err := h.St.Orders(r.Context(), storeFilter(me, "", "", "", false))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	out := []taskJSON{}
	for _, o := range orders {
		t := taskJSON{ID: o.ID, OrderRef: o.Ref, At: o.UpdatedAt}
		if o.IsTerminal() {
			t.State, t.Title = "done", "Settled"
		} else {
			p, actor, has := o.PhaseFor(me)
			if !has {
				continue // 没有阶段的单不进待办列表
			}
			t.Title = phaseTitles[p]
			if actor == order.ViewerYou {
				t.State = "you"
			} else {
				t.State = "run"
			}
		}
		out = append(out, t)
	}
	// 该你动手的排最前，其次进行中，最后已完成；同组按更新时间倒序。
	rank := map[string]int{"you": 0, "run": 1, "done": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].State] != rank[out[j].State] {
			return rank[out[i].State] < rank[out[j].State]
		}
		return out[i].At.After(out[j].At)
	})
	ok(w, map[string]any{"tasks": out})
}
