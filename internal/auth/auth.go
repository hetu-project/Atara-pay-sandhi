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

type token struct {
	userID string
	digest string
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
func (c *Confirmations) Issue(userID, digest string) (string, time.Time) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	t := hex.EncodeToString(b)
	exp := time.Now().Add(confirmTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[t] = token{userID: userID, digest: digest, expiry: exp}
	return t, exp
}

// Consume 校验并作废令牌。一次性——重放一笔已确认的支付不该再通过。
func (c *Confirmations) Consume(raw, userID, digest string) error {
	if raw == "" {
		return httpx.Fail(http.StatusUnauthorized, "CONFIRMATION_REQUIRED", "",
			"this moves money — confirm with your passkey first")
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
	return nil
}
