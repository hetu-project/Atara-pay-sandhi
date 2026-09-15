package api

import (
	"log"
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

// AdminOrderDetail 是单笔订单的全貌（本体 + 事件时间线 + 争议案卷）。只读。
func (h *Handler) AdminOrderDetail(w http.ResponseWriter, r *http.Request) {
	d, err := h.St.AdminOrderDetail(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, httpx.NotFound("order"))
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

// ── 写动作 ──

// AdminForceDelist 强制下架挂单。币留锁定（见 store 说明）。
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
	id := chi.URLParam(r, "id")
	if err := h.St.AdminSetWithdrawalReview(r.Context(), id, req.Flag); err != nil {
		httpx.Error(w, httpx.NotFound("withdrawal"))
		return
	}
	h.audit(r, "withdrawal.review", "withdrawal", id, req.Flag)
	ok(w, map[string]any{"ok": true})
}

// AdminVerifyWithdrawal 拿提现的 tx_hash 去链上核验真伪，并据结果打复核标记。
// mock 链无法核验（哈希是合成的），返回 supported=false、不改标记。
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
		// 不改标记，如实告诉前端这条链核不了。
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

// AdminKycList 列出身份核验记录。?status=review 只看待人工复核的那批。只读。
func (h *Handler) AdminKycList(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	rows, err := h.St.AdminKycList(r.Context(), status, limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"checks": rows})
}

// AdminKycDetail 按 reference 取单次核验详情（证件 + 风险警告）。只读。
func (h *Handler) AdminKycDetail(w http.ResponseWriter, r *http.Request) {
	d, err := h.St.AdminKycByReference(r.Context(), chi.URLParam(r, "reference"))
	if err != nil {
		httpx.Error(w, httpx.NotFound("kyc check"))
		return
	}
	ok(w, d)
}

// AdminAiPrompt 返回 AI 提示词：可编辑人设 + 锁死护栏。
func (h *Handler) AdminAiPrompt(w http.ResponseWriter, r *http.Request) {
	p, err := h.Svc.GetDeskPrompt(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, p)
}

// AdminSetAiPrompt 保存后台改过的人设。护栏改不了——只收 persona。
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

// AdminResetAiPrompt 恢复默认人设。
func (h *Handler) AdminResetAiPrompt(w http.ResponseWriter, r *http.Request) {
	if err := h.Svc.ResetDeskPrompt(r.Context()); err != nil {
		httpx.Error(w, err)
		return
	}
	h.audit(r, "ai.prompt", "setting", "desk_persona", "reset to default")
	ok(w, map[string]any{"ok": true})
}

// AdminAiStats 是 AI 调用聚合概览。只读。
func (h *Handler) AdminAiStats(w http.ResponseWriter, r *http.Request) {
	st, err := h.St.AdminAiStats(r.Context())
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, st)
}

// AdminAiCalls 是 AI 调用日志（最近在前）。只读。
func (h *Handler) AdminAiCalls(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminAiCalls(r.Context(), limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"calls": rows})
}

// AdminAiConversations 列出跟 Atara AI 聊过的用户。只读。
func (h *Handler) AdminAiConversations(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminAiConversations(r.Context(), store.DeskID, limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"conversations": rows})
}

// AdminAiThread 读某个用户跟 AI 的整段对话。只读。
func (h *Handler) AdminAiThread(w http.ResponseWriter, r *http.Request) {
	msgs, err := h.St.AdminAiThread(r.Context(), store.DeskID, chi.URLParam(r, "user_id"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"messages": msgs})
}

// AdminAudit 是操作审计列表（最近在前）。只读。
func (h *Handler) AdminAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := h.St.AdminAudit(r.Context(), limitParam(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"entries": rows})
}

// AdminBanUser 封禁/解封账户。body: {"banned": true|false}。
func (h *Handler) AdminBanUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Banned bool `json:"banned"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	// 不许封自己——否则一手把自己锁在门外，连解封都做不了。
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

// AdminRevokeMaker 撤销做市资格（approved→0）。已挂出的单要另走强制下架。
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

// audit 记一条后台操作。失败只记日志、不影响已成功的动作——审计缺一条比
// 让业务动作回滚轻。
func (h *Handler) audit(r *http.Request, action, targetType, targetID, detail string) {
	if err := h.St.LogAudit(r.Context(), h.actorID(r), action, targetType, targetID, detail); err != nil {
		log.Printf("admin audit: %s %s/%s: %v", action, targetType, targetID, err)
	}
}

// limitParam 读 ?limit=，非法或缺省交给 store 层兜底（那里有上限保护）。
func limitParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}
