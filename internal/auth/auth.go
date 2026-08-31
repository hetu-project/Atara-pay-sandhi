// Package auth 是一期的鉴权与支付确认。
//
// 鉴权是 mock：X-Atara-User 直接注入 actor，没有密码也没有会话。
// 但支付确认令牌是真实实现——生成、绑定操作摘要、一次性消费、过期
// 全都照真的做，接真 Passkey 时只换验签那一步。
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/advaita/atara-pay/internal/httpx"
)

type ctxKey int

const userKey ctxKey = 1

const (
	HeaderUser    = "X-Atara-User"
	HeaderConfirm = "X-Atara-Confirmation"
)

type Lookup func(ctx context.Context, handle string) (*model.User, error)

// Middleware 把 actor 放进请求上下文。没带头就落到 demo 用户，
// 因为一期不做注册，前端也没有登录页。
func Middleware(defaultHandle string, lookup Lookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handle := r.Header.Get(HeaderUser)
			if handle == "" {
				handle = defaultHandle
			}
			u, err := lookup(r.Context(), handle)
			if err != nil {
				httpx.Error(w, httpx.Fail(http.StatusUnauthorized, "UNKNOWN_ACTOR", "", "no such user: "+handle))
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
		})
	}
}

func Actor(ctx context.Context) *model.User {
	u, _ := ctx.Value(userKey).(*model.User)
	return u
}

// ── 支付确认令牌 ──

const confirmTTL = 120 * time.Second

// Grade 是确认的分级。前端把这件事说得很清楚：
// Passkey 签名动钱，普通按钮只表示"我承诺接这单"。
// 后端不能把两者当成一回事——否则要么签名成了摆设，
// 要么接单也要摸指纹。
type Grade string

const (
	// GradeSignature：动钱。Passkey 签的是那笔链上转账本身。
	GradeSignature Grade = "signature"
	// GradeCommit：只承诺，不动钱。接单、"我已经打款了"属于这一档。
	GradeCommit Grade = "commit"
)

type token struct {
	userID string
	digest string
	grade  Grade
	expiry time.Time
}

// Confirmations 保管短时、一次性的支付确认令牌。
// R2 动钱必确认：每一笔资金流出都要过支付确认面板 + Passkey，无金额豁免。
type Confirmations struct {
	mu sync.Mutex
	m  map[string]token
}

func NewConfirmations() *Confirmations { return &Confirmations{m: map[string]token{}} }

// Issue 签发一枚绑定到 (用户, 操作摘要) 的令牌。
// digest 是「这次要确认的到底是哪笔」——换了金额或对手方，旧令牌就不认了。
func (c *Confirmations) Issue(userID, digest string, g Grade) (string, time.Time) {
	if g == "" {
		g = GradeSignature
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	t := hex.EncodeToString(b)
	exp := time.Now().Add(confirmTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[t] = token{userID: userID, digest: digest, grade: g, expiry: exp}
	return t, exp
}

// Consume 校验并作废令牌。一次性——重放一笔已确认的支付不该再通过。
// Consume 校验并作废令牌。need 是这次操作要求的最低档：
// 要求签名档时，一张只承诺过的令牌不能放行。
func (c *Confirmations) Consume(raw, userID, digest string, need Grade) error {
	if need == "" {
		need = GradeSignature
	}
	if raw == "" {
		msg := "this moves money — sign it with your passkey first"
		if need == GradeCommit {
			msg = "confirm the order first"
		}
		return httpx.Fail(http.StatusUnauthorized, "CONFIRMATION_REQUIRED", "", msg)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.m[raw]
	if !ok {
		return httpx.Fail(http.StatusUnauthorized, "CONFIRMATION_INVALID", "", "confirmation already used or unknown")
	}
	delete(c.m, raw)
	if time.Now().After(t.expiry) {
		return httpx.Fail(http.StatusUnauthorized, "CONFIRMATION_INVALID", "", "confirmation expired")
	}
	if t.userID != userID {
		return httpx.Fail(http.StatusUnauthorized, "CONFIRMATION_INVALID", "", "confirmation belongs to another account")
	}
	if t.digest != "" && digest != "" && t.digest != digest {
		return httpx.Fail(http.StatusUnauthorized, "CONFIRMATION_INVALID", "",
			"the payment changed after you confirmed it — confirm again")
	}
	// 承诺档不能冒充签名档。反过来可以：签名比承诺更强。
	if need == GradeSignature && t.grade != GradeSignature {
		return httpx.Fail(http.StatusUnauthorized, "SIGNATURE_REQUIRED", "",
			"this moves funds — it needs a passkey signature, not just a confirmation")
	}
	return nil
}
