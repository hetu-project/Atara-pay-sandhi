package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/chain"
	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/shopspring/decimal"
)

// EscrowedFor 是这个人此刻锁在合约里的量：挂单锁的 + 未结工单锁的。
// 它读的是链，不是平台的账——平台没有账。
func (s *Service) EscrowedFor(ctx context.Context, userID, asset string) decimal.Decimal {
	total := decimal.Zero
	u, err := s.St.User(ctx, userID)
	if err != nil {
		return total
	}
	if lk, ok := s.Ch.(interface {
		ListingLocked(context.Context, string, string) (decimal.Decimal, error)
	}); ok {
		if v, err := lk.ListingLocked(ctx, u.Address, asset); err == nil {
			total = total.Add(v)
		}
	}
	orders, err := s.St.Orders(ctx, store.OrderFilter{Owner: userID, Open: true})
	if err != nil {
		return total
	}
	for _, o := range orders {
		if o.Asset != asset {
			continue
		}
		p, err := s.Ch.Position(ctx, o.ID)
		if err != nil || p == nil || p.Status != "escrowed" {
			continue
		}
		// 挂单锁的那部分已经在 ListingLocked 里算过了，别算两遍
		if p.OfferID != "" || p.Owner != u.Address {
			continue
		}
		total = total.Add(p.Amount)
	}
	return total
}

// ── 登录 ──

// Connect 是四种登录方式的落点。它们的差别只在拿到地址的路径：
// 自建 passkey 钱包、连已有钱包、Google、邮箱——最后都是一个地址。
// wallet_kind 决定额度怎么签发：atara 写账户合约策略，ext 走 approve。
func (s *Service) Connect(ctx context.Context, method, address, email, name string) (*model.User, bool, error) {
	switch method {
	case "passkey", "wallet", "google", "email":
	default:
		return nil, false, httpx.Fail(http.StatusBadRequest, "UNKNOWN_METHOD", "method",
			"connect with a passkey wallet, an existing wallet, Google, or an email")
	}
	walletKind := "atara"
	if method == "wallet" {
		walletKind = "ext" // 外部钱包：我们没有它的钥匙，额度只能靠 approve
		if strings.TrimSpace(address) == "" {
			return nil, false, httpx.Fail(http.StatusBadRequest, "ADDRESS_REQUIRED", "address",
				"paste the address of the wallet you want to connect")
		}
	}
	if address == "" {
		// 自托管钱包由 passkey 持有；Google / 邮箱路径也给一个地址——
		// 身份就是地址，不给地址就等于没开户。
		address = deriveAddress(method, email, name)
	}
	if u, err := s.St.UserByAddress(ctx, address); err == nil {
		return u, false, nil
	}
	if name == "" {
		name = shortAddr(address)
	}
	u := &model.User{
		ID: store.NewID(), Address: address, DisplayName: name, Email: email,
		Kind: "person", WalletKind: walletKind, LoginMethod: method, CreatedAt: time.Now().UTC(),
	}
	if err := s.St.Tx(ctx, func(tx *sql.Tx) error { return s.St.InsertUser(tx, u) }); err != nil {
		return nil, false, err
	}
	return u, true, nil
}

// deriveAddress 给 demo 造一个确定性地址：同一个邮箱每次进来都是同一个账户。
// 真实实现里这来自 passkey 生成的密钥对或钱包连接。
func deriveAddress(method, email, name string) string {
	seed := method + "|" + email + "|" + name
	h := 0
	for _, c := range seed {
		h = (h*31 + int(c)) % 999983
	}
	const abc = "abcdefghijkmnpqrstuvwxyz23456789"
	out := []byte("T")
	for i := 0; i < 33; i++ {
		out = append(out, abc[(h*(i+7)+i*13)%len(abc)])
	}
	return string(out)
}

func shortAddr(a string) string {
	if len(a) < 12 {
		return a
	}
	return a[:6] + "…" + a[len(a)-4:]
}

// ── 额度 ──

type AllowanceReq struct {
	ID         string `json:"-"`
	Spender    string `json:"spender"`
	Kind       string `json:"kind"`
	PerPayment string `json:"per_payment"`
	WindowCap  string `json:"window_cap"`
	Cycle      string `json:"cycle"`
	Expires    string `json:"expires"` // "30 days" | "90 days" | "" = 不过期
	Recipients string `json:"recipients"`
}

// SaveAllowance 开一份或改一份额度。
//
// 签发方式随钱包类型分叉：Atara 钱包写进账户合约策略，
// 外部钱包是对支出合约的 approve。两种都要 Passkey/钱包签名——
// 额度就是支配权，签发它本身就是一次授权动作。
func (s *Service) SaveAllowance(ctx context.Context, ownerID, confirmToken string, req AllowanceReq) (*model.Allowance, error) {
	u, err := s.St.User(ctx, ownerID)
	if err != nil {
		return nil, httpx.NotFound("user")
	}
	per, err1 := decimal.NewFromString(strings.TrimSpace(req.PerPayment))
	capv, err2 := decimal.NewFromString(strings.TrimSpace(req.WindowCap))
	if err1 != nil || !per.IsPositive() {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "INVALID_AMOUNT", "per_payment",
			"amounts must be above 0")
	}
	if err2 != nil || !capv.IsPositive() {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "INVALID_AMOUNT", "window_cap",
			"amounts must be above 0")
	}
	if per.GreaterThan(capv) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "CAP_ABOVE_WINDOW", "per_payment",
			"per payment cannot exceed the window total")
	}
	cycle := req.Cycle
	if cycle != "monthly" {
		cycle = "weekly"
	}
	if err := s.Confirm.Consume(confirmToken, ownerID,
		Digest("allowance", req.Spender, per.String(), capv.String()), auth.GradeSignature); err != nil {
		return nil, err
	}

	a := &model.Allowance{
		ID: req.ID, OwnerID: ownerID, Spender: req.Spender, Kind: req.Kind, Asset: "USDT",
		PerPayment: per, WindowCap: capv, Cycle: cycle, Recipients: req.Recipients,
		WalletKind: u.WalletKind, Status: "live",
	}
	if a.ID == "" {
		a.ID = store.NewID()
	} else if old, err := s.St.Allowance(ctx, a.ID); err == nil {
		if old.OwnerID != ownerID {
			return nil, httpx.Fail(http.StatusForbidden, "ALLOWANCE_FOREIGN", "", "not your allowance")
		}
		a.Used, a.Kind, a.Template, a.Note = old.Used, old.Kind, old.Template, old.Note
		if a.Spender == "" {
			a.Spender = old.Spender
		}
	}
	if a.Kind == "" {
		a.Kind = "agent"
	}
	if a.Recipients == "" {
		a.Recipients = "Any"
	}
	if d := expiryDays(req.Expires); d > 0 {
		t := time.Now().AddDate(0, 0, d).UTC()
		a.ExpiresAt = &t
	}
	if u.WalletKind == "ext" {
		a.Note = "Backed by an on-chain allowance — revoking cancels the approve"
	} else {
		a.Note = "Written into your account contract — revoking takes effect next block"
	}

	tx, err := s.Ch.GrantAllowance(ctx, chain.AllowanceGrant{
		ID: a.ID, Account: u.Address, WalletKind: u.WalletKind, Spender: a.Spender,
		Asset: a.Asset, PerPayment: per, WindowCap: capv, Cycle: cycle, ExpiresAt: a.ExpiresAt,
	})
	if err != nil {
		return nil, chainErr(err)
	}
	a.ChainTx = tx
	if err := s.St.SaveAllowance(ctx, a); err != nil {
		return nil, err
	}
	return s.St.Allowance(ctx, a.ID)
}

func expiryDays(s string) int {
	switch strings.TrimSpace(s) {
	case "30 days":
		return 30
	case "90 days":
		return 90
	}
	return 0
}

func (s *Service) RevokeAllowance(ctx context.Context, ownerID, id string) (*model.Allowance, error) {
	a, err := s.St.Allowance(ctx, id)
	if err != nil {
		return nil, httpx.NotFound("allowance")
	}
	if a.OwnerID != ownerID {
		return nil, httpx.Fail(http.StatusForbidden, "ALLOWANCE_FOREIGN", "", "not your allowance")
	}
	tx, err := s.Ch.RevokeAllowance(ctx, id)
	if err != nil {
		return nil, chainErr(err)
	}
	a.Status, a.ChainTx = "revoked", tx
	if err := s.St.SaveAllowance(ctx, a); err != nil {
		return nil, err
	}
	return s.St.Allowance(ctx, id)
}

// ── 联系人 ──

// AddContact 收一个字段：名字或地址。
// 不做模糊搜索——那等于开放一个可以遍历用户的接口。
func (s *Service) AddContact(ctx context.Context, ownerID, query, label, nickname string) (*model.Contact, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, httpx.Fail(http.StatusBadRequest, "CONTACT_REQUIRED", "query",
			"enter a name or an address")
	}
	u, err := s.St.ResolveContact(ctx, query)
	if err != nil {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "NO_SUCH_ACCOUNT", "query",
			fmt.Sprintf("no account matches %q — check the address", query))
	}
	if u.ID == ownerID {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "SELF_CONTACT", "query",
			"that is your own account")
	}
	if label == "" {
		label = "Client"
	}
	if err := s.St.AddContact(ctx, ownerID, u.ID, label, nickname); err != nil {
		return nil, err
	}
	return &model.Contact{ContactID: u.ID, Address: u.Address, Name: u.DisplayName,
		Kind: u.Kind, Label: label, Nickname: nickname}, nil
}

// ── 线程 ──

// Thread 是一个对手方的整条流：聊天、订单卡、系统播报共用一条。
func (s *Service) Thread(ctx context.Context, ownerID, peerID string) (map[string]any, error) {
	peer, err := s.St.User(ctx, peerID)
	if err != nil {
		return nil, httpx.NotFound("counterparty")
	}
	msgs, err := s.St.Thread(ctx, ownerID, peerID)
	if err != nil {
		return nil, err
	}
	orders, err := s.St.Orders(ctx, store.OrderFilter{Owner: ownerID, Peer: peerID})
	if err != nil {
		return nil, err
	}
	live := []*order.Order{}
	for _, o := range orders {
		oo, err := s.Order(ctx, o.ID)
		if err == nil {
			live = append(live, oo)
		}
	}
	out := map[string]any{"peer": peer, "messages": msgs, "orders": live}
	if m, err := s.St.Merchant(ctx, peerID); err == nil {
		out["merchant"] = m
	}
	return out, nil
}

func (s *Service) PostChat(ctx context.Context, ownerID, peerID, body string) (*model.Message, error) {
	if strings.TrimSpace(body) == "" {
		return nil, httpx.Fail(http.StatusBadRequest, "EMPTY_MESSAGE", "body", "nothing to send")
	}
	m := &model.Message{Author: "me", Kind: "chat", Body: body}
	if err := s.St.Post(ctx, ownerID, peerID, m); err != nil {
		return nil, err
	}
	return m, nil
}
