// Package order 是工单聚合根。
//
// R1 一笔一工单：任何支付发起即建一条工单，有终态；终态后只读。
// 两种 kind 共用同一张表、同一套账本与同一个 Settle——
// 四种终态的资金处置只能有一份实现。
package order

import (
	"time"

	"github.com/advaita/atara-pay/internal/domain/condition"
	"github.com/shopspring/decimal"
)

type Kind string

const (
	ConditionalTransfer Kind = "conditional_transfer"
	OTCTake             Kind = "otc_take"
)

type State string

// 条件支付托管的状态
const (
	Locked               State = "locked"
	AwaitingCounterparty State = "awaiting_counterparty"
	AwaitingMe           State = "awaiting_me"
	Releasing            State = "releasing"
	Released             State = "released"
)

// OTC 成交的状态。站名与前端 steps() 一致：
// Matched / Escrow funded / Your transfer / Verify & release
const (
	Match State = "match"
	S1    State = "s1"
	S3    State = "s3"
	S4    State = "s4"
	S5    State = "s5"
)

// 两种 kind 共用的终态状态
const (
	Cancelled State = "cancelled"
	Expired   State = "expired"
	Disputed  State = "disputed"
)

// Terminal 是四种终态。互斥，全流程通用。
type Terminal string

const (
	TermNone      Terminal = ""
	TermCompleted Terminal = "completed" // 条件成立且放行共识通过 → 给收款方，正向回写
	TermCancelled Terminal = "cancelled" // 条件成立前主动撤 → 原路退回，不回写
	TermExpired   Terminal = "expired"   // 到期未履约 → 原路退回，负向回写
	TermDisputed  Terminal = "disputed"  // 窗口内提出异议 → 保持锁定，待裁决
)

type Actor string

const (
	ActorOwner        Actor = "owner"
	ActorCounterparty Actor = "counterparty"
	ActorSystem       Actor = "system"
	ActorAgent        Actor = "agent"
)

type Event string

const (
	EvCreate      Event = "create"
	EvTick        Event = "tick"
	EvConfirm     Event = "confirm"
	EvEvidence    Event = "evidence"
	EvCancel      Event = "cancel"
	EvDispute     Event = "dispute"
	EvReleaseVote Event = "release_vote"
	EvAccept      Event = "accept"
	EvFund        Event = "fund"
	EvReceipt     Event = "receipt"
)

type Conditional struct {
	Main              condition.MainBranch `json:"main_branch"`
	WaitingOn         condition.WaitingOn  `json:"waiting_on"`
	Text              string               `json:"condition_text"`
	FallbackDays      int                  `json:"fallback_days"`
	DisputeWindowSecs int                  `json:"dispute_window_secs"`
}

type OTC struct {
	OfferID    string          `json:"offer_id"`
	Side       string          `json:"side"` // taker 视角：buy | sell
	UnitPrice  decimal.Decimal `json:"unit_price"`
	FiatCode   string          `json:"fiat_code"`
	FiatAmount decimal.Decimal `json:"fiat_amount"`
	Network    string          `json:"network"`
}

type Order struct {
	ID             string
	Ref            string // ATR-8F42C1
	Kind           Kind
	OwnerID        string
	CounterpartyID string
	Asset          string
	Amount         decimal.Decimal
	Note           string
	CardID         string
	State          State
	Terminal       Terminal
	StateDeadline  *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time

	Cond  *Conditional
	Conds []condition.Atom
	OTC   *OTC
}

func (o *Order) IsTerminal() bool { return o.Terminal != TermNone }

type Log struct {
	Seq       int               `json:"seq"`
	From      string            `json:"from_state"`
	To        string            `json:"to_state"`
	Actor     Actor             `json:"actor"`
	Reason    string            `json:"reason"`
	Payload   map[string]string `json:"payload,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}
