package api

import (
	"net/http"

	"github.com/advaita/atara-pay/internal/app"
	"github.com/advaita/atara-pay/internal/auth"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/shopspring/decimal"
)

// Wallet 是右栏那块：总额 / 可用 / 托管 + 分币种。
// 只有数字资产——法币不入账。
func (h *Handler) Wallet(w http.ResponseWriter, r *http.Request) {
	ws, err := h.St.Wallets(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	total, avail, esc := decimal.Zero, decimal.Zero, decimal.Zero
	type row struct {
		Asset     string   `json:"asset"`
		Available string   `json:"available"`
		Escrowed  string   `json:"escrowed"`
		USD       string   `json:"usd_value"`
		Networks  []string `json:"networks"`
	}
	rows := make([]row, 0, len(ws))
	for _, x := range ws {
		a, _ := money.Lookup(x.Asset)
		v := money.New(x.Available.Add(x.Escrowed), x.Asset).USD()
		total = total.Add(v)
		avail = avail.Add(money.New(x.Available, x.Asset).USD())
		esc = esc.Add(money.New(x.Escrowed, x.Asset).USD())
		rows = append(rows, row{x.Asset, x.Available.String(), x.Escrowed.String(), v.Round(2).String(), a.Networks})
	}
	ok(w, map[string]any{
		"total_usd": total.Round(2).String(), "available_usd": avail.Round(2).String(),
		"escrowed_usd": esc.Round(2).String(), "assets": rows,
	})
}

func (h *Handler) Cards(w http.ResponseWriter, r *http.Request) {
	cs, err := h.St.Cards(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, map[string]any{"cards": cs})
}

func (h *Handler) Counterparties(w http.ResponseWriter, r *http.Request) {
	cps, err := h.St.Counterparties(r.Context(), h.actorID(r))
	if err != nil {
		httpx.Error(w, err)
		return
	}
	type row struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Deals    int    `json:"deals"`
		FillRate string `json:"fill_rate"`
	}
	out := make([]row, 0, len(cps))
	for _, c := range cps {
		x := row{ID: c.ID, Name: c.DisplayName, Kind: c.Kind}
		if m, err := h.St.Merchant(r.Context(), c.ID); err == nil {
			x.Deals, x.FillRate = m.Deals, m.FillRate.String()
		}
		out = append(out, x)
	}
	ok(w, map[string]any{"counterparties": out})
}

// PasskeyAssert 换取支付确认令牌。R2 动钱必确认，无金额豁免。
// 一期不做真实 WebAuthn 验签，但令牌的绑定、一次性与过期都是真的。
func (h *Handler) PasskeyAssert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope string   `json:"scope"` // order | accept | offer
		Parts []string `json:"parts"` // 参与摘要的字段，顺序敏感
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	digest := app.Digest(append([]string{req.Scope}, req.Parts...)...)
	tok, exp := h.Svc.Confirm.Issue(h.actorID(r), digest)
	ok(w, map[string]any{"confirmation": tok, "expires_at": exp, "header": auth.HeaderConfirm})
}
