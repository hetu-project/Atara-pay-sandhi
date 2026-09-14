package kyc

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Webhook 的两个头。
const (
	HeaderSignature = "X-IDA-Signature"
	HeaderTimestamp = "X-IDA-Timestamp"
)

// MaxSkew 是允许的时间差。超出就当重放拒掉。
//
// 五分钟是 ID Analyzer 文档给的数。放宽等于给抓到过一份回调的人一个
// 更长的窗口重新打进来——那一份的签名永远是有效的，能挡住它的只有时间。
const MaxSkew = 5 * time.Minute

var (
	ErrNoSignature  = errors.New("webhook 没带签名")
	ErrBadSignature = errors.New("webhook 签名对不上")
	ErrStale        = errors.New("webhook 时间戳超出允许范围")
)

// VerifyWebhook 校验一条回调确实来自 ID Analyzer。
//
// 签名原文是 时间戳 + "." + 原始请求体。必须是**原始字节**：
// 先解成 map 再 Marshal 回去，键序和空白都会变，签名必然对不上。
//
// 没有这一步的话，这个接口就是「任何人 POST 一段 JSON 过来就能把自己
// 标成已通过 KYC」——它改的是放不放行一个账户去收别人的法币。
func VerifyWebhook(secret, sigHeader, tsHeader string, body []byte, now time.Time) error {
	if secret == "" {
		// 没配密钥就没法校验。这里必须是错误而不是放行：
		// 「没配置」和「验过了」是两件事，混成一件就等于永远不校验。
		return errors.New("webhook 签名密钥没配")
	}
	if sigHeader == "" || tsHeader == "" {
		return ErrNoSignature
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(tsHeader), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: 时间戳不是整秒: %q", ErrNoSignature, tsHeader)
	}
	if d := now.Sub(time.Unix(secs, 0)); d > MaxSkew || d < -MaxSkew {
		return ErrStale
	}

	want := Sign(secret, tsHeader, body)
	// 定长比较。用 == 的话，比较提前退出的时机会随前缀匹配的长度变化，
	// 逐字节试探就能把签名问出来。
	if !hmac.Equal([]byte(want), []byte(strings.TrimSpace(sigHeader))) {
		return ErrBadSignature
	}
	return nil
}

// Sign 按 ID Analyzer 的规矩算一条签名：v1=HEX(HMAC_SHA256(secret, ts + "." + body))。
func Sign(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strings.TrimSpace(timestamp)))
	m.Write([]byte("."))
	m.Write(body)
	return "v1=" + hex.EncodeToString(m.Sum(nil))
}
