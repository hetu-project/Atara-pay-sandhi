package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/advaita/atara-pay/internal/agent"
	"github.com/advaita/atara-pay/internal/domain/condition"
	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/ledger"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/advaita/atara-pay/internal/store"
)

type CreateOrderReq struct {
	CounterpartyID string           `json:"counterparty_id"`
	Asset          string           `json:"asset"`
	Amount         string           `json:"amount"`
	Note           string           `json:"note"`
	CardID         string           `json:"card_id"`
	Conditions     []condition.Atom `json:"conditions"`
	FallbackDays   int              `json:"fallback_days"`
}

// CreateConditional 建一笔条件支付托管单。
// R1 一笔一工单：提交即建单，锁币与建单在同一个事务里。
func (s *Service) CreateConditional(ctx context.Context, ownerID, confirmToken string, req CreateOrderReq) (*order.Order, error) {
	if req.FallbackDays <= 0 {
		req.FallbackDays = 14
	}
	if !money.IsCrypto(req.Asset) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "UNKNOWN_ASSET", "asset",
			fmt.Sprintf("%q is not a settleable asset", req.Asset))
	}
	amt, err := money.Parse(req.Amount, req.Asset)
	if err != nil || !amt.IsPositive() {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "INVALID_AMOUNT", "amount", "amount must be greater than zero")
	}
	if req.CounterpartyID == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "NO_COUNTERPARTY", "counterparty_id", "pick who gets paid")
	}
	if _, err := s.St.User(ctx, req.CounterpartyID); err != nil {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "NO_COUNTERPARTY", "counterparty_id", "no such counterparty")
	}
	if err := condition.Validate(req.Conditions); err != nil {
		code := "INVALID_CONDITION"
		if errors.Is(err, condition.ErrTooMany) {
			code = "TOO_MANY_CONDITIONS"
		}
		return nil, httpx.Fail(http.StatusBadRequest, code, "conditions", err.Error())
	}
	if err := s.requireBalance(ctx, ownerID, req.Asset, amt.Value); err != nil {
		return nil, err
	}
	cardID, err := s.checkCard(ctx, ownerID, req.CardID, amt)
	if err != nil {
		return nil, err
	}

	// R2 动钱必确认：无金额豁免。
	if err := s.Confirm.Consume(confirmToken, ownerID,
		Digest("order", req.CounterpartyID, req.Asset, amt.String())); err != nil {
		return nil, err
	}

	c := condition.Compile(req.Conditions, req.FallbackDays)
	now := time.Now().UTC()
	o := &order.Order{
		ID: store.NewID(), Ref: Ref(), Kind: order.ConditionalTransfer,
		OwnerID: ownerID, CounterpartyID: req.CounterpartyID,
		Asset: req.Asset, Amount: amt.Value, Note: req.Note, CardID: cardID,
		State: order.Locked, CreatedAt: now, UpdatedAt: now,
		Cond: &order.Conditional{
			Main: c.Main, WaitingOn: c.Waiting, Text: c.Text,
			FallbackDays: req.FallbackDays, DisputeWindowSecs: int(s.Cfg.T.Dispute.Seconds()),
		},
		Conds: req.Conditions,
	}
	o.StateDeadline = s.deadlineFor(o)

	err = s.St.Tx(ctx, func(tx *sql.Tx) error {
		if err := s.St.InsertOrder(tx, o); err != nil {
			return err
		}
		if err := ledger.Lock(tx, ownerID, o.Asset, o.Amount, o.ID, "escrow funded on create"); err != nil {
			if errors.Is(err, store.ErrInsufficient) {
				return httpx.Fail(http.StatusUnprocessableEntity, "INSUFFICIENT_BALANCE", "amount", err.Error())
			}
			return err
		}
		if err := s.St.SpendCard(tx, cardID, amt.USD()); err != nil {
			return err
		}
		return store.AppendEvent(tx, o.ID, "", string(order.Locked), order.ActorOwner,
			"Order created · funds locked in escrow", map[string]string{"condition": c.Text})
	})
	if err != nil {
		return nil, err
	}
	return s.St.Order(ctx, o.ID)
}

// ── 人触发的转移 ──

func (s *Service) ConfirmReceipt(ctx context.Context, actorID, orderID string) (*order.Order, error) {
	o, err := s.mine(ctx, actorID, orderID)
	if err != nil {
		return nil, err
	}
	return s.advance(ctx, o.ID, order.EvConfirm, order.ActorOwner, order.Releasing,
		"You confirmed receipt · running release consensus", nil, nil)
}

// Evidence 是对手方上传凭证。凭证来自对手方，所以 actor 是 counterparty。
func (s *Service) Evidence(ctx context.Context, actorID, orderID, fileRef, proof string) (*order.Order, error) {
	o, err := s.St.Order(ctx, orderID)
	if err != nil {
		return nil, httpx.NotFound("order")
	}
	if o.CounterpartyID != actorID {
		return nil, httpx.Fail(http.StatusForbidden, "NOT_YOURS", "", "only the counterparty uploads evidence here")
	}
	return s.advance(ctx, o.ID, order.EvEvidence, order.ActorCounterparty, order.AwaitingMe,
		fmt.Sprintf("%s uploaded the %s · your window is open", proof, proof),
		map[string]string{"file_ref": fileRef, "proof": proof}, nil)
}

func (s *Service) Cancel(ctx context.Context, actorID, orderID string) (*order.Order, error) {
	o, err := s.mine(ctx, actorID, orderID)
	if err != nil {
		return nil, err
	}
	reason := "Cancelled — funds returned to your balance. No default recorded."
	return s.advance(ctx, o.ID, order.EvCancel, order.ActorOwner, order.Cancelled, reason, nil,
		func(tx *sql.Tx, oo *order.Order) error { return s.releaseReservation(tx, oo) })
}

func (s *Service) Dispute(ctx context.Context, actorID, orderID string) (*order.Order, error) {
	o, err := s.mine(ctx, actorID, orderID)
	if err != nil {
		return nil, err
	}
	return s.advance(ctx, o.ID, order.EvDispute, order.ActorOwner, order.Disputed,
		"You disputed within the window — escalated to review. Funds stay locked.", nil, nil)
}

// Accept 是 OTC 的承诺点：Passkey 与授权卡在这里校验，taker 卖币时在这里锁币。
func (s *Service) Accept(ctx context.Context, actorID, orderID, confirmToken string) (*order.Order, error) {
	o, err := s.mine(ctx, actorID, orderID)
	if err != nil {
		return nil, err
	}
	if o.Kind != order.OTCTake || o.OTC == nil {
		return nil, httpx.Fail(http.StatusConflict, "INVALID_TRANSITION", "", "not an OTC order")
	}
	amt := money.New(o.Amount, o.Asset)
	if err := s.Confirm.Consume(confirmToken, actorID, Digest("accept", o.ID)); err != nil {
		return nil, err
	}
	sellSide := o.OTC.Side == "sell"
	if sellSide {
		if err := s.requireBalance(ctx, actorID, o.Asset, o.Amount); err != nil {
			return nil, err
		}
		if _, err := s.checkCard(ctx, actorID, o.CardID, amt); err != nil {
			return nil, err
		}
	}
	return s.advance(ctx, o.ID, order.EvAccept, order.ActorOwner, order.S1,
		"You confirmed · waiting on the counterparty to fund escrow", nil,
		func(tx *sql.Tx, oo *order.Order) error {
			if !sellSide {
				return nil
			}
			return ledger.Lock(tx, actorID, oo.Asset, oo.Amount, oo.ID, "taker escrow on accept")
		})
}

// Fund 是 maker 注资托管。种子商家没有客户端，调度器会代跑这一步——
// 但接口是真的，接真商家时不用改。
func (s *Service) Fund(ctx context.Context, actorID, orderID string, actor order.Actor) (*order.Order, error) {
	o, err := s.St.Order(ctx, orderID)
	if err != nil {
		return nil, httpx.NotFound("order")
	}
	if o.OTC == nil {
		return nil, httpx.Fail(http.StatusConflict, "INVALID_TRANSITION", "", "not an OTC order")
	}
	// taker 买币时 maker 出币，而 maker 的币在挂单那一刻就锁进托管了——
	// 「挂出即锁币」的意思就是买家看到的可成交量真的在托管里。
	// 所以这一站不再锁一次，否则同一批币会被锁两遍。
	return s.advance(ctx, o.ID, order.EvFund, actor, order.S3,
		"Escrow funded — the coins are locked. Your turn to send the bank transfer.", nil, nil)
}

func (s *Service) Receipt(ctx context.Context, actorID, orderID, fileRef string) (*order.Order, error) {
	o, err := s.mine(ctx, actorID, orderID)
	if err != nil {
		return nil, err
	}
	if fileRef == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "RECEIPT_REQUIRED", "file_ref",
			"attach the bank receipt — release is decided on the receipt, not on anyone's say-so")
	}
	return s.advance(ctx, o.ID, order.EvReceipt, order.ActorOwner, order.S4,
		"Receipt uploaded · the platform is verifying it against the escrow",
		map[string]string{"file_ref": fileRef},
		func(tx *sql.Tx, oo *order.Order) error {
			_, err := store.InsertReceipt(tx, oo.ID, actorID, fileRef)
			return err
		})
}

// releaseReservation 把撤销/超时的 OTC 单预留的可成交量还给挂单。
func (s *Service) releaseReservation(tx *sql.Tx, o *order.Order) error {
	if o.Kind != order.OTCTake || o.OTC == nil {
		return nil
	}
	return store.ReserveQty(tx, o.OTC.OfferID, o.Amount)
}

func (s *Service) mine(ctx context.Context, actorID, orderID string) (*order.Order, error) {
	o, err := s.St.Order(ctx, orderID)
	if err != nil {
		return nil, httpx.NotFound("order")
	}
	if o.OwnerID != actorID {
		return nil, httpx.Fail(http.StatusForbidden, "NOT_YOURS", "", "this order belongs to another account")
	}
	return o, nil
}

// ── 系统推进 ──

// Tick 由调度器调用：把一笔到期的工单推到它的下一站。
func (s *Service) Tick(ctx context.Context, o *order.Order) error {
	switch o.Kind {
	case order.ConditionalTransfer:
		return s.tickConditional(ctx, o)
	case order.OTCTake:
		return s.tickOTC(ctx, o)
	}
	return nil
}

func (s *Service) tickConditional(ctx context.Context, o *order.Order) error {
	c := o.Cond
	switch o.State {
	case order.Locked:
		if c != nil && c.Main == condition.Immediate {
			_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Releasing,
				"Conditions are empty — releasing straight away", nil, nil)
			return err
		}
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.AwaitingCounterparty,
			"Waiting on "+waitingText(c), nil, nil)
		return err

	case order.AwaitingCounterparty:
		// on_date / API data 这两档不需要对方做动作，到点直接进放行共识。
		if c != nil && c.Main == condition.OnDate {
			_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Releasing,
				"Condition met — running release consensus", nil, nil)
			return err
		}
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.AwaitingMe,
			"The counterparty marked it delivered · evidence attached to the record", nil, nil)
		return err

	case order.AwaitingMe:
		if c != nil && c.Main == condition.ProofWindow {
			// 窗口内没人异议就自动放行——沉默即同意，这是凭证档的定义。
			_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Releasing,
				"Dispute window closed with no objection — releasing", nil, nil)
			return err
		}
		// 到期未履约：原路退回并负向回写。与主动撤销严格区分。
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Expired,
			"Nobody acted before the deadline — escrow returned, default recorded", nil, nil)
		return err

	case order.Releasing:
		return s.runReleaseConsensus(ctx, o)
	}
	return nil
}

func (s *Service) tickOTC(ctx context.Context, o *order.Order) error {
	switch o.State {
	case order.Match:
		// 吃单后没确认就放着：这只是没成交，不是违约。
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Cancelled,
			"Match expired before you confirmed — recorded as unfilled, no default", nil,
			func(tx *sql.Tx, oo *order.Order) error { return s.releaseReservation(tx, oo) })
		return err
	case order.S1:
		_, err := s.Fund(ctx, "", o.ID, order.ActorSystem)
		return err
	case order.S3:
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Expired,
			"Payment window missed · escrow returned to the counterparty", nil,
			func(tx *sql.Tx, oo *order.Order) error { return s.releaseReservation(tx, oo) })
		return err
	case order.S4:
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.S5,
			"Verified and released · settlement complete", nil, nil)
		return err
	}
	return nil
}

// runReleaseConsensus 跑放行共识。它没有裁量权：只能放行，或拦下转人工。
func (s *Service) runReleaseConsensus(ctx context.Context, o *order.Order) error {
	peerName, disputes := "the counterparty", 0
	if u, err := s.St.User(ctx, o.CounterpartyID); err == nil {
		peerName = u.DisplayName
	}
	if m, err := s.St.Merchant(ctx, o.CounterpartyID); err == nil {
		disputes = m.Disputes
	}
	condText := ""
	if o.Cond != nil {
		condText = o.Cond.Text
	}
	dec, err := s.Ag.Vote(ctx, agent.ReleaseInput{
		OrderID: o.ID, Asset: o.Asset, AmountUSD: money.New(o.Amount, o.Asset).USD().String(),
		PeerName: peerName, PeerDisputes: disputes, ConditionText: condText,
	})
	if err != nil {
		return err
	}
	to := order.Released
	if dec.Outcome == agent.OutcomeHoldForReview {
		to = order.AwaitingMe
	}
	_, err = s.advance(ctx, o.ID, order.EvReleaseVote, order.ActorAgent, to, dec.Rationale,
		map[string]string{"outcome": string(dec.Outcome)}, nil)
	return err
}

// ReleaseConsensus 把放行共识的结果读出来给前端画共识环。
func (s *Service) ReleaseConsensus(ctx context.Context, orderID string) (agent.Decision, error) {
	o, err := s.St.Order(ctx, orderID)
	if err != nil {
		return agent.Decision{}, httpx.NotFound("order")
	}
	disputes := 0
	if m, err := s.St.Merchant(ctx, o.CounterpartyID); err == nil {
		disputes = m.Disputes
	}
	condText := ""
	if o.Cond != nil {
		condText = o.Cond.Text
	}
	return s.Ag.Vote(ctx, agent.ReleaseInput{
		OrderID: o.ID, Asset: o.Asset, AmountUSD: money.New(o.Amount, o.Asset).USD().String(),
		PeerDisputes: disputes, ConditionText: condText,
	})
}

func waitingText(c *order.Conditional) string {
	if c == nil {
		return "the counterparty"
	}
	switch c.WaitingOn {
	case condition.WaitApprove:
		return "the counterparty to deliver"
	case condition.WaitEvidence:
		return "the counterparty to upload evidence"
	case condition.WaitData:
		return "the metric to hit its target"
	case condition.WaitTime:
		return "the date"
	}
	return "the counterparty"
}
