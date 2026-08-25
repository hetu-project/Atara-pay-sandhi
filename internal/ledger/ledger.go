// Package ledger 是资金处置的唯一实现点。
//
// 平台只持有数字资产。法币腿点对点走银行，平台只核验回执——
// 所以 wallets 永远没有法币行，法币在系统里唯一的痕迹是 fiat_receipts。
package ledger

import (
	"database/sql"

	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/shopspring/decimal"
)

const (
	KindLock        = "lock"
	KindUnlock      = "unlock"
	KindReleaseOut  = "release_out"
	KindReleaseIn   = "release_in"
	KindOfferLock   = "offer_lock"
	KindOfferUnlock = "offer_unlock"
	KindDeposit     = "deposit"
)

// Lock 把可用余额锁进托管。
func Lock(tx *sql.Tx, userID, asset string, amt decimal.Decimal, orderID, memo string) error {
	return store.Move(tx, userID, asset, KindLock, amt.Neg(), amt, orderID, "", memo)
}

// Unlock 把托管里的钱原路退回可用余额。
func Unlock(tx *sql.Tx, userID, asset string, amt decimal.Decimal, orderID, memo string) error {
	return store.Move(tx, userID, asset, KindUnlock, amt, amt.Neg(), orderID, "", memo)
}

// Release 把托管里的钱交给收款方。
func Release(tx *sql.Tx, fromUser, toUser, asset string, amt decimal.Decimal, orderID string) error {
	if err := store.Move(tx, fromUser, asset, KindReleaseOut, decimal.Zero, amt.Neg(), orderID, "", "released to counterparty"); err != nil {
		return err
	}
	return store.Move(tx, toUser, asset, KindReleaseIn, amt, decimal.Zero, orderID, "", "released from escrow")
}

// OfferLock/OfferUnlock：挂出即锁币，下架即解锁。
// 买家看到的可成交量必须真的在托管里。
func OfferLock(tx *sql.Tx, userID, asset string, amt decimal.Decimal, offerID string) error {
	return store.Move(tx, userID, asset, KindOfferLock, amt.Neg(), amt, "", offerID, "listing locked")
}

func OfferUnlock(tx *sql.Tx, userID, asset string, amt decimal.Decimal, offerID string) error {
	return store.Move(tx, userID, asset, KindOfferUnlock, amt, amt.Neg(), "", offerID, "listing delisted")
}

// Escrowed 描述一笔工单此刻把谁的钱锁在了托管里。
//
// 必须在状态推进**之前**求值：推进之后 o.State 已经是新状态，
// Funded 会算成 false，放款那一步就会静默地什么都不做。
type Escrowed struct {
	Payer, Payee string
	Asset        string
	Amount       decimal.Decimal
	Funded       bool
	// FromListing 表示这批币是挂单「挂出即锁币」时就锁好的，不是这笔单自己锁的。
	// 撤销时不能把它解回可用余额——币还在backing那条挂单，
	// 归还的是可成交量（ReserveQty），不是币本身。
	FromListing bool
}

func EscrowOf(o *order.Order) Escrowed {
	switch o.Kind {
	case order.ConditionalTransfer:
		return Escrowed{
			Payer: o.OwnerID, Payee: o.CounterpartyID, Asset: o.Asset, Amount: o.Amount,
			Funded: in(o.State, order.Locked, order.AwaitingCounterparty, order.AwaitingMe, order.Releasing),
		}
	case order.OTCTake:
		// taker 买币 → 锁的是 maker 的币，maker 在 s1 之后才注资；
		// taker 卖币 → 锁的是 taker 自己的币，accept 那一刻就锁了。
		if o.OTC != nil && o.OTC.Side == "sell" {
			return Escrowed{Payer: o.OwnerID, Payee: o.CounterpartyID, Asset: o.Asset, Amount: o.Amount,
				Funded: in(o.State, order.S1, order.S3, order.S4)}
		}
		// taker 买币：币早在 maker 挂单时就锁进托管了，s1 那一站没有新的资金动作。
		return Escrowed{Payer: o.CounterpartyID, Payee: o.OwnerID, Asset: o.Asset, Amount: o.Amount,
			Funded: in(o.State, order.S1, order.S3, order.S4), FromListing: true}
	}
	return Escrowed{}
}

func in(s order.State, xs ...order.State) bool {
	for _, x := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// Settle 是四种终态资金处置的唯一实现。
// 选择单一 orders 聚合的全部理由就是让这个函数只存在一份。
// esc 必须由调用方在状态推进前求出。
func Settle(tx *sql.Tx, st *store.Store, o *order.Order, esc Escrowed, term order.Terminal) error {
	switch term {
	case order.TermCompleted: // 给收款方，正向回写
		if esc.Funded && esc.Amount.IsPositive() {
			if err := Release(tx, esc.Payer, esc.Payee, esc.Asset, esc.Amount, o.ID); err != nil {
				return err
			}
		}
		if o.CounterpartyID != "" {
			_ = st.BumpMerchant(tx, o.CounterpartyID, true)
		}
	case order.TermCancelled: // 原路退回，不回写违约
		// 挂单预锁的币不解锁：它还 backing 着那条挂单，归还的是可成交量。
		if esc.Funded && !esc.FromListing && esc.Amount.IsPositive() {
			if err := Unlock(tx, esc.Payer, esc.Asset, esc.Amount, o.ID, "cancelled"); err != nil {
				return err
			}
		}
	case order.TermExpired: // 原路退回，负向回写
		// 挂单预锁的币不解锁：它还 backing 着那条挂单，归还的是可成交量。
		if esc.Funded && !esc.FromListing && esc.Amount.IsPositive() {
			if err := Unlock(tx, esc.Payer, esc.Asset, esc.Amount, o.ID, "expired"); err != nil {
				return err
			}
		}
		if o.CounterpartyID != "" {
			_ = st.BumpMerchant(tx, o.CounterpartyID, false)
		}
	case order.TermDisputed: // 保持锁定，待裁决——这里刻意什么都不做
	}
	// 撤销与超时释放授权卡的周期额度占用；完成的那笔是真花掉了，不退。
	if term == order.TermCancelled || term == order.TermExpired {
		if o.CardID != "" {
			_ = st.SpendCard(tx, o.CardID, money.New(o.Amount, o.Asset).USD().Neg())
		}
	}
	return nil
}
