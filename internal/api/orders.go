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
	o, err := h.St.Order(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.NotFound("order"))
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
		out = append(out, h.toOrder(r.Context(), o, false))
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

func (h *Handler) Accept(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, func(id string) (*order.Order, error) {
		return h.Svc.Accept(r.Context(), h.actorID(r), id, h.confirmToken(r))
	})
}

func (h *Handler) Fund(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, func(id string) (*order.Order, error) {
		return h.Svc.Fund(r.Context(), h.actorID(r), id, order.ActorCounterparty)
	})
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
