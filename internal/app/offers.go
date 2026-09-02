package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/advaita/atara-pay/internal/agent"
	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/httpx"
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

	maker, err := s.St.User(ctx, makerID)
	if err != nil {
		return nil, httpx.NotFound("user")
	}
	// 做市准入的闸门。两段审核都过了才能挂单——只在前端隐藏按钮拦不住 curl，
	// 而挂卖单会真的上链锁币，让未核验身份的人做这件事是不能接受的。
	if !s.St.MakerApproved(ctx, makerID) {
		return nil, httpx.Fail(http.StatusForbidden, "MAKER_NOT_APPROVED", "",
			"your maker application has not cleared yet — submit it under Discover and wait for review")
	}
	// 卖单锁的是要交割的币——挂出即锁币，锁进合约，不是锁在平台。
	// 买单不锁币：法币腿走银行，平台不代收法币，所以只是一句承诺。
	lockTx := ""
	if req.Side == "sell" {
		if err := s.Confirm.Consume(ctx, confirmToken, makerID,
			Digest("offer", req.Asset, qty.String()), auth.GradeSignature); err != nil {
			return nil, err
		}
		if err := s.requireOnChain(ctx, maker.Address, req.Asset, qty); err != nil {
			return nil, err
		}
	} else if err := s.Confirm.Consume(ctx, confirmToken, makerID,
		Digest("offer", req.Asset, qty.String()), auth.GradeCommit); err != nil {
		return nil, err
	}

	o := &model.Offer{
		ID: store.NewID(), MakerID: makerID, Side: req.Side, Asset: req.Asset,
		Network: req.Network, Networks: req.Networks, Fiat: req.Fiat,
		UnitPrice: price, Qty: qty, RemainingQty: qty, MinLot: minLot,
		Status: "active", CreatedAt: time.Now().UTC(),
	}
	if req.Side == "sell" {
		// 先上链再入库：链动作没有回滚，必须先成功。
		if lockTx, err = s.Ch.LockListing(ctx, o.ID, maker.Address, o.Asset, qty); err != nil {
			return nil, chainErr(err)
		}
		o.LockTx = lockTx
	}
	err = s.St.Tx(ctx, func(tx *sql.Tx) error {
		if err := s.St.InsertOffer(tx, o); err != nil {
			return err
		}
		if lockTx == "" {
			return nil
		}
		return store.LogChain(tx, makerID, store.ChainEvent{
			Kind: "listing_lock", Asset: o.Asset, Amount: qty, TxHash: lockTx,
			OfferID: o.ID, Memo: "posted and locked",
		})
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
	unlockTx := ""
	if o.Side == "sell" && o.RemainingQty.IsPositive() {
		// 下架即解锁：合约把剩下的币还回钱包。
		if unlockTx, err = s.Ch.UnlockListing(ctx, o.ID); err != nil {
			return chainErr(err)
		}
	}
	return s.St.Tx(ctx, func(tx *sql.Tx) error {
		if err := store.SetOfferStatus(tx, o.ID, "delisted"); err != nil {
			return err
		}
		if unlockTx == "" {
			return nil
		}
		return store.LogChain(tx, makerID, store.ChainEvent{
			Kind: "listing_unlock", Asset: o.Asset, Amount: o.RemainingQty,
			TxHash: unlockTx, OfferID: o.ID, Memo: "delisted",
		})
	})
}

type TakeReq struct {
	Amount      string `json:"amount"`
	AmountKind  string `json:"amount_kind"` // coin | fiat
	Network     string `json:"network"`
	AllowanceID string `json:"card_id"`
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
		Asset: o.Asset, Amount: coinQty, AllowanceID: req.AllowanceID,
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
		if err := store.AppendEvent(tx, ord.ID, "", string(order.Match), order.ActorOwner,
			"Matched with "+o.Maker.DisplayName, map[string]string{"offer_id": o.ID}); err != nil {
			return err
		}
		return store.PostTx(tx, takerID, o.MakerID, &model.Message{
			Author: "system", Kind: "order",
			Body: "Matched with " + o.Maker.DisplayName, OrderID: ord.ID,
		})
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

// ── 快捷交易的撮合拍 ──

type MatchReq struct {
	Intent     string `json:"intent"` // buy | sell
	Amount     string `json:"amount"`
	AmountKind string `json:"amount_kind"`
	Asset      string `json:"asset"`
	Fiat       string `json:"fiat"`
	// CounterpartyID 空表示 Any，也就是原来的快捷交易。指定了就只在这个人的
	// 挂单里撮合，撮不到就明确失败——用户点了「跟他交易」，成交对象却是别人，
	// 是最坏的结果，所以绝不静默回退到 Any。
	CounterpartyID string `json:"counterparty_id"`
}

type Candidate struct {
	OfferID    string `json:"offer_id"`
	Name       string `json:"name"`
	PeerID     string `json:"peer_id"`
	TrustScore int    `json:"trust_score"`
	Deals      int    `json:"deals"`
	UnitPrice  string `json:"unit_price"`
	Fiat       string `json:"fiat"`
	Coin       string `json:"coin_amount"`
	FiatAmount string `json:"fiat_amount"`
}

type MatchResp struct {
	Scanned    int         `json:"scanned"`
	Candidates []Candidate `json:"candidates"`
	Violation  *httpx.Err  `json:"violation,omitempty"`
}

// Match 先撮合、后评估：从池子里挑出成绩最好的三个候选。
//
// 顺序是刻意的。直接跳评估是逻辑倒置——对手方还没出现，评的是谁？
func (s *Service) Match(ctx context.Context, req MatchReq) (*MatchResp, error) {
	wantSide := "sell"
	if req.Intent == "sell" {
		wantSide = "buy"
	}
	all, err := s.St.Offers(ctx, store.OfferFilter{
		Side: wantSide, Asset: req.Asset, Fiat: req.Fiat, Status: "active",
		Maker: req.CounterpartyID})
	if err != nil {
		return nil, err
	}
	resp := &MatchResp{Scanned: len(all), Candidates: []Candidate{}}
	if len(all) == 0 {
		if req.CounterpartyID != "" {
			resp.Violation = httpx.Fail(422, "NO_MATCH_WITH_COUNTERPARTY", "counterparty_id",
				"that counterparty has no offer that can fill this order right now")
			return resp, nil
		}
		resp.Violation = httpx.Fail(422, "NO_COUNTERPARTY", "",
			"No live offers on that side right now")
		return resp, nil
	}
	// 成绩最好的排前面——快捷交易默认走第一个，所以排序就是默认选择
	sort.Slice(all, func(i, j int) bool { return score(all[i]) > score(all[j]) })
	for _, o := range all {
		if len(resp.Candidates) == 3 {
			break
		}
		coin, fiat, err := s.resolveAmount(o, TakeReq{Amount: req.Amount, AmountKind: req.AmountKind})
		if err != nil {
			continue
		}
		if v := s.checkLot(o, fiat); v != nil {
			// 装不下这笔量的挂单不该出现在候选里
			continue
		}
		c := Candidate{OfferID: o.ID, PeerID: o.MakerID, UnitPrice: o.UnitPrice.String(),
			Fiat: o.Fiat, Coin: coin.String(), FiatAmount: fiat.Round(2).String()}
		if o.Maker != nil {
			c.Name = o.Maker.DisplayName
		}
		if o.Merchant != nil {
			c.TrustScore, c.Deals = o.Merchant.TrustScore, o.Merchant.Deals
		}
		resp.Candidates = append(resp.Candidates, c)
	}
	if len(resp.Candidates) == 0 {
		best := all[0]
		coin, fiat, _ := s.resolveAmount(best, TakeReq{Amount: req.Amount, AmountKind: req.AmountKind})
		_ = coin
		resp.Violation = s.checkLot(best, fiat)
	}
	return resp, nil
}

func score(o *model.Offer) int {
	if o.Merchant == nil {
		return 0
	}
	return o.Merchant.TrustScore
}
