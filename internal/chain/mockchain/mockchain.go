// Package mockchain 是 chain.Chain 的模拟实现。
//
// 它自己持有余额与仓位（chain_* 表），Atara 后端只能隔着接口读——
// 换成真链时删掉这个包即可，上面几层不动。
// 确认数按墙钟时间推算，不需要后台任务去 tick。
package mockchain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/advaita/atara-pay/internal/chain"
	"github.com/shopspring/decimal"
)

//go:embed schema.sql
var schema string

// Timing 决定 demo 里链看起来有多快。真链上这些由网络决定。
type Timing struct {
	PerConfirmation time.Duration // 每个确认之间的间隔
	DetectExternal  time.Duration // 外部钱包转账被扫到的延迟
}

func DemoTiming() Timing {
	return Timing{PerConfirmation: 900 * time.Millisecond, DetectExternal: 3 * time.Second}
}

type Chain struct {
	db *sql.DB
	t  Timing
	mu sync.Mutex
}

func New(ctx context.Context, db *sql.DB, t Timing) (*Chain, error) {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("chain migrate: %w", err)
	}
	return &Chain{db: db, t: t}, nil
}

// ── 地址 ──

// 一律 EVM 风格。登录只剩 MetaMask，用户手里拿到的是 0x 地址——
// 这时候把托管合约写成 TRON 的 T 开头，同一个界面上就有两套地址格式，
// 读的人会以为自己转错了链。
//
// 之前这里还有一个 "0x5eC4a9Fb2d31AtaraEscrow40b8f21c9aD1"：里面嵌着
// "AtaraEscrow" 几个字母，根本不是合法的十六进制，复制去区块浏览器查会是空的。
var escrowAddr = map[string][2]string{
	"USDT": {"0x5ec4a9fb2d31a7c3e0b8f21c9ad140b8f21c9ad1", "Ethereum"},
	"USDC": {"0x7b1f3d92ce4408a5d61e0f27b3c8a9d40e5b2c16", "Polygon"},
}

const spendingAddr = "0x9d3a1c58e7b24f06a8c15d93f2e470b6c8a3d519"

func (c *Chain) EscrowAddress(asset string) (string, string) {
	if a, ok := escrowAddr[asset]; ok {
		return a[0], a[1]
	}
	a := escrowAddr["USDT"]
	return a[0], a[1]
}

func (c *Chain) SpendingAddress() string { return spendingAddr }

// ExplorerURL 在 mock 下返回空串：这条链不存在，给一个 etherscan 链接
// 只会把人带到「查无此地址」的页面，比不给链接更让人怀疑是不是坏了。
func (c *Chain) ExplorerURL(asset, address string) string {
	return ""
}

// ── 余额 ──

func (c *Chain) Balance(ctx context.Context, address, asset string) (decimal.Decimal, error) {
	var s string
	err := c.db.QueryRowContext(ctx,
		`select amount from chain_balances where address=? and asset=?`, address, asset).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return decimal.Zero, nil
	}
	if err != nil {
		return decimal.Zero, err
	}
	return dec(s), nil
}

// Credit 只给种子数据用：给某个地址凭空记一笔链上余额。
// 生产链上没有这个动作，所以它不在 chain.Chain 接口里。
func (c *Chain) Credit(ctx context.Context, address, asset string, amt decimal.Decimal) error {
	return c.move(ctx, address, asset, amt)
}

func (c *Chain) move(ctx context.Context, address, asset string, delta decimal.Decimal) error {
	cur, err := c.Balance(ctx, address, asset)
	if err != nil {
		return err
	}
	next := cur.Add(delta)
	if next.IsNegative() {
		return fmt.Errorf("%w: %s has %s %s, needs %s", chain.ErrInsufficientOnChain, address, cur, asset, delta.Neg())
	}
	_, err = c.db.ExecContext(ctx,
		`insert into chain_balances(address,asset,amount) values(?,?,?)
		 on conflict(address,asset) do update set amount=excluded.amount`,
		address, asset, next.String())
	return err
}

// ── 入金 ──

func (c *Chain) SignDeposit(ctx context.Context, from, orderID, asset string, amt decimal.Decimal) (*chain.Deposit, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 内置钱包签名：钱当场从签名者的链上余额里出去，进合约。
	if err := c.move(ctx, from, asset, amt.Neg()); err != nil {
		return nil, err
	}
	tx := txHash("dep", orderID, from)
	now := time.Now().UTC()
	if err := c.putDeposit(ctx, orderID, asset, amt, from, string(chain.ViaWallet), tx, now, &now); err != nil {
		return nil, err
	}
	if err := c.putPosition(ctx, orderID, "", from, asset, amt, tx, "pending"); err != nil {
		return nil, err
	}
	return c.Deposit(ctx, orderID)
}

func (c *Chain) WatchDeposit(ctx context.Context, orderID, asset string, amt decimal.Decimal) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 外部钱包：现在还没收到钱，只是开始盯着合约。
	// detected_at 留空，Deposit() 按 DetectExternal 推算什么时候"扫到"。
	return c.putDeposit(ctx, orderID, asset, amt, "", string(chain.ViaExternal), "", time.Now().UTC(), nil)
}

// Deposit 按墙钟时间推算入金进度。
// 外部转账先要被扫到，然后确认数才开始涨。
func (c *Chain) Deposit(ctx context.Context, orderID string) (*chain.Deposit, error) {
	var asset, amount, from, via, tx, started string
	var detected sql.NullString
	var required int
	err := c.db.QueryRowContext(ctx,
		`select asset,amount,from_addr,via,tx_hash,started_at,detected_at,required
		   from chain_deposits where order_id=?`, orderID).
		Scan(&asset, &amount, &from, &via, &tx, &started, &detected, &required)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	d := &chain.Deposit{TxHash: tx, From: from, Asset: asset, Amount: dec(amount), Required: required}

	detectAt := parseTS(started)
	if chain.FundingVia(via) == chain.ViaExternal {
		detectAt = detectAt.Add(c.t.DetectExternal)
	}
	if time.Now().Before(detectAt) {
		return d, nil // 还没扫到
	}
	d.DetectedAt = detectAt
	if d.TxHash == "" {
		d.TxHash = txHash("ext", orderID, asset)
		_, _ = c.db.ExecContext(ctx, `update chain_deposits set tx_hash=?, detected_at=? where order_id=?`,
			d.TxHash, ts(detectAt), orderID)
		// 外部转入的钱不从我们记的余额里扣——它本来就在别处
		_ = c.putPosition(ctx, orderID, "", from, asset, d.Amount, d.TxHash, "pending")
	}
	n := int(time.Since(detectAt) / c.t.PerConfirmation)
	if n > required {
		n = required
	}
	d.Confirmations = n
	if d.Settled() {
		_, _ = c.db.ExecContext(ctx,
			`update chain_positions set status='escrowed', tx_hash=? where order_id=? and status='pending'`,
			d.TxHash, orderID)
	}
	return d, nil
}

func (c *Chain) putDeposit(ctx context.Context, orderID, asset string, amt decimal.Decimal,
	from, via, tx string, started time.Time, detected *time.Time) error {
	var det any
	if detected != nil {
		det = ts(*detected)
	}
	_, err := c.db.ExecContext(ctx,
		`insert into chain_deposits(order_id,asset,amount,from_addr,via,tx_hash,started_at,detected_at,required)
		 values(?,?,?,?,?,?,?,?,?)
		 on conflict(order_id) do update set asset=excluded.asset, amount=excluded.amount,
		   from_addr=excluded.from_addr, via=excluded.via, started_at=excluded.started_at, required=excluded.required`,
		orderID, asset, amt.String(), from, via, tx, ts(started), det, chain.Confirmations)
	return err
}

// ── 仓位 ──

func (c *Chain) BindListingLock(ctx context.Context, orderID, offerID, owner, asset string, amt decimal.Decimal) (*chain.Position, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var status string
	err := c.db.QueryRowContext(ctx, `select status from chain_listing_locks where offer_id=?`, offerID).Scan(&status)
	if err != nil || status != "locked" {
		return nil, chain.ErrNoListingLock
	}
	// 买方路径没有新的资金动作：币在挂单那一刻就进合约了，
	// 这里只是把已有的锁仓绑到这笔订单上——所以是瞬时的。
	tx := txHash("bind", orderID, offerID)
	if err := c.putPosition(ctx, orderID, offerID, owner, asset, amt, tx, "escrowed"); err != nil {
		return nil, err
	}
	return c.Position(ctx, orderID)
}

func (c *Chain) Position(ctx context.Context, orderID string) (*chain.Position, error) {
	var p chain.Position
	var amount string
	err := c.db.QueryRowContext(ctx,
		`select order_id,offer_id,owner,asset,amount,contract,network,tx_hash,status
		   from chain_positions where order_id=?`, orderID).
		Scan(&p.OrderID, &p.OfferID, &p.Owner, &p.Asset, &amount, &p.Contract, &p.Network, &p.TxHash, &p.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Amount = dec(amount)
	return &p, nil
}

func (c *Chain) putPosition(ctx context.Context, orderID, offerID, owner, asset string,
	amt decimal.Decimal, tx, status string) error {
	addr, net := c.EscrowAddress(asset)
	_, err := c.db.ExecContext(ctx,
		`insert into chain_positions(order_id,offer_id,owner,asset,amount,contract,network,tx_hash,status)
		 values(?,?,?,?,?,?,?,?,?)
		 on conflict(order_id) do update set status=excluded.status, tx_hash=excluded.tx_hash`,
		orderID, offerID, owner, asset, amt.String(), addr, net, tx, status)
	return err
}

// Release 的 auth 在 mock 里只记进日志——真实链实现会把它签成 EIP-712 证明。
func (c *Chain) Release(ctx context.Context, orderID, to string, _ chain.ReleaseAuth) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := c.Position(ctx, orderID)
	if err != nil || p == nil {
		return "", chain.ErrNoPosition
	}
	if p.Status != "escrowed" {
		return "", fmt.Errorf("position is %s, cannot release", p.Status)
	}
	if err := c.move(ctx, to, p.Asset, p.Amount); err != nil {
		return "", err
	}
	tx := txHash("rel", orderID, to)
	_, err = c.db.ExecContext(ctx, `update chain_positions set status='released', tx_hash=? where order_id=?`, tx, orderID)
	// 从挂单锁仓里划走的，锁仓总量同步减少
	if p.OfferID != "" {
		_ = c.reduceLock(ctx, p.OfferID, p.Amount)
	}
	return tx, err
}

func (c *Chain) Refund(ctx context.Context, orderID string, _ chain.ReleaseAuth) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := c.Position(ctx, orderID)
	if err != nil || p == nil {
		return "", nil // 没入金就没什么可退的——不是错误
	}
	if p.Status != "escrowed" && p.Status != "pending" {
		return "", nil
	}
	// 挂单锁仓的钱退回挂单本身，不退给个人余额——币还 backing 着那条挂单
	if p.OfferID == "" {
		if err := c.move(ctx, p.Owner, p.Asset, p.Amount); err != nil {
			return "", err
		}
	}
	tx := txHash("ref", orderID, p.Owner)
	_, err = c.db.ExecContext(ctx, `update chain_positions set status='refunded', tx_hash=? where order_id=?`, tx, orderID)
	return tx, err
}

// ── 挂单锁仓 ──

func (c *Chain) LockListing(ctx context.Context, offerID, owner, asset string, amt decimal.Decimal) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.move(ctx, owner, asset, amt.Neg()); err != nil {
		return "", err
	}
	tx := txHash("lock", offerID, owner)
	_, err := c.db.ExecContext(ctx,
		`insert into chain_listing_locks(offer_id,owner,asset,amount,tx_hash,status)
		 values(?,?,?,?,?,'locked')
		 on conflict(offer_id) do update set amount=excluded.amount, status='locked'`,
		offerID, owner, asset, amt.String(), tx)
	return tx, err
}

func (c *Chain) UnlockListing(ctx context.Context, offerID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var owner, asset, amount, status string
	err := c.db.QueryRowContext(ctx,
		`select owner,asset,amount,status from chain_listing_locks where offer_id=?`, offerID).
		Scan(&owner, &asset, &amount, &status)
	if errors.Is(err, sql.ErrNoRows) || status != "locked" {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := c.move(ctx, owner, asset, dec(amount)); err != nil {
		return "", err
	}
	tx := txHash("unlock", offerID, owner)
	_, err = c.db.ExecContext(ctx,
		`update chain_listing_locks set status='unlocked', tx_hash=? where offer_id=?`, tx, offerID)
	return tx, err
}

// ListingLocked 给种子与账户页读挂单锁了多少。
func (c *Chain) ListingLocked(ctx context.Context, owner, asset string) (decimal.Decimal, error) {
	rows, err := c.db.QueryContext(ctx,
		`select amount from chain_listing_locks where owner=? and asset=? and status='locked'`, owner, asset)
	if err != nil {
		return decimal.Zero, err
	}
	defer rows.Close()
	total := decimal.Zero
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return decimal.Zero, err
		}
		total = total.Add(dec(s))
	}
	return total, rows.Err()
}

func (c *Chain) reduceLock(ctx context.Context, offerID string, amt decimal.Decimal) error {
	var s string
	if err := c.db.QueryRowContext(ctx, `select amount from chain_listing_locks where offer_id=?`, offerID).Scan(&s); err != nil {
		return err
	}
	next := dec(s).Sub(amt)
	if next.IsNegative() {
		next = decimal.Zero
	}
	_, err := c.db.ExecContext(ctx, `update chain_listing_locks set amount=? where offer_id=?`, next.String(), offerID)
	return err
}

// ── 额度 ──

func (c *Chain) GrantAllowance(ctx context.Context, a chain.AllowanceGrant) (string, error) {
	var exp any
	if a.ExpiresAt != nil {
		exp = ts(*a.ExpiresAt)
	}
	tx := txHash("allow", a.ID, a.Spender)
	_, err := c.db.ExecContext(ctx,
		`insert into chain_allowances(id,account,wallet_kind,spender,asset,per_payment,window_cap,cycle,expires_at,tx_hash,status)
		 values(?,?,?,?,?,?,?,?,?,?,'live')
		 on conflict(id) do update set per_payment=excluded.per_payment, window_cap=excluded.window_cap,
		   cycle=excluded.cycle, expires_at=excluded.expires_at, tx_hash=excluded.tx_hash, status='live'`,
		a.ID, a.Account, a.WalletKind, a.Spender, a.Asset,
		a.PerPayment.String(), a.WindowCap.String(), a.Cycle, exp, tx)
	return tx, err
}

func (c *Chain) RevokeAllowance(ctx context.Context, allowanceID string) (string, error) {
	tx := txHash("revoke", allowanceID, "")
	_, err := c.db.ExecContext(ctx,
		`update chain_allowances set status='revoked', tx_hash=? where id=?`, tx, allowanceID)
	return tx, err
}

// ── 小工具 ──

func txHash(kind, a, b string) string {
	h := sha256.Sum256([]byte(kind + "|" + a + "|" + b))
	return "0x" + hex.EncodeToString(h[:])[:40]
}

func dec(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return v
}
func ts(t time.Time) string      { return t.UTC().Format(time.RFC3339Nano) }
func parseTS(s string) time.Time { t, _ := time.Parse(time.RFC3339Nano, s); return t }

// DeriveAddress 生成确定性的 EVM 地址。
//
// 用 SHA-256 而不是「乘 31 取模」那种手写散列：地址是唯一键，
// 十几个种子名就能把弱散列撞出重复，撞了会在灌种子时炸在唯一约束上。
func (c *Chain) DeriveAddress(seed string) string {
	sum := sha256.Sum256([]byte("atara-mock|" + seed))
	return "0x" + hex.EncodeToString(sum[:20])
}

// AllowanceState 读 mock 链上这份支配权的状态。
//
// mock 链不跟踪窗口用量（那需要一份支出流水），所以 Used 恒为 0、
// Available 恒等于单笔上限。真实实现（evmchain）从策略合约读真实用量。
// 这一处简化写在这里而不是藏起来：上层不该以为 mock 链能给出真实余量。
func (c *Chain) AllowanceState(ctx context.Context, allowanceID string) (*chain.AllowanceState, error) {
	var per, status string
	var exp sql.NullString
	err := c.db.QueryRowContext(ctx,
		`select per_payment, status, expires_at from chain_allowances where id=?`,
		allowanceID).Scan(&per, &status, &exp)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &chain.AllowanceState{Live: false}, nil
		}
		return nil, err
	}
	live := status == "live"
	if live && exp.Valid {
		if t, e := time.Parse(time.RFC3339Nano, exp.String); e == nil && time.Now().After(t) {
			live = false
		}
	}
	amt, _ := decimal.NewFromString(per)
	st := &chain.AllowanceState{Live: live, Used: decimal.Zero}
	if live {
		st.Available = amt
	}
	return st, nil
}
