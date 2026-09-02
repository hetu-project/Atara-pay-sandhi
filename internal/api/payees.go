package api

import (
	"net/http"

	"github.com/advaita/atara-pay/internal/app"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/go-chi/chi/v5"
)

func (h *Handler) Payees(w http.ResponseWriter, r *http.Request) {
	ps, err := h.St.Payees(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"payees": ps})
}

func (h *Handler) AddPayee(w http.ResponseWriter, r *http.Request) {
	var req app.AddPayeeReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	p, err := h.Svc.AddPayee(r.Context(), h.actorID(r), req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, p)
}

func (h *Handler) DeletePayee(w http.ResponseWriter, r *http.Request) {
	if err := h.St.DeletePayee(r.Context(), h.actorID(r), chi.URLParam(r, "id")); err != nil {
		httpx.Error(w, httpx.NotFound("payee"))
		return
	}
	ok(w, map[string]string{"status": "deleted"})
}

func (h *Handler) Withdrawals(w http.ResponseWriter, r *http.Request) {
	ws, err := h.St.Withdrawals(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"withdrawals": ws})
}

// CreateWithdrawal 收下前端四步提现的全部内容。链上那笔转账由用户自己签，
// 这里只记意图与合规材料——但动钱必确认，签名档令牌照样要。
func (h *Handler) CreateWithdrawal(w http.ResponseWriter, r *http.Request) {
	var req app.WithdrawReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	wd, err := h.Svc.CreateWithdrawal(r.Context(), h.actorID(r), h.confirmToken(r), req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, wd)
}

// BroadcastWithdrawal 收下用户自己签出来的交易哈希。
// 平台不代发——这一步是「我签完了」，不是「你帮我发」。
func (h *Handler) BroadcastWithdrawal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TxHash string `json:"tx_hash"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	wd, err := h.Svc.BroadcastWithdrawal(r.Context(), h.actorID(r), chi.URLParam(r, "id"), req.TxHash)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, wd)
}
