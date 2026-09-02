// Package settlement 处理四种终态的资金归属。
//
// 与托管模型的关键差别：这里不动任何余额。
// 资金在托管合约里，放款与退回都是合约动作——本包只负责
// 「决定该调哪个合约动作」以及「把看到的结果记进证据链」。
//
// 链上动作与数据库写入不可能在同一个事务里，这不是缺陷：
// 跟链之间本来就没有分布式事务。所以顺序是先动链、后记账，
// 记账失败可以补记，链上事实不会因此改变。
package settlement

import (
	"context"
	"database/sql"

	"github.com/advaita/atara-pay/internal/chain"
	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/store"
)

type Settler struct {
	St string
}

// Outcome 是一次结算在链上做了什么，供调用方记进证据链。
type Outcome struct {
	Action string // release | refund | none
	TxHash string
}

// Settle 是四种终态资金处置的唯一实现。
//
//	completed → 合约放款给收款方，正向回写履约
//	cancelled → 合约原路退回，不回写违约
//	expired   → 合约原路退回，负向回写违约
//	disputed  → 什么都不做，资金留在合约里等裁决
//
// Settle 的 auth 是放行依据。它必须由调用方显式给出，不能在这里凭 term 猜——
// 「评分决定放行」这件事一旦靠推断，就等于没有这道闸门。
func Settle(ctx context.Context, ch chain.Chain, o *order.Order, term order.Terminal,
	auth chain.ReleaseAuth) (Outcome, error) {
	switch term {
	case order.TermCompleted:
		payee := payeeOf(o)
		if payee == "" {
			return Outcome{Action: "none"}, nil
		}
		tx, err := ch.Release(ctx, o.ID, payee, auth)
		if err != nil {
			return Outcome{}, err
		}
		return Outcome{Action: "release", TxHash: tx}, nil

	case order.TermCancelled, order.TermExpired:
		// 退回是合约的事。挂单锁的币退回挂单本身，不退给个人——
		// 币还 backing 着那条挂单，归还的是可成交量。合约里这一层判断在 Refund。
		tx, err := ch.Refund(ctx, o.ID, auth)
		if err != nil {
			return Outcome{}, err
		}
		if tx == "" {
			return Outcome{Action: "none"}, nil
		}
		return Outcome{Action: "refund", TxHash: tx}, nil

	case order.TermDisputed:
		// 资金保持锁定，待裁决——刻意什么都不做
		return Outcome{Action: "none"}, nil
	}
	return Outcome{Action: "none"}, nil
}

// payeeOf 说这笔单放款该打给谁的地址。
func payeeOf(o *order.Order) string {
	switch o.Kind {
	case order.ConditionalTransfer:
		return o.PayeeAddr
	case order.OTCTake:
		// taker 卖币 → 币给 maker；taker 买币 → 币给 taker
		if o.OTC != nil && o.OTC.Side == "sell" {
			return o.PayeeAddr
		}
		return o.OwnerAddr
	}
	return ""
}

// Record 把链上结果与履约回写落进库。
func Record(tx *sql.Tx, st *store.Store, o *order.Order, term order.Terminal, out Outcome) error {
	if out.Action != "none" {
		if err := store.LogChain(tx, o.OwnerID, store.ChainEvent{
			Kind: out.Action, Asset: o.Asset, Amount: o.Amount,
			TxHash: out.TxHash, OrderID: o.ID,
			Memo: "settled as " + string(term),
		}); err != nil {
			return err
		}
	}
	if o.CounterpartyID == "" {
		return nil
	}
	switch term {
	case order.TermCompleted:
		return st.BumpMerchant(tx, o.CounterpartyID, true)
	case order.TermExpired:
		// 到期未履约才负向回写。主动撤销不回写——它与逾期严格区分。
		return st.BumpMerchant(tx, o.CounterpartyID, false)
	}
	return nil
}
