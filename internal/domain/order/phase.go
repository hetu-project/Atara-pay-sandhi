package order

// Phase 是前端 OSTATE 的五个阶段。取值是前端的键名，不能改。
type Phase string

const (
	PhasePay    Phase = "pay"    // 该你打法币了
	PhaseVerify Phase = "verify" // 对方回执到了，该你核验
	PhaseWait   Phase = "wait"   // 等对方打法币
	PhaseLock   Phase = "lock"   // 锁仓中，没人需要动手
	PhaseRel    Phase = "rel"    // 放款中
)

// Viewer 说这一步该谁动手。前端据此决定按钮是亮的还是灰的。
type Viewer string

const (
	ViewerYou  Viewer = "you"
	ViewerThem Viewer = "them"
	ViewerAuto Viewer = "auto"
)

// fiatPayer 返回这笔 OTC 里出法币的那个人。
// OTC 只有一条法币腿：买币的一方付法币。Side 是 taker（= OwnerID）的视角，
// 所以 taker 买币时法币付方是 taker，卖币时是 maker。
func (o *Order) fiatPayer() string {
	if o.OTC != nil && o.OTC.Side == "sell" {
		return o.CounterpartyID
	}
	return o.OwnerID
}

// PhaseFor 算出这笔单在 viewerID 眼里的阶段。
// ok 为 false 表示此刻没有阶段可展示：终态、条件支付、尚未接单的 Match 站，
// 或者提问的人根本不是这笔单的两方之一。
//
// s1 归入 lock 而不是 pay：卖方向的 s1 是 taker 往合约注资的阶段，
// 币还没锁进托管。这时候催法币付方打钱，是在托管成立之前就让他掏钱。
func (o *Order) PhaseFor(viewerID string) (Phase, Viewer, bool) {
	if o.Kind != OTCTake || o.OTC == nil || o.IsTerminal() {
		return "", "", false
	}
	if viewerID != o.OwnerID && viewerID != o.CounterpartyID {
		return "", "", false
	}
	payer := o.fiatPayer()
	switch o.State {
	case S1, S4:
		return PhaseLock, ViewerAuto, true
	case S3:
		if viewerID == payer {
			return PhasePay, ViewerYou, true
		}
		return PhaseWait, ViewerThem, true
	case S3V:
		// 付方已经打完款，等对方核验——他没有动作可做。
		if viewerID == payer {
			return PhaseLock, ViewerAuto, true
		}
		return PhaseVerify, ViewerYou, true
	case S5:
		return PhaseRel, ViewerAuto, true
	}
	return "", "", false
}
