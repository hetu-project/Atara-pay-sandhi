package api

import (
	"log"
	"net/http"
	"strconv"

	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/go-chi/chi/v5"
)

// Admin console data endpoints. All sit behind admin session auth (RequireAdmin,
// see router.go) -- a Bearer token issued at login, not the spoofable X-Atara-User
// header. Authorization (the admin role) still lives in atara-pay.

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

// AdminOrderDetail is a single order in full (the order + event timeline + dispute case). Read-only.
func (h *Handler) AdminOrderDetail(w http.ResponseWriter, r *http.Request) {
	d, err := h.St.AdminOrderDetail(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.NotFound("order"))
		return
	}
	ok(w, d)
}

// AdminResolveDispute settles a disputed order: release (pay the buyer) | refund (return to the seller).
func (h *Handler) AdminResolveDispute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Decision string `json:"decision"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.Svc.ResolveDispute(r.Context(), h.actorID(r), id, req.Decision); err != nil {
		httpx.Error(w, err)
		return
	}
	h.audit(r, "dispute.resolve", "order", id, req.Decision)
	// Return the admin detail shape (lowercase fields), consistent with GET /admin/orders/{id}.
	d, err := h.St.AdminOrderDetail(r.Context(), id)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, d)
}

func (h *Handler) AdminWithdrawals(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminWithdrawals(r.Context(), limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"withdrawals": rows})
}

func (h *Handler) AdminTrends(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days == 0 {
		days = 30
	}
	pts, err := h.St.AdminTrends(r.Context(), days)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"points": pts})
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

// -- Write actions --

// AdminForceDelist force-delists an offer. Funds stay locked (see the store note).
func (h *Handler) AdminForceDelist(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.St.AdminForceDelist(r.Context(), id); err != nil {
		httpx.Error(w, httpx.Fail(http.StatusConflict, "DELIST_FAILED", "",
			"挂单不存在，或已不是 active 状态"))
		return
	}
	h.audit(r, "offer.delist", "offer", id, "")
	ok(w, map[string]any{"ok": true})
}

// AdminReviewWithdrawal sets/clears a withdrawal review flag (suspicious | held | cleared | empty).
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
	id := chi.URLParam(r, "id")
	if err := h.St.AdminSetWithdrawalReview(r.Context(), id, req.Flag); err != nil {
		httpx.Error(w, httpx.NotFound("withdrawal"))
		return
	}
	h.audit(r, "withdrawal.review", "withdrawal", id, req.Flag)
	ok(w, map[string]any{"ok": true})
}

// AdminVerifyWithdrawal takes the withdrawal tx_hash, verifies it on chain, and sets
// a review flag from the result. The mock chain cannot verify (synthetic hashes) --
// it returns supported=false and leaves the flag untouched.
func (h *Handler) AdminVerifyWithdrawal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tx, exists := h.St.AdminWithdrawalTx(r.Context(), id)
	if !exists {
		httpx.Error(w, httpx.NotFound("withdrawal"))
		return
	}
	if tx == "" {
		httpx.Error(w, httpx.Fail(http.StatusUnprocessableEntity, "NO_TX", "",
			"这笔提现还没有交易哈希，无从核验"))
		return
	}
	v, err := h.Svc.Ch.VerifyTx(r.Context(), tx)
	if err != nil {
		httpx.Error(w, httpx.Fail(http.StatusBadGateway, "CHAIN_ERROR", "", "链上查询失败："+err.Error()))
		return
	}
	if !v.Supported {
		// Don't change the flag; honestly tell the frontend this chain cannot verify.
		ok(w, map[string]any{"verification": v, "message": "当前链无法核验（mock 链的哈希是合成的）"})
		return
	}
	flag := "suspicious"
	detail := "链上未找到该交易"
	if v.Found && v.Success {
		flag = "cleared"
		detail = "链上核实成功"
	} else if v.Found {
		detail = "交易存在但执行失败"
	}
	if err := h.St.AdminSetWithdrawalReview(r.Context(), id, flag); err != nil {
		httpx.Error(w, err)
		return
	}
	h.audit(r, "withdrawal.verify", "withdrawal", id, detail)
	ok(w, map[string]any{"verification": v, "flag": flag, "message": detail})
}

// AdminKycList lists KYC checks. ?status=review shows only those awaiting human review. Read-only.
func (h *Handler) AdminKycList(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	rows, err := h.St.AdminKycList(r.Context(), status, limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"checks": rows})
}

// AdminKycDetail returns one check by reference (document fields + risk warnings). Read-only.
func (h *Handler) AdminKycDetail(w http.ResponseWriter, r *http.Request) {
	d, err := h.St.AdminKycByReference(r.Context(), chi.URLParam(r, "reference"))
	if err != nil {
		httpx.Error(w, httpx.NotFound("kyc check"))
		return
	}
	ok(w, d)
}

// AdminAiPrompt returns the AI prompt: the editable persona + the locked guardrails.
func (h *Handler) AdminAiPrompt(w http.ResponseWriter, r *http.Request) {
	p, err := h.Svc.GetDeskPrompt(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, p)
}

// AdminSetAiPrompt saves an edited persona. Guardrails can't be changed -- it only accepts persona.
func (h *Handler) AdminSetAiPrompt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Persona string `json:"persona"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	if err := h.Svc.SetDeskPrompt(r.Context(), req.Persona, h.actorID(r)); err != nil {
		httpx.Error(w, err)
		return
	}
	custom := len(req.Persona) > 0
	detail := "reset to default"
	if custom {
		detail = "updated"
	}
	h.audit(r, "ai.prompt", "setting", "desk_persona", detail)
	ok(w, map[string]any{"ok": true})
}

// AdminResetAiPrompt restores the default persona.
func (h *Handler) AdminResetAiPrompt(w http.ResponseWriter, r *http.Request) {
	if err := h.Svc.ResetDeskPrompt(r.Context()); err != nil {
		httpx.Error(w, err)
		return
	}
	h.audit(r, "ai.prompt", "setting", "desk_persona", "reset to default")
	ok(w, map[string]any{"ok": true})
}

// AdminAiStats is the aggregate AI-call overview. Read-only.
func (h *Handler) AdminAiStats(w http.ResponseWriter, r *http.Request) {
	st, err := h.St.AdminAiStats(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, st)
}

// AdminAiCalls is the AI call log (most recent first). Read-only.
func (h *Handler) AdminAiCalls(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminAiCalls(r.Context(), limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"calls": rows})
}

// AdminAiConversations lists users who have chatted with Atara AI. Read-only.
func (h *Handler) AdminAiConversations(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminAiConversations(r.Context(), store.DeskID, limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"conversations": rows})
}

// AdminAiThread reads a user's full conversation with the AI. Read-only.
func (h *Handler) AdminAiThread(w http.ResponseWriter, r *http.Request) {
	msgs, err := h.St.AdminAiThread(r.Context(), store.DeskID, chi.URLParam(r, "user_id"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"messages": msgs})
}

// AdminAudit is the action audit list (most recent first). Read-only.
func (h *Handler) AdminAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminAudit(r.Context(), limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"entries": rows})
}

// AdminBanUser bans/unbans an account. body: {"banned": true|false}.
func (h *Handler) AdminBanUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Banned bool `json:"banned"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	// You can't ban yourself -- otherwise you lock yourself out with no way to unban.
	if id == h.actorID(r) {
		httpx.Error(w, httpx.Fail(http.StatusConflict, "CANNOT_BAN_SELF", "", "你不能封禁自己"))
		return
	}
	if err := h.St.AdminSetUserBanned(r.Context(), id, req.Banned); err != nil {
		httpx.Error(w, httpx.NotFound("user"))
		return
	}
	action := "user.ban"
	if !req.Banned {
		action = "user.unban"
	}
	h.audit(r, action, "user", id, "")
	ok(w, map[string]any{"ok": true})
}

// AdminRevokeMaker revokes maker approval (approved->0). Already-posted offers must be force-delisted separately.
func (h *Handler) AdminRevokeMaker(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.St.AdminRevokeMaker(r.Context(), id); err != nil {
		httpx.Error(w, httpx.Fail(http.StatusConflict, "REVOKE_FAILED", "",
			"该账户没有已过审的做市资格"))
		return
	}
	h.audit(r, "user.revoke_maker", "user", id, "")
	ok(w, map[string]any{"ok": true})
}

// audit records one admin action. On failure it only logs -- it does not undo the
// action that already succeeded; a missing audit row is lighter than a rollback.
func (h *Handler) audit(r *http.Request, action, targetType, targetID, detail string) {
	if err := h.St.LogAudit(r.Context(), h.actorID(r), action, targetType, targetID, detail); err != nil {
		log.Printf("admin audit: %s %s/%s: %v", action, targetType, targetID, err)
	}
}

// limitParam reads ?limit=; invalid or missing is left to the store layer (which caps it).
func limitParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}
