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

// AppealMakerApplication 是申请人说「你判错了」的那个出口。
//
// 为什么必须有：预审判错了而没有任何路径能推翻它，这个商户就被永久锁在
// 门外了——他改也没用，因为他本来就没错。这不是流程问题，是系统里必须
// 存在一个能推翻机器的出口。它可以一个月零次，但不能不存在。
func (h *Handler) AppealMakerApplication(w http.ResponseWriter, r *http.Request) {
	var req app.MakerAppealReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	a, err := h.Svc.AppealMakerApplication(r.Context(), h.actorID(r), req)
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

// ReviewedMakerApplications 是审核历史（已审过的申请，最近在前）。只读。
func (h *Handler) ReviewedMakerApplications(w http.ResponseWriter, r *http.Request) {
	as, err := h.St.ReviewedMakerApps(r.Context(), limitParam(r))
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
	userID := chi.URLParam(r, "user_id")
	a, err := h.Svc.ReviewMakerApplication(r.Context(), h.actorID(r), userID, req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	h.audit(r, "maker.review", "application", userID, req.Stage+":"+req.Decision)
	ok(w, a)
}
