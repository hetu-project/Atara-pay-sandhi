package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/advaita/atara-pay/internal/agent"
	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/chain"
	"github.com/advaita/atara-pay/internal/domain/condition"
	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/advaita/atara-pay/internal/settlement"
	"github.com/advaita/atara-pay/internal/store"
)

type CreateOrderReq struct {
	CounterpartyID string           `json:"counterparty_id"`
	Asset          string           `json:"asset"`
	Amount         string           `json:"amount"`
	Note           string           `json:"note"`
	AllowanceID    string           `json:"allowance_id"`
	Conditions     []condition.Atom `json:"conditions"`
	FallbackDays   int              `json:"fallback_days"`
}

// CreateConditional 建一笔条件支付托管单。
//
// 非托管：这一步**不动钱**。工单建出来停在 fund 站，
// 合约在等这一方入金——所以确认档是「承诺」，不是「签名」。
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
	peer, err := s.St.User(ctx, req.CounterpartyID)
	if err != nil {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "NO_COUNTERPARTY", "counterparty_id", "no such counterparty")
	}
	if err := condition.Validate(req.Conditions); err != nil {
		code := "INVALID_CONDITION"
		if errors.Is(err, condition.ErrTooMany) {
			code = "TOO_MANY_CONDITIONS"
		}
		return nil, httpx.Fail(http.StatusBadRequest, code, "conditions", err.Error())
	}
	allowID, err := s.checkAllowance(ctx, ownerID, req.AllowanceID, amt)
	if err != nil {
		return nil, err
	}
	// 建单只是承诺这笔单的条款；动钱在 fund 那一步签名。
	if err := s.Confirm.Consume(ctx, confirmToken, ownerID,
		Digest("order", req.CounterpartyID, req.Asset, amt.String()), auth.GradeCommit); err != nil {
		return nil, err
	}

	c := condition.Compile(req.Conditions, req.FallbackDays)
	addr, net := s.Ch.EscrowAddress(req.Asset)
	now := time.Now().UTC()
	o := &order.Order{
		ID: store.NewID(), Ref: Ref(), Kind: order.ConditionalTransfer,
		OwnerID: ownerID, CounterpartyID: req.CounterpartyID,
		Asset: req.Asset, Amount: amt.Value, Note: req.Note, AllowanceID: allowID,
		State: order.Fund, CreatedAt: now, UpdatedAt: now,
		EscrowAddr: addr, EscrowNetwork: net,
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
		if err := s.St.SpendAllowance(tx, allowID, amt.USD()); err != nil {
			return err
		}
		if err := store.AppendEvent(tx, o.ID, "", string(order.Fund), order.ActorOwner,
			"Order created · the escrow contract is waiting for your deposit",
			map[string]string{"condition": c.Text}); err != nil {
			return err
		}
		return store.PostTx(tx, ownerID, peer.ID, &model.Message{
			Author: "system", Kind: "order",
			Body: "Order created · the escrow contract is waiting for your deposit", OrderID: o.ID,
		})
	})
	if err != nil {
		return nil, err
	}
	return s.Order(ctx, o.ID)
}

// ── 入金：非托管的第一拍 ──

type FundReq struct {
	Via string `json:"via"` // wallet | external
}

// Fund 把钱送进托管合约。两条路：
//
//	wallet   —— 内置钱包签名转入，Passkey 签的就是这笔转账（签名档）
//	external —— 用户自己的钱包往合约地址打款，我们监听入金（承诺档，"我已经打款了"）
//
// 两条路殊途同归：确认数走满，订单才离开 fund 站。
func (s *Service) Fund(ctx context.Context, actorID, orderID, confirmToken string, req FundReq) (*order.Order, error) {
	o, err := s.Order(ctx, orderID)
	if err != nil {
		return nil, err
	}
	payer, err := s.payerOf(ctx, o)
	if err != nil {
		return nil, err
	}
	if payer.ID != actorID {
		return nil, httpx.Fail(http.StatusForbidden, "NOT_YOURS", "",
			"only the party sending crypto funds this escrow")
	}
	if !o.NeedsFunding() {
		return nil, httpx.Fail(http.StatusConflict, "NOTHING_TO_FUND", "",
			"this order is not waiting on a deposit")
	}
	via := chain.FundingVia(req.Via)
	if via != chain.ViaWallet && via != chain.ViaExternal {
		via = chain.ViaWallet
	}

	if via == chain.ViaWallet {
		// 签的是这笔链上转账本身，所以必须是签名档。
		if err := s.Confirm.Consume(ctx, confirmToken, actorID,
			Digest("fund", o.ID, o.Asset, o.Amount.String()), auth.GradeSignature); err != nil {
			return nil, err
		}
		if err := s.requireOnChain(ctx, payer.Address, o.Asset, o.Amount); err != nil {
			return nil, err
		}
		if _, err := s.Ch.SignDeposit(ctx, payer.Address, o.ID, o.Asset, o.Amount); err != nil {
			return nil, chainErr(err)
		}
	} else {
		// 外部钱包：钱从我们看不见的地方来，这里只是开始盯合约。
		if err := s.Confirm.Consume(ctx, confirmToken, actorID,
			Digest("fund", o.ID, o.Asset, o.Amount.String()), auth.GradeCommit); err != nil {
			return nil, err
		}
		if err := s.Ch.WatchDeposit(ctx, o.ID, o.Asset, o.Amount); err != nil {
			return nil, chainErr(err)
		}
	}

	// 状态不变——钱还没到确认数，订单仍停在 fund/s1。
	// 只记下选了哪条路，让调度器知道该盯着链。
	err = s.St.Tx(ctx, func(tx *sql.Tx) error {
		fresh, err := store.OrderTx(tx, o.ID)
		if err != nil {
			return err
		}
		fresh.FundingVia = string(via)
		fresh.StateDeadline = s.deadlineFor(fresh)
		if err := store.SaveState(tx, fresh); err != nil {
			return err
		}
		msg := "You signed the deposit · confirming on-chain"
		if via == chain.ViaExternal {
			msg = "Watching the contract for your transfer"
		}
		if err := store.AppendEvent(tx, fresh.ID, string(fresh.State), string(fresh.State),
			order.ActorOwner, msg, map[string]string{"funding_via": string(via)}); err != nil {
			return err
		}
		if fresh.CounterpartyID != "" {
			return store.PostTx(tx, fresh.OwnerID, fresh.CounterpartyID, &model.Message{
				Author: "system", Kind: "system", Body: msg, OrderID: fresh.ID,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Order(ctx, o.ID)
}

// payerOf 说这笔单该由谁出币。
// 条件支付是付款方；OTC 是卖币的那一方。
func (s *Service) payerOf(ctx context.Context, o *order.Order) (*model.User, error) {
	id := o.OwnerID
	if o.Kind == order.OTCTake && o.OTC != nil && o.OTC.Side == "buy" {
		id = o.CounterpartyID // taker 买币 → maker 出币
	}
	u, err := s.St.User(ctx, id)
	if err != nil {
		return nil, httpx.NotFound("user")
	}
	return u, nil
}

// ── 人触发的转移 ──

func (s *Service) ConfirmReceipt(ctx context.Context, actorID, orderID string) (*order.Order, error) {
	if _, err := s.mine(ctx, actorID, orderID); err != nil {
		return nil, err
	}
	return s.advance(ctx, orderID, order.EvConfirm, order.ActorOwner, order.Releasing,
		"You confirmed receipt · running release consensus", nil, nil, nil)
}

func (s *Service) Evidence(ctx context.Context, actorID, orderID, fileRef, proof string) (*order.Order, error) {
	o, err := s.St.Order(ctx, orderID)
	if err != nil {
		return nil, httpx.NotFound("order")
	}
	if o.CounterpartyID != actorID {
		return nil, httpx.Fail(http.StatusForbidden, "NOT_YOURS", "", "only the counterparty uploads evidence here")
	}
	return s.advance(ctx, o.ID, order.EvEvidence, order.ActorCounterparty, order.AwaitingMe,
		fmt.Sprintf("They uploaded the %s · your window is open", proof),
		map[string]string{"file_ref": fileRef, "proof": proof}, nil, nil)
}

func (s *Service) Cancel(ctx context.Context, actorID, orderID string) (*order.Order, error) {
	if _, err := s.mine(ctx, actorID, orderID); err != nil {
		return nil, err
	}
	return s.advance(ctx, orderID, order.EvCancel, order.ActorOwner, order.Cancelled,
		"Cancelled — the contract returned the funds. No default recorded.", nil, nil,
		func(tx *sql.Tx, oo *order.Order) error { return s.releaseReservation(tx, oo) })
}

func (s *Service) Dispute(ctx context.Context, actorID, orderID string) (*order.Order, error) {
	if _, err := s.mine(ctx, actorID, orderID); err != nil {
		return nil, err
	}
	return s.advance(ctx, orderID, order.EvDispute, order.ActorOwner, order.Disputed,
		"You disputed within the window — escalated to review. Funds stay locked in the contract.", nil, nil, nil)
}

type AcceptReq struct {
	Via string `json:"via"` // 只在 taker 卖币时有意义
}

// Accept 是 OTC 的承诺点。
//
// 分级在这里最明显：taker 买币时什么都不动（对方的币早锁好了），
// 普通按钮就够；taker 卖币时要出币，得签名。
func (s *Service) Accept(ctx context.Context, actorID, orderID, confirmToken string, req AcceptReq) (*order.Order, error) {
	o, err := s.mine(ctx, actorID, orderID)
	if err != nil {
		return nil, err
	}
	if o.Kind != order.OTCTake || o.OTC == nil {
		return nil, httpx.Fail(http.StatusConflict, "INVALID_TRANSITION", "", "not an OTC order")
	}
	sellSide := o.OTC.Side == "sell"
	grade := auth.GradeCommit
	if sellSide && chain.FundingVia(req.Via) != chain.ViaExternal {
		grade = auth.GradeSignature
	}
	if err := s.Confirm.Consume(ctx, confirmToken, actorID, Digest("accept", o.ID), grade); err != nil {
		return nil, err
	}

	payload := map[string]string{}
	reason := "You confirmed · verifying the escrow they locked at listing"
	if sellSide {
		amt := money.New(o.Amount, o.Asset)
		if _, err := s.checkAllowance(ctx, actorID, o.AllowanceID, amt); err != nil {
			return nil, err
		}
		via := chain.FundingVia(req.Via)
		if via != chain.ViaExternal {
			via = chain.ViaWallet
		}
		payload["funding_via"] = string(via)
		u, err := s.St.User(ctx, actorID)
		if err != nil {
			return nil, httpx.NotFound("user")
		}
		if via == chain.ViaWallet {
			if err := s.requireOnChain(ctx, u.Address, o.Asset, o.Amount); err != nil {
				return nil, err
			}
			if _, err := s.Ch.SignDeposit(ctx, u.Address, o.ID, o.Asset, o.Amount); err != nil {
				return nil, chainErr(err)
			}
			reason = "You signed the deposit · your coins are confirming into escrow"
		} else {
			if err := s.Ch.WatchDeposit(ctx, o.ID, o.Asset, o.Amount); err != nil {
				return nil, chainErr(err)
			}
			reason = "Order confirmed · watching the contract for your deposit"
		}
	}
	return s.advance(ctx, o.ID, order.EvAccept, order.ActorOwner, order.S1, reason, payload, nil, nil)
}

// Receipt 收法币回执。放款依据是银行凭证，不是任何一方的确认意愿。
func (s *Service) Receipt(ctx context.Context, actorID, orderID, fileRef string) (*order.Order, error) {
	o, err := s.St.Order(ctx, orderID)
	if err != nil {
		return nil, httpx.NotFound("order")
	}
	if o.OwnerID != actorID && o.CounterpartyID != actorID {
		return nil, httpx.Fail(http.StatusForbidden, "NOT_YOURS", "", "this order belongs to another account")
	}
	if fileRef == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "RECEIPT_REQUIRED", "file_ref",
			"attach the bank receipt — release is decided on the receipt, not on anyone's say-so")
	}
	actor := order.ActorOwner
	if o.OwnerID != actorID {
		actor = order.ActorCounterparty
	}
	return s.advance(ctx, o.ID, order.EvReceipt, actor, order.S3V,
		"Receipt uploaded · waiting on the other side to check it against the order",
		map[string]string{"file_ref": fileRef}, nil,
		func(tx *sql.Tx, oo *order.Order) error {
			_, err := store.InsertReceipt(tx, oo.ID, actorID, fileRef)
			return err
		})
}

// VerifyReceipt 是 OTC 的放行闸门。放行不等对方开口，等的是回执被核过——
// 所以核验只能由收法币的那一方做，上传者不能自己核自己的。转移表里这两条边
// 的 actor 集合放得比较宽（含系统），收紧就落在这里。
func (s *Service) VerifyReceipt(ctx context.Context, actorID, orderID string,
	okFlag bool, reason string) (*order.Order, error) {
	o, err := s.St.Order(ctx, orderID)
	if err != nil {
		return nil, httpx.NotFound("order")
	}
	if o.OwnerID != actorID && o.CounterpartyID != actorID {
		return nil, httpx.Fail(http.StatusForbidden, "NOT_YOURS", "", "this order belongs to another account")
	}
	rc, found := s.St.LatestReceipt(ctx, o.ID)
	if !found {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "NO_RECEIPT", "",
			"there is no receipt to check yet")
	}
	if rc.UploaderID == actorID {
		return nil, httpx.Fail(http.StatusForbidden, "NOT_YOUR_CALL", "",
			"the side that uploaded the receipt cannot be the one that clears it")
	}
	actor := order.ActorOwner
	if o.OwnerID != actorID {
		actor = order.ActorCounterparty
	}
	if !okFlag {
		if reason == "" {
			reason = "the receipt does not match this order"
		}
		return s.advance(ctx, o.ID, order.EvDispute, actor, order.Disputed,
			"Receipt rejected · "+reason,
			map[string]string{"receipt_id": rc.ID, "reason": reason}, nil, nil)
	}
	now := time.Now()
	return s.advance(ctx, o.ID, order.EvVerify, actor, order.S4,
		"Receipt verified · releasing to them",
		map[string]string{"receipt_id": rc.ID}, nil,
		func(tx *sql.Tx, _ *order.Order) error {
			return store.MarkReceiptVerified(tx, rc.ID, now)
		})
}

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

func (s *Service) Tick(ctx context.Context, o *order.Order) error {
	switch o.Kind {
	case order.ConditionalTransfer:
		return s.tickConditional(ctx, o)
	case order.OTCTake:
		return s.tickOTC(ctx, o)
	}
	return nil
}

// waitForDeposit 查链上确认数。没走满就把 deadline 往后推一秒接着等。
func (s *Service) waitForDeposit(ctx context.Context, o *order.Order, to order.State, reason string) error {
	d, err := s.Ch.Deposit(ctx, o.ID)
	if err != nil {
		return err
	}
	if d == nil || !d.Settled() {
		return s.touchDeadline(ctx, o)
	}
	_, err = s.advance(ctx, o.ID, order.EvFunded, order.ActorSystem, to,
		fmt.Sprintf("%s · %d/%d confirmations", reason, d.Confirmations, d.Required),
		map[string]string{"escrow_tx": d.TxHash}, nil, nil)
	return err
}

// touchDeadline 把这笔单的下一次检查往后推，不改状态。
func (s *Service) touchDeadline(ctx context.Context, o *order.Order) error {
	return s.St.Tx(ctx, func(tx *sql.Tx) error {
		fresh, err := store.OrderTx(tx, o.ID)
		if err != nil {
			return err
		}
		fresh.StateDeadline = s.deadlineFor(fresh)
		return store.SaveState(tx, fresh)
	})
}

func (s *Service) tickConditional(ctx context.Context, o *order.Order) error {
	c := o.Cond
	switch o.State {
	case order.Fund:
		if o.FundingVia == "" {
			// 一直没选付款方式，兜底到期：链上什么都没发生，直接作废。
			_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Expired,
				"Nobody funded the escrow before the deadline — the order lapsed", nil, nil, nil)
			return err
		}
		return s.waitForDeposit(ctx, o, order.Locked, "Escrow funded")

	case order.Locked:
		if c != nil && c.Main == condition.Immediate {
			_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Releasing,
				"Conditions are empty — releasing straight away", nil, nil, nil)
			return err
		}
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.AwaitingCounterparty,
			"Waiting on "+waitingText(c), nil, nil, nil)
		return err

	case order.AwaitingCounterparty:
		if c != nil && c.Main == condition.OnDate {
			_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Releasing,
				"Condition met — running release consensus", nil, nil, nil)
			return err
		}
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.AwaitingMe,
			"The counterparty marked it delivered · evidence attached to the record", nil, nil, nil)
		return err

	case order.AwaitingMe:
		if c != nil && c.Main == condition.ProofWindow {
			_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Releasing,
				"Dispute window closed with no objection — releasing", nil, nil, nil)
			return err
		}
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Expired,
			"Nobody acted before the deadline — the contract returned the funds, default recorded", nil, nil, nil)
		return err

	case order.Releasing:
		return s.runReleaseConsensus(ctx, o)
	}
	return nil
}

func (s *Service) tickOTC(ctx context.Context, o *order.Order) error {
	switch o.State {
	case order.Match:
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Cancelled,
			"Match expired before you confirmed — recorded as unfilled, no default", nil, nil,
			func(tx *sql.Tx, oo *order.Order) error { return s.releaseReservation(tx, oo) })
		return err

	case order.S1:
		if o.OTC != nil && o.OTC.Side == "buy" {
			// 买方向：币在对方挂单那一刻就进合约了。这里只是查锁仓、绑订单——瞬时。
			return s.bindListingLock(ctx, o)
		}
		if o.FundingVia == "" {
			_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Cancelled,
				"You never funded the escrow — the match lapsed, no default recorded", nil, nil,
				func(tx *sql.Tx, oo *order.Order) error { return s.releaseReservation(tx, oo) })
			return err
		}
		return s.waitForDeposit(ctx, o, order.S3, "Escrow funded · your coins are locked")

	case order.S3:
		// 卖方向：法币是对方打的。种子商家没有客户端，调度器代传那张回执——
		// 与代跑注资同一个道理，接真商户时删掉这一支即可。
		if o.OTC != nil && o.OTC.Side == "sell" {
			_, err := s.advance(ctx, o.ID, order.EvReceipt, order.ActorCounterparty, order.S3V,
				"They uploaded a receipt · check it against the order", nil, nil,
				func(tx *sql.Tx, oo *order.Order) error {
					_, err := store.InsertReceipt(tx, oo.ID, oo.CounterpartyID, "simulated-counterparty-receipt")
					return err
				})
			return err
		}
		// 买方向：你付法币，到点没付就是你逾期。
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Expired,
			"Payment window missed · the contract returned the coins to the counterparty", nil, nil,
			func(tx *sql.Tx, oo *order.Order) error { return s.releaseReservation(tx, oo) })
		return err

	case order.S3V:
		// 买方向：法币是你打的，该核验回执的是对方 maker。种子商家没有客户端，
		// 调度器代他核——与上面代传回执同一个道理，接真商户时删掉这一支即可。
		if o.OTC != nil && o.OTC.Side == "buy" {
			rc, found := s.St.LatestReceipt(ctx, o.ID)
			if !found {
				return fmt.Errorf("order %s is at s3v with no receipt", o.Ref)
			}
			now := time.Now()
			_, err := s.advance(ctx, o.ID, order.EvVerify, order.ActorCounterparty, order.S4,
				"Receipt verified · releasing to you", map[string]string{"receipt_id": rc.ID}, nil,
				func(tx *sql.Tx, _ *order.Order) error {
					return store.MarkReceiptVerified(tx, rc.ID, now)
				})
			return err
		}
		// 卖方向：法币是对方打的，该核验的是你本人。到点没核就是你逾期——
		// 放行只认核过的回执，没人核就没有放行依据。
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.Expired,
			"You never checked their receipt · the window closed and the contract unwound", nil, nil,
			func(tx *sql.Tx, oo *order.Order) error { return s.releaseReservation(tx, oo) })
		return err

	case order.S4:
		_, err := s.advance(ctx, o.ID, order.EvTick, order.ActorSystem, order.S5,
			"Verified and released · settlement complete", nil, nil, nil)
		return err
	}
	return nil
}

// bindListingLock 把挂单时锁好的仓位绑到这笔订单。买方向的 s1 就是这一步。
func (s *Service) bindListingLock(ctx context.Context, o *order.Order) error {
	maker, err := s.St.User(ctx, o.CounterpartyID)
	if err != nil {
		return err
	}
	_, err = s.advance(ctx, o.ID, order.EvBind, order.ActorSystem, order.S3,
		"Escrow verified · their coins were locked at listing, now bound to this order", nil,
		func(oo *order.Order) (settlement.Outcome, error) {
			p, err := s.Ch.BindListingLock(ctx, oo.ID, oo.OTC.OfferID, maker.Address, oo.Asset, oo.Amount)
			if err != nil {
				return settlement.Outcome{}, err
			}
			return settlement.Outcome{Action: "bind", TxHash: p.TxHash}, nil
		}, nil)
	return err
}

// runReleaseConsensus 跑放行共识。它没有裁量权：只能放行，或拦下转人工。
func (s *Service) runReleaseConsensus(ctx context.Context, o *order.Order) error {
	disputes := 0
	if m, err := s.St.Merchant(ctx, o.CounterpartyID); err == nil {
		disputes = m.Disputes
	}
	condText := ""
	if o.Cond != nil {
		condText = o.Cond.Text
	}
	d, err := s.Ag.Vote(ctx, agent.ReleaseInput{
		OrderID: o.ID, Asset: o.Asset, AmountUSD: money.New(o.Amount, o.Asset).USD().String(),
		PeerDisputes: disputes, ConditionText: condText,
	})
	if err != nil {
		return err
	}
	to := order.Released
	if d.Outcome == agent.OutcomeHoldForReview {
		to = order.AwaitingMe
	}
	_, err = s.advance(ctx, o.ID, order.EvReleaseVote, order.ActorAgent, to, d.Rationale,
		map[string]string{"outcome": string(d.Outcome)}, nil, nil)
	return err
}

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
