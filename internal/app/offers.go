package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/advaita/atara-pay/internal/agent"
	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/chain"
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

	// OfferID 是 /offers/prepare 预先分配的号。真链上卖单必须带它：
	// 币是做市方自己的钱包锁进合约的，锁的时候就得知道锁到哪个号下面，
	// 所以号要先发出去，锁完再拿着它来建挂单。
	OfferID string `json:"offer_id"`
	// LockTx 是那笔锁币交易。只用来存档——**判断依据是链上状态，不是这个哈希**。
	// 信哈希等于信客户端：随便贴一个别人的交易哈希也能过。
	LockTx string `json:"lock_tx"`
}

// PreparedOffer 是「你去锁币吧」这句话要说清的全部东西。
type PreparedOffer struct {
	// OfferID 建挂单时原样传回来。
	OfferID string `json:"offer_id"`
	// OfferKey 是合约里那个 bytes32。哈希规则留在后端一处——
	// 两边各算各的，一旦不一致，币会锁到一个后端找不到的号下面。
	OfferKey string `json:"offer_key"`
	Escrow   string `json:"escrow"`
	Token    string `json:"token"`
	Decimals int    `json:"decimals"`
	// AmountWei 是要锁的量，已经按代币精度换算好。前端不该自己乘 10^n：
	// BSC 上稳定币 18 位、以太坊上 6 位，算错就是 10^12 倍的差。
	AmountWei string `json:"amount_wei"`
	ChainID   int64  `json:"chain_id"`
	Network   string `json:"network"`
}

// PrepareOffer 发一个挂单号，并把锁币要用的参数一并算好。
//
// 为什么要有这一步：合约里 lockListing(offerId,…) 的 offerId 是主键，
// 前端发交易时就得带上它，而号是后端发的。所以顺序只能是「先要号、
// 再锁币、最后建挂单」——建挂单时后端去链上核对这个号下面到底锁了什么。
func (s *Service) PrepareOffer(ctx context.Context, makerID string, req CreateOfferReq) (*PreparedOffer, error) {
	if !money.IsCrypto(req.Asset) || !money.Tradable(req.Asset) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "UNKNOWN_ASSET", "asset",
			fmt.Sprintf("%s is not tradable — this version settles USDT and USDC", req.Asset))
	}
	qty, err := decimal.NewFromString(req.Qty)
	if err != nil || !qty.IsPositive() {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "INVALID_AMOUNT", "qty",
			"quantity must be greater than zero")
	}
	if !s.St.MakerApproved(ctx, makerID) {
		return nil, httpx.Fail(http.StatusForbidden, "MAKER_NOT_APPROVED", "",
			"your maker application has not cleared yet")
	}
	info := s.Ch.Info(ctx)
	tok := info.Tokens[strings.ToUpper(req.Asset)]
	if info.Impl != "evm" || tok.Address == "" {
		return nil, httpx.Fail(http.StatusConflict, "CHAIN_NOT_READY", "",
			"this deployment is not connected to a chain — listings do not lock on chain here")
	}
	id := store.NewID()
	return &PreparedOffer{
		OfferID: id, OfferKey: chain.OfferKey(id),
		Escrow: info.Escrow, Token: tok.Address, Decimals: tok.Decimals,
		AmountWei: toWei(qty, tok.Decimals), ChainID: info.ChainID, Network: info.Network,
	}, nil
}

// toWei 把人看的数换成合约里的最小单位。用 decimal 而不是 float：
// 0.1 在二进制里没有精确表示，用 float 换算金额迟早会差几个最小单位。
func toWei(v decimal.Decimal, decimals int) string {
	return v.Shift(int32(decimals)).Truncate(0).String()
}

// CreateOffer 挂单。挂出即锁币——买家看到的可成交量必须真的在托管里。
func (s *Service) CreateOffer(ctx context.Context, makerID, confirmToken string, req CreateOfferReq) (*model.Offer, error) {
	if req.Side != "buy" && req.Side != "sell" {
		return nil, httpx.Fail(http.StatusBadRequest, "INVALID_SIDE", "side", "side must be buy or sell")
	}
	/* 交易范围是产品决策，挂单这一关必须认它——
	   目录不发的币对，接口也不能收，否则范围就只是界面上的说法。 */
	if !money.IsCrypto(req.Asset) || !money.Tradable(req.Asset) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "UNKNOWN_ASSET", "asset",
			fmt.Sprintf("%s is not tradable — this version settles USDT and USDC", req.Asset))
	}
	if !money.IsFiat(req.Fiat) || !money.Tradable(req.Fiat) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "UNKNOWN_FIAT", "fiat",
			fmt.Sprintf("%s is not a settlement currency here — this version settles CNY, HKD and USD", req.Fiat))
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
	// selfLocked：币已经由做市方自己的钱包锁进合约了，后端只负责核验。
	// 真链上这是唯一正确的路径——合约认 msg.sender 当 maker，后端代签
	// 就变成后端的币进了托管，那不是非托管。
	selfLocked := false
	if req.Side == "sell" {
		if err := s.Confirm.Consume(ctx, confirmToken, makerID,
			Digest("offer", req.Asset, qty.String()), auth.GradeSignature); err != nil {
			return nil, err
		}
		if req.OfferID != "" {
			if err := s.verifyListingLock(ctx, req.OfferID, maker.Address, req.Asset, qty); err != nil {
				return nil, err
			}
			selfLocked = true
			lockTx = req.LockTx
		} else if err := s.requireOnChain(ctx, maker.Address, req.Asset, qty); err != nil {
			return nil, err
		}
	} else if err := s.Confirm.Consume(ctx, confirmToken, makerID,
		Digest("offer", req.Asset, qty.String()), auth.GradeCommit); err != nil {
		return nil, err
	}

	id := req.OfferID
	if id == "" {
		id = store.NewID()
	} else if existing, err := s.St.Offer(ctx, id); err == nil && existing != nil {
		// 同一个号建两次挂单：第二次会把第一次那笔锁仓算进来，凭空多出可成交量。
		return nil, httpx.Fail(http.StatusConflict, "OFFER_EXISTS", "offer_id",
			"that listing has already been created")
	}

	o := &model.Offer{
		ID: id, MakerID: makerID, Side: req.Side, Asset: req.Asset,
		Network: req.Network, Networks: req.Networks, Fiat: req.Fiat,
		UnitPrice: price, Qty: qty, RemainingQty: qty, MinLot: minLot,
		Status: "active", CreatedAt: time.Now().UTC(),
	}
	if req.Side == "sell" && !selfLocked {
		// mock 链与本地演示走这条：后端代锁。真链上走不到这儿——
		// 上面 selfLocked 已经把币核验过了。
		// 先上链再入库：链动作没有回滚，必须先成功。
		if lockTx, err = s.Ch.LockListing(ctx, o.ID, maker.Address, o.Asset, qty); err != nil {
			return nil, chainErr(err)
		}
	}
	o.LockTx = lockTx
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

// signerAddress 是后端那把私钥对应的地址。用来分辨「这笔锁仓是后端代锁的
// 还是用户自己锁的」——两者的解锁路径不一样。
func (s *Service) signerAddress() string {
	type signerer interface{ SignerAddress() string }
	if sa, ok := s.Ch.(signerer); ok {
		return sa.SignerAddress()
	}
	return ""
}

// PrepareDelist 给前端发解锁那一笔要用的参数。
func (s *Service) PrepareDelist(ctx context.Context, makerID, offerID string) (*PreparedOffer, error) {
	o, err := s.St.Offer(ctx, offerID)
	if err != nil {
		return nil, httpx.NotFound("offer")
	}
	if o.MakerID != makerID {
		return nil, httpx.Fail(http.StatusForbidden, "NOT_YOURS", "", "that listing belongs to another account")
	}
	info := s.Ch.Info(ctx)
	return &PreparedOffer{
		OfferID: o.ID, OfferKey: chain.OfferKey(o.ID),
		Escrow: info.Escrow, ChainID: info.ChainID, Network: info.Network,
	}, nil
}

// verifyListingLock 去链上核对这笔挂单到底锁了什么。
//
// 前端说它锁了，后端不看链就信，等于任何人都能 POST 一个挂单说自己锁了
// 100 万。四件事都要对上：锁的人是他、币种没错、挂单还开着、量够。
func (s *Service) verifyListingLock(ctx context.Context, offerID, maker, asset string,
	qty decimal.Decimal) error {
	l, err := s.Ch.ListingLockOf(ctx, offerID)
	if err != nil {
		return chainErr(err)
	}
	bad := func(msg string) error {
		return httpx.Fail(http.StatusUnprocessableEntity, "LOCK_NOT_FOUND", "offer_id", msg)
	}
	if l == nil {
		return bad("no coins are locked under that listing id yet — send the lock transaction first")
	}
	if !strings.EqualFold(l.Maker, maker) {
		return bad("those coins were locked by a different wallet")
	}
	if !strings.EqualFold(l.Token, asset) {
		return bad(fmt.Sprintf("that listing locked %s, not %s", l.Token, asset))
	}
	if !l.Open {
		return bad("that listing lock has already been released")
	}
	if l.Available().LessThan(qty) {
		return bad(fmt.Sprintf("only %s %s is locked — you are listing %s",
			l.Available(), asset, qty))
	}
	return nil
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
		//
		// 谁来解锁取决于当初是谁锁的。真链上是做市方自己的钱包锁的，
		// 合约只认原 maker —— 后端去调必然 revert。所以那条路上后端只核验
		// 「链上已经解开了」，解锁那一下由前端发。
		l, lerr := s.Ch.ListingLockOf(ctx, o.ID)
		selfLocked := lerr == nil && l != nil && !strings.EqualFold(l.Maker, s.signerAddress())
		switch {
		case selfLocked && l.Open:
			return httpx.Fail(http.StatusConflict, "UNLOCK_REQUIRED", "",
				"those coins were locked by your wallet — send the unlock transaction first").
				With(&httpx.Remedy{Action: "unlock_listing", Value: chain.OfferKey(o.ID)})
		case selfLocked:
			// 已经解开了，只剩记账
		default:
			if unlockTx, err = s.Ch.UnlockListing(ctx, o.ID); err != nil {
				return chainErr(err)
			}
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
	id := store.NewID()
	// 下单那一刻算一次分、收一次费、跑一次评估，三样都存进工单。
	// 之后只读——见 app/score.go 与 money/fee.go 里的说明。
	peer, _ := s.St.Merchant(ctx, o.MakerID)
	snap := ""
	if a, err := s.Assess(ctx, o.ID); err == nil {
		if b, err := json.Marshal(a); err == nil {
			snap = string(b)
		}
	}
	ord := &order.Order{
		ID: id, Ref: Ref(), Kind: order.OTCTake,
		OwnerID: takerID, CounterpartyID: o.MakerID,
		Asset: o.Asset, Amount: coinQty, AllowanceID: req.AllowanceID,
		TrustScore: ScoreOrder(id, peer, money.New(coinQty, o.Asset).USD()),
		FeeAmount:  money.Fee(fiatAmt, o.Fiat), FeeBps: money.TakerFeeBps,
		Assessment: snap,
		State:      order.Match, CreatedAt: now, UpdatedAt: now,
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
