package api

import (
	"github.com/advaita/atara-pay/internal/domain/condition"
	"github.com/advaita/atara-pay/internal/domain/order"
)

// rail 画执行轨道。站名跟真实条件类型走，不跟内部编译分支走——
// 等指标不是等日期，哪怕两者编译到同一条主分支。
// 每一站都写清「谁在等谁」。
func rail(o *order.Order) []railStop {
	if o.Kind == order.OTCTake {
		return otcRail(o)
	}
	return condRail(o)
}

// OTC 的轨道按买卖方向分叉。这不是文案差异，是事实差异：
// taker 买币时对方的币早在挂单那一刻就锁进合约了，所以第二站是「验证」，秒级；
// taker 卖币时是自己的币要上链，所以第二站是「入金」，要等确认数。
func otcRail(o *order.Order) []railStop {
	sell := o.OTC != nil && o.OTC.Side == "sell"
	var stops []railStop
	if sell {
		stops = []railStop{
			{"match", "Matched", "", "you to confirm"},
			{"s1", "Escrow funded", "", "the chain"},
			{"s3", "Their transfer", "", "the counterparty"},
			{"s4", "Verify & release", "", "the platform"},
		}
	} else {
		stops = []railStop{
			{"match", "Matched", "", "you to confirm"},
			{"s1", "Escrow verified", "", "the chain"},
			{"s3", "Your transfer", "", "you"},
			{"s4", "Verify & release", "", "the platform"},
		}
	}
	idx := map[order.State]int{order.Match: 0, order.S1: 1, order.S3: 2, order.S4: 3, order.S5: 4}
	return mark(stops, idx[o.State], o)
}

func condRail(o *order.Order) []railStop {
	// fund 站是非托管的第一拍：钱还没动，合约在等这一方入金。
	fund := railStop{"fund", "Fund escrow", "", "you"}
	lock := railStop{"lock", "Funds locked", "", ""}
	consensus := railStop{"cons", "Release consensus", "", "the protocol"}
	paid := railStop{"rel", "Released", "", ""}

	waiting := condition.WaitNow
	if o.Cond != nil {
		waiting = o.Cond.WaitingOn
	}
	var stops []railStop
	switch waiting {
	case condition.WaitApprove:
		stops = []railStop{fund, lock,
			{"deliv", "Their delivery", "", "the counterparty"},
			{"mine", "Your confirmation", "", "you"},
			consensus, paid}
	case condition.WaitEvidence:
		stops = []railStop{fund, lock,
			{"deliv", "Their evidence", "", "the counterparty"},
			{"mine", "Your window", "", "you"},
			consensus, paid}
	case condition.WaitData:
		stops = []railStop{fund, lock,
			{"deliv", "Waiting on the metric", "", "the data source"},
			consensus, paid}
	case condition.WaitTime:
		stops = []railStop{fund, lock,
			{"deliv", "Waiting for the date", "", "the clock"},
			consensus, paid}
	default:
		stops = []railStop{fund, lock, consensus, paid}
	}

	pos := map[order.State]int{order.Fund: 0, order.Locked: 1}
	switch waiting {
	case condition.WaitApprove, condition.WaitEvidence:
		pos[order.AwaitingCounterparty] = 2
		pos[order.AwaitingMe] = 3
		pos[order.Releasing] = 4
		pos[order.Released] = 5
	case condition.WaitData, condition.WaitTime:
		pos[order.AwaitingCounterparty] = 2
		pos[order.Releasing] = 3
		pos[order.Released] = 4
	default:
		pos[order.Releasing] = 2
		pos[order.Released] = 3
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
