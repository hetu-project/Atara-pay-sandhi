package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/advaita/atara-pay/internal/agent"
	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/advaita/atara-pay/internal/store"
)

// amountJSON 是金额的线上格式：字符串主单位 + 资产码 + 精度。
// 不用 JSON number——解析端的 float 会悄悄改掉尾数。
type amountJSON struct {
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	Scale  int32  `json:"scale"`
}

func amt(v interface{ String() string }, asset string) amountJSON {
	return amountJSON{Amount: v.String(), Asset: asset, Scale: money.Scale(asset)}
}

type offerJSON struct {
	ID        string    `json:"id"`
	Side      string    `json:"side"`
	Asset     string    `json:"asset"`
	Network   string    `json:"network"`
	Networks  []string  `json:"networks"`
	Fiat      string    `json:"fiat"`
	Price     string    `json:"unit_price"`
	Qty       string    `json:"qty"`
	Remaining string    `json:"remaining_qty"`
	Ceiling   string    `json:"fiat_ceiling"`
	MinLot    string    `json:"min_lot"`
	Status    string    `json:"status"`
	Maker     makerJSON `json:"maker"`
	Created   time.Time `json:"created_at"`
}

// makerJSON 是挂单卡上必须出现的那组字段。
// 资质件缺项也照发——缺件也公开，让买家自己给缺口定价。
type makerJSON struct {
	Name        string          `json:"name"`
	PeerCode    string          `json:"peer_code"`
	TrustScore  int             `json:"trust_score"`
	Deals       int             `json:"deals"`
	Disputes    int             `json:"disputes"`
	FillRate    string          `json:"fill_rate"`
	ReleaseSecs int             `json:"median_release_secs"`
	Docs        map[string]bool `json:"docs"`
}

func toOffer(o *model.Offer) offerJSON {
	j := offerJSON{
		ID: o.ID, Side: o.Side, Asset: o.Asset, Network: o.Network, Networks: o.Networks,
		Fiat: o.Fiat, Price: o.UnitPrice.String(), Qty: o.Qty.String(),
		Remaining: o.RemainingQty.String(), Ceiling: o.FiatCeiling().Round(2).String(),
		MinLot: o.MinLot.String(), Status: o.Status, Created: o.CreatedAt,
	}
	if o.Maker != nil {
		j.Maker.Name = o.Maker.DisplayName
	}
	if o.Merchant != nil {
		j.Maker.PeerCode = o.Merchant.PeerCode
		j.Maker.TrustScore = o.Merchant.TrustScore
		j.Maker.Deals = o.Merchant.Deals
		j.Maker.Disputes = o.Merchant.Disputes
		j.Maker.FillRate = o.Merchant.FillRate.String()
		j.Maker.ReleaseSecs = o.Merchant.MedianReleaseSecs
		j.Maker.Docs = o.Merchant.Docs
	}
	return j
}

type railStop struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	State string `json:"state"` // done | now | next
	Who   string `json:"waiting_on,omitempty"`
}

type orderJSON struct {
	ID          string      `json:"id"`
	Ref         string      `json:"ref"`
	Kind        string      `json:"kind"`
	State       string      `json:"state"`
	Terminal    string      `json:"terminal,omitempty"`
	Phase       *string     `json:"phase"`
	Actor       *string     `json:"actor"`
	Amount      amountJSON  `json:"amount"`
	Note        string      `json:"note,omitempty"`
	Peer        string      `json:"counterparty_name,omitempty"`
	PeerID      string      `json:"counterparty_id,omitempty"`
	AllowanceID string      `json:"card_id,omitempty"`
	Deadline    *time.Time  `json:"state_deadline,omitempty"`
	SecondsLeft int         `json:"seconds_left"`
	Escrow      *escrowJSON `json:"escrow,omitempty"`
	Rail        []railStop  `json:"rail"`
	Condition   *condJSON   `json:"condition,omitempty"`
	OTC         *otcJSON    `json:"otc,omitempty"`
	Events      []order.Log `json:"events,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	// TrustScore 是下单那一刻算出来的风控评分（60–99）。存在工单上，
	// 每次拉取原样发出去，不重算——重算的话历史单的分会跟着后来的事变。
	TrustScore int `json:"trust_score"`

	// 下面三块让同一份响应能画出工单卡的每一个状态：待确认那张、
	// 进行中那张、以及完成后那张证据卡。它们的内容本来就是同一份，
	// 只是状态不同——所以不该是三个接口。
	//
	// PeerProfile 是对手方的成绩单与资质件（Track record / Documents 两行）。
	PeerProfile *peerJSON `json:"peer_profile,omitempty"`
	// Fee 是这一单的手续费，下单那一刻定死的。
	Fee *feeJSON `json:"fee,omitempty"`
	// Assessment 是下单前那次风控评估的快照。没跑过就没有。
	Assessment *assessJSON `json:"assessment,omitempty"`
	// Evidence 只有终态才有：这单最后靠什么收的口。
	Evidence *evidenceJSON `json:"evidence,omitempty"`
}

// peerJSON 是对手方的公开成绩单。分数和资质件都是给人做决定用的，
// 所以跟工单一起发——让前端再去拉一次，两个数就可能来自不同时刻。
type peerJSON struct {
	Name       string          `json:"name"`
	PeerCode   string          `json:"peer_code,omitempty"`
	Deals      int             `json:"deals"`
	Disputes   int             `json:"disputes"`
	TrustScore int             `json:"trust_score"`
	Docs       map[string]bool `json:"docs,omitempty"`
}

type feeJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
	// Bps 是基点。前端要显示 0.08% 就自己除 100——存整数没有小数歧义。
	Bps int `json:"bps"`
}

// assessJSON 是评估快照。字段跟 agent.Assessment 对齐，外加读了多少来源——
// 「读了 23 个来源 447 条记录」这句话得有数支撑，不能是前端编的。
type assessJSON struct {
	Score     int          `json:"score"`
	Passed    int          `json:"passed"`
	Total     int          `json:"total"`
	Threshold int          `json:"threshold"`
	Summary   string       `json:"summary"`
	Votes     []agent.Vote `json:"votes"`
	Sources   int          `json:"sources"`
	Records   int          `json:"records"`
	// TookMs 是这次评估真正花了多少毫秒。前端据此决定印不印秒数。
	TookMs int64 `json:"took_ms,omitempty"`
}

// evidenceJSON 是结算记录：这单凭什么放的款。
//
// 界面上那句「The evidence pack is the settlement record — receipt, escrow
// release and both signatures」说的就是这三样，所以这里就得给这三样，
// 不能只给一个链接。
type evidenceJSON struct {
	// ReceiptRef 是付款方交上来的银行凭证。
	ReceiptRef string `json:"receipt_ref,omitempty"`
	// Chain 是这单在链上留下的每一步：锁仓、绑定、放款。
	Chain []evidenceTx `json:"chain,omitempty"`
	// Outcome 是终态：completed / cancelled / expired / disputed。
	Outcome   string     `json:"outcome,omitempty"`
	SettledAt *time.Time `json:"settled_at,omitempty"`
}

type evidenceTx struct {
	Kind   string `json:"kind"`
	Amount string `json:"amount,omitempty"`
	TxHash string `json:"tx_hash,omitempty"`
	// Explorer 是这笔交易在区块浏览器上的地址。哈希不给链接的话，
	// 用户要自己认出这是哪条链、再去找对应的浏览器——而这条链是哪条，
	// 只有我们知道。
	Explorer string    `json:"explorer,omitempty"`
	Memo     string    `json:"memo,omitempty"`
	At       time.Time `json:"at"`
}

type condJSON struct {
	Main      string          `json:"main_branch"`
	WaitingOn string          `json:"waiting_on"`
	Text      string          `json:"condition_text"`
	Fallback  int             `json:"fallback_days"`
	Atoms     []conditionAtom `json:"atoms"`
}

type conditionAtom struct {
	Type   string            `json:"atom_type"`
	Params map[string]string `json:"params"`
}

// escrowJSON 是那个链上观察窗要的东西：合约地址、走了几个确认、tx 在哪看。
// 这些是链的事实，不是平台的账。
type escrowJSON struct {
	Contract      string `json:"contract"`
	Network       string `json:"network"`
	Explorer      string `json:"explorer"`
	FundingVia    string `json:"funding_via,omitempty"`
	TxHash        string `json:"tx_hash,omitempty"`
	Confirmations int    `json:"confirmations"`
	Required      int    `json:"required"`
	NeedsFunding  bool   `json:"needs_funding"`
}

type otcJSON struct {
	OfferID    string `json:"offer_id"`
	Side       string `json:"side"`
	FundingVia string `json:"funding_via,omitempty"`
	UnitPrice  string `json:"unit_price"`
	Fiat       string `json:"fiat_code"`
	FiatAmt    string `json:"fiat_amount"`
	Network    string `json:"network"`
	Receipt    string `json:"receipt_ref,omitempty"`
}

func (h *Handler) toOrder(ctx context.Context, viewerID string, o *order.Order, withEvents bool) orderJSON {
	j := orderJSON{
		ID: o.ID, Ref: o.Ref, Kind: string(o.Kind), State: string(o.State),
		Terminal: string(o.Terminal), Amount: amt(o.Amount, o.Asset), Note: o.Note,
		PeerID: o.CounterpartyID, AllowanceID: o.AllowanceID, Deadline: o.StateDeadline,
		Rail: rail(o), TrustScore: o.TrustScore, CreatedAt: o.CreatedAt,
	}
	if o.StateDeadline != nil {
		if d := int(time.Until(*o.StateDeadline).Seconds()); d > 0 {
			j.SecondsLeft = d
		}
	}
	if u, err := h.St.User(ctx, o.CounterpartyID); err == nil {
		j.Peer = u.DisplayName
		// 对手方的成绩单跟工单一起发。让前端再拉一次的话，卡上那两行
		// 可能来自不同时刻——「124 trades」和「score 90」对不上就很难解释。
		p := &peerJSON{Name: u.DisplayName}
		if m, err := h.St.Merchant(ctx, o.CounterpartyID); err == nil {
			p.PeerCode, p.Deals, p.Disputes = m.PeerCode, m.Deals, m.Disputes
			p.TrustScore, p.Docs = m.TrustScore, m.Docs
		}
		j.PeerProfile = p
	}
	if o.FeeBps > 0 || o.FeeAmount.IsPositive() {
		ccy := ""
		if o.OTC != nil {
			ccy = o.OTC.FiatCode
		}
		j.Fee = &feeJSON{Amount: o.FeeAmount.String(), Currency: ccy, Bps: o.FeeBps}
	}
	if o.Assessment != "" {
		var a assessJSON
		if err := json.Unmarshal([]byte(o.Assessment), &a); err == nil {
			// 来源数与记录数由评估器报，接口层不补也不猜——
			// 那句话是在向用户交代「凭什么」，编一个好看的数就是拿假话
			// 去支撑一个判断。旧数据里没有就是没有，前端不显示那一行。
			j.Assessment = &a
		}
	}
	// 证据包只有终态才有：这单最后靠什么收的口。
	if o.Terminal != "" {
		ev := &evidenceJSON{Outcome: string(o.Terminal)}
		if r, ok := h.St.LatestReceipt(ctx, o.ID); ok {
			ev.ReceiptRef = r.FileRef
			ev.SettledAt = r.VerifiedAt
		}
		if evs, err := h.St.ChainEvents(ctx, o.ID); err == nil {
			for _, e := range evs {
				ev.Chain = append(ev.Chain, evidenceTx{
					Kind: e.Kind, Amount: e.Amount.String(), TxHash: e.TxHash,
					// 链接由链自己给：mock 下是空串，前端就只印哈希不给链接。
					// 按 EscrowNetwork 去 money.ChainOf 查是错的——mock 把网络
					// 名报成 "Ethereum"，于是假哈希配上了 etherscan 的真链接。
					Explorer: h.Svc.Ch.TxURL(e.TxHash),
					Memo:     e.Memo, At: e.At,
				})
			}
		}
		j.Evidence = ev
	}
	if o.Cond != nil {
		c := &condJSON{Main: string(o.Cond.Main), WaitingOn: string(o.Cond.WaitingOn),
			Text: o.Cond.Text, Fallback: o.Cond.FallbackDays}
		for _, a := range o.Conds {
			c.Atoms = append(c.Atoms, conditionAtom{string(a.Type), a.Params})
		}
		j.Condition = c
	}
	// 阶段是按观察者算的：同一张单，付法币的一方看到 pay，另一方看到 wait。
	if p, a, ok := o.PhaseFor(viewerID); ok {
		ps, as := string(p), string(a)
		j.Phase, j.Actor = &ps, &as
	}
	j.Escrow = &escrowJSON{
		Contract: o.EscrowAddr, Network: o.EscrowNetwork,
		Explorer:   h.Svc.Ch.ExplorerURL(o.Asset, o.EscrowAddr),
		FundingVia: o.FundingVia, TxHash: o.EscrowTx,
		Confirmations: o.Confirmations, Required: o.Required,
		NeedsFunding: o.NeedsFunding(),
	}
	if o.OTC != nil {
		t := &otcJSON{OfferID: o.OTC.OfferID, Side: o.OTC.Side, FundingVia: o.OTC.FundingVia,
			UnitPrice: o.OTC.UnitPrice.String(),
			Fiat:      o.OTC.FiatCode, FiatAmt: o.OTC.FiatAmount.String(), Network: o.OTC.Network}
		if ref, ok := h.St.Receipt(ctx, o.ID); ok {
			t.Receipt = ref
		}
		j.OTC = t
	}
	if withEvents {
		j.Events, _ = h.St.Events(ctx, o.ID)
	}
	return j
}

var _ = store.NewID
