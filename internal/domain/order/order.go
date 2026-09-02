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
	// Fund 是非托管的第一拍：钱还没动，合约在等这一方入金。
	// 旧版「建单即锁币」是托管模型的写法，平台不再持有资金，这一站就必须存在。
	Fund                 State = "fund"
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
	// S3V 是回执已上传、等对方核验的中间态。
	// V1 前端的放行依据是「核验对方的银行回执」，不是「等对方点确认」，
	// 所以核验必须是一个显式的人工动作，不能由 tick 代劳。
	S3V State = "s3v"
	S4  State = "s4"
	S5  State = "s5"
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
	EvFunded      Event = "funded" // 链上确认数走满，合约真的拿到钱了
	EvTick        Event = "tick"
	EvConfirm     Event = "confirm"
	EvEvidence    Event = "evidence"
	EvCancel      Event = "cancel"
	EvDispute     Event = "dispute"
	EvReleaseVote Event = "release_vote"
	EvAccept      Event = "accept"
	EvFund        Event = "fund"
	EvBind        Event = "bind" // 绑定挂单时已锁的仓位
	EvReceipt     Event = "receipt"
	EvVerify      Event = "verify" // 核验对方的法币回执
)

type Conditional struct {
	Main              condition.MainBranch `json:"main_branch"`
	WaitingOn         condition.WaitingOn  `json:"waiting_on"`
	Text              string               `json:"condition_text"`
	FallbackDays      int                  `json:"fallback_days"`
	DisputeWindowSecs int                  `json:"dispute_window_secs"`
}

type OTC struct {
	OfferID string `json:"offer_id"`
	// FundingVia 只在 taker 卖币时有意义：他要出币，所以要选怎么出。
	// taker 买币时币是对方挂单时就锁好的，没有这个选择。
	FundingVia string          `json:"funding_via,omitempty"` // wallet | external
	Side       string          `json:"side"`                  // taker 视角：buy | sell
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
	AllowanceID    string
	State          State
	Terminal       Terminal
	StateDeadline  *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time

	Cond  *Conditional
	Conds []condition.Atom
	OTC   *OTC

	// 地址是放款的目的地。非托管下钱打给地址，不是打给某个平台账户。
	OwnerAddr string
	PayeeAddr string

	// 链上事实。这几个字段是从 chain.Chain 读回来的快照，不是平台自己的账。
	FundingVia    string
	EscrowTx      string
	EscrowAddr    string
	EscrowNetwork string
	Confirmations int
	Required      int
}

// NeedsFunding 说这笔单还等着谁把钱打进合约。
// taker 买币的 OTC 单不需要——币在对方挂单那一刻就在合约里了。
func (o *Order) NeedsFunding() bool {
	if o.Kind == ConditionalTransfer {
		return o.State == Fund
	}
	return o.OTC != nil && o.OTC.Side == "sell" && o.State == S1
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
