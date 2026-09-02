// Package app 是用例编排层。事务边界全在这里，handler 不碰 *sql.Tx。
//
// 非托管下多了一条纪律：链上动作在事务之外先做，做成了再进事务记账。
// 跟链之间没有分布式事务，假装有才是错的。
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
	"github.com/advaita/atara-pay/internal/chain"
	"github.com/advaita/atara-pay/internal/config"
	"github.com/advaita/atara-pay/internal/domain/condition"
	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/advaita/atara-pay/internal/settlement"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/shopspring/decimal"
)

type Service struct {
	St      *store.Store
	Ag      agent.Suite
	Ch      chain.Chain
	Cfg     config.Config
	Confirm *auth.Confirmations
}

func New(st *store.Store, ag agent.Suite, ch chain.Chain, cfg config.Config, c *auth.Confirmations) *Service {
	return &Service{St: st, Ag: ag, Ch: ch, Cfg: cfg, Confirm: c}
}

// Ref 生成工单号。单据引用的就是这个号，只读不可改。
func Ref() string {
	const hexes = "0123456789ABCDEF"
	b := make([]byte, 6)
	for i := range b {
		b[i] = hexes[rand.Intn(len(hexes))]
	}
	return "ATR-" + string(b)
}

// Digest 是确认令牌绑定的操作摘要：换了金额或对手方，旧令牌就不认了。
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
		case order.Fund:
			// 入金站：等的是链。每秒回来看一眼确认数，直到兜底超时。
			if o.FundingVia == "" {
				return at(T.Fallback) // 还没选付款方式，等人
			}
			return at(time.Second)
		case order.Locked:
			// 锁定是一个瞬时事实，不是一段等待——停一拍让人看清，然后就走。
			return at(time.Second)
		case order.AwaitingCounterparty:
			return at(T.CondSettle)
		case order.AwaitingMe:
			if o.Cond != nil && o.Cond.Main == condition.ProofWindow {
				return at(T.Dispute) // 窗口内不异议就自动放行
			}
			return at(T.Fallback)
		case order.Releasing:
			return at(time.Second) // 下一拍跑放行共识
		}
	case order.OTCTake:
		switch o.State {
		case order.Match:
			return at(T.OTCMatch)
		case order.S1:
			// 买方向：查挂单锁仓，瞬时；卖方向：等链上确认数。
			if o.OTC != nil && o.OTC.Side == "buy" {
				return at(T.OTCBind)
			}
			if o.FundingVia == "" {
				return at(T.OTCS1) // 还没选付款方式
			}
			return at(time.Second)
		case order.S3:
			// 买方向：你付法币，到点是你逾期。
			// 卖方向：对方付法币，到点是对方把钱打来了——两件事，两个时长。
			if o.OTC != nil && o.OTC.Side == "sell" {
				return at(T.OTCTheirPay)
			}
			return at(T.OTCS3)
		case order.S3V:
			return at(T.OTCVerify)
		case order.S4:
			return at(T.OTCS4)
		}
	}
	return nil
}

// advance 是状态推进的唯一入口。
// chainDo 在事务之外先跑——链动作没有回滚，所以它必须先成功。
func (s *Service) advance(ctx context.Context, orderID string, ev order.Event, actor order.Actor,
	to order.State, reason string, payload map[string]string,
	chainDo func(*order.Order) (settlement.Outcome, error),
	extra func(*sql.Tx, *order.Order) error) (*order.Order, error) {
	return s.advanceAuth(ctx, orderID, ev, actor, to, reason, payload, chain.ReleaseAuth{}, chainDo, extra)
}

// advanceAuth 与 advance 相同，但显式带上放行依据。
// 只有真的会走到 completed 的两处需要它：条件支付的放行共识，与 OTC 的核验后放款。
func (s *Service) advanceAuth(ctx context.Context, orderID string, ev order.Event, actor order.Actor,
	to order.State, reason string, payload map[string]string, auth chain.ReleaseAuth,
	chainDo func(*order.Order) (settlement.Outcome, error),
	extra func(*sql.Tx, *order.Order) error) (*order.Order, error) {

	o, err := s.St.Order(ctx, orderID)
	if err != nil {
		return nil, httpx.NotFound("order")
	}
	if o.IsTerminal() {
		return nil, httpx.Fail(http.StatusConflict, "ORDER_TERMINAL", "",
			"this order reached a final state and is read-only")
	}
	if err := s.hydrateAddrs(ctx, o); err != nil {
		return nil, err
	}
	// 先校验转移合不合法，再动链——不合法的转移不该在链上留下痕迹。
	term, err := order.Check(o.Kind, o.State, ev, actor, to)
	if err != nil {
		return nil, httpx.Fail(http.StatusConflict, "INVALID_TRANSITION", "", err.Error())
	}

	out := settlement.Outcome{Action: "none"}
	if chainDo != nil {
		if out, err = chainDo(o); err != nil {
			return nil, chainErr(err)
		}
	}
	if term != order.TermNone && chainDo == nil {
		if out, err = settlement.Settle(ctx, s.Ch, o, term, auth); err != nil {
			return nil, chainErr(err)
		}
	}

	var id string
	err = s.St.Tx(ctx, func(tx *sql.Tx) error {
		fresh, err := store.OrderTx(tx, orderID)
		if err != nil {
			return httpx.NotFound("order")
		}
		fresh.OwnerAddr, fresh.PayeeAddr = o.OwnerAddr, o.PayeeAddr
		fresh.OTC = o.OTC
		from := fresh.State
		if err := fresh.Apply(ev, actor, to); err != nil {
			return httpx.Fail(http.StatusConflict, "INVALID_TRANSITION", "", err.Error())
		}
		if payload != nil {
			if v, ok := payload["funding_via"]; ok {
				fresh.FundingVia = v
			}
			if v, ok := payload["escrow_tx"]; ok {
				fresh.EscrowTx = v
			}
		}
		if fresh.Terminal != order.TermNone {
			if err := settlement.Record(tx, s.St, fresh, fresh.Terminal, out); err != nil {
				return err
			}
			if fresh.AllowanceID != "" && fresh.Terminal != order.TermCompleted {
				_ = s.St.SpendAllowance(tx, fresh.AllowanceID, money.New(fresh.Amount, fresh.Asset).USD().Neg())
			}
		} else if out.Action != "none" {
			if err := store.LogChain(tx, fresh.OwnerID, store.ChainEvent{
				Kind: out.Action, Asset: fresh.Asset, Amount: fresh.Amount,
				TxHash: out.TxHash, OrderID: fresh.ID, Memo: reason,
			}); err != nil {
				return err
			}
		}
		if extra != nil {
			if err := extra(tx, fresh); err != nil {
				return err
			}
		}
		fresh.StateDeadline = s.deadlineFor(fresh)
		if err := store.SaveState(tx, fresh); err != nil {
			return err
		}
		if err := store.AppendEvent(tx, fresh.ID, string(from), string(fresh.State), actor, reason, payload); err != nil {
			return err
		}
		// 状态变化落进线程：一个对手方一条流，播报和订单卡在一起
		if fresh.CounterpartyID != "" && reason != "" {
			_ = store.PostTx(tx, fresh.OwnerID, fresh.CounterpartyID, &model.Message{
				Author: "system", Kind: "system", Body: reason, OrderID: fresh.ID,
			})
		}
		id = fresh.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Order(ctx, id)
}

// Order 读一笔工单，并把链上事实贴上去。
// 确认数这类信息属于链，不落库——落了就会和链不一致。
func (s *Service) Order(ctx context.Context, id string) (*order.Order, error) {
	o, err := s.St.Order(ctx, id)
	if err != nil {
		return nil, httpx.NotFound("order")
	}
	if err := s.hydrateAddrs(ctx, o); err != nil {
		return nil, err
	}
	addr, net := s.Ch.EscrowAddress(o.Asset)
	o.EscrowAddr, o.EscrowNetwork = addr, net
	o.Required = chain.Confirmations
	if d, err := s.Ch.Deposit(ctx, o.ID); err == nil && d != nil {
		o.Confirmations = d.Confirmations
		if d.TxHash != "" {
			o.EscrowTx = d.TxHash
		}
	}
	if p, err := s.Ch.Position(ctx, o.ID); err == nil && p != nil && o.EscrowTx == "" {
		o.EscrowTx = p.TxHash
	}
	return o, nil
}

func (s *Service) hydrateAddrs(ctx context.Context, o *order.Order) error {
	if u, err := s.St.User(ctx, o.OwnerID); err == nil {
		o.OwnerAddr = u.Address
	}
	if o.CounterpartyID != "" {
		if u, err := s.St.User(ctx, o.CounterpartyID); err == nil {
			o.PayeeAddr = u.Address
		}
	}
	return nil
}

// checkAllowance 校验额度。过期与撤销是两回事，报错也要分开说。
func (s *Service) checkAllowance(ctx context.Context, ownerID, id string, amt money.Amount) (string, error) {
	if id == "" {
		as, err := s.St.Allowances(ctx, ownerID)
		if err != nil || len(as) == 0 {
			return "", nil
		}
		for _, a := range as {
			if a.Kind == "person" {
				id = a.ID
				break
			}
		}
		if id == "" {
			id = as[0].ID
		}
	}
	a, err := s.St.Allowance(ctx, id)
	if err != nil {
		return "", httpx.NotFound("allowance")
	}
	if a.OwnerID != ownerID {
		return "", httpx.Fail(http.StatusForbidden, "ALLOWANCE_FOREIGN", "allowance_id",
			"that allowance belongs to another account")
	}
	if a.Status == "revoked" {
		return "", httpx.Fail(http.StatusUnprocessableEntity, "ALLOWANCE_REVOKED", "allowance_id",
			fmt.Sprintf("%s's allowance was revoked on-chain", a.Spender))
	}
	if a.Expired() {
		return "", httpx.Fail(http.StatusUnprocessableEntity, "ALLOWANCE_EXPIRED", "allowance_id",
			fmt.Sprintf("%s's allowance expired on %s", a.Spender, a.ExpiresAt.Format("Jan 2, 2006"))).
			With(&httpx.Remedy{Action: "edit_allowance", Value: a.ID, Label: "Extend the allowance"})
	}
	usd := amt.USD()
	if a.PerPayment.IsPositive() && usd.GreaterThan(a.PerPayment) {
		return "", httpx.Fail(http.StatusUnprocessableEntity, "OVER_CAP", "amount",
			fmt.Sprintf("$%s is over the $%s per-payment cap on %s", usd.Round(0), a.PerPayment.Round(0), a.Spender)).
			With(&httpx.Remedy{Action: "change_allowance", Label: "Use an allowance that covers this amount"})
	}
	if a.WindowCap.IsPositive() && a.Used.Add(usd).GreaterThan(a.WindowCap) {
		left := a.WindowCap.Sub(a.Used)
		return "", httpx.Fail(http.StatusUnprocessableEntity, "OVER_QUOTA", "amount",
			fmt.Sprintf("only $%s left in %s's %s window", left.Round(0), a.Spender, a.Cycle)).
			With(&httpx.Remedy{Action: "request_approval", Label: "Send it for your approval instead"})
	}

	// 最后一道：链上策略说了算。
	//
	// 上面那几条查的是平台库里的记录。如果链上这份支配权已经被撤销或余量
	// 不够，而平台仅凭自己的记录就放行，那「额度是签进链上的支配权」这句话
	// 就是假的——链是权威，平台的记录只是缓存。
	//
	// 链上没有这份策略时（额度未上链，或用 mock 链）返回 nil，退回只看
	// 平台侧记录——那种情况下平台记录就是唯一的事实，不必假装有链上依据。
	st, err := s.Ch.AllowanceState(ctx, a.ID)
	if err != nil {
		return "", err
	}
	if st == nil {
		return a.ID, nil
	}
	if !st.Live {
		return "", httpx.Fail(http.StatusUnprocessableEntity, "ALLOWANCE_REVOKED", "allowance_id",
			fmt.Sprintf("%s's allowance is not live on-chain", a.Spender))
	}
	if st.Available.IsPositive() && usd.GreaterThan(st.Available) {
		return "", httpx.Fail(http.StatusUnprocessableEntity, "OVER_QUOTA", "amount",
			fmt.Sprintf("only $%s left on-chain for %s", st.Available.Round(0), a.Spender)).
			With(&httpx.Remedy{Action: "request_approval", Label: "Send it for your approval instead"})
	}
	return a.ID, nil
}

// requireOnChain 查链上余额够不够。注意是查链，不是查平台的账——
// 平台没有账。
func (s *Service) requireOnChain(ctx context.Context, addr, asset string, amt decimal.Decimal) error {
	bal, err := s.Ch.Balance(ctx, addr, asset)
	if err != nil {
		return err
	}
	if bal.LessThan(amt) {
		return httpx.Fail(http.StatusUnprocessableEntity, "INSUFFICIENT_BALANCE", "amount",
			fmt.Sprintf("your wallet holds %s %s, this needs %s", bal, asset, amt)).
			With(&httpx.Remedy{Action: "fund_external", Label: "Pay from an external wallet instead"})
	}
	return nil
}

func chainErr(err error) error {
	if e, ok := err.(*httpx.Err); ok {
		return e
	}
	return httpx.Fail(http.StatusUnprocessableEntity, "CHAIN_REJECTED", "", err.Error())
}
