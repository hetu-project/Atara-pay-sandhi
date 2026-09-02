package api

import (
	"net/http"

	"github.com/advaita/atara-pay/internal/app"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/go-chi/chi/v5"
)

func (h *Handler) MakerApplication(w http.ResponseWriter, r *http.Request) {
	a, err := h.Svc.MakerApplication(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, a)
}

func (h *Handler) SubmitMakerApplication(w http.ResponseWriter, r *http.Request) {
	var req app.MakerSubmitReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	a, err := h.Svc.SubmitMakerApplication(r.Context(), h.actorID(r), req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, a)
}

func (h *Handler) PendingMakerApplications(w http.ResponseWriter, r *http.Request) {
	as, err := h.St.PendingMakerApps(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"applications": as})
}

// ReviewMakerApplication 是真人审核入口，挂在 reviewer 角色后面。
func (h *Handler) ReviewMakerApplication(w http.ResponseWriter, r *http.Request) {
	var req app.MakerReviewReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	a, err := h.Svc.ReviewMakerApplication(r.Context(), h.actorID(r),
		chi.URLParam(r, "user_id"), req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, a)
}
