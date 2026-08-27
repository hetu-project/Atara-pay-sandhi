package api

import (
	"net/http"

	"github.com/advaita/atara-pay/internal/app"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/go-chi/chi/v5"
)

func (h *Handler) ListOffers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	side := q.Get("side")
	if side == "" {
		side = "buy"
	}
	os, err := h.Svc.Offers(r.Context(), side, q.Get("asset"), q.Get("fiat"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	out := make([]offerJSON, 0, len(os))
	for _, o := range os {
		out = append(out, toOffer(o))
	}
	ok(w, map[string]any{"offers": out})
}

func (h *Handler) MyOffers(w http.ResponseWriter, r *http.Request) {
	os, err := h.St.Offers(r.Context(), store.OfferFilter{Maker: h.actorID(r)})
	if err != nil {
		httpx.Error(w, err)
		return
	}
	out := make([]offerJSON, 0, len(os))
	for _, o := range os {
		out = append(out, toOffer(o))
	}
	ok(w, map[string]any{"offers": out})
}

func (h *Handler) GetOffer(w http.ResponseWriter, r *http.Request) {
	o, err := h.St.Offer(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.NotFound("offer"))
		return
	}
	ok(w, toOffer(o))
}

// Dossier 是对手方资质件的展开。缺件也照发——买家自己给缺口定价。
func (h *Handler) Dossier(w http.ResponseWriter, r *http.Request) {
	o, err := h.St.Offer(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.NotFound("offer"))
		return
	}
	labels := map[string]string{
		"kyc": "Identity verified by the platform", "pof": "Proof of funds",
		"stm": "Bank statement", "poa": "Power of attorney",
		"sow": "Source of wealth", "chain": "On-chain provenance",
	}
	type doc struct {
		Key    string `json:"key"`
		Label  string `json:"label"`
		Shared bool   `json:"will_share"`
	}
	docs := make([]doc, 0, len(labels))
	for _, k := range []string{"kyc", "pof", "stm", "poa", "sow", "chain"} {
		shared := o.Merchant != nil && o.Merchant.Docs[k]
		docs = append(docs, doc{k, labels[k], shared})
	}
	ok(w, map[string]any{"maker": toOffer(o).Maker, "documents": docs})
}

func (h *Handler) Assessment(w http.ResponseWriter, r *http.Request) {
	a, err := h.Svc.Assess(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, a)
}

func (h *Handler) CreateOffer(w http.ResponseWriter, r *http.Request) {
	var req app.CreateOfferReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	o, err := h.Svc.CreateOffer(r.Context(), h.actorID(r), h.confirmToken(r), req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toOffer(o))
}

func (h *Handler) Delist(w http.ResponseWriter, r *http.Request) {
	if err := h.Svc.Delist(r.Context(), h.actorID(r), chi.URLParam(r, "id")); err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"status": "delisted"})
}

func (h *Handler) Take(w http.ResponseWriter, r *http.Request) {
	var req app.TakeReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	o, err := h.Svc.Take(r.Context(), h.actorID(r), chi.URLParam(r, "id"), req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, h.toOrder(r.Context(), o, true))
}

func storeFilter(owner, kind, state, terminal string, open bool) store.OrderFilter {
	return store.OrderFilter{Owner: owner, Kind: kind, State: state, Terminal: terminal, Open: open}
}
