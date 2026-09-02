// Package evmchain 是 chain.Chain 的真实实现，对着 AtaraEscrow 合约说话。
//
// 目标链是 BSC（BSC 测试网 chainId 97，主网 56），资产只支持 USDT / USDC。
// 但代码里没有写死链和币：链 ID 从 RPC 读，代币精度从代币合约读——
// BSC 上 USDT(BSC-USD) 与 USDC 都是 18 位而不是以太坊上的 6 位，
// 写死精度会让金额差 10^12。
//
// # 关于「一把私钥」
//
// Demo 配置是单签名方、阈值 1，签名方就是这里持有的这把私钥。
// 这个配置下「共识决定放行」在链上退化成「后端决定放行」——这把私钥丢了，
// 合约里的钱就能被放走。合约的阈值机制还在，把签名方名单换成多个独立主机上
// 的独立密钥、阈值调到 2 以上就恢复了，不需要改合约。
// 这是 Demo 阶段的明确取舍，不是设计缺陷；上真钱之前必须改回多签。
package evmchain

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/advaita/atara-pay/internal/chain"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/shopspring/decimal"
)

// escrowABI 只声明用得到的那几个方法与事件，不引 abigen 生成的大文件。
const escrowABI = `[
{"type":"function","name":"deposit","inputs":[
  {"name":"orderId","type":"bytes32"},{"name":"token","type":"address"},
  {"name":"amount","type":"uint256"},{"name":"beneficiary","type":"address"}],"outputs":[]},
{"type":"function","name":"lockListing","inputs":[
  {"name":"offerId","type":"bytes32"},{"name":"token","type":"address"},
  {"name":"amount","type":"uint256"}],"outputs":[]},
{"type":"function","name":"unlockListing","inputs":[
  {"name":"offerId","type":"bytes32"}],"outputs":[]},
{"type":"function","name":"bindListingLock","inputs":[
  {"name":"orderId","type":"bytes32"},{"name":"offerId","type":"bytes32"},
  {"name":"amount","type":"uint256"},{"name":"beneficiary","type":"address"}],"outputs":[]},
{"type":"function","name":"release","inputs":[
  {"name":"att","type":"tuple","components":[
    {"name":"orderId","type":"bytes32"},{"name":"verdict","type":"uint8"},
    {"name":"score","type":"uint16"},{"name":"nonce","type":"uint256"},
    {"name":"deadline","type":"uint256"}]},
  {"name":"signatures","type":"bytes[]"}],"outputs":[]},
{"type":"function","name":"refund","inputs":[
  {"name":"att","type":"tuple","components":[
    {"name":"orderId","type":"bytes32"},{"name":"verdict","type":"uint8"},
    {"name":"score","type":"uint16"},{"name":"nonce","type":"uint256"},
    {"name":"deadline","type":"uint256"}]},
  {"name":"signatures","type":"bytes[]"}],"outputs":[]},
{"type":"function","name":"positionOf","stateMutability":"view","inputs":[
  {"name":"orderId","type":"bytes32"}],"outputs":[
  {"name":"","type":"tuple","components":[
    {"name":"token","type":"address"},{"name":"amount","type":"uint256"},
    {"name":"payer","type":"address"},{"name":"beneficiary","type":"address"},
    {"name":"offerId","type":"bytes32"},{"name":"status","type":"uint8"}]}]},
{"type":"function","name":"listingAvailable","stateMutability":"view","inputs":[
  {"name":"offerId","type":"bytes32"}],"outputs":[{"name":"","type":"uint256"}]},
{"type":"function","name":"minScore","stateMutability":"view","inputs":[],
  "outputs":[{"name":"","type":"uint16"}]},
{"type":"function","name":"threshold","stateMutability":"view","inputs":[],
  "outputs":[{"name":"","type":"uint256"}]},
{"type":"function","name":"hashAttestation","stateMutability":"view","inputs":[
  {"name":"att","type":"tuple","components":[
    {"name":"orderId","type":"bytes32"},{"name":"verdict","type":"uint8"},
    {"name":"score","type":"uint16"},{"name":"nonce","type":"uint256"},
    {"name":"deadline","type":"uint256"}]}],"outputs":[{"name":"","type":"bytes32"}]}
]`

// mintABI 只有测试币才有。真实的 USDT/USDC 没有公开 mint——
// 所以 Credit 在真网上必然失败，那是对的。
const mintABI = `[
{"type":"function","name":"mint","inputs":[
  {"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[]}
]`

const erc20ABI = `[
{"type":"function","name":"balanceOf","stateMutability":"view","inputs":[
  {"name":"","type":"address"}],"outputs":[{"name":"","type":"uint256"}]},
{"type":"function","name":"decimals","stateMutability":"view","inputs":[],
  "outputs":[{"name":"","type":"uint8"}]},
{"type":"function","name":"approve","inputs":[
  {"name":"spender","type":"address"},{"name":"amount","type":"uint256"}],
  "outputs":[{"name":"","type":"bool"}]},
{"type":"function","name":"transfer","inputs":[
  {"name":"to","type":"address"},{"name":"amount","type":"uint256"}],
  "outputs":[{"name":"","type":"bool"}]}
]`

// verdict 与合约的 enum 对齐。
const (
	verdictRelease uint8 = 0
	verdictRefund  uint8 = 1
)

// Config 是链层需要的全部配置。
type Config struct {
	RPCURL string
	// EscrowAddr 是已部署的 AtaraEscrow 地址。
	EscrowAddr string
	// SignerKeyHex 是 Demo 里唯一的私钥：既签交易，也签放行证明。
	// 生产环境这两件事应当分开，且证明的签名方要是多个独立主机。
	SignerKeyHex string
	// Tokens 是资产码到代币合约地址的映射，如 {"USDT": "0x...", "USDC": "0x..."}。
	Tokens map[string]string
	// Network 是给前端看的网络名，如 BSC / BSC-TESTNET。
	Network string
	// ExplorerBase 如 https://testnet.bscscan.com。
	ExplorerBase string
	// Confirmations 是入金被认作已托管所需的确认数。
	Confirmations int
}

type Chain struct {
	cfg       Config
	cli       *ethclient.Client
	escrow    common.Address
	escrowABI abi.ABI
	tokenABI  abi.ABI
	key       *ecdsa.PrivateKey
	signer    common.Address
	chainID   *big.Int

	// 代币精度从链上读一次就缓存——它不会变。
	decMu    sync.RWMutex
	decimals map[string]uint8

	// 入金监听的登记表。真实实现应当靠事件订阅；这里按需查仓位状态，
	// 单实例 Demo 够用，且不依赖 RPC 的日志保留策略。
	watchMu sync.Mutex
	watches map[string]*watch
}

type watch struct {
	asset  string
	amount decimal.Decimal
	since  time.Time
}

// New 连上链并核对合约配置。
//
// 构造时就把 minScore 与 threshold 读出来核对：配错了要在启动时炸，
// 而不是等到第一笔放款失败。
func New(ctx context.Context, cfg Config) (*Chain, error) {
	if cfg.Confirmations <= 0 {
		cfg.Confirmations = chain.Confirmations
	}
	cli, err := ethclient.DialContext(ctx, cfg.RPCURL)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.RPCURL, err)
	}
	cid, err := cli.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("chain id: %w", err)
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.SignerKeyHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("signer key: %w", err)
	}
	ea, err := abi.JSON(strings.NewReader(escrowABI))
	if err != nil {
		return nil, err
	}
	ta, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil {
		return nil, err
	}
	if !common.IsHexAddress(cfg.EscrowAddr) {
		return nil, fmt.Errorf("bad escrow address %q", cfg.EscrowAddr)
	}

	c := &Chain{
		cfg: cfg, cli: cli,
		escrow:    common.HexToAddress(cfg.EscrowAddr),
		escrowABI: ea, tokenABI: ta,
		key: key, signer: crypto.PubkeyToAddress(key.PublicKey),
		chainID:  cid,
		decimals: map[string]uint8{},
		watches:  map[string]*watch{},
	}

	// 启动即核对：合约在不在、阈值是不是 1（Demo 配置）
	vals, err := c.callView(ctx, "threshold")
	if err != nil {
		return nil, fmt.Errorf("escrow at %s unreachable: %w", cfg.EscrowAddr, err)
	}
	thr, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("threshold: unexpected type %T", vals[0])
	}
	if thr.Cmp(big.NewInt(1)) != 0 {
		return nil, fmt.Errorf(
			"escrow threshold is %s but this build signs with a single key — "+
				"either redeploy with threshold 1 or wire in the other signers", thr)
	}
	return c, nil
}

func (c *Chain) Close() { c.cli.Close() }

// SignerAddress 供启动日志打印，便于核对它是否真是合约认的签名方。
func (c *Chain) SignerAddress() string { return c.signer.Hex() }

// ── 地址与展示 ──

func (c *Chain) EscrowAddress(string) (string, string) {
	return c.escrow.Hex(), c.cfg.Network
}

// SpendingAddress 在这一版与托管合约同一个：额度的链上执行还没做，
// 返回托管地址至少不会让前端拼出一个不存在的链接。
func (c *Chain) SpendingAddress() string { return c.escrow.Hex() }

func (c *Chain) ExplorerURL(_, address string) string {
	if c.cfg.ExplorerBase == "" || address == "" {
		return ""
	}
	return strings.TrimSuffix(c.cfg.ExplorerBase, "/") + "/address/" + address
}

// DeriveAddress 从身份种子派生一个确定性的 EVM 地址。
//
// 取 keccak 的后 20 字节。这些地址**没有对应的私钥**——Demo 里签名只有
// 后端那一把，这些地址只是收款目的地与身份标识。真实用户接入时地址来自
// 他自己的钱包，不走这里。
func (c *Chain) DeriveAddress(seed string) string {
	h := crypto.Keccak256([]byte("atara-demo|" + seed))
	return common.BytesToAddress(h[12:]).Hex()
}

// ── 金额与精度 ──

func (c *Chain) tokenOf(asset string) (common.Address, error) {
	a, ok := c.cfg.Tokens[strings.ToUpper(asset)]
	if !ok {
		return common.Address{}, fmt.Errorf("%w: %s", ErrAssetUnsupported, asset)
	}
	if !common.IsHexAddress(a) {
		return common.Address{}, fmt.Errorf("bad token address for %s: %q", asset, a)
	}
	return common.HexToAddress(a), nil
}

// tokenDecimals 从代币合约读精度并缓存。
//
// 不写死：BSC 上 USDT 与 USDC 都是 18 位，以太坊上是 6 位。
// 猜错会让金额差 10^12，那是灾难性的，所以宁可多一次 RPC。
func (c *Chain) tokenDecimals(ctx context.Context, asset string) (uint8, error) {
	key := strings.ToUpper(asset)
	c.decMu.RLock()
	if d, ok := c.decimals[key]; ok {
		c.decMu.RUnlock()
		return d, nil
	}
	c.decMu.RUnlock()

	tok, err := c.tokenOf(asset)
	if err != nil {
		return 0, err
	}
	vals, err := c.callViewAt(ctx, tok, c.tokenABI, "decimals")
	if err != nil {
		return 0, fmt.Errorf("decimals of %s: %w", asset, err)
	}
	dec, ok := vals[0].(uint8)
	if !ok {
		return 0, fmt.Errorf("decimals of %s: unexpected type %T", asset, vals[0])
	}
	c.decMu.Lock()
	c.decimals[key] = dec
	c.decMu.Unlock()
	return dec, nil
}

// toWei 把主单位十进制换成代币的最小单位整数。
//
// 有余数就报错而不是截断：截断会静默吞掉用户的钱。
func (c *Chain) toWei(ctx context.Context, asset string, amt decimal.Decimal) (*big.Int, error) {
	dec, err := c.tokenDecimals(ctx, asset)
	if err != nil {
		return nil, err
	}
	scaled := amt.Shift(int32(dec))
	if !scaled.Equal(scaled.Truncate(0)) {
		return nil, fmt.Errorf("%w: %s %s has more precision than the token's %d decimals",
			ErrPrecision, amt, asset, dec)
	}
	return scaled.BigInt(), nil
}

func (c *Chain) fromWei(ctx context.Context, asset string, v *big.Int) (decimal.Decimal, error) {
	dec, err := c.tokenDecimals(ctx, asset)
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromBigInt(v, -int32(dec)), nil
}

var (
	ErrAssetUnsupported = errors.New("asset not supported on this chain")
	ErrPrecision        = errors.New("amount exceeds token precision")
)

// ── 读 ──

func (c *Chain) Balance(ctx context.Context, address, asset string) (decimal.Decimal, error) {
	if !common.IsHexAddress(address) {
		return decimal.Zero, fmt.Errorf("bad address %q", address)
	}
	tok, err := c.tokenOf(asset)
	if err != nil {
		// 这条链只上了 USDT / USDC。目录里还有 BTC / ETH——查它们的余额
		// 应当得到 0，而不是让整个钱包页 500。
		if errors.Is(err, ErrAssetUnsupported) {
			return decimal.Zero, nil
		}
		return decimal.Zero, err
	}
	vals, err := c.callViewAt(ctx, tok, c.tokenABI, "balanceOf",
		common.HexToAddress(address))
	if err != nil {
		return decimal.Zero, err
	}
	bal, ok := vals[0].(*big.Int)
	if !ok {
		return decimal.Zero, fmt.Errorf("balanceOf: unexpected type %T", vals[0])
	}
	return c.fromWei(ctx, asset, bal)
}

// positionRaw 是 positionOf 的返回结构，字段顺序与合约一致。
type positionRaw struct {
	Token       common.Address
	Amount      *big.Int
	Payer       common.Address
	Beneficiary common.Address
	OfferId     [32]byte //nolint:revive // 与 ABI 字段名一致
	Status      uint8
}

var statusNames = map[uint8]string{
	0: "", 1: "escrowed", 2: "released", 3: "refunded", 4: "disputed",
}

func (c *Chain) Position(ctx context.Context, orderID string) (*chain.Position, error) {
	vals, err := c.callView(ctx, "positionOf", idHash(orderID))
	if err != nil {
		return nil, err
	}
	// 单个 tuple 返回值：ConvertType 把匿名结构体转成我们的类型。
	raw := *abi.ConvertType(vals[0], new(positionRaw)).(*positionRaw)
	if raw.Status == 0 {
		return nil, chain.ErrNoPosition
	}
	asset := c.assetOf(raw.Token)
	amt, err := c.fromWei(ctx, asset, raw.Amount)
	if err != nil {
		return nil, err
	}
	p := &chain.Position{
		OrderID: orderID, Owner: raw.Payer.Hex(), Asset: asset, Amount: amt,
		Contract: c.escrow.Hex(), Network: c.cfg.Network,
		Status: statusNames[raw.Status],
	}
	if raw.OfferId != [32]byte{} {
		// 合约里存的是 offerId 的哈希，反查不回原串——记一个标记就够了，
		// 上层只用它判断「这批币是不是挂单锁的」。
		p.OfferID = "listing"
	}
	return p, nil
}

func (c *Chain) assetOf(token common.Address) string {
	for code, addr := range c.cfg.Tokens {
		if common.IsHexAddress(addr) && common.HexToAddress(addr) == token {
			return code
		}
	}
	return token.Hex()
}

// ── 入金 ──

// SignDeposit 用后端持有的私钥把币转进托管合约。
//
// 注意这只在「Atara 钱包」路径下成立：那种钱包的私钥由平台代持（passkey 派生）。
// Demo 里退化成后端的这一把。用户自带钱包必须走 WatchDeposit——
// 我们没有他的私钥，也不该有。
func (c *Chain) SignDeposit(ctx context.Context, from, orderID, asset string,
	amt decimal.Decimal) (*chain.Deposit, error) {
	tok, err := c.tokenOf(asset)
	if err != nil {
		return nil, err
	}
	wei, err := c.toWei(ctx, asset, amt)
	if err != nil {
		return nil, err
	}
	if !common.IsHexAddress(from) {
		return nil, fmt.Errorf("bad from address %q", from)
	}

	// 先 approve 再 deposit。approve 给足这一笔就好，不给无限额度——
	// 无限授权在合约被攻破时会把整个余额暴露出去。
	if _, err := c.send(ctx, tok, c.tokenABI, "approve", c.escrow, wei); err != nil {
		return nil, fmt.Errorf("approve: %w", err)
	}
	rcpt, err := c.send(ctx, c.escrow, c.escrowABI, "deposit",
		idHash(orderID), tok, wei, common.HexToAddress(from))
	if err != nil {
		return nil, fmt.Errorf("deposit: %w", err)
	}

	c.watchMu.Lock()
	c.watches[orderID] = &watch{asset: asset, amount: amt, since: time.Now()}
	c.watchMu.Unlock()

	return &chain.Deposit{
		TxHash: rcpt.Hex(), From: from, Asset: asset, Amount: amt,
		Confirmations: 0, Required: c.cfg.Confirmations, DetectedAt: time.Now(),
	}, nil
}

// WatchDeposit 登记一笔待观察的外部入金。
func (c *Chain) WatchDeposit(_ context.Context, orderID, asset string,
	amt decimal.Decimal) error {
	if _, err := c.tokenOf(asset); err != nil {
		return err
	}
	c.watchMu.Lock()
	c.watches[orderID] = &watch{asset: asset, amount: amt, since: time.Now()}
	c.watchMu.Unlock()
	return nil
}

// Deposit 查入金进度。
//
// 判据是仓位在合约里是不是 escrowed——那是合约自己的账，比数确认数可靠。
// 确认数按当前区块与仓位所在区块的差算；这一版简化为「仓位存在即视为
// 确认数走满」，因为在 BSC 上从交易被打包到 6 个确认只有十几秒，
// 而 Demo 的调度器本来就是秒级轮询。这一处简化写在这里而不是藏起来。
func (c *Chain) Deposit(ctx context.Context, orderID string) (*chain.Deposit, error) {
	c.watchMu.Lock()
	w := c.watches[orderID]
	c.watchMu.Unlock()

	p, err := c.Position(ctx, orderID)
	if err != nil {
		if errors.Is(err, chain.ErrNoPosition) {
			if w == nil {
				return nil, nil
			}
			// 登记了但链上还没看到：确认数 0
			return &chain.Deposit{
				Asset: w.asset, Amount: w.amount,
				Confirmations: 0, Required: c.cfg.Confirmations, DetectedAt: w.since,
			}, nil
		}
		return nil, err
	}
	if p.Status != "escrowed" {
		return nil, nil
	}
	return &chain.Deposit{
		TxHash: p.TxHash, Asset: p.Asset, Amount: p.Amount,
		Confirmations: c.cfg.Confirmations, Required: c.cfg.Confirmations,
		DetectedAt: time.Now(),
	}, nil
}

// ── 挂单锁仓 ──

func (c *Chain) LockListing(ctx context.Context, offerID, owner, asset string,
	amt decimal.Decimal) (string, error) {
	tok, err := c.tokenOf(asset)
	if err != nil {
		// 这条链只上了 USDT / USDC。种子里还有 BTC / ETH 的挂单——
		// 跳过而不是报错，否则整个种子灌不进去。返回空哈希表示没有链上动作，
		// 挂单会以「未锁币」的状态存在，前端看到的可成交量就是 0，
		// 这比假装锁了要诚实。
		if errors.Is(err, ErrAssetUnsupported) {
			return "", nil
		}
		return "", err
	}
	wei, err := c.toWei(ctx, asset, amt)
	if err != nil {
		return "", err
	}
	_ = owner // 锁的是后端签名方持有的币；真实做市方自签时由前端直接调合约
	if _, err := c.send(ctx, tok, c.tokenABI, "approve", c.escrow, wei); err != nil {
		return "", fmt.Errorf("approve: %w", err)
	}
	h, err := c.send(ctx, c.escrow, c.escrowABI, "lockListing", idHash(offerID), tok, wei)
	if err != nil {
		return "", err
	}
	return h.Hex(), nil
}

func (c *Chain) UnlockListing(ctx context.Context, offerID string) (string, error) {
	h, err := c.send(ctx, c.escrow, c.escrowABI, "unlockListing", idHash(offerID))
	if err != nil {
		return "", err
	}
	return h.Hex(), nil
}

func (c *Chain) BindListingLock(ctx context.Context, orderID, offerID, owner, asset string,
	amt decimal.Decimal) (*chain.Position, error) {
	wei, err := c.toWei(ctx, asset, amt)
	if err != nil {
		return nil, err
	}
	vals, err := c.callView(ctx, "listingAvailable", idHash(offerID))
	if err != nil {
		return nil, err
	}
	avail, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("listingAvailable: unexpected type %T", vals[0])
	}
	if avail.Cmp(wei) < 0 {
		return nil, chain.ErrNoListingLock
	}
	if !common.IsHexAddress(owner) {
		return nil, fmt.Errorf("bad beneficiary %q", owner)
	}
	if _, err := c.send(ctx, c.escrow, c.escrowABI, "bindListingLock",
		idHash(orderID), idHash(offerID), wei, common.HexToAddress(owner)); err != nil {
		return nil, err
	}
	return c.Position(ctx, orderID)
}

// ── 放行与退款：要签证明 ──

func (c *Chain) Release(ctx context.Context, orderID, to string,
	auth chain.ReleaseAuth) (string, error) {
	_ = to // 收款方在入金时就写进了仓位，合约按仓位放款，不接受调用方指定
	return c.attest(ctx, orderID, verdictRelease, auth.Score, "release")
}

func (c *Chain) Refund(ctx context.Context, orderID string,
	_ chain.ReleaseAuth) (string, error) {
	// 退款不看分数：条件没成立、超时、撤单与风险评分无关。
	return c.attest(ctx, orderID, verdictRefund, 0, "refund")
}

// attest 组一份 EIP-712 证明、签名、提交。
//
// 摘要直接向合约索取（hashAttestation），不在 Go 里重算一遍——
// 两处各算一份，哪天合约改了 typehash 就会静默签出对不上的东西。
func (c *Chain) attest(ctx context.Context, orderID string, verdict uint8, score uint16,
	what string) (string, error) {
	att := struct {
		OrderId  [32]byte //nolint:revive // 与 ABI 字段名一致
		Verdict  uint8
		Score    uint16
		Nonce    *big.Int
		Deadline *big.Int
	}{
		OrderId:  idHash(orderID),
		Verdict:  verdict,
		Score:    score,
		Nonce:    big.NewInt(time.Now().UnixNano()),
		Deadline: big.NewInt(time.Now().Add(10 * time.Minute).Unix()),
	}

	vals, err := c.callView(ctx, "hashAttestation", att)
	if err != nil {
		return "", fmt.Errorf("hash attestation: %w", err)
	}
	digest, ok := vals[0].([32]byte)
	if !ok {
		return "", fmt.Errorf("hashAttestation: unexpected type %T", vals[0])
	}
	sig, err := crypto.Sign(digest[:], c.key)
	if err != nil {
		return "", err
	}
	// go-ethereum 的 v 是 0/1，合约两种编码都收，这里补成 27/28 与主流一致
	sig[64] += 27

	h, err := c.send(ctx, c.escrow, c.escrowABI, what, att, [][]byte{sig})
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	return h.Hex(), nil
}

// ── 种子数据 ──

// Credit 给某个地址铸一笔测试币。
//
// 它实现的是 store.Funder，只给种子数据用。这个方法刻意**不在**
// chain.Chain 接口里：真链上没有「凭空记一笔余额」这种动作。
// 真实的 USDT/USDC 没有公开 mint，所以这个方法在主网上必然失败——
// 那正是应有的行为，不要为了让它成功去加什么后门。
func (c *Chain) Credit(ctx context.Context, address, asset string, amt decimal.Decimal) error {
	tok, err := c.tokenOf(asset)
	if err != nil {
		// 种子里有 BTC / ETH，这条链上没有它们的代币——跳过而不是报错，
		// 否则整个种子灌不进去。
		if errors.Is(err, ErrAssetUnsupported) {
			return nil
		}
		return err
	}
	wei, err := c.toWei(ctx, asset, amt)
	if err != nil {
		return err
	}
	if !common.IsHexAddress(address) {
		return fmt.Errorf("bad address %q", address)
	}
	ma, err := abi.JSON(strings.NewReader(mintABI))
	if err != nil {
		return err
	}
	_, err = c.send(ctx, tok, ma, "mint", common.HexToAddress(address), wei)
	if err != nil {
		return fmt.Errorf("mint %s to %s (test tokens only): %w", asset, address, err)
	}
	return nil
}

// ── 额度：这一版不上链 ──

// GrantAllowance 在这一版不产生链上动作。
//
// 额度的链上执行（账户合约策略 / 对支出合约的 approve）不在本次范围内，
// 返回空哈希而不是假装成功——上层据此知道这份额度只有平台侧的记录。
func (c *Chain) GrantAllowance(context.Context, chain.AllowanceGrant) (string, error) {
	return "", nil
}

func (c *Chain) RevokeAllowance(context.Context, string) (string, error) { return "", nil }

// ── 底层调用 ──

// callView 读一个 view 方法，返回原始解包结果。
//
// 不用 UnpackIntoInterface：它对「单个 tuple 返回值」会把结构体当切片处理，
// 直接 panic（reflect.Value.Len on struct Value）。abigen 生成的代码走的是
// Unpack + ConvertType 这条路，这里照它来。
func (c *Chain) callView(ctx context.Context, method string, args ...any) ([]any, error) {
	return c.callViewAt(ctx, c.escrow, c.escrowABI, method, args...)
}

func (c *Chain) callViewAt(ctx context.Context, to common.Address, a abi.ABI,
	method string, args ...any) ([]any, error) {
	data, err := a.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", method, err)
	}
	res, err := c.cli.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("%s returned no data (wrong address or not deployed?)", method)
	}
	vals, err := a.Unpack(method, res)
	if err != nil {
		return nil, fmt.Errorf("unpack %s: %w", method, err)
	}
	if len(vals) == 0 {
		return nil, fmt.Errorf("%s returned nothing", method)
	}
	return vals, nil
}

// send 发一笔交易并等回执。
//
// 等回执而不是发完就走：链动作没有回滚，上层要在「链上确实成功了」之后
// 才记账。回执 status 为 0 表示合约 revert，必须当失败处理。
func (c *Chain) send(ctx context.Context, to common.Address, a abi.ABI,
	method string, args ...any) (common.Hash, error) {
	data, err := a.Pack(method, args...)
	if err != nil {
		return common.Hash{}, fmt.Errorf("pack %s: %w", method, err)
	}
	opts, err := bind.NewKeyedTransactorWithChainID(c.key, c.chainID)
	if err != nil {
		return common.Hash{}, err
	}
	opts.Context = ctx
	bound := bind.NewBoundContract(to, a, c.cli, c.cli, c.cli)
	tx, err := bound.RawTransact(opts, data)
	if err != nil {
		return common.Hash{}, err
	}
	rcpt, err := bind.WaitMined(ctx, c.cli, tx)
	if err != nil {
		return common.Hash{}, err
	}
	if rcpt.Status != 1 {
		return common.Hash{}, fmt.Errorf("%s reverted (tx %s)", method, tx.Hash().Hex())
	}
	return tx.Hash(), nil
}

// idHash 把后端的字符串 ID 变成合约用的 bytes32。
//
// 用 keccak 而不是截断字符串：ID 是 UUID，截断会撞；哈希不会，
// 而且长度天然固定。代价是链上看不出原始 ID——事件里有，够查。
func idHash(id string) [32]byte {
	return crypto.Keccak256Hash([]byte(id))
}

var _ chain.Chain = (*Chain)(nil)
