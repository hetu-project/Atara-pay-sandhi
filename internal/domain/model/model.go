// Package model 放跨层共用的领域实体。
// 工单聚合根另有 domain/order（它带状态机，不适合和纯数据结构混在一起）。
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

type User struct {
	ID          string    `json:"id"`
	Handle      string    `json:"handle"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Kind        string    `json:"kind"` // person | firm | agent
	CreatedAt   time.Time `json:"created_at"`
}

// Merchant 是挂单卡上必须出现的那组字段。
// 缺件也公开——让买家自己给缺口定价，而不是平台替他隐藏。
type Merchant struct {
	UserID            string          `json:"user_id"`
	PeerCode          string          `json:"peer_code"` // D118500
	TrustScore        int             `json:"trust_score"`
	Deals             int             `json:"deals"`
	Disputes          int             `json:"disputes"`
	FillRate          decimal.Decimal `json:"fill_rate"`
	MedianReleaseSecs int             `json:"median_release_secs"`
	Docs              map[string]bool `json:"docs"` // kyc/pof/stm/poa/sow/chain
}

type Wallet struct {
	ID        string          `json:"-"`
	UserID    string          `json:"-"`
	Asset     string          `json:"asset"`
	Available decimal.Decimal `json:"available"`
	Escrowed  decimal.Decimal `json:"escrowed"`
}

// Card 是授权卡。人自己也是一张卡，agent 各是一张——
// 它们本来就是同一种对象：主账户上的一份支配权，带额度与条件。
type Card struct {
	ID         string           `json:"id"`
	OwnerID    string           `json:"-"`
	Name       string           `json:"name"`
	Kind       string           `json:"kind"`  // person | agent
	Cycle      string           `json:"cycle"` // weekly | monthly
	Quota      *decimal.Decimal `json:"quota"` // USD 口径；nil = 无周期上限
	Used       decimal.Decimal  `json:"used"`
	PerDealCap *decimal.Decimal `json:"per_deal_cap"`
	Allowlist  string           `json:"allowlist"`
	Template   string           `json:"template"`
	Enabled    bool             `json:"enabled"`
	Note       string           `json:"note"`
}

type Offer struct {
	ID           string          `json:"id"`
	MakerID      string          `json:"-"`
	Side         string          `json:"side"` // maker 视角：buy | sell
	Asset        string          `json:"asset"`
	Network      string          `json:"network"`
	Networks     []string        `json:"networks"`
	Fiat         string          `json:"fiat"`
	UnitPrice    decimal.Decimal `json:"unit_price"`
	Qty          decimal.Decimal `json:"qty"`
	RemainingQty decimal.Decimal `json:"remaining_qty"`
	MinLot       decimal.Decimal `json:"min_lot"` // 法币口径
	Status       string          `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`

	Maker    *User     `json:"-"`
	Merchant *Merchant `json:"-"`
}

// FiatCeiling 是这条挂单的可成交上限（法币口径）。
func (o Offer) FiatCeiling() decimal.Decimal { return o.RemainingQty.Mul(o.UnitPrice) }
