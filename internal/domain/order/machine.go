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
//	                    ┌── immediate ─────────────┐
//	创建 → locked ───────┤                          ├→ releasing → released ✅
//	                    └→ awaiting_counterparty → awaiting_me ─┘
//	                                                    └→ disputed ⚠️ 资金保持锁定
var conditionalEdges = []edge{
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
//	match → s1 → s3 → s4 → s5 ✅
//	  │           │
//	  └ cancelled └ 超时 → expired ⚠️ 负向回写
var otcEdges = []edge{
	{OTCTake, Match, EvAccept, owner, S1, TermNone}, // 承诺点：Passkey + 授权卡在这里校验
	{OTCTake, S1, EvFund, both, S3, TermNone},       // maker 注资托管
	{OTCTake, S3, EvReceipt, owner, S4, TermNone},   // 回传法币回执
	{OTCTake, S4, EvTick, sys, S5, TermCompleted},   // 平台核验通过并放款

	{OTCTake, Match, EvCancel, owner, Cancelled, TermCancelled},
	{OTCTake, S3, EvCancel, owner, Cancelled, TermCancelled},

	// match 超时只是没成交，不是违约——所以是 cancelled 不是 expired。
	// 两者都记 expired 会让履约率无故变差。
	{OTCTake, Match, EvTick, sys, Cancelled, TermCancelled},
	{OTCTake, S1, EvTick, sys, S3, TermNone}, // 调度器代跑种子商家的注资
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
