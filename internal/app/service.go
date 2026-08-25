// Package app 是用例编排层。事务边界全在这里，handler 不碰 *sql.Tx。
package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	"github.com/advaita/atara-pay/internal/agent"
	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/config"
	"github.com/advaita/atara-pay/internal/domain/condition"
	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/ledger"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/shopspring/decimal"
)

type Service struct {
	St      *store.Store
	Ag      agent.Suite
	Cfg     config.Config
	Confirm *auth.Confirmations
}

func New(st *store.Store, ag agent.Suite, cfg config.Config, c *auth.Confirmations) *Service {
	return &Service{St: st, Ag: ag, Cfg: cfg, Confirm: c}
}

// Ref 生成工单号。单据与联系人引用的就是这个号，只读不可改。
func Ref() string {
	const hexes = "0123456789ABCDEF"
	b := make([]byte, 6)
	for i := range b {
		b[i] = hexes[rand.Intn(len(hexes))]
	}
	return "ATR-" + string(b)
}

// Digest 是支付确认令牌绑定的操作摘要：换了金额或对手方，旧令牌就不认了。
func Digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// deadlineFor 算出工单进入某个状态后该在什么时候被系统推一把。
// 返回 nil 表示这一站在等人，不等钟。
func (s *Service) deadlineFor(o *order.Order) *time.Time {
	now := time.Now()
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	T := s.Cfg.T

	if o.IsTerminal() {
		return nil
	}
	switch o.Kind {
	case order.ConditionalTransfer:
		switch o.State {
		case order.Locked:
			// 锁定是一个瞬时事实，不是一段等待——停一拍让人看清，然后就走。
			return at(time.Second)
		case order.AwaitingCounterparty:
			return at(T.CondSettle)
		case order.AwaitingMe:
			if o.Cond != nil && o.Cond.Main == condition.ProofWindow {
				return at(T.Dispute) // 窗口内不异议就自动放行
			}
			return at(T.Fallback) // 到期未履约转人工
		case order.Releasing:
			return at(time.Second) // 下一拍跑放行共识
		}
	case order.OTCTake:
		switch o.State {
		case order.Match:
			return at(T.OTCMatch)
		case order.S1:
			return at(T.OTCS1)
		case order.S3:
			return at(T.OTCS3)
		case order.S4:
			return at(T.OTCS4)
		}
	}
	return nil
}

// advance 是状态推进的唯一入口。每一次推进都在一个事务里完成：
// 校验转移 → 资金处置 → 落状态 → 追加事件。
func (s *Service) advance(ctx context.Context, orderID string, ev order.Event, actor order.Actor,
	to order.State, reason string, payload map[string]string, extra func(*sql.Tx, *order.Order) error) (*order.Order, error) {

	var id string
	err := s.St.Tx(ctx, func(tx *sql.Tx) error {
		o, err := store.OrderTx(tx, orderID)
		if err != nil {
			return httpx.NotFound("order")
		}
		if o.IsTerminal() {
			return httpx.Fail(http.StatusConflict, "ORDER_TERMINAL", "",
				"this order reached a final state and is read-only")
		}
		from := o.State

		// 托管快照必须在推进之前取——推进之后状态已变，就算不出钱在谁手里了。
		esc := ledger.EscrowOf(o)

		if err := o.Apply(ev, actor, to); err != nil {
			return httpx.Fail(http.StatusConflict, "INVALID_TRANSITION", "", err.Error())
		}
		if o.Terminal != order.TermNone {
			if err := ledger.Settle(tx, s.St, o, esc, o.Terminal); err != nil {
				return err
			}
		}
		if extra != nil {
			if err := extra(tx, o); err != nil {
				return err
			}
		}
		o.StateDeadline = s.deadlineFor(o)
		if err := store.SaveState(tx, o); err != nil {
			return err
		}
		if err := store.AppendEvent(tx, o.ID, string(from), string(o.State), actor, reason, payload); err != nil {
			return err
		}
		id = o.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.St.Order(ctx, id)
}

// checkCard 校验授权卡。R2 的第三段：周期已用/上限、单笔上限。
func (s *Service) checkCard(ctx context.Context, ownerID, cardID string, amt money.Amount) (string, error) {
	if cardID == "" {
		cards, err := s.St.Cards(ctx, ownerID)
		if err != nil || len(cards) == 0 {
			return "", nil
		}
		cardID = cards[0].ID // 本人卡
	}
	c, err := s.St.Card(ctx, cardID)
	if err != nil {
		return "", httpx.NotFound("card")
	}
	if c.OwnerID != ownerID {
		return "", httpx.Fail(http.StatusForbidden, "CARD_FOREIGN", "card_id", "that card belongs to another account")
	}
	if !c.Enabled {
		return "", httpx.Fail(http.StatusUnprocessableEntity, "CARD_DISABLED", "card_id",
			fmt.Sprintf("%s is switched off — set an allowlist before enabling", c.Name))
	}
	usd := amt.USD()
	if c.PerDealCap != nil && usd.GreaterThan(*c.PerDealCap) {
		return "", httpx.Fail(http.StatusUnprocessableEntity, "OVER_CAP", "amount",
			fmt.Sprintf("$%s is over the $%s per-payment cap on %s", usd.Round(0), c.PerDealCap.Round(0), c.Name)).
			With(&httpx.Remedy{Action: "change_card", Label: "Pay with a card that allows this amount"})
	}
	if c.Quota != nil && c.Used.Add(usd).GreaterThan(*c.Quota) {
		left := c.Quota.Sub(c.Used)
		return "", httpx.Fail(http.StatusUnprocessableEntity, "OVER_QUOTA", "amount",
			fmt.Sprintf("only $%s left in %s's %s budget", left.Round(0), c.Name, c.Cycle)).
			With(&httpx.Remedy{Action: "request_approval", Label: "Send it for your approval instead"})
	}
	return c.ID, nil
}

func (s *Service) requireBalance(ctx context.Context, userID, asset string, amt decimal.Decimal) error {
	w, err := s.St.Wallet(ctx, userID, asset)
	if err != nil {
		return err
	}
	if w.Available.LessThan(amt) {
		return httpx.Fail(http.StatusUnprocessableEntity, "INSUFFICIENT_BALANCE", "amount",
			fmt.Sprintf("you have %s %s available, this needs %s", w.Available, asset, amt))
	}
	return nil
}
