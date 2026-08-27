package api

import (
	"github.com/advaita/atara-pay/internal/domain/condition"
	"github.com/advaita/atara-pay/internal/domain/order"
)

// rail 画执行轨道。站名跟真实条件类型走，不跟内部编译分支走——
// 等指标不是等日期，哪怕两者编译到同一条主分支。
// 每一站都写清「谁在等谁」。
func rail(o *order.Order) []railStop {
	var stops []railStop
	var cur order.State

	if o.Kind == order.OTCTake {
		stops = []railStop{
			{"match", "Matched", "", "you to confirm"},
			{"s1", "Escrow funded", "", "the counterparty"},
			{"s3", "Your transfer", "", "you"},
			{"s4", "Verify & release", "", "the platform"},
		}
		cur = o.State
		idx := map[order.State]int{order.Match: 0, order.S1: 1, order.S3: 2, order.S4: 3, order.S5: 4}
		return mark(stops, idx[cur], o)
	}

	lock := railStop{"lock", "Funds locked", "", ""}
	consensus := railStop{"consensus", "Release consensus", "", "the protocol"}
	paid := railStop{"paid", "Released", "", ""}

	waiting := condition.WaitNow
	if o.Cond != nil {
		waiting = o.Cond.WaitingOn
	}
	switch waiting {
	case condition.WaitApprove:
		stops = []railStop{lock,
			{"deliv", "They deliver", "", "the counterparty"},
			{"mine", "You confirm", "", "you"},
			consensus, paid}
	case condition.WaitEvidence:
		stops = []railStop{lock,
			{"deliv", "They upload evidence", "", "the counterparty"},
			{"mine", "Your dispute window", "", "you"},
			consensus, paid}
	case condition.WaitData:
		stops = []railStop{lock,
			{"deliv", "Waiting for the metric to hit target", "", "the data source"},
			consensus, paid}
	case condition.WaitTime:
		stops = []railStop{lock,
			{"deliv", "Waiting for the date", "", "the clock"},
			consensus, paid}
	default:
		stops = []railStop{lock, consensus, paid}
	}

	pos := map[order.State]int{order.Locked: 0}
	switch waiting {
	case condition.WaitApprove, condition.WaitEvidence:
		pos[order.AwaitingCounterparty] = 1
		pos[order.AwaitingMe] = 2
		pos[order.Releasing] = 3
		pos[order.Released] = 4
	case condition.WaitData, condition.WaitTime:
		pos[order.AwaitingCounterparty] = 1
		pos[order.Releasing] = 2
		pos[order.Released] = 3
	default:
		pos[order.Releasing] = 1
		pos[order.Released] = 2
	}
	return mark(stops, pos[o.State], o)
}

func mark(stops []railStop, at int, o *order.Order) []railStop {
	// 终态：撤销/超时/异议不落在轨道上，整条轨道停在它当时走到的地方。
	if o.IsTerminal() && o.Terminal != order.TermCompleted {
		at = -1
	}
	for i := range stops {
		switch {
		case at < 0:
			stops[i].State = "next"
		case i < at:
			stops[i].State = "done"
		case i == at:
			stops[i].State = "now"
		default:
			stops[i].State = "next"
		}
	}
	if at >= len(stops) {
		for i := range stops {
			stops[i].State = "done"
		}
	}
	return stops
}
