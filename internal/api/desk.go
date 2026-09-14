package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/store"
)

// DeskMessage 收下发给 Atara AI 的一句话，用 SSE 把回答一段一段送回去。
//
// 为什么不复用 POST /threads/{peer}/messages：那个端点回的是一条完整的
// JSON 消息，契约固定，前端和 contract-check 都按那个形状在读。对话台要的是
// 一条流——同一个端点回两种东西，两边都得先猜「这次是哪种」。
//
// 事件：
//
//	user  {id, body, created_at}   你那句已经存下了，界面可以定稿
//	delta {text}                   回答的下一段
//	done  {id, body}               整段存库完成
//	error {code, message}          出事了；此前的 delta 仍然有效
func (h *Handler) DeskMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}

	/* Flusher 拿不到就没法流。这只会在被别的中间件包过一层 ResponseWriter
	   时发生——早说比让前端对着一个永远不吐字的连接干等好。 */
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.Error(w, httpx.Fail(http.StatusInternalServerError, "NO_STREAMING", "",
			"this server cannot stream responses"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	/* nginx 默认会把上游响应攒够一块再发（proxy_buffering on）。对 SSE 来说
	   那等于没有流：本地一个字一个字出，上线之后整段一起蹦出来，而两边代码
	   一模一样——这种只在生产复现的差异最难查。这个头让 nginx 对这条响应
	   关掉缓冲，不必去改 nginx 配置。 */
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	send := func(event string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	/* 客户端走了就别再问模型要字了——每一段都在花钱，而没人会看到。
	   r.Context() 在连接断开时会被取消，所以直接拿它当这次对话的生命周期。 */
	ctx := r.Context()

	reply, err := h.Svc.DeskReply(ctx, h.actorID(r), req.Body,
		func(chunk string) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return send("delta", map[string]string{"text": chunk})
		})

	if err != nil {
		/* 到这儿 HTTP 状态码已经发出去了（200），改不了了——所以错误只能
		   走事件通道。前端按 error 事件处理，不要指望状态码。 */
		code, msg := "DESK_FAILED", "the assistant could not answer"
		if e, isAPI := err.(*httpx.Err); isAPI {
			code, msg = e.Code, e.Message
		} else {
			// 模型侧的原始错误只进日志：里面可能带上游的 URL 或请求 id。
			log.Printf("desk: %v", err)
		}
		_ = send("error", map[string]string{"code": code, "message": msg})
		return
	}

	_ = send("done", map[string]any{"id": reply.ID, "body": reply.Body, "created_at": reply.CreatedAt})
}

// DeskInfo 告诉前端这条线程是谁、能不能真的回话。
// 前端据此决定要不要把输入框的提示写成「暂时只能留言」。
func (h *Handler) DeskInfo(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{
		"peer_id":    store.DeskID,
		"name":       "Atara AI",
		"subtitle":   "Verification and listing desk",
		"configured": h.Svc.DeskConfigured(),
	})
}
