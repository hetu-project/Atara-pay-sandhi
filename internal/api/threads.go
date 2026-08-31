package api

import (
	"net/http"

	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/go-chi/chi/v5"
)

// 一个对手方一条线程。聊天、订单卡、系统播报共用一条流——
// 消息归人，状态归事，但它们出现在同一个地方。

func (h *Handler) Threads(w http.ResponseWriter, r *http.Request) {
	ts, err := h.St.Threads(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"threads": ts})
}

func (h *Handler) Thread(w http.ResponseWriter, r *http.Request) {
	out, err := h.Svc.Thread(r.Context(), h.actorID(r), chi.URLParam(r, "peer"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, out)
}

func (h *Handler) PostChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	m, err := h.Svc.PostChat(r.Context(), h.actorID(r), chi.URLParam(r, "peer"), req.Body)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, m)
}
