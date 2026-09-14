package api

import (
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/advaita/atara-pay/internal/app"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/kyc"
)

// StartKyc 开一次身份核验会话。POST /api/v1/kyc/session
//
// 回的是 reference / URL / 二维码。API key 不在里面，也永远不会在里面。
func (h *Handler) StartKyc(w http.ResponseWriter, r *http.Request) {
	sess, err := h.Svc.StartKyc(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, sess)
}

// KycStatus 是前端的查询接口。GET /api/v1/kyc/status
//
// 默认顺带去上游拉一次——本地开发和用内置预设时根本收不到 webhook，
// 只等回调的话这个状态永远停在 pending。带 ?refresh=0 可以只读库。
func (h *Handler) KycStatus(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") != "0"
	st, err := h.Svc.KycStatusFor(r.Context(), h.actorID(r), refresh)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, st)
}

// IDAnalyzerWebhook 收 ID Analyzer 的回调。POST /webhooks/idanalyzer
//
// 挂在 /api/v1 外面，因为它不带我们的身份头——打进来的是 ID Analyzer，
// 不是某个登录用户。它的身份由 HMAC 签名证明，不由 X-Atara-User 证明。
//
// 三件事的顺序不能换：
//  1. 读原始字节。签名是按原文算的，先反序列化再编码回去必然对不上。
//  2. 验签。没配密钥就拒收——"没配置"和"验过了"是两件事，混成一件
//     等于这个接口谁都能打，而它改的是放不放行一个账户去收别人的法币。
//  3. 才是解析和落库。
func (h *Handler) IDAnalyzerWebhook(w http.ResponseWriter, r *http.Request) {
	// 限一下读取量，免得一份异常请求把内存吃掉。
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		httpx.Error(w, httpx.Fail(http.StatusBadRequest, "BAD_BODY", "", "could not read body"))
		return
	}

	if err := kyc.VerifyWebhook(h.Cfg.KYC.WebhookSecret,
		r.Header.Get(kyc.HeaderSignature), r.Header.Get(kyc.HeaderTimestamp),
		body, time.Now()); err != nil {
		// 日志里不带 body：那份 JSON 里有证件号和人像的引用。
		log.Printf("kyc webhook: 拒收: %v", err)
		httpx.Error(w, httpx.Fail(http.StatusUnauthorized, "BAD_SIGNATURE", "",
			"signature verification failed"))
		return
	}

	res, err := kyc.ParseResult(body)
	if err != nil {
		httpx.Error(w, httpx.Fail(http.StatusBadRequest, "BAD_PAYLOAD", "", "payload is not valid JSON"))
		return
	}
	if err := h.Svc.IngestWebhook(r.Context(), res, body); err != nil {
		// 认不出的会话重投多少次都是同一个结果，回 200 收下不再追。
		if errors.Is(err, app.ErrKycUnknownSession) {
			log.Printf("kyc webhook: 丢弃一条指向未知会话的回调: %q", res.DocuPass)
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}
		// 其余的回 500 让它重投——对方失败后会自动重试 4 次
		//（约 10 分钟、1 小时、12 小时、24 小时）。这里回 200 等于把这次结果丢了。
		log.Printf("kyc webhook: 落库 %s: %v", res.DocuPass, err)
		httpx.Error(w, httpx.Fail(http.StatusInternalServerError, "INGEST_FAILED", "",
			"could not record the result"))
		return
	}
	// 对方要求 10 秒内拿到 200，否则算投递失败。
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
