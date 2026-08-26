package api

import (
	"context"
	"time"

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
	Amount      amountJSON  `json:"amount"`
	Note        string      `json:"note,omitempty"`
	Peer        string      `json:"counterparty_name,omitempty"`
	PeerID      string      `json:"counterparty_id,omitempty"`
	CardID      string      `json:"card_id,omitempty"`
	Deadline    *time.Time  `json:"state_deadline,omitempty"`
	SecondsLeft int         `json:"seconds_left"`
	Rail        []railStop  `json:"rail"`
	Condition   *condJSON   `json:"condition,omitempty"`
	OTC         *otcJSON    `json:"otc,omitempty"`
	Events      []order.Log `json:"events,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
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

type otcJSON struct {
	OfferID   string `json:"offer_id"`
	Side      string `json:"side"`
	UnitPrice string `json:"unit_price"`
	Fiat      string `json:"fiat_code"`
	FiatAmt   string `json:"fiat_amount"`
	Network   string `json:"network"`
	Receipt   string `json:"receipt_ref,omitempty"`
}

func (h *Handler) toOrder(ctx context.Context, o *order.Order, withEvents bool) orderJSON {
	j := orderJSON{
		ID: o.ID, Ref: o.Ref, Kind: string(o.Kind), State: string(o.State),
		Terminal: string(o.Terminal), Amount: amt(o.Amount, o.Asset), Note: o.Note,
		PeerID: o.CounterpartyID, CardID: o.CardID, Deadline: o.StateDeadline,
		Rail: rail(o), CreatedAt: o.CreatedAt,
	}
	if o.StateDeadline != nil {
		if d := int(time.Until(*o.StateDeadline).Seconds()); d > 0 {
			j.SecondsLeft = d
		}
	}
	if u, err := h.St.User(ctx, o.CounterpartyID); err == nil {
		j.Peer = u.DisplayName
	}
	if o.Cond != nil {
		c := &condJSON{Main: string(o.Cond.Main), WaitingOn: string(o.Cond.WaitingOn),
			Text: o.Cond.Text, Fallback: o.Cond.FallbackDays}
		for _, a := range o.Conds {
			c.Atoms = append(c.Atoms, conditionAtom{string(a.Type), a.Params})
		}
		j.Condition = c
	}
	if o.OTC != nil {
		t := &otcJSON{OfferID: o.OTC.OfferID, Side: o.OTC.Side, UnitPrice: o.OTC.UnitPrice.String(),
			Fiat: o.OTC.FiatCode, FiatAmt: o.OTC.FiatAmount.String(), Network: o.OTC.Network}
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
