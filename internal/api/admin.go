package api

import (
	"net/http"
	"strconv"

	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/go-chi/chi/v5"
)

// 管理后台的只读数据端点。全部挂在 reviewer 角色后面（见 router.go）。
//
// 提醒：这一版鉴权是 mock——X-Atara-User 头写谁就是谁，reviewer 角色能被
// 冒充。所以这些端点在接入真会话验签之前，不能把后端直接暴露到网络上。

func (h *Handler) AdminOverview(w http.ResponseWriter, r *http.Request) {
	c, err := h.St.AdminCounts(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, c)
}

func (h *Handler) AdminOrders(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminOrders(r.Context(), limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"orders": rows})
}

func (h *Handler) AdminWithdrawals(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminWithdrawals(r.Context(), limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"withdrawals": rows})
}

func (h *Handler) AdminOffers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminOffers(r.Context(), limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"offers": rows})
}

func (h *Handler) AdminUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminUsers(r.Context(), limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"users": rows})
}

func (h *Handler) AdminUserDetail(w http.ResponseWriter, r *http.Request) {
	d, err := h.St.AdminUserDetail(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.NotFound("user"))
		return
	}
	ok(w, d)
}

// ── 写动作 ──

// AdminForceDelist 强制下架挂单。币留锁定（见 store 说明）。
func (h *Handler) AdminForceDelist(w http.ResponseWriter, r *http.Request) {
	if err := h.St.AdminForceDelist(r.Context(), chi.URLParam(r, "id")); err != nil {
		httpx.Error(w, httpx.Fail(http.StatusConflict, "DELIST_FAILED", "",
			"挂单不存在，或已不是 active 状态"))
		return
	}
	ok(w, map[string]any{"ok": true})
}

// AdminReviewWithdrawal 打/撤提现复核标记（suspicious | held | cleared | 空）。
func (h *Handler) AdminReviewWithdrawal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Flag string `json:"flag"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	if !store.AdminReviewValues[req.Flag] {
		httpx.Error(w, httpx.Fail(http.StatusUnprocessableEntity, "BAD_FLAG", "flag",
			"flag must be one of: suspicious, held, cleared, or empty to clear"))
		return
	}
	if err := h.St.AdminSetWithdrawalReview(r.Context(), chi.URLParam(r, "id"), req.Flag); err != nil {
		httpx.Error(w, httpx.NotFound("withdrawal"))
		return
	}
	ok(w, map[string]any{"ok": true})
}

// limitParam 读 ?limit=，非法或缺省交给 store 层兜底（那里有上限保护）。
func limitParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}
