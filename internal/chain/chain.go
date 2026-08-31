// Package chain 是链的边界。
//
// 非托管的含义在代码结构上必须是真的：资金余额与托管仓位属于链，
// Atara 后端只能通过这个接口去读、去请求，不能直接改。
// 所以这里只有接口——任何"平台自己记一笔余额"的写法都过不了这道门。
package chain

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// Confirmations 是入金被认作"已托管"需要的确认数，与前端的 6/6 一致。
const Confirmations = 6

type Deposit struct {
	TxHash        string
	From          string
	Asset         string
	Amount        decimal.Decimal
	Confirmations int
	Required      int
	DetectedAt    time.Time
}

func (d *Deposit) Settled() bool { return d != nil && d.Confirmations >= d.Required }

// Position 是一笔工单在托管合约里的仓位。
// 它不是平台的账，是合约的账——平台只是读它。
type Position struct {
	OrderID  string
	OfferID  string // 非空表示这批币是挂单时锁的，退回时归还给挂单而不是个人余额
	Owner    string // 出币的那一方
	Asset    string
	Amount   decimal.Decimal
	Contract string
	Network  string
	TxHash   string
	Status   string // pending | escrowed | released | refunded
}

// FundingVia 是入金的两条路。两条殊途同归：合约收到钱、确认数走满，订单才进下一站。
type FundingVia string

const (
	// ViaWallet：内置钱包签名转入。Passkey 签的是这笔转账本身。
	ViaWallet FundingVia = "wallet"
	// ViaExternal：用户拿自己的钱包往合约地址打款，我们监听合约的入金事件。
	ViaExternal FundingVia = "external"
)

type Chain interface {
	// EscrowAddress 返回某币种的托管合约地址与所在网络。
	EscrowAddress(asset string) (address, network string)
	// SpendingAddress 是外部钱包 approve 额度的目标合约。
	SpendingAddress() string
	// ExplorerURL 给前端拼浏览器链接。
	ExplorerURL(asset, address string) string

	// Balance 读链上余额。注意是"读"——平台没有权力改它。
	Balance(ctx context.Context, address, asset string) (decimal.Decimal, error)

	// SignDeposit 由内置钱包签名并广播一笔转入托管合约的转账。
	SignDeposit(ctx context.Context, from, orderID, asset string, amt decimal.Decimal) (*Deposit, error)
	// WatchDeposit 开始监听某笔工单的外部转入。
	WatchDeposit(ctx context.Context, orderID, asset string, amt decimal.Decimal) error
	// Deposit 查这笔工单的入金进度（确认数）。
	Deposit(ctx context.Context, orderID string) (*Deposit, error)

	// BindListingLock 把挂单时就锁好的仓位绑到这笔订单上。
	// 买方路径下没有新的资金动作——币在挂单那一刻就进合约了，这里只是查锁仓并绑定。
	BindListingLock(ctx context.Context, orderID, offerID, owner, asset string, amt decimal.Decimal) (*Position, error)

	Position(ctx context.Context, orderID string) (*Position, error)
	// Release 把仓位放给收款方；Refund 原路退回。两者都是合约动作。
	Release(ctx context.Context, orderID, to string) (txHash string, err error)
	Refund(ctx context.Context, orderID string) (txHash string, err error)

	// LockListing / UnlockListing：挂出即锁币、下架即解锁。
	LockListing(ctx context.Context, offerID, owner, asset string, amt decimal.Decimal) (txHash string, err error)
	UnlockListing(ctx context.Context, offerID string) (txHash string, err error)

	// GrantAllowance 签发额度。Atara 钱包写进账户合约策略，
	// 外部钱包是对支出合约的 approve——两种执行方式，同一个接口。
	GrantAllowance(ctx context.Context, a AllowanceGrant) (txHash string, err error)
	RevokeAllowance(ctx context.Context, allowanceID string) (txHash string, err error)
}

type AllowanceGrant struct {
	ID         string
	Account    string
	WalletKind string // atara | ext
	Spender    string
	Asset      string
	PerPayment decimal.Decimal
	WindowCap  decimal.Decimal
	Cycle      string
	ExpiresAt  *time.Time
}

var (
	ErrInsufficientOnChain = errors.New("insufficient on-chain balance")
	ErrNoListingLock       = errors.New("no listing lock to bind")
	ErrNoPosition          = errors.New("no escrow position for this order")
)
