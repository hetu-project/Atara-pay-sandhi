package order

import "testing"

// 合法转移必须被接受，非法转移必须被拒绝。
// 转移表是「不许跳站」和「终态后只读」的唯一执行点，所以两个方向都要测。
func TestTransitions(t *testing.T) {
	legal := []struct {
		name  string
		kind  Kind
		from  State
		ev    Event
		actor Actor
		to    State
		term  Terminal
	}{
		{"immediate 分支直接进放行", ConditionalTransfer, Locked, EvTick, ActorSystem, Releasing, TermNone},
		{"锁定后等对手方", ConditionalTransfer, Locked, EvTick, ActorSystem, AwaitingCounterparty, TermNone},
		{"对手方上传凭证", ConditionalTransfer, AwaitingCounterparty, EvEvidence, ActorCounterparty, AwaitingMe, TermNone},
		{"我确认收货", ConditionalTransfer, AwaitingMe, EvConfirm, ActorOwner, Releasing, TermNone},
		{"放行共识通过", ConditionalTransfer, Releasing, EvReleaseVote, ActorAgent, Released, TermCompleted},
		{"放行共识拦下转人工", ConditionalTransfer, Releasing, EvReleaseVote, ActorAgent, AwaitingMe, TermNone},
		{"窗口内异议", ConditionalTransfer, AwaitingMe, EvDispute, ActorOwner, Disputed, TermDisputed},
		{"条件成立前撤单", ConditionalTransfer, AwaitingCounterparty, EvCancel, ActorOwner, Cancelled, TermCancelled},
		{"OTC 确认成交", OTCTake, Match, EvAccept, ActorOwner, S1, TermNone},
		{"OTC 上传回执", OTCTake, S3, EvReceipt, ActorOwner, S4, TermNone},
		{"OTC 核验放款", OTCTake, S4, EvTick, ActorSystem, S5, TermCompleted},
		{"OTC 转账逾期", OTCTake, S3, EvTick, ActorSystem, Expired, TermExpired},
	}
	for _, c := range legal {
		t.Run(c.name, func(t *testing.T) {
			term, err := Check(c.kind, c.from, c.ev, c.actor, c.to)
			if err != nil {
				t.Fatalf("legal transition rejected: %v", err)
			}
			if term != c.term {
				t.Fatalf("terminal = %q, want %q", term, c.term)
			}
		})
	}

	illegal := []struct {
		name  string
		kind  Kind
		from  State
		ev    Event
		actor Actor
		to    State
	}{
		{"不许跳过对手方交付直接放款", ConditionalTransfer, AwaitingCounterparty, EvConfirm, ActorOwner, Released},
		{"我不能替对手方上传凭证", ConditionalTransfer, AwaitingCounterparty, EvEvidence, ActorOwner, AwaitingMe},
		{"对手方不能替我确认", ConditionalTransfer, AwaitingMe, EvConfirm, ActorCounterparty, Releasing},
		{"对手方不能撤我的单", ConditionalTransfer, Locked, EvCancel, ActorCounterparty, Cancelled},
		{"放行共识不能改判成撤销", ConditionalTransfer, Releasing, EvReleaseVote, ActorAgent, Cancelled},
		{"放行共识不能改判成异议", ConditionalTransfer, Releasing, EvReleaseVote, ActorAgent, Disputed},
		{"OTC 不许跳过注资直接转账", OTCTake, Match, EvReceipt, ActorOwner, S4},
		{"OTC 不许从 s1 直接放款", OTCTake, S1, EvTick, ActorSystem, S5},
		{"OTC 已进 s4 不能再撤", OTCTake, S4, EvCancel, ActorOwner, Cancelled},
		{"OTC 的事件不能用在条件支付上", ConditionalTransfer, Locked, EvAccept, ActorOwner, S1},
	}
	for _, c := range illegal {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Check(c.kind, c.from, c.ev, c.actor, c.to); err == nil {
				t.Fatalf("illegal transition accepted: %s %s -%s-> %s as %s", c.kind, c.from, c.ev, c.to, c.actor)
			}
		})
	}
}

// 终态后只读：四种终态的工单都不能再被推进。
func TestTerminalIsReadOnly(t *testing.T) {
	for _, term := range []Terminal{TermCompleted, TermCancelled, TermExpired, TermDisputed} {
		o := &Order{Kind: ConditionalTransfer, State: AwaitingMe, Terminal: term}
		if err := o.Apply(EvConfirm, ActorOwner, Releasing); err == nil {
			t.Fatalf("terminal %q accepted a transition", term)
		}
	}
}
