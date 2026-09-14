package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/advaita/atara-pay/internal/httpx"
)

// 讯飞实时语音听写（IAT）的接入点。写死是对的：这两个值变了签名原文也得跟着变，
// 做成可配只会让「签名对不上」多一个可能的出处。
const (
	iatHost = "iat-api.xfyun.cn"
	iatPath = "/v2/iat"
)

// IflytekToken 签一枚讯飞 WebSocket 的鉴权 URL 给前端。
//
// 这个接口只做签名，不碰音频：录音、推流、收文字全在浏览器里，
// WSS 是浏览器直连讯飞的，不过我们这一跳。后端存在的唯一理由是
// **APIKey / APISecret 不能出站**——签名算完就把密钥留在这里。
//
// 讯飞那边的 date 有效期约五分钟，所以这枚 URL 是短时的：前端每次开录音
// 都要重新来拿，不要缓存。
func (h *Handler) IflytekToken(w http.ResponseWriter, r *http.Request) {
	v := h.Cfg.Voice
	if !v.Configured() {
		// 500 而不是 404：接口在，是这台机器没配密钥。前端据此提示
		// 「语音没开」，而不是当成版本不匹配。
		httpx.Error(w, httpx.Fail(http.StatusInternalServerError, "VOICE_NOT_CONFIGURED", "",
			"voice dictation is not configured on this server"))
		return
	}

	// GMT HTTP Date。必须是 RFC1123 GMT 那个形式（讯飞按字面校验），
	// 而且下面签名用的和 URL 上带的必须是同一个字符串——分两次取会差一秒。
	date := time.Now().UTC().Format(http.TimeFormat)

	// 签名原文三行，换行是 \n，行内一个空格，末尾不能有多余空白。
	// 这段格式错一个字符，讯飞只回一句 handshake 失败，查起来没有线索。
	origin := fmt.Sprintf("host: %s\ndate: %s\nGET %s HTTP/1.1", iatHost, date, iatPath)

	mac := hmac.New(sha256.New, []byte(v.APISecret))
	mac.Write([]byte(origin))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	auth := fmt.Sprintf(
		`api_key="%s", algorithm="hmac-sha256", headers="host date request-line", signature="%s"`,
		v.APIKey, sig)

	q := url.Values{}
	q.Set("authorization", base64.StdEncoding.EncodeToString([]byte(auth)))
	q.Set("date", date)
	q.Set("host", iatHost)

	ok(w, map[string]any{
		"url":    fmt.Sprintf("wss://%s%s?%s", iatHost, iatPath, q.Encode()),
		"app_id": v.AppID,
	})
}
