package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/advaita/atara-pay/internal/agent"
	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/ledger"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/shopspring/decimal"
)

// Offers 列挂单池。side 是**买家想做的方向**：想买就去看别人的卖单。
func (s *Service) Offers(ctx context.Context, wantSide, asset, fiat string) ([]*model.Offer, error) {
	f := store.OfferFilter{Asset: asset, Fiat: fiat, Status: "active"}
	switch wantSide {
	case "buy":
		f.Side = "sell"
	case "sell":
		f.Side = "buy"
	}
	return s.St.Offers(ctx, f)
}

type CreateOfferReq struct {
	Side      string   `json:"side"`
	Asset     string   `json:"asset"`
	Network   string   `json:"network"`
	Networks  []string `json:"networks"`
	Fiat      string   `json:"fiat"`
	UnitPrice string   `json:"unit_price"`
	Qty       string   `json:"qty"`
	MinLot    string   `json:"min_lot"`
}

// CreateOffer 挂单。挂出即锁币——买家看到的可成交量必须真的在托管里。
func (s *Service) CreateOffer(ctx context.Context, makerID, confirmToken string, req CreateOfferReq) (*model.Offer, error) {
	if req.Side != "buy" && req.Side != "sell" {
		return nil, httpx.Fail(http.StatusBadRequest, "INVALID_SIDE", "side", "side must be buy or sell")
	}
	if !money.IsCrypto(req.Asset) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "UNKNOWN_ASSET", "asset", "not a settleable asset")
	}
	if !money.IsFiat(req.Fiat) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "UNKNOWN_FIAT", "fiat", "not a settlement currency")
	}
	price, err1 := decimal.NewFromString(req.UnitPrice)
	qty, err2 := decimal.NewFromString(req.Qty)
	minLot, err3 := decimal.NewFromString(req.MinLot)
	if err1 != nil || !price.IsPositive() {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "INVALID_PRICE", "unit_price", "unit price must be greater than zero")
	}
	if err2 != nil || !qty.IsPositive() {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "INVALID_AMOUNT", "qty", "quantity must be greater than zero")
	}
	ceiling := qty.Mul(price)
	if err3 != nil || !minLot.IsPositive() || minLot.GreaterThan(ceiling) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "INVALID_MIN_LOT", "min_lot",
			fmt.Sprintf("the smallest lot must be between 0 and %s %s", ceiling.Round(2), req.Fiat))
	}
	if len(req.Networks) == 0 {
		req.Networks = []string{req.Network}
	}

	// 卖单锁的是要交割的币；买单不锁币（法币腿走银行，平台不代收法币）。
	if req.Side == "sell" {
		if err := s.requireBalance(ctx, makerID, req.Asset, qty); err != nil {
			return nil, err
		}
		if err := s.Confirm.Consume(confirmToken, makerID, Digest("offer", req.Asset, qty.String())); err != nil {
			return nil, err
		}
	}

	o := &model.Offer{
		ID: store.NewID(), MakerID: makerID, Side: req.Side, Asset: req.Asset,
		Network: req.Network, Networks: req.Networks, Fiat: req.Fiat,
		UnitPrice: price, Qty: qty, RemainingQty: qty, MinLot: minLot,
		Status: "active", CreatedAt: time.Now().UTC(),
	}
	err := s.St.Tx(ctx, func(tx *sql.Tx) error {
		if err := s.St.InsertOffer(tx, o); err != nil {
			return err
		}
		if req.Side == "sell" {
			return ledger.OfferLock(tx, makerID, o.Asset, qty, o.ID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.St.Offer(ctx, o.ID)
}

// Delist 下架。下架即解锁——挂着的币解回可用余额。
func (s *Service) Delist(ctx context.Context, makerID, offerID string) error {
	o, err := s.St.Offer(ctx, offerID)
	if err != nil {
		return httpx.NotFound("offer")
	}
	if o.MakerID != makerID {
		return httpx.Fail(http.StatusForbidden, "NOT_YOURS", "", "that listing belongs to another account")
	}
	if o.Status == "delisted" {
		return nil
	}
	return s.St.Tx(ctx, func(tx *sql.Tx) error {
		if err := store.SetOfferStatus(tx, o.ID, "delisted"); err != nil {
			return err
		}
		if o.Side == "sell" && o.RemainingQty.IsPositive() {
			return ledger.OfferUnlock(tx, makerID, o.Asset, o.RemainingQty, o.ID)
		}
		return nil
	})
}

type TakeReq struct {
	Amount     string `json:"amount"`
	AmountKind string `json:"amount_kind"` // coin | fiat
	Network    string `json:"network"`
	CardID     string `json:"card_id"`
}

// Take 吃单：建一条 otc_take 工单，软预留可成交量，**不动钱**。
// 承诺点在 Accept，不在这里——吃单那一刻还没有资金流出。
func (s *Service) Take(ctx context.Context, takerID, offerID string, req TakeReq) (*order.Order, error) {
	o, err := s.St.Offer(ctx, offerID)
	if err != nil {
		return nil, httpx.NotFound("offer")
	}
	if o.Status != "active" {
		return nil, httpx.Fail(http.StatusConflict, "OFFER_CLOSED", "", "that listing is no longer open")
	}
	if o.MakerID == takerID {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "SELF_TRADE", "", "you cannot take your own listing")
	}
	coinQty, fiatAmt, err := s.resolveAmount(o, req)
	if err != nil {
		return nil, err
	}
	if req.Network == "" {
		req.Network = o.Network
	}
	if !contains(o.Networks, req.Network) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "NETWORK_UNSUPPORTED", "network",
			fmt.Sprintf("%s does not settle on %s", o.Maker.DisplayName, req.Network)).
			With(&httpx.Remedy{Action: "set_network", Value: o.Network, Values: o.Networks,
				Label: "Settle on " + o.Network + " instead"})
	}
	if v := s.checkLot(o, fiatAmt); v != nil {
		return nil, v
	}

	// maker 卖 → taker 买；maker 买 → taker 卖
	takerSide := "buy"
	if o.Side == "buy" {
		takerSide = "sell"
	}

	now := time.Now().UTC()
	ord := &order.Order{
		ID: store.NewID(), Ref: Ref(), Kind: order.OTCTake,
		OwnerID: takerID, CounterpartyID: o.MakerID,
		Asset: o.Asset, Amount: coinQty, CardID: req.CardID,
		State: order.Match, CreatedAt: now, UpdatedAt: now,
		OTC: &order.OTC{
			OfferID: o.ID, Side: takerSide, UnitPrice: o.UnitPrice,
			FiatCode: o.Fiat, FiatAmount: fiatAmt, Network: req.Network,
		},
	}
	ord.StateDeadline = s.deadlineFor(ord)

	err = s.St.Tx(ctx, func(tx *sql.Tx) error {
		if err := s.St.InsertOrder(tx, ord); err != nil {
			return err
		}
		// 预留可成交量。并发吃同一挂单时这里是唯一的守门人。
		if err := store.ReserveQty(tx, o.ID, coinQty.Neg()); err != nil {
			return httpx.Fail(http.StatusConflict, "ABOVE_AVAILABLE_QTY", "amount",
				"someone else just took that volume — try a smaller amount")
		}
		return store.AppendEvent(tx, ord.ID, "", string(order.Match), order.ActorOwner,
			"Matched with "+o.Maker.DisplayName, map[string]string{"offer_id": o.ID})
	})
	if err != nil {
		return nil, err
	}
	return s.St.Order(ctx, ord.ID)
}

// resolveAmount 把「按币」或「按法币」两种口径换算成这笔单的两个数字。
func (s *Service) resolveAmount(o *model.Offer, req TakeReq) (coin, fiat decimal.Decimal, err error) {
	v, e := decimal.NewFromString(req.Amount)
	if e != nil || !v.IsPositive() {
		return coin, fiat, httpx.Fail(http.StatusUnprocessableEntity, "INVALID_AMOUNT", "amount",
			"amount must be greater than zero")
	}
	if req.AmountKind == "fiat" {
		return v.DivRound(o.UnitPrice, money.Scale(o.Asset)), v, nil
	}
	return v, v.Mul(o.UnitPrice).Round(2), nil
}

// checkLot 是 R4 前置拦截：低于最小单 / 超过可成交量都在提交前拦下，
// 并且各给一条点一下就能走通的出路。
func (s *Service) checkLot(o *model.Offer, fiatAmt decimal.Decimal) *httpx.Err {
	if fiatAmt.LessThan(o.MinLot) {
		return httpx.Fail(http.StatusUnprocessableEntity, "BELOW_MIN_LOT", "amount",
			fmt.Sprintf("%s %s is below %s's smallest lot", fiatAmt.Round(2), o.Fiat, o.Maker.DisplayName)).
			With(&httpx.Remedy{Action: "set_amount", Value: o.MinLot.String(),
				Label: fmt.Sprintf("Use the smallest lot — %s %s", o.MinLot.Round(2), o.Fiat)})
	}
	if ceiling := o.FiatCeiling(); fiatAmt.GreaterThan(ceiling) {
		return httpx.Fail(http.StatusUnprocessableEntity, "ABOVE_AVAILABLE_QTY", "amount",
			fmt.Sprintf("only %s %s is available on this listing", ceiling.Round(2), o.Fiat)).
			With(&httpx.Remedy{Action: "set_amount", Value: ceiling.String(),
				Label: fmt.Sprintf("Take the whole listing — %s %s", ceiling.Round(2), o.Fiat)})
	}
	return nil
}

// Assess 是对手方风控共识：挂单卡点进去要看的那张评估。
func (s *Service) Assess(ctx context.Context, offerID string) (agent.Assessment, error) {
	o, err := s.St.Offer(ctx, offerID)
	if err != nil {
		return agent.Assessment{}, httpx.NotFound("offer")
	}
	in := agent.AssessInput{PeerName: o.Maker.DisplayName}
	if o.Merchant != nil {
		in.TrustScore, in.Deals, in.Disputes, in.Docs =
			o.Merchant.TrustScore, o.Merchant.Deals, o.Merchant.Disputes, o.Merchant.Docs
	}
	return s.Ag.Assess(ctx, in)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
