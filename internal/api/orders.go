package api

import (
	"net/http"

	"github.com/advaita/atara-pay/internal/app"
	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/go-chi/chi/v5"
)

func (h *Handler) Parse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	d, err := h.Svc.Parse(r.Context(), h.actorID(r), req.Text)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, d)
}

func (h *Handler) Quote(w http.ResponseWriter, r *http.Request) {
	var req app.QuoteReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	resp, err := h.Svc.Quote(r.Context(), h.actorID(r), req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, resp)
}

func (h *Handler) CreateOrder(w http.ResponseWriter, r *http.Request) {
	var req app.CreateOrderReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	o, err := h.Svc.CreateConditional(r.Context(), h.actorID(r), h.confirmToken(r), req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, h.toOrder(r.Context(), o, true))
}

func (h *Handler) GetOrder(w http.ResponseWriter, r *http.Request) {
	// 走 Svc.Order 而不是仓储：确认数与合约地址是链的事实，
	// 不落库，每次读单都要现问链一次。
	o, err := h.Svc.Order(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, h.toOrder(r.Context(), o, true))
}

func (h *Handler) ListOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	os, err := h.St.Orders(r.Context(), storeFilter(h.actorID(r), q.Get("kind"), q.Get("state"), q.Get("terminal"), q.Get("open") == "true"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	out := make([]orderJSON, 0, len(os))
	for _, o := range os {
		full, err := h.Svc.Order(r.Context(), o.ID)
		if err != nil {
			full = o
		}
		out = append(out, h.toOrder(r.Context(), full, false))
	}
	ok(w, map[string]any{"orders": out})
}

func (h *Handler) OrderEvents(w http.ResponseWriter, r *http.Request) {
	evs, err := h.St.Events(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"events": evs})
}

func (h *Handler) ReleaseConsensus(w http.ResponseWriter, r *http.Request) {
	d, err := h.Svc.ReleaseConsensus(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, d)
}

// ── 转移 ──

func (h *Handler) Confirm(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, func(id string) (*order.Order, error) {
		return h.Svc.ConfirmReceipt(r.Context(), h.actorID(r), id)
	})
}

func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, func(id string) (*order.Order, error) {
		return h.Svc.Cancel(r.Context(), h.actorID(r), id)
	})
}

func (h *Handler) Dispute(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, func(id string) (*order.Order, error) {
		return h.Svc.Dispute(r.Context(), h.actorID(r), id)
	})
}

func (h *Handler) Evidence(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FileRef string `json:"file_ref"`
		Proof   string `json:"proof"`
	}
	_ = httpx.Decode(r, &req)
	if req.Proof == "" {
		req.Proof = "Delivery record"
	}
	h.transition(w, r, func(id string) (*order.Order, error) {
		return h.Svc.Evidence(r.Context(), h.actorID(r), id, req.FileRef, req.Proof)
	})
}

// Accept 是 OTC 的承诺点。taker 卖币时 body 里带 via 选入金方式。
func (h *Handler) Accept(w http.ResponseWriter, r *http.Request) {
	var req app.AcceptReq
	_ = httpx.Decode(r, &req)
	h.transition(w, r, func(id string) (*order.Order, error) {
		return h.Svc.Accept(r.Context(), h.actorID(r), id, h.confirmToken(r), req)
	})
}

// Fund 把钱送进托管合约：内置钱包签名转入，或往合约地址打款后我们监听。
func (h *Handler) Fund(w http.ResponseWriter, r *http.Request) {
	var req app.FundReq
	_ = httpx.Decode(r, &req)
	h.transition(w, r, func(id string) (*order.Order, error) {
		return h.Svc.Fund(r.Context(), h.actorID(r), id, h.confirmToken(r), req)
	})
}

// Escrow 给前端画那个观察窗：合约地址、确认数、tx。
func (h *Handler) Escrow(w http.ResponseWriter, r *http.Request) {
	o, err := h.Svc.Order(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	body := map[string]any{
		"contract":      o.EscrowAddr,
		"network":       o.EscrowNetwork,
		"explorer":      h.Svc.Ch.ExplorerURL(o.Asset, o.EscrowAddr),
		"asset":         o.Asset,
		"amount":        o.Amount.String(),
		"funding_via":   o.FundingVia,
		"tx_hash":       o.EscrowTx,
		"confirmations": o.Confirmations,
		"required":      o.Required,
		"needs_funding": o.NeedsFunding(),
	}
	if p, err := h.Svc.Ch.Position(r.Context(), o.ID); err == nil && p != nil {
		body["position_status"] = p.Status
	}
	ok(w, body)
}

// Match 是快捷交易的撮合拍：先给候选，再评估。
// 对手方还没出现就跑评估，评的是谁？
func (h *Handler) Match(w http.ResponseWriter, r *http.Request) {
	var req app.MatchReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	out, err := h.Svc.Match(r.Context(), req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, out)
}

func (h *Handler) Receipt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FileRef string `json:"file_ref"`
	}
	_ = httpx.Decode(r, &req)
	h.transition(w, r, func(id string) (*order.Order, error) {
		return h.Svc.Receipt(r.Context(), h.actorID(r), id, req.FileRef)
	})
}

func (h *Handler) transition(w http.ResponseWriter, r *http.Request, fn func(string) (*order.Order, error)) {
	o, err := fn(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, h.toOrder(r.Context(), o, true))
}
