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

	"github.com/ethereum/go-ethereum/crypto"
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

// Info 是前端要自己发交易时需要知道的一切：该在哪条网络上、
// approve 给谁、锁进哪个合约、代币合约是哪个、精度多少。
//
// 非托管这条路上这些不能写死在前端：合约换一次地址，前端就会把钱
// approve 给一个旧合约。所以由链层报出来，接口发下去。
type Info struct {
	// Impl 是 mock 还是 evm。mock 下面所有地址都是空的——那条链上没有
	// 合约可调，前端据此知道「这一版不发交易」，而不是拿着空地址去调。
	Impl     string           `json:"impl"`
	Network  string           `json:"network"`
	ChainID  int64            `json:"chain_id"`
	RPCURL   string           `json:"rpc_url"`
	Explorer string           `json:"explorer"`
	Escrow   string           `json:"escrow"`
	Spending string           `json:"spending"`
	Tokens   map[string]Token `json:"tokens"`
}

// Token 是一种币在这条链上的合约。
type Token struct {
	Address string `json:"address"`
	// Decimals 从合约读，不猜：BSC 上稳定币是 18 位，以太坊上是 6 位，
	// 猜错金额差 10^12。读不到时给 0，前端自己去合约读一次。
	Decimals int `json:"decimals"`
}

type Chain interface {
	// Info 报出前端发交易要用的网络与合约地址。
	Info(ctx context.Context) Info
	// EscrowAddress 返回某币种的托管合约地址与所在网络。
	EscrowAddress(asset string) (address, network string)
	// SpendingAddress 是外部钱包 approve 额度的目标合约。
	SpendingAddress() string
	// ExplorerURL 给前端拼浏览器链接。
	ExplorerURL(asset, address string) string
	// TxURL 是一笔交易在区块浏览器上的地址。
	//
	// 和 ExplorerURL 一样由链自己回答，而不是上层按网络名去拼：mock 链的
	// 哈希不是链上的东西，任何链接过去都是 404——只有链自己知道这一点。
	TxURL(txHash string) string
	// DeriveAddress 按**这条链的地址格式**，为一个身份种子派生确定性地址。
	//
	// 地址格式是链的属性，不是平台的：TRON 是 base58 的 T 开头，EVM 是 0x 十六进制。
	// 把它写死在上层，换链时种子和开户都会生成一堆这条链不认的地址。
	DeriveAddress(seed string) string

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
	//
	// auth 带的是放行依据。真实链实现要把它签成一份 EIP-712 证明再提交——
	// 合约只认带证明的调用，没有「运营方直接放行」那条路。
	// 签名留在链层内部：app 层不该拿到、也拿不到私钥。
	Release(ctx context.Context, orderID, to string, auth ReleaseAuth) (txHash string, err error)
	Refund(ctx context.Context, orderID string, auth ReleaseAuth) (txHash string, err error)

	// LockListing / UnlockListing：挂出即锁币、下架即解锁。
	//
	// 这两个是**后端代签**的路径，只在 mock 链和种子数据里用。真链上锁币的
	// 是做市方自己的钱包——合约认 msg.sender 当 maker，后端签就变成后端的币
	// 进了托管，那不是非托管。真实路径是前端签完，后端用 ListingLockOf 核验。
	LockListing(ctx context.Context, offerID, owner, asset string, amt decimal.Decimal) (txHash string, err error)
	UnlockListing(ctx context.Context, offerID string) (txHash string, err error)

	// ListingLockOf 读挂单此刻在合约里的锁仓。查不到返回 nil。
	//
	// 这是「前端自己发交易」那条路的验收口：前端说它锁了，后端不信它说的，
	// 去链上看。不看的话，任何人都能 POST 一个挂单说自己锁了 100 万。
	ListingLockOf(ctx context.Context, offerID string) (*ListingLock, error)

	// GrantAllowance 签发额度。Atara 钱包写进账户合约策略，
	// 外部钱包是对支出合约的 approve——两种执行方式，同一个接口。
	GrantAllowance(ctx context.Context, a AllowanceGrant) (txHash string, err error)
	RevokeAllowance(ctx context.Context, allowanceID string) (txHash string, err error)
	// AllowanceState 读链上这份支配权此刻的状态。
	//
	// 它存在的意义是让**链上策略成为额度校验的权威**：只有平台库记着的
	// 额度是装饰，链上撤了平台还放行就是假的非托管。
	AllowanceState(ctx context.Context, allowanceID string) (*AllowanceState, error)
}

// OfferKey 把挂单号换成合约里的 bytes32。
//
// 规则只有这一处：前端要拿它去调 lockListing，后端要拿它去查。两边各写
// 一份的话，哪天改了一处，币就会锁到一个后端找不到的号下面——而且不报错。
func OfferKey(offerID string) string {
	return crypto.Keccak256Hash([]byte(offerID)).Hex()
}

// ListingLock 是挂单在托管合约里的锁仓状态。
type ListingLock struct {
	// Maker 是锁币的人。真链上它必须等于做市方自己的地址——
	// 不等于就说明这笔币不是他锁的，挂单不能算数。
	Maker string
	Token string
	// Total 锁进来的总量，Bound 已经被订单绑走的量。
	Total decimal.Decimal
	Bound decimal.Decimal
	Open  bool
}

// Available 是还能被新订单吃掉的量。
func (l *ListingLock) Available() decimal.Decimal { return l.Total.Sub(l.Bound) }

// ReleaseAuth 是放行/退款的依据，由共识产出。
//
// Score 是共识评分，合约要求它不低于部署时设定的 minScore。
// 退款不看分数——条件没成立、超时、撤单与风险评分无关。
type ReleaseAuth struct {
	Score uint16
	// Rationale 只进事件与日志，不进链上证明——摘要里放自由文本
	// 会让证明变长且没有额外保证。
	Rationale string
}

// AllowanceState 是链上支配权的实况。
type AllowanceState struct {
	// Live 为 false 表示已撤销、已过期，或链上根本没有这份策略。
	Live bool
	// Available 是此刻还能花多少（已把周期窗口滚动算进去）。
	Available decimal.Decimal
	// Used 是当前窗口已用。
	Used decimal.Decimal
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
