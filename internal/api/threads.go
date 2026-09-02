package api

import (
	"github.com/advaita/atara-pay/internal/domain/order"
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
	me := h.actorID(r)
	out, err := h.Svc.Thread(r.Context(), me, chi.URLParam(r, "peer"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	// app 层回的是领域结构体，裸序列化出去是 ID/Amount/Kind 这样的驼峰，
	// 而且没有 phase/actor/rail——和 /orders 完全不是一个形状。
	// 同一种东西在两个端点上两种形状，前端就得写两套类型，迟早出错。
	if os, isOrders := out["orders"].([]*order.Order); isOrders {
		js := make([]orderJSON, 0, len(os))
		for _, o := range os {
			js = append(js, h.toOrder(r.Context(), me, o, false))
		}
		out["orders"] = js
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
