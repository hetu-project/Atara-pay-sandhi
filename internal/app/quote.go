package app

import (
	"context"
	"fmt"

	"github.com/advaita/atara-pay/internal/agent"
	"github.com/advaita/atara-pay/internal/domain/condition"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/shopspring/decimal"
)

// Parse 把一句话变成订单草稿。推断出来的槽位列在 Guessed 里，
// 前端据此把它们标成虚线——「系统猜的」和「你说的」必须看得出区别。
func (s *Service) Parse(ctx context.Context, actorID, text string) (agent.Draft, error) {
	contacts, _ := s.St.Counterparties(ctx, actorID)
	in := agent.ParseInput{Text: text}
	for _, a := range money.Cryptos() {
		in.Assets = append(in.Assets, a.Code)
	}
	for _, f := range money.Fiats() {
		in.Fiats = append(in.Fiats, f.Code)
	}
	for _, c := range contacts {
		in.Contacts = append(in.Contacts, agent.Contact{ID: c.ID, Name: c.DisplayName})
	}
	return s.Ag.Parse(ctx, in)
}

type QuoteReq struct {
	Intent         string           `json:"intent"` // buy | sell | transfer
	Amount         string           `json:"amount"`
	AmountKind     string           `json:"amount_kind"`
	Asset          string           `json:"asset"`
	Fiat           string           `json:"fiat"`
	CounterpartyID string           `json:"counterparty_id"`
	CardID         string           `json:"card_id"`
	Conditions     []condition.Atom `json:"conditions"`
}

type QuoteResp struct {
	OK         bool          `json:"ok"`
	Violations []*httpx.Err  `json:"violations"`
	Preview    *QuotePreview `json:"preview,omitempty"`
}

type QuotePreview struct {
	Asset     string `json:"asset"`
	Coin      string `json:"coin_amount"`
	Fiat      string `json:"fiat_amount,omitempty"`
	FiatCode  string `json:"fiat_code,omitempty"`
	UnitPrice string `json:"unit_price,omitempty"`
	OfferID   string `json:"offer_id,omitempty"`
	PeerName  string `json:"counterparty_name,omitempty"`
	AmountUSD string `json:"amount_usd"`
	Condition string `json:"condition_text,omitempty"`
	WaitingOn string `json:"waiting_on,omitempty"`
}

// Quote 是 R4 前置拦截的接口化：后续必然失败的，在提交前一次性全报出来。
// 刻意收集**全部** violations 而不是第一个——前端才能一次把所有错标在字段上。
func (s *Service) Quote(ctx context.Context, actorID string, req QuoteReq) (*QuoteResp, error) {
	resp := &QuoteResp{Violations: []*httpx.Err{}}
	add := func(e *httpx.Err) { resp.Violations = append(resp.Violations, e) }

	amt, err := decimal.NewFromString(req.Amount)
	if err != nil || !amt.IsPositive() {
		add(httpx.Fail(422, "INVALID_AMOUNT", "amount", "amount must be greater than zero"))
		amt = decimal.Zero
	}
	if !money.IsCrypto(req.Asset) {
		add(httpx.Fail(422, "UNKNOWN_ASSET", "asset", fmt.Sprintf("%q is not a settleable asset", req.Asset)))
	}
	if err := condition.Validate(req.Conditions); err != nil {
		add(httpx.Fail(400, "TOO_MANY_CONDITIONS", "conditions", err.Error()))
	}

	prev := &QuotePreview{Asset: req.Asset, Coin: amt.String()}
	c := condition.Compile(req.Conditions, 14)
	prev.Condition, prev.WaitingOn = c.Text, string(c.Waiting)

	if req.Intent == "transfer" {
		if req.CounterpartyID == "" {
			add(httpx.Fail(422, "NO_COUNTERPARTY", "counterparty_id", "pick who gets paid"))
		} else if u, err := s.St.User(ctx, req.CounterpartyID); err != nil {
			add(httpx.Fail(422, "NO_COUNTERPARTY", "counterparty_id", "no such counterparty"))
		} else {
			prev.PeerName = u.DisplayName
		}
		if money.IsCrypto(req.Asset) && amt.IsPositive() {
			if err := s.requireBalance(ctx, actorID, req.Asset, amt); err != nil {
				add(err.(*httpx.Err))
			}
			if _, err := s.checkCard(ctx, actorID, req.CardID, money.New(amt, req.Asset)); err != nil {
				add(err.(*httpx.Err))
			}
		}
		prev.AmountUSD = money.New(amt, req.Asset).USD().Round(2).String()
		resp.Preview = prev
		resp.OK = len(resp.Violations) == 0
		return resp, nil
	}

	// buy / sell：先看这个币种/法币到底有没有对手方
	wantSide := "sell"
	if req.Intent == "sell" {
		wantSide = "buy"
	}
	offers, _ := s.St.Offers(ctx, store.OfferFilter{Side: wantSide, Asset: req.Asset, Fiat: req.Fiat, Status: "active"})
	if len(offers) == 0 {
		alts, _ := s.St.Offers(ctx, store.OfferFilter{Side: wantSide, Asset: req.Asset, Status: "active"})
		seen := map[string]bool{}
		var fiats []string
		for _, o := range alts {
			if !seen[o.Fiat] {
				seen[o.Fiat] = true
				fiats = append(fiats, o.Fiat)
			}
		}
		e := httpx.Fail(422, "NO_FIAT_CORRIDOR", "fiat",
			fmt.Sprintf("no %s/%s counterparty in the pool right now", req.Asset, req.Fiat))
		if len(fiats) > 0 {
			e = e.With(&httpx.Remedy{Action: "set_fiat", Value: fiats[0], Values: fiats,
				Label: "Settle in " + fiats[0] + " instead"})
		} else {
			e.Code = "NO_COUNTERPARTY"
		}
		add(e)
		resp.Preview = prev
		return resp, nil
	}

	// 挑信任分最高的那条作为报价基准
	best := offers[0]
	for _, o := range offers {
		if o.Merchant != nil && best.Merchant != nil && o.Merchant.TrustScore > best.Merchant.TrustScore {
			best = o
		}
	}
	coin, fiat, err2 := s.resolveAmount(best, TakeReq{Amount: req.Amount, AmountKind: req.AmountKind})
	if err2 == nil {
		prev.Coin, prev.Fiat = coin.String(), fiat.Round(2).String()
		prev.FiatCode, prev.UnitPrice, prev.OfferID = best.Fiat, best.UnitPrice.String(), best.ID
		prev.PeerName = best.Maker.DisplayName
		prev.AmountUSD = money.New(coin, best.Asset).USD().Round(2).String()
		if v := s.checkLot(best, fiat); v != nil {
			add(v)
		}
	}
	resp.Preview = prev
	resp.OK = len(resp.Violations) == 0
	return resp, nil
}
