package order

import "testing"

// 阶段是「这条法币腿在两个视角下的投影」。
// OTC 只有一条法币腿：买币的一方付法币，卖币的一方收法币并核验。
// 同一张单在两个人眼里必须是互补的，所以每个用例都同时断言两侧。
func TestPhaseFor(t *testing.T) {
	// side 是 taker（= OwnerID）视角。taker 买币 → taker 付法币。
	mk := func(state State, side string) *Order {
		return &Order{
			Kind: OTCTake, State: state,
			OwnerID: "taker", CounterpartyID: "maker",
			OTC: &OTC{Side: side},
		}
	}

	cases := []struct {
		name   string
		order  *Order
		viewer string
		phase  Phase
		actor  Viewer
	}{
		// taker 买币：taker 是法币付方
		{"s1 双方都在等锁仓（买）", mk(S1, "buy"), "taker", PhaseLock, ViewerAuto},
		{"s3 付方该打款", mk(S3, "buy"), "taker", PhasePay, ViewerYou},
		{"s3 收方在等对方打款", mk(S3, "buy"), "maker", PhaseWait, ViewerThem},
		{"s3v 付方等对方核验", mk(S3V, "buy"), "taker", PhaseLock, ViewerAuto},
		{"s3v 收方该核验", mk(S3V, "buy"), "maker", PhaseVerify, ViewerYou},
		{"s4 锁仓中（买）", mk(S4, "buy"), "taker", PhaseLock, ViewerAuto},

		// taker 卖币：maker 是法币付方，两侧对调
		{"s3 卖方向：maker 该打款", mk(S3, "sell"), "maker", PhasePay, ViewerYou},
		{"s3 卖方向：taker 在等", mk(S3, "sell"), "taker", PhaseWait, ViewerThem},
		{"s3v 卖方向：taker 该核验", mk(S3V, "sell"), "taker", PhaseVerify, ViewerYou},
		{"s3v 卖方向：maker 等核验", mk(S3V, "sell"), "maker", PhaseLock, ViewerAuto},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, a, ok := c.order.PhaseFor(c.viewer)
			if !ok {
				t.Fatalf("期望有阶段，得到 ok=false")
			}
			if p != c.phase || a != c.actor {
				t.Fatalf("= (%q,%q), want (%q,%q)", p, a, c.phase, c.actor)
			}
		})
	}
}

// 没有阶段的三种情况：终态、Conditional、尚未接单的 Match 站。
func TestPhaseForReturnsNothing(t *testing.T) {
	cases := []struct {
		name  string
		order *Order
	}{
		{"终态单没有阶段", &Order{Kind: OTCTake, State: S5, Terminal: TermCompleted,
			OwnerID: "taker", CounterpartyID: "maker", OTC: &OTC{Side: "buy"}}},
		{"撤销的单没有阶段", &Order{Kind: OTCTake, State: Cancelled, Terminal: TermCancelled,
			OwnerID: "taker", CounterpartyID: "maker", OTC: &OTC{Side: "buy"}}},
		{"Match 站还没接单", &Order{Kind: OTCTake, State: Match,
			OwnerID: "taker", CounterpartyID: "maker", OTC: &OTC{Side: "buy"}}},
		{"条件支付不产出阶段", &Order{Kind: ConditionalTransfer, State: Locked,
			OwnerID: "taker", CounterpartyID: "maker"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, ok := c.order.PhaseFor("taker"); ok {
				t.Fatal("期望 ok=false")
			}
		})
	}
}

// 局外人看不到阶段。
func TestPhaseForStranger(t *testing.T) {
	o := &Order{Kind: OTCTake, State: S3, OwnerID: "taker", CounterpartyID: "maker",
		OTC: &OTC{Side: "buy"}}
	if _, _, ok := o.PhaseFor("someone-else"); ok {
		t.Fatal("局外人不该拿到阶段")
	}
}
