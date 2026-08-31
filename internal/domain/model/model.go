// Package model 放跨层共用的领域实体。
// 工单聚合根另有 domain/order（它带状态机，不适合和纯数据结构混在一起）。
//
// 注意这里没有 Wallet：非托管模型下余额属于链，
// 由 chain.Chain 读出来，不落成平台的实体。
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// User 的身份就是地址。Email 只是通知渠道，不是登录名。
type User struct {
	ID          string    `json:"id"`
	Address     string    `json:"address"`
	DisplayName string    `json:"display_name"`
	Email       string    `json:"email,omitempty"`
	Kind        string    `json:"kind"`        // person | firm | agent
	WalletKind  string    `json:"wallet_kind"` // atara（账户合约策略） | ext（对支出合约 approve）
	LoginMethod string    `json:"login_method"`
	CreatedAt   time.Time `json:"created_at"`
}

// Merchant 是挂单卡上必须出现的那组字段。
// 缺件也公开——让买家自己给缺口定价，而不是平台替他隐藏。
type Merchant struct {
	UserID            string          `json:"user_id"`
	PeerCode          string          `json:"peer_code"`
	TrustScore        int             `json:"trust_score"`
	Deals             int             `json:"deals"`
	Disputes          int             `json:"disputes"`
	FillRate          decimal.Decimal `json:"fill_rate"`
	MedianReleaseSecs int             `json:"median_release_secs"`
	Docs              map[string]bool `json:"docs"`
}

// Contact：一个字段收名字或地址，不再有 ATR ID。
type Contact struct {
	ContactID string `json:"id"`
	Address   string `json:"address"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Label     string `json:"label"` // Supplier / Client / Colleague / Friend / My agent
	Nickname  string `json:"nickname,omitempty"`
}

// Allowance 是一份签进链上的支配权：谁能花、单笔多少、窗口内多少、到什么时候、能付给谁。
// 它不是平台的额度表——平台只是记着链上签发了什么。
type Allowance struct {
	ID         string          `json:"id"`
	OwnerID    string          `json:"-"`
	Spender    string          `json:"spender"`
	Kind       string          `json:"kind"` // person | agent
	Asset      string          `json:"asset"`
	PerPayment decimal.Decimal `json:"per_payment"`
	WindowCap  decimal.Decimal `json:"window_cap"`
	Used       decimal.Decimal `json:"used"`
	Cycle      string          `json:"cycle"` // weekly | monthly
	ExpiresAt  *time.Time      `json:"expires_at"`
	Recipients string          `json:"recipients"`
	Template   string          `json:"template,omitempty"`
	WalletKind string          `json:"wallet_kind"`
	ChainTx    string          `json:"chain_tx,omitempty"`
	Status     string          `json:"status"` // live | revoked
	Note       string          `json:"note,omitempty"`
}

func (a Allowance) Live() bool {
	if a.Status != "live" {
		return false
	}
	return a.ExpiresAt == nil || a.ExpiresAt.After(time.Now())
}

// Expired 与 revoked 是两件事：过期是自然到点，撤销是主动收回。
func (a Allowance) Expired() bool {
	return a.ExpiresAt != nil && !a.ExpiresAt.After(time.Now())
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
	MinLot       decimal.Decimal `json:"min_lot"`
	LockTx       string          `json:"lock_tx,omitempty"`
	Status       string          `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`

	Maker    *User     `json:"-"`
	Merchant *Merchant `json:"-"`
}

// FiatCeiling 是这条挂单的可成交上限（法币口径）。
func (o Offer) FiatCeiling() decimal.Decimal { return o.RemainingQty.Mul(o.UnitPrice) }

// Message 是线程里的一条。一个对手方一条线程，
// 聊天、订单卡、系统播报、评估结论共用同一条流。
type Message struct {
	ID        string            `json:"id"`
	PeerID    string            `json:"peer_id"`
	Author    string            `json:"author"` // me | peer | system
	Kind      string            `json:"kind"`   // chat | system | order | assessment
	Body      string            `json:"body"`
	OrderID   string            `json:"order_id,omitempty"`
	Payload   map[string]string `json:"payload,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}
