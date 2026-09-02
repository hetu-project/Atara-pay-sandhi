package order

import "fmt"

// edge 是转移表里的一条边。表里没有的转移一律拒绝——
// 这是「终态后只读」与「不许跳站」的唯一执行点。
type edge struct {
	Kind   Kind
	From   State
	Event  Event
	Actors []Actor
	To     State
	Term   Terminal
}

// 条件支付托管的转移表。
//
// 非托管：创建时钱一分没动，合约在 fund 站等这一方入金；
// 确认数走满才算真的锁住。
//
//	                                     ┌── immediate ─────────────┐
//	创建 → fund ──入金确认──→ locked ──────┤                          ├→ releasing → released ✅
//	                                     └→ awaiting_counterparty → awaiting_me ─┘
//	                                                                     └→ disputed ⚠️ 资金保持锁定
var conditionalEdges = []edge{
	// 入金：Passkey 签名转入，或往合约地址打款后被扫到。
	// 两条路殊途同归——确认数走满，合约拿到钱，才进 locked。
	{ConditionalTransfer, Fund, EvFunded, both, Locked, TermNone},
	// 还没入金就撤，链上什么都没发生
	{ConditionalTransfer, Fund, EvCancel, owner, Cancelled, TermCancelled},
	{ConditionalTransfer, Fund, EvTick, sys, Expired, TermExpired},

	{ConditionalTransfer, Locked, EvTick, sys, AwaitingCounterparty, TermNone},
	{ConditionalTransfer, Locked, EvTick, sys, Releasing, TermNone}, // immediate 分支

	{ConditionalTransfer, AwaitingCounterparty, EvEvidence, []Actor{ActorCounterparty}, AwaitingMe, TermNone},
	{ConditionalTransfer, AwaitingCounterparty, EvTick, sys, AwaitingMe, TermNone},
	{ConditionalTransfer, AwaitingCounterparty, EvTick, sys, Releasing, TermNone},
	{ConditionalTransfer, AwaitingCounterparty, EvTick, sys, Expired, TermExpired},

	{ConditionalTransfer, AwaitingMe, EvConfirm, []Actor{ActorOwner}, Releasing, TermNone},
	{ConditionalTransfer, AwaitingMe, EvTick, sys, Releasing, TermNone}, // 异议窗口静默到期
	{ConditionalTransfer, AwaitingMe, EvTick, sys, Expired, TermExpired},
	{ConditionalTransfer, AwaitingMe, EvDispute, []Actor{ActorOwner}, Disputed, TermDisputed},

	// 放行共识只有两个出口：放行，或拦下转人工。没有「改判条件」。
	{ConditionalTransfer, Releasing, EvReleaseVote, agent, Released, TermCompleted},
	{ConditionalTransfer, Releasing, EvReleaseVote, agent, AwaitingMe, TermNone},

	// 条件成立前随时可撤：原路退回，不记违约
	{ConditionalTransfer, Locked, EvCancel, owner, Cancelled, TermCancelled},
	{ConditionalTransfer, AwaitingCounterparty, EvCancel, owner, Cancelled, TermCancelled},
	{ConditionalTransfer, AwaitingMe, EvCancel, owner, Cancelled, TermCancelled},
}

// OTC 成交的转移表。
//
//	match → s1 → s3 → s3v → s4 → s5 ✅
//	  │           │     │
//	  └ cancelled └ 超时 └ 超时 → expired ⚠️ 负向回写
//
// OTC 的 s1 按方向分叉，这是非托管模型下最重要的一处不对称：
//
//	taker 买币：币在对方挂单时就锁进合约了 → s1 只是查锁仓并绑单，瞬时
//	taker 卖币：币要从 taker 自己的钱包出去 → s1 是真上链，走确认数
var otcEdges = []edge{
	{OTCTake, Match, EvAccept, owner, S1, TermNone}, // 承诺点

	// 买方向：验证对方挂单时锁的仓，绑到这笔订单上。没有新的资金动作。
	{OTCTake, S1, EvBind, sys, S3, TermNone},
	// 卖方向：taker 入金的确认数走满。
	{OTCTake, S1, EvFunded, both, S3, TermNone},
	{OTCTake, S1, EvCancel, owner, Cancelled, TermCancelled}, // 还没入金就反悔

	{OTCTake, S3, EvReceipt, both, S3V, TermNone}, // 谁付法币谁传回执

	// 核验是人工动作，只有收法币的一方能做。放行不等对方开口，
	// 等的是回执本身被核过——这正是 OTC 不需要对方点确认的原因。
	{OTCTake, S3V, EvVerify, both, S4, TermNone},
	{OTCTake, S3V, EvDispute, both, Disputed, TermDisputed}, // 回执对不上
	{OTCTake, S3V, EvTick, sys, Expired, TermExpired},       // 核验窗口过期

	{OTCTake, S4, EvTick, sys, S5, TermCompleted},

	{OTCTake, Match, EvCancel, owner, Cancelled, TermCancelled},
	{OTCTake, S3, EvCancel, owner, Cancelled, TermCancelled},

	// match 超时只是没成交，不是违约——所以是 cancelled 不是 expired。
	// 两者都记 expired 会让履约率无故变差。
	{OTCTake, Match, EvTick, sys, Cancelled, TermCancelled},
	{OTCTake, S3, EvTick, sys, Expired, TermExpired},
}

var (
	sys   = []Actor{ActorSystem}
	agent = []Actor{ActorAgent}
	owner = []Actor{ActorOwner}
	both  = []Actor{ActorOwner, ActorCounterparty, ActorSystem}
)

var table = append(append([]edge{}, conditionalEdges...), otcEdges...)

// Targets 返回 (kind, from, event, actor) 下所有合法的目标状态。
func Targets(k Kind, from State, ev Event, actor Actor) []State {
	var out []State
	for _, e := range table {
		if e.Kind == k && e.From == from && e.Event == ev && hasActor(e.Actors, actor) {
			out = append(out, e.To)
		}
	}
	return out
}

// Check 校验一次转移是否在表里。to 为空表示只问「这个事件此刻允许吗」。
func Check(k Kind, from State, ev Event, actor Actor, to State) (Terminal, error) {
	for _, e := range table {
		if e.Kind != k || e.From != from || e.Event != ev || !hasActor(e.Actors, actor) {
			continue
		}
		if to == "" || e.To == to {
			return e.Term, nil
		}
	}
	return TermNone, fmt.Errorf("%w: %s cannot %s from %s as %s", ErrInvalidTransition, k, ev, from, actor)
}

// Apply 在校验通过后就地推进工单。终态工单一律拒绝。
func (o *Order) Apply(ev Event, actor Actor, to State) error {
	if o.IsTerminal() {
		return ErrTerminal
	}
	term, err := Check(o.Kind, o.State, ev, actor, to)
	if err != nil {
		return err
	}
	o.State = to
	o.Terminal = term
	o.StateDeadline = nil
	return nil
}

func hasActor(xs []Actor, a Actor) bool {
	for _, x := range xs {
		if x == a {
			return true
		}
	}
	return false
}

type transitionError struct{ msg string }

func (e transitionError) Error() string { return e.msg }

var (
	ErrInvalidTransition = transitionError{"invalid transition"}
	ErrTerminal          = transitionError{"order is terminal and read-only"}
)
