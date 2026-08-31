package api

import (
	"net/http"
	"time"

	"github.com/advaita/atara-pay/internal/app"
	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/domain/model"
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
		Asset    string   `json:"asset"`
		OnChain  string   `json:"on_chain"`
		InEscrow string   `json:"in_escrow"`
		USD      string   `json:"usd_value"`
		Networks []string `json:"networks"`
	}
	rows := make([]row, 0, 4)
	onChain, escrowed := decimal.Zero, decimal.Zero
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
		rows = append(rows, row{a.Code, bal.String(), esc.String(),
			money.New(bal.Add(esc), a.Code).USD().Round(2).String(), a.Networks})
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
	cs, err := h.St.Contacts(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if cs == nil {
		cs = []*model.Contact{}
	}
	ok(w, map[string]any{"contacts": cs})
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
	tok, exp := h.Svc.Confirm.Issue(h.actorID(r), digest, g)
	ok(w, map[string]any{
		"confirmation": tok, "expires_at": exp, "grade": g, "header": auth.HeaderConfirm,
	})
}

var _ = time.Now
