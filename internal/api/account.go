package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/advaita/atara-pay/internal/app"
	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"
)

// Wallet 是账户页那块。非托管：余额读的是链，托管仓位是合约里的仓位——
// 两者读起来是两个地方，因为它们本来就是两个地方。
func (h *Handler) Wallet(w http.ResponseWriter, r *http.Request) {
	u := auth.Actor(r.Context())
	type row struct {
		Asset string `json:"asset"`
		// Network 是这笔余额实际所在的链，不是「这个币支持哪些链」。
		Network  string `json:"network"`
		OnChain  string `json:"on_chain"`
		InEscrow string `json:"in_escrow"`
		USD      string `json:"usd_value"`
	}
	rows := make([]row, 0, 4)
	onChain, escrowed := decimal.Zero, decimal.Zero
	info := h.Svc.Ch.Info(r.Context())
	// 余额是「某条链上的某个代币合约里有多少」，不是一个抽象的数。所以每行
	// 都标出它到底在哪条链上——原来标的是目录里那一串支持的网络，读的人会
	// 以为这笔钱在第一条链上，而它其实在后端连着的那条。
	//
	// 只列可交易的两种（USDT / USDC）：BTC / ETH 这一版没有合约地址，
	// 读不到余额，列出来只会是恒 0 的两行。
	net := info.Network
	if net == "" {
		net = "—"
	}
	for _, a := range money.Cryptos() {
		bal, err := h.Svc.Ch.Balance(r.Context(), u.Address, a.Code)
		if err != nil {
			httpx.Error(w, err)
			return
		}
		esc := h.Svc.EscrowedFor(r.Context(), u.ID, a.Code)
		if bal.IsZero() && esc.IsZero() {
			continue
		}
		onChain = onChain.Add(money.New(bal, a.Code).USD())
		escrowed = escrowed.Add(money.New(esc, a.Code).USD())
		rows = append(rows, row{Asset: a.Code, Network: net,
			OnChain: bal.String(), InEscrow: esc.String(),
			USD: money.New(bal.Add(esc), a.Code).USD().Round(2).String()})
	}
	escAddr, escNet := h.Svc.Ch.EscrowAddress("USDT")
	ok(w, map[string]any{
		// 地址就是账户。这里给的是身份，不是一个"充值地址"。
		"address":           u.Address,
		"wallet_kind":       u.WalletKind,
		"custody":           "self", // 平台不持有——这个字段是给前端写死那句话用的
		"on_chain_usd":      onChain.Round(2).String(),
		"in_escrow_usd":     escrowed.Round(2).String(),
		"total_usd":         onChain.Add(escrowed).Round(2).String(),
		"assets":            rows,
		"escrow_contract":   map[string]string{"address": escAddr, "network": escNet},
		"spending_contract": h.Svc.Ch.SpendingAddress(),
	})
}

// SearchAccounts 找人加联系人。名字模糊、地址精确。
//
// 为什么要有这个接口：前端原来是从公开挂单里推人名的，那只覆盖「正在挂单
// 的人」——对方没挂单就永远搜不到，哪怕账户真实存在。
func (h *Handler) SearchAccounts(w http.ResponseWriter, r *http.Request) {
	us, err := h.Svc.SearchAccounts(r.Context(), h.actorID(r), r.URL.Query().Get("q"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	type row struct {
		ID      string `json:"id"`
		Address string `json:"address"`
		Name    string `json:"name"`
		Kind    string `json:"kind"`
		// 已经互相加过的人不在结果里（SQL 已经排掉），这里带上成绩单，
		// 让人在点「添加」之前就看得见对方是谁。
		Deals      int `json:"deals"`
		TrustScore int `json:"trust_score"`
	}
	out := make([]row, 0, len(us))
	for _, u := range us {
		x := row{ID: u.ID, Address: u.Address, Name: u.DisplayName, Kind: u.Kind}
		if m, err := h.St.Merchant(r.Context(), u.ID); err == nil {
			x.Deals, x.TrustScore = m.Deals, m.TrustScore
		}
		out = append(out, x)
	}
	ok(w, map[string]any{"accounts": out})
}

// ContactRequests 是别人发给我、还没点头的请求。
func (h *Handler) ContactRequests(w http.ResponseWriter, r *http.Request) {
	cs, err := h.St.PendingRequests(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"requests": cs})
}

// AcceptContact 接受一条请求。
func (h *Handler) AcceptContact(w http.ResponseWriter, r *http.Request) {
	if err := h.Svc.AcceptContact(r.Context(), h.actorID(r), chi.URLParam(r, "id")); err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"status": "accepted"})
}

// ── 额度 ──

func (h *Handler) Allowances(w http.ResponseWriter, r *http.Request) {
	as, err := h.St.Allowances(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"allowances": as, "spending_contract": h.Svc.Ch.SpendingAddress()})
}

func (h *Handler) SaveAllowance(w http.ResponseWriter, r *http.Request) {
	var req app.AllowanceReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	req.ID = chi.URLParam(r, "id")
	a, err := h.Svc.SaveAllowance(r.Context(), h.actorID(r), h.confirmToken(r), req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	status := http.StatusOK
	if req.ID == "" {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, a)
}

func (h *Handler) RevokeAllowance(w http.ResponseWriter, r *http.Request) {
	a, err := h.Svc.RevokeAllowance(r.Context(), h.actorID(r), chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, a)
}

// ── 联系人 ──

func (h *Handler) Contacts(w http.ResponseWriter, r *http.Request) {
	cs, err := h.Svc.ContactCards(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"contacts": cs, "relationships": app.Relationships})
}

// AddContact 收一个字段：名字或地址。没有 ATR ID 了。
func (h *Handler) AddContact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query    string `json:"query"` // 名字或地址
		Label    string `json:"label"`
		Nickname string `json:"nickname"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	c, err := h.Svc.AddContact(r.Context(), h.actorID(r), req.Query, req.Label, req.Nickname)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, c)
}

// ── 登录与确认 ──

// Connect 是登录。浏览是开放的，动作才要连上账户。
// 四种方式落到同一个结果：一个地址。
func (h *Handler) Connect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method  string `json:"method"` // passkey | wallet | google | email
		Address string `json:"address"`
		Email   string `json:"email"`
		Name    string `json:"name"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	u, created, err := h.Svc.Connect(r.Context(), req.Method, req.Address, req.Email, req.Name)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, map[string]any{
		"user": u, "address": u.Address, "wallet_kind": u.WalletKind,
		// demo 里没有真会话：把地址回给前端，之后请求带 X-Atara-User 即可
		"header": auth.HeaderUser,
	})
}

func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	ok(w, auth.Actor(r.Context()))
}

// UpdateMe 目前只改展示名。
//
// 不开放改地址：地址是账户的唯一键，改它等于换一个账户，
// 而已有的订单、额度、联系人全都挂在旧地址上。
func (h *Handler) UpdateMe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName string `json:"display_name"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if name == "" {
		httpx.Error(w, httpx.Fail(http.StatusBadRequest, "NAME_REQUIRED", "display_name",
			"give the account a name"))
		return
	}
	if len([]rune(name)) > 64 {
		httpx.Error(w, httpx.Fail(http.StatusBadRequest, "NAME_TOO_LONG", "display_name",
			"64 characters at most"))
		return
	}
	u := auth.Actor(r.Context())
	if err := h.Svc.St.RenameUser(r.Context(), u.ID, name); err != nil {
		httpx.Error(w, err)
		return
	}
	u.DisplayName = name
	ok(w, u)
}

// PasskeyAssert 换取确认令牌。
// grade 分两档：signature 动钱，commit 只承诺——前端点的是哪个按钮，这里就是哪一档。
func (h *Handler) PasskeyAssert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope string   `json:"scope"`
		Parts []string `json:"parts"`
		Grade string   `json:"grade"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	g := auth.Grade(req.Grade)
	if g != auth.GradeCommit {
		g = auth.GradeSignature
	}
	digest := app.Digest(append([]string{req.Scope}, req.Parts...)...)
	tok, exp, err := h.Svc.Confirm.Issue(r.Context(), h.actorID(r), digest, g)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{
		"confirmation": tok, "expires_at": exp, "grade": g, "header": auth.HeaderConfirm,
	})
}

var _ = time.Now

// ── 法币收款账户 ──

func (h *Handler) BankAccounts(w http.ResponseWriter, r *http.Request) {
	u := auth.Actor(r.Context())
	list, err := h.Svc.BankAccounts(r.Context(), u.ID)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"accounts": list})
}

func (h *Handler) SaveBankAccount(w http.ResponseWriter, r *http.Request) {
	var req app.BankReq
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	u := auth.Actor(r.Context())
	a, err := h.Svc.SaveBankAccount(r.Context(), u.ID, chi.URLParam(r, "id"), req)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, a)
}

func (h *Handler) DeleteBankAccount(w http.ResponseWriter, r *http.Request) {
	u := auth.Actor(r.Context())
	if err := h.Svc.DeleteBankAccount(r.Context(), u.ID, chi.URLParam(r, "id")); err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]string{"status": "deleted"})
}
