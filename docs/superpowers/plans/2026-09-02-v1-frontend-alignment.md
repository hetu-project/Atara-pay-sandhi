# 按 V1 前端对齐后端接口 · 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 atara-pay 后端对齐到 advaita-web 分支 `v1`（末位提交 `b801bdf`）的前端，并把最后一处内存态搬进数据库。

**Architecture:** 分层不变——`api`（DTO 与 handler，不碰事务）→ `app`（用例编排，事务边界全在这里）→ `store`（SQL）+ `domain`（纯逻辑，无 IO）。本次新增的阶段派生放进 `domain/order` 作纯函数，新增的表按现有 `store/*.go` 一表一文件的惯例摆放。状态机只动一处：`s3` 与 `s4` 之间插入待核验态 `s3v`。

**Tech Stack:** Go 1.25.6 · chi v5 · modernc.org/sqlite（纯 Go，无 cgo）· shopspring/decimal · 标准库 testing

**Spec:** `docs/superpowers/specs/2026-09-02-v1-frontend-alignment-design.md`

## Global Constraints

- 模块路径 `github.com/advaita/atara-pay`，Go 版本 `1.25.6`
- **金额一律 `decimal.Decimal`，线上格式是字符串主单位**，绝不用 JSON number 或 float
- **法币不入账**：`wallets` 永远没有法币行，法币只出现在目录、挂单价格与回执里
- **api 层不开事务、不碰 `*sql.Tx`**；事务边界全在 `app` 层的 `s.St.Tx(...)` 里
- **链上动作在事务之外先做，做成了再进事务记账**——跟链之间没有分布式事务
- 迁移方式：`schema.sql` 启动时整份执行，全部 `create table if not exists`，**零 `alter table`**。新增列直接改建表语句，改完必须 `make clean` 重建 `atara.db`
- 注释与提交信息用中文，句子写完整；不加 `Co-Authored-By`
- 每个任务结束时 `go build ./... && go test ./...` 必须全绿

## 与 spec 的一处偏离（实施时按本计划）

Spec 第 1 节的派生表把 `s1` 与 `s3` 一起映射为 `pay`/`wait`。写计划时发现这在卖方向是错的：

- taker 卖币时，`s1` 是 taker 往合约注资的阶段，**币还没锁进托管**
- 此时让法币付方（maker）看到 `pay`，等于催他在托管未成立时先打钱

因此本计划把 `s1` 映射为 `lock`/`auto`（正在锁仓），`pay`/`wait` 从 `s3` 才开始。
`lock` 的前端文案是 "Locking into escrow"，语义正好吻合。Task 4 的测试固化这一点。

---

## 文件结构

| 文件 | 职责 | 动作 |
|---|---|---|
| `internal/domain/order/order.go` | 状态、事件、聚合根 | 改：加 `S3V`、`EvVerify` |
| `internal/domain/order/machine.go` | 转移表 | 改：OTC 四条边 |
| `internal/domain/order/phase.go` | **视角阶段派生（纯函数，无 IO）** | 新建 |
| `internal/domain/order/phase_test.go` | 阶段派生的表驱动测试 | 新建 |
| `internal/store/schema.sql` | 建表 | 改：3 个新列 + 4 张新表 |
| `internal/store/confirmations.go` | 确认令牌的 SQL | 新建 |
| `internal/store/payees.go` | 收款方与提现的 SQL | 新建 |
| `internal/store/maker.go` | Maker 申请与审核的 SQL | 新建 |
| `internal/store/offers.go` | 挂单查询 | 改：加候选对手方查询 |
| `internal/auth/auth.go` | 确认令牌 | 改：内存 map 换成 store |
| `internal/app/orders.go` | 订单用例 | 改：加 `VerifyReceipt` |
| `internal/app/payees.go` | 收款方与提现用例 | 新建 |
| `internal/app/maker.go` | Maker 申请与审核用例 | 新建 |
| `internal/api/dto.go` | 序列化 | 改：`orderJSON` 加 `phase`/`actor` |
| `internal/api/payees.go` | 收款方与提现 handler | 新建 |
| `internal/api/maker.go` | Maker 与审核 handler | 新建 |
| `internal/api/discover.go` | Discover 与 Tasks handler | 新建 |
| `internal/api/router.go` | 路由 | 改：挂新端点 |
| `scripts/smoke.py` | 端到端 | 改：补核验步骤 |

---

## Task 1: 状态机插入待核验态

**Files:**
- Modify: `internal/domain/order/order.go`（State 常量组、Event 常量组）
- Modify: `internal/domain/order/machine.go:66-85`（`otcEdges`）
- Modify: `internal/config/config.go`（`Timings` 加一个字段）
- Test: `internal/domain/order/machine_test.go`

**Interfaces:**
- Consumes: 无
- Produces: `order.S3V State = "s3v"`、`order.EvVerify Event = "verify"`。Task 4 用 `S3V` 派生阶段，Task 5 用 `EvVerify` 推进状态。

- [ ] **Step 1: 先写失败的测试**

在 `machine_test.go` 的 `legal` 切片里，把这一行

```go
		{"OTC 上传回执", OTCTake, S3, EvReceipt, ActorOwner, S4, TermNone},
```

替换成这四行：

```go
		{"OTC 上传回执后待对方核验", OTCTake, S3, EvReceipt, ActorOwner, S3V, TermNone},
		{"OTC 核验通过进入锁仓", OTCTake, S3V, EvVerify, ActorCounterparty, S4, TermNone},
		{"OTC 核验不通过转异议", OTCTake, S3V, EvDispute, ActorCounterparty, Disputed, TermDisputed},
		{"OTC 核验超时逾期", OTCTake, S3V, EvTick, ActorSystem, Expired, TermExpired},
```

在 `illegal` 切片里追加三条：

```go
		{"OTC 回执上传后不能直接进 s4", OTCTake, S3, EvReceipt, ActorOwner, S4},
		{"OTC 待核验时不能靠 tick 放款", OTCTake, S3V, EvTick, ActorSystem, S5},
		{"OTC 待核验时不能撤单", OTCTake, S3V, EvCancel, ActorOwner, Cancelled},
```

- [ ] **Step 2: 跑测试确认它失败**

Run: `go test ./internal/domain/order/ -run TestTransitions -v`
Expected: FAIL，报 `undefined: S3V` 与 `undefined: EvVerify`（编译错误）

- [ ] **Step 3: 加状态与事件常量**

`order.go` 的 OTC 状态常量组改成：

```go
// OTC 成交的状态。站名与前端 steps() 一致：
// Matched / Escrow funded / Your transfer / Verify & release
const (
	Match State = "match"
	S1    State = "s1"
	S3    State = "s3"
	// S3V 是回执已上传、等对方核验的中间态。
	// V1 前端的放行依据是「核验对方的银行回执」，不是「等对方点确认」，
	// 所以核验必须是一个显式的人工动作，不能由 tick 代劳。
	S3V State = "s3v"
	S4  State = "s4"
	S5  State = "s5"
)
```

`Event` 常量组末尾加一行：

```go
	EvVerify      Event = "verify" // 核验对方的法币回执
```

- [ ] **Step 4: 改转移表**

`machine.go` 的 `otcEdges` 里，把这两行

```go
	{OTCTake, S3, EvReceipt, both, S4, TermNone}, // 谁付法币谁传回执
	{OTCTake, S4, EvTick, sys, S5, TermCompleted},
```

替换成：

```go
	{OTCTake, S3, EvReceipt, both, S3V, TermNone}, // 谁付法币谁传回执

	// 核验是人工动作，只有收法币的一方能做。放行不等对方开口，
	// 等的是回执本身被核过——这正是 OTC 不需要对方点确认的原因。
	{OTCTake, S3V, EvVerify, both, S4, TermNone},
	{OTCTake, S3V, EvDispute, both, Disputed, TermDisputed}, // 回执对不上
	{OTCTake, S3V, EvTick, sys, Expired, TermExpired},       // 核验窗口过期

	{OTCTake, S4, EvTick, sys, S5, TermCompleted},
```

同时把 `otcEdges` 上方的注释图改成：

```go
//	match → s1 → s3 → s3v → s4 → s5 ✅
//	  │           │     │
//	  └ cancelled └ 超时 └ 超时 → expired ⚠️ 负向回写
```

- [ ] **Step 5: 加核验窗口时长**

`config.go` 的 `Timings` 结构体里，在 `OTCS4` 之前插入：

```go
	OTCVerify   time.Duration // 对方核验你的回执（真实 2h）
```

在两套口径的赋值处各加一行。演示口径给 `6 * time.Second`，真实口径给 `2 * time.Hour`。
（`OTCS4` 保留原值不动，它现在只覆盖「核验通过后锁仓到放款」这一小段。）

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/domain/order/ -v`
Expected: PASS，`TestTransitions` 与 `TestTerminalIsReadOnly` 全绿

- [ ] **Step 7: 确认全仓仍能编译**

Run: `go build ./... && go test ./...`
Expected: 编译通过。`app/orders.go:333` 的 `Receipt` 此刻仍写死目标态 `order.S4`，
转移表已不允许 `S3 --EvReceipt--> S4`，但那是运行期检查，编译不报错。Task 5 修它。

- [ ] **Step 8: 提交**

```bash
git add internal/domain/order/ internal/config/config.go
git commit -m "OTC 放行链路插入待核验态，核验改成显式人工动作"
```

---

## Task 2: 表结构改动

**Files:**
- Modify: `internal/store/schema.sql`
- Modify: `internal/domain/model/model.go`（`User` 加三个字段）
- Modify: `internal/store/accounts.go:34`（`scanUser`）
- Modify: `internal/store/seed.go`（种子里补 reviewer 身份与 hue）

**Interfaces:**
- Consumes: 无
- Produces: 表 `payees`、`withdrawals`、`maker_applications`、`confirmations`；
  `users` 新增列 `hue`、`avatar_url`、`role`；
  `model.User` 新增字段 `Hue int`、`AvatarURL string`、`Role string`。
  Task 3/6/7/9 依赖这些。

- [ ] **Step 1: 改 users 建表语句**

`schema.sql` 里 `create table if not exists users (...)` 改为：

```sql
create table if not exists users (
  id            text primary key,
  address       text unique not null,
  display_name  text not null,
  email         text not null default '',
  kind          text not null default 'person' check (kind in ('person','firm','agent')),
  wallet_kind   text not null default 'atara' check (wallet_kind in ('atara','ext')),
  login_method  text not null default 'passkey',  -- passkey | wallet | google | email
  -- hue 为 0 表示前端按 id 哈希取色，与前端 PAV_HUES 的逻辑一致
  hue           integer not null default 0,
  avatar_url    text not null default '',
  -- reviewer 能审 maker 申请。审核不是 agent 共识，必须有真人入口。
  role          text not null default 'user' check (role in ('user','reviewer')),
  created_at    text not null
);
```

- [ ] **Step 2: 追加四张新表**

`schema.sql` 末尾追加：

```sql
-- 收款方：非托管下链上转账由用户自己签，平台只记地址簿。
create table if not exists payees (
  id         text primary key,
  owner_id   text not null references users(id),
  label      text not null,
  chain      text not null,
  address    text not null,
  created_at text not null,
  unique (owner_id, chain, address)
);

-- 提现：记的是意图与合规材料，不代持资金。tx_hash 由用户签完回填。
create table if not exists withdrawals (
  id            text primary key,
  owner_id      text not null references users(id),
  payee_id      text not null references payees(id),
  asset_code    text not null,
  amount        text not null,
  purpose       text not null,
  doc_upload_id text not null default '',
  tx_hash       text not null default '',
  state         text not null default 'draft'
                check (state in ('draft','submitted','broadcast','confirmed','failed')),
  created_at    text not null,
  updated_at    text not null
);

-- Maker 申请。九步 KYC 字段太碎且前端仍在改，整体存 JSON blob。
create table if not exists maker_applications (
  user_id       text primary key references users(id),
  phase         text not null default 'kyc' check (phase in ('kyc','listing')),
  kyc_done      integer not null default 0,
  kyc_ok        integer not null default 0,
  listing_done  integer not null default 0,
  approved      integer not null default 0,
  form_json     text not null default '{}',
  reject_reason text not null default '',
  submitted_at  text,
  reviewed_at   text,
  reviewer_id   text references users(id),
  updated_at    text not null
);

-- 支付确认令牌。原先是进程内的 map，重启即丢；落库后重启不影响未过期的令牌。
create table if not exists confirmations (
  token       text primary key,
  user_id     text not null references users(id),
  digest      text not null,
  grade       text not null,
  expires_at  text not null,
  consumed_at text
);
create index if not exists idx_confirmations_expiry on confirmations(expires_at);
```

- [ ] **Step 3: 给 model.User 加字段**

`model.go` 的 `User` 结构体加三个字段（放在 `LoginMethod` 之后）：

```go
	Hue         int    `json:"hue"`
	AvatarURL   string `json:"avatar_url"`
	Role        string `json:"role"`
```

- [ ] **Step 4: 改 scanUser 与所有 select users 的列清单**

`accounts.go` 的 `scanUser` 加三个扫描目标，并把该文件里三处
`select ... from users` 的列清单同步加上 `hue, avatar_url, role`。
`InsertUser` 的 insert 列与占位符同步补齐。

具体做法：先跑 `grep -n 'from users\|into users' internal/store/*.go` 列出所有点，逐个改。

- [ ] **Step 5: 种子补 reviewer 与 hue**

`seed.go` 里新增一个 handle 为 `reviewer` 的用户，`role` 设 `reviewer`；
给已有的 demo 用户各分配一个 `hue`（取值参考前端 `PAV_HUES = [221,190,266,152,36,320]`）。

- [ ] **Step 6: 重建库并验证**

Run:
```bash
make clean && go build ./... && go run ./cmd/atara-pay &
sleep 3 && curl -s localhost:8080/healthz && kill %1
```
Expected: `{"status":"ok"}`，且启动无 `migrate:` 报错

- [ ] **Step 7: 跑全量测试**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 8: 提交**

```bash
git add internal/store/ internal/domain/model/model.go
git commit -m "建表：收款方、提现、maker 申请、确认令牌，用户加头像与角色"
```

---

## Task 3: 确认令牌搬进数据库

**Files:**
- Create: `internal/store/confirmations.go`
- Create: `internal/store/confirmations_test.go`
- Modify: `internal/auth/auth.go:78-160`
- Modify: `internal/app/orders.go`、`internal/app/offers.go`（4 处 `Consume` 调用点）
- Modify: `internal/scheduler/scheduler.go`（挂过期清理）

**Interfaces:**
- Consumes: Task 2 的 `confirmations` 表
- Produces:
  - `store.InsertConfirmation(ctx, token, userID, digest, grade string, expiresAt time.Time) error`
  - `store.ConsumeConfirmation(ctx, token, userID string, now time.Time) (*store.ConfirmRow, error)` —— 原子消费，未命中返回 `sql.ErrNoRows`
  - `store.PurgeConfirmations(ctx, before time.Time) error`
  - `auth.Confirmations` 的方法签名改为带 `ctx`：
    `Issue(ctx, userID, digest string, g Grade) (string, time.Time, error)`
    `Consume(ctx context.Context, raw, userID, digest string, need Grade) error`

- [ ] **Step 1: 先写失败的测试**

新建 `internal/store/confirmations_test.go`：

```go
package store

import (
	"context"
	"testing"
	"time"
)

// 一次性消费：并发下同一枚令牌只能成功一次。
// 这是「动钱必确认」里最关键的一条——重放一笔已确认的支付不该再通过。
func TestConsumeConfirmationIsOnceOnly(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	exp := time.Now().Add(2 * time.Minute)
	if err := st.InsertConfirmation(ctx, "tok1", "u1", "dig1", "signature", exp); err != nil {
		t.Fatalf("insert: %v", err)
	}

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := st.ConsumeConfirmation(ctx, "tok1", "u1", time.Now())
			errs <- err
		}()
	}
	okCount := 0
	for i := 0; i < n; i++ {
		if <-errs == nil {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("成功次数 = %d, want 1", okCount)
	}
}

// 过期的令牌不能消费。
func TestConsumeConfirmationRejectsExpired(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Second)
	if err := st.InsertConfirmation(ctx, "tok2", "u1", "dig1", "signature", past); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.ConsumeConfirmation(ctx, "tok2", "u1", time.Now()); err == nil {
		t.Fatal("过期令牌被接受了")
	}
}

// 别人的令牌不能消费。
func TestConsumeConfirmationRejectsOtherUser(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.InsertConfirmation(ctx, "tok3", "u1", "dig1", "signature",
		time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.ConsumeConfirmation(ctx, "tok3", "u2", time.Now()); err == nil {
		t.Fatal("他人令牌被接受了")
	}
}
```

同一文件里加测试夹具（本仓库此前没有 store 层测试，这是第一个）：

```go
// openTestStore 开一个临时库，跑完即弃。
// 外键打开、连接池为 1，与生产一致——否则并发用例测不出真实行为。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(
		`insert into users(id,address,display_name,created_at) values
		 ('u1','0xu1','U1',datetime('now')),('u2','0xu2','U2',datetime('now'))`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return st
}
```

- [ ] **Step 2: 跑测试确认它失败**

Run: `go test ./internal/store/ -run TestConsumeConfirmation -v`
Expected: FAIL，`undefined: (*Store).InsertConfirmation`

- [ ] **Step 3: 写 store 层实现**

新建 `internal/store/confirmations.go`：

```go
package store

import (
	"context"
	"database/sql"
	"time"
)

// ConfirmRow 是消费成功时回传的令牌事实，供 auth 层做分级与摘要校验。
type ConfirmRow struct {
	UserID string
	Digest string
	Grade  string
}

func (s *Store) InsertConfirmation(ctx context.Context,
	token, userID, digest, grade string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`insert into confirmations(token,user_id,digest,grade,expires_at)
		 values(?,?,?,?,?)`,
		token, userID, digest, grade, expiresAt.UTC().Format(time.RFC3339Nano))
	return err
}

// ConsumeConfirmation 原子地作废一枚令牌。
// 判定与作废必须是同一条 UPDATE——读后写会让并发重放挤进那道缝。
// RowsAffected==0 即失败，具体原因另查一次，只用于错误信息，不影响判定。
func (s *Store) ConsumeConfirmation(ctx context.Context,
	token, userID string, now time.Time) (*ConfirmRow, error) {
	ts := now.UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx,
		`update confirmations set consumed_at=?
		  where token=? and user_id=? and consumed_at is null and expires_at>?`,
		ts, token, userID, ts)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, sql.ErrNoRows
	}
	var row ConfirmRow
	if err := s.db.QueryRowContext(ctx,
		`select user_id,digest,grade from confirmations where token=?`, token).
		Scan(&row.UserID, &row.Digest, &row.Grade); err != nil {
		return nil, err
	}
	return &row, nil
}

// PurgeConfirmations 清掉过期行。挂在 scheduler 的同一个循环里。
func (s *Store) PurgeConfirmations(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`delete from confirmations where expires_at < ?`,
		before.UTC().Format(time.RFC3339Nano))
	return err
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -run TestConsumeConfirmation -v`
Expected: PASS，三个用例全绿

- [ ] **Step 5: 改 auth.Confirmations 用 store**

`auth.go` 里删掉 `sync` 与 `token` 结构体，`Confirmations` 改成：

```go
// Store 是 Confirmations 需要的持久化能力。定义在这里而不是引 store 包，
// 是为了不让 auth 反向依赖 store——auth 被 store 之外的地方也用。
type Store interface {
	InsertConfirmation(ctx context.Context, token, userID, digest, grade string, expiresAt time.Time) error
	ConsumeConfirmation(ctx context.Context, token, userID string, now time.Time) (*ConfirmRow, error)
}

// ConfirmRow 与 store.ConfirmRow 同形，避免 auth 依赖 store。
type ConfirmRow struct {
	UserID string
	Digest string
	Grade  string
}

type Confirmations struct{ st Store }

func NewConfirmations(st Store) *Confirmations { return &Confirmations{st: st} }
```

`Issue` 改为：

```go
func (c *Confirmations) Issue(ctx context.Context, userID, digest string, g Grade) (string, time.Time, error) {
	if g == "" {
		g = GradeSignature
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	t := hex.EncodeToString(b)
	exp := time.Now().Add(confirmTTL)
	if err := c.st.InsertConfirmation(ctx, t, userID, digest, string(g), exp); err != nil {
		return "", time.Time{}, err
	}
	return t, exp, nil
}
```

`Consume` 改为（错误码与文案一字不改，前端在依赖它们）：

```go
func (c *Confirmations) Consume(ctx context.Context, raw, userID, digest string, need Grade) error {
	if need == "" {
		need = GradeSignature
	}
	if raw == "" {
		msg := "this moves money — sign it with your passkey first"
		if need == GradeCommit {
			msg = "confirm the order first"
		}
		return httpx.Fail(http.StatusUnauthorized, "CONFIRMATION_REQUIRED", "", msg)
	}
	row, err := c.st.ConsumeConfirmation(ctx, raw, userID, time.Now())
	if err != nil {
		return httpx.Fail(http.StatusUnauthorized, "CONFIRMATION_INVALID", "",
			"confirmation expired, already used, or belongs to another account")
	}
	if row.Digest != "" && digest != "" && row.Digest != digest {
		return httpx.Fail(http.StatusUnauthorized, "CONFIRMATION_INVALID", "",
			"the payment changed after you confirmed it — confirm again")
	}
	if need == GradeSignature && Grade(row.Grade) != GradeSignature {
		return httpx.Fail(http.StatusUnauthorized, "SIGNATURE_REQUIRED", "",
			"this moves funds — it needs a passkey signature, not just a confirmation")
	}
	return nil
}
```

注意：摘要不匹配时令牌已被作废。这是有意的——摘要对不上说明这枚令牌
本就不该用于这次操作，留着它只会给重放留口子。

- [ ] **Step 6: 改 4 处调用点与构造点**

Run: `grep -rn 'Confirm.Consume\|Confirm.Issue\|NewConfirmations' internal/ cmd/`
把每处补上 `ctx` 参数；`cmd/atara-pay/main.go` 里 `auth.NewConfirmations()` 改成
`auth.NewConfirmations(st)`（`store.Store` 天然满足 `auth.Store` 接口）。
`api/account.go` 的 `PasskeyAssert` 处理 `Issue` 新增的 error 返回值。

- [ ] **Step 7: 挂过期清理**

`scheduler.go` 的 `sweep` 末尾加：

```go
	// 过期令牌不清会一直堆着。挂在同一个循环里，不另起 goroutine。
	if err := s.Svc.St.PurgeConfirmations(ctx, time.Now()); err != nil {
		log.Printf("scheduler: purge confirmations: %v", err)
	}
```

- [ ] **Step 8: 跑全量测试与构建**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 9: 提交**

```bash
git add internal/store/confirmations.go internal/store/confirmations_test.go \
        internal/auth/auth.go internal/app/ internal/api/ internal/scheduler/ cmd/
git commit -m "确认令牌从内存搬进数据库，一次性消费靠条件 UPDATE 保证"
```

---

## Task 4: 视角阶段派生

**Files:**
- Create: `internal/domain/order/phase.go`
- Create: `internal/domain/order/phase_test.go`
- Modify: `internal/api/dto.go`（`orderJSON` 与 `toOrder`）

**Interfaces:**
- Consumes: Task 1 的 `order.S3V`
- Produces:
  - `order.Phase` / `order.Viewer` 两个字符串类型与各自的常量
  - `func (o *Order) PhaseFor(viewerID string) (Phase, Viewer, bool)` —— 第三个返回值为 false 表示这笔单此刻没有阶段（终态、Conditional、或 Match 站）
  - `orderJSON` 新增 `phase` 与 `actor` 两个字段，终态时为 `null`
  Task 10 的 Tasks 投影复用 `PhaseFor`。

- [ ] **Step 1: 先写失败的测试**

新建 `internal/domain/order/phase_test.go`：

```go
package order

import "testing"

// 阶段是「这条法币腿在两个视角下的投影」。
// OTC 只有一条法币腿：买币的一方付法币，卖币的一方收法币并核验。
// 同一张单在两个人眼里必须是互补的，所以每个用例都同时断言两侧。
func TestPhaseFor(t *testing.T) {
	// side 是 taker（= OwnerID）视角。taker 买币 → taker 付法币。
	mk := func(state State, side string) *Order {
		return &Order{
			Kind: OTCTake, State: state,
			OwnerID: "taker", CounterpartyID: "maker",
			OTC: &OTC{Side: side},
		}
	}

	cases := []struct {
		name        string
		order       *Order
		viewer      string
		phase       Phase
		actor       Viewer
	}{
		// taker 买币：taker 是法币付方
		{"s1 双方都在等锁仓（买）", mk(S1, "buy"), "taker", PhaseLock, ViewerAuto},
		{"s3 付方该打款", mk(S3, "buy"), "taker", PhasePay, ViewerYou},
		{"s3 收方在等对方打款", mk(S3, "buy"), "maker", PhaseWait, ViewerThem},
		{"s3v 付方等对方核验", mk(S3V, "buy"), "taker", PhaseLock, ViewerAuto},
		{"s3v 收方该核验", mk(S3V, "buy"), "maker", PhaseVerify, ViewerYou},
		{"s4 锁仓中（买）", mk(S4, "buy"), "taker", PhaseLock, ViewerAuto},

		// taker 卖币：maker 是法币付方，两侧对调
		{"s3 卖方向：maker 该打款", mk(S3, "sell"), "maker", PhasePay, ViewerYou},
		{"s3 卖方向：taker 在等", mk(S3, "sell"), "taker", PhaseWait, ViewerThem},
		{"s3v 卖方向：taker 该核验", mk(S3V, "sell"), "taker", PhaseVerify, ViewerYou},
		{"s3v 卖方向：maker 等核验", mk(S3V, "sell"), "maker", PhaseLock, ViewerAuto},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, a, ok := c.order.PhaseFor(c.viewer)
			if !ok {
				t.Fatalf("期望有阶段，得到 ok=false")
			}
			if p != c.phase || a != c.actor {
				t.Fatalf("= (%q,%q), want (%q,%q)", p, a, c.phase, c.actor)
			}
		})
	}
}

// 没有阶段的三种情况：终态、Conditional、尚未接单的 Match 站。
func TestPhaseForReturnsNothing(t *testing.T) {
	cases := []struct {
		name  string
		order *Order
	}{
		{"终态单没有阶段", &Order{Kind: OTCTake, State: S5, Terminal: TermCompleted,
			OwnerID: "taker", CounterpartyID: "maker", OTC: &OTC{Side: "buy"}}},
		{"撤销的单没有阶段", &Order{Kind: OTCTake, State: Cancelled, Terminal: TermCancelled,
			OwnerID: "taker", CounterpartyID: "maker", OTC: &OTC{Side: "buy"}}},
		{"Match 站还没接单", &Order{Kind: OTCTake, State: Match,
			OwnerID: "taker", CounterpartyID: "maker", OTC: &OTC{Side: "buy"}}},
		{"条件支付不产出阶段", &Order{Kind: ConditionalTransfer, State: Locked,
			OwnerID: "taker", CounterpartyID: "maker"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, ok := c.order.PhaseFor("taker"); ok {
				t.Fatal("期望 ok=false")
			}
		})
	}
}

// 局外人看不到阶段。
func TestPhaseForStranger(t *testing.T) {
	o := &Order{Kind: OTCTake, State: S3, OwnerID: "taker", CounterpartyID: "maker",
		OTC: &OTC{Side: "buy"}}
	if _, _, ok := o.PhaseFor("someone-else"); ok {
		t.Fatal("局外人不该拿到阶段")
	}
}
```

- [ ] **Step 2: 跑测试确认它失败**

Run: `go test ./internal/domain/order/ -run TestPhase -v`
Expected: FAIL，`undefined: PhaseFor`

- [ ] **Step 3: 写实现**

新建 `internal/domain/order/phase.go`：

```go
package order

// Phase 是前端 OSTATE 的五个阶段。取值是前端的键名，不能改。
type Phase string

const (
	PhasePay    Phase = "pay"    // 该你打法币了
	PhaseVerify Phase = "verify" // 对方回执到了，该你核验
	PhaseWait   Phase = "wait"   // 等对方打法币
	PhaseLock   Phase = "lock"   // 锁仓中，没人需要动手
	PhaseRel    Phase = "rel"    // 放款中
)

// Viewer 说这一步该谁动手。前端据此决定按钮是亮的还是灰的。
type Viewer string

const (
	ViewerYou  Viewer = "you"
	ViewerThem Viewer = "them"
	ViewerAuto Viewer = "auto"
)

// fiatPayer 返回这笔 OTC 里出法币的那个人。
// OTC 只有一条法币腿：买币的一方付法币。Side 是 taker（= OwnerID）的视角，
// 所以 taker 买币时法币付方是 taker，卖币时是 maker。
func (o *Order) fiatPayer() string {
	if o.OTC != nil && o.OTC.Side == "sell" {
		return o.CounterpartyID
	}
	return o.OwnerID
}

// PhaseFor 算出这笔单在 viewerID 眼里的阶段。
// ok 为 false 表示此刻没有阶段可展示：终态、条件支付、尚未接单的 Match 站，
// 或者提问的人根本不是这笔单的两方之一。
//
// s1 归入 lock 而不是 pay：卖方向的 s1 是 taker 往合约注资的阶段，
// 币还没锁进托管。这时候催法币付方打钱，是在托管成立之前就让他掏钱。
func (o *Order) PhaseFor(viewerID string) (Phase, Viewer, bool) {
	if o.Kind != OTCTake || o.OTC == nil || o.IsTerminal() {
		return "", "", false
	}
	if viewerID != o.OwnerID && viewerID != o.CounterpartyID {
		return "", "", false
	}
	payer := o.fiatPayer()
	switch o.State {
	case S1, S4:
		return PhaseLock, ViewerAuto, true
	case S3:
		if viewerID == payer {
			return PhasePay, ViewerYou, true
		}
		return PhaseWait, ViewerThem, true
	case S3V:
		// 付方已经打完款，等对方核验——他没有动作可做。
		if viewerID == payer {
			return PhaseLock, ViewerAuto, true
		}
		return PhaseVerify, ViewerYou, true
	case S5:
		return PhaseRel, ViewerAuto, true
	}
	return "", "", false
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/domain/order/ -v`
Expected: PASS，三个新测试函数全绿

- [ ] **Step 5: 接进 DTO**

`api/dto.go` 的 `orderJSON` 结构体，在 `Terminal` 之后加两个字段：

```go
	Phase    *string `json:"phase"`
	PhaseFor *string `json:"actor"`
```

用指针是为了终态时序列化成 `null` 而不是空字符串——spec 要求前端按 `terminal` 渲染，
空字符串会让前端误以为有阶段。

`toOrder` 里，在 `j.Escrow = ...` 之前插入：

```go
	// 阶段是按观察者算的：同一张单，付法币的一方看到 pay，另一方看到 wait。
	if p, a, ok := o.PhaseFor(viewerID); ok {
		ps, as := string(p), string(a)
		j.Phase, j.PhaseFor = &ps, &as
	}
```

`toOrder` 的签名改为带上观察者：

```go
func (h *Handler) toOrder(ctx context.Context, viewerID string, o *order.Order, withEvents bool) orderJSON {
```

Run `grep -rn 'toOrder(' internal/api/` 找出全部调用点，各自传 `h.actorID(r)`。

- [ ] **Step 6: 构建并跑全量测试**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 7: 手工验一次 JSON 形状**

Run:
```bash
make clean && (go run ./cmd/atara-pay &) && sleep 3
curl -s -H 'X-Atara-User: demo' localhost:8080/api/v1/orders | head -c 600
pkill -f atara-pay
```
Expected: 订单对象里出现 `"phase"` 与 `"actor"` 两个键

- [ ] **Step 8: 提交**

```bash
git add internal/domain/order/phase.go internal/domain/order/phase_test.go internal/api/dto.go
git commit -m "订单按观察者视角派生阶段，前端拿到就能直接渲染"
```

---

## Task 5: 核验回执端点

**Files:**
- Modify: `internal/app/orders.go:317-340`（`Receipt` 的目标态）
- Modify: `internal/app/orders.go`（新增 `VerifyReceipt`）
- Modify: `internal/store/orders.go:277`（`Receipt` 查询补上传者与核验时间）
- Modify: `internal/api/orders.go`（新增 handler）
- Modify: `internal/api/router.go`
- Test: `internal/domain/order/machine_test.go` 已在 Task 1 覆盖状态机；本任务补 app 层的权限断言

**Interfaces:**
- Consumes: Task 1 的 `order.EvVerify`、`order.S3V`
- Produces:
  - `store.ReceiptRow{ID, FileRef, UploaderID string; VerifiedAt *time.Time}`
  - `(*Store).LatestReceipt(ctx, orderID string) (*ReceiptRow, bool)`
  - `store.MarkReceiptVerified(tx *sql.Tx, receiptID string, at time.Time) error`
  - `(*Service).VerifyReceipt(ctx, actorID, orderID string, okFlag bool, reason string) (*order.Order, error)`
  - `POST /api/v1/orders/{id}/verify-receipt`

- [ ] **Step 1: 改 Receipt 的目标态**

`app/orders.go` 的 `Receipt` 方法里，把 `order.S4` 改成 `order.S3V`，
并把 reason 文案改成：

```go
		"Receipt uploaded · waiting on the other side to check it against the order",
```

- [ ] **Step 2: 扩展 store 的回执查询**

`store/orders.go` 里，保留原 `Receipt` 不动（`toOrder` 还在用），新增：

```go
// ReceiptRow 是核验要用到的回执事实：谁传的、核过没有。
type ReceiptRow struct {
	ID         string
	FileRef    string
	UploaderID string
	VerifiedAt *time.Time
}

func (s *Store) LatestReceipt(ctx context.Context, orderID string) (*ReceiptRow, bool) {
	var r ReceiptRow
	var verified sql.NullString
	err := s.db.QueryRowContext(ctx,
		`select id, file_ref, uploader_id, verified_at from fiat_receipts
		  where order_id=? order by created_at desc limit 1`, orderID).
		Scan(&r.ID, &r.FileRef, &r.UploaderID, &verified)
	if err != nil {
		return nil, false
	}
	if verified.Valid {
		if t, e := time.Parse(time.RFC3339Nano, verified.String); e == nil {
			r.VerifiedAt = &t
		}
	}
	return &r, true
}

func MarkReceiptVerified(tx *sql.Tx, receiptID string, at time.Time) error {
	_, err := tx.Exec(`update fiat_receipts set verified_at=? where id=?`,
		at.UTC().Format(time.RFC3339Nano), receiptID)
	return err
}
```

- [ ] **Step 3: 写 app 层用例**

`app/orders.go` 里 `Receipt` 之后新增：

```go
// VerifyReceipt 是 OTC 的放行闸门。放行不等对方开口，等的是回执被核过——
// 所以只有收法币的那一方能核，上传者不能自己核自己的。
func (s *Service) VerifyReceipt(ctx context.Context, actorID, orderID string,
	okFlag bool, reason string) (*order.Order, error) {
	o, err := s.St.Order(ctx, orderID)
	if err != nil {
		return nil, httpx.NotFound("order")
	}
	if o.OwnerID != actorID && o.CounterpartyID != actorID {
		return nil, httpx.Fail(http.StatusForbidden, "NOT_YOURS", "", "this order belongs to another account")
	}
	rc, found := s.St.LatestReceipt(ctx, o.ID)
	if !found {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "NO_RECEIPT", "",
			"there is no receipt to check yet")
	}
	if rc.UploaderID == actorID {
		return nil, httpx.Fail(http.StatusForbidden, "NOT_YOUR_CALL", "",
			"the side that uploaded the receipt cannot be the one that clears it")
	}
	actor := order.ActorOwner
	if o.OwnerID != actorID {
		actor = order.ActorCounterparty
	}
	if !okFlag {
		if reason == "" {
			reason = "the receipt does not match this order"
		}
		return s.advance(ctx, o.ID, order.EvDispute, actor, order.Disputed,
			"Receipt rejected · "+reason,
			map[string]string{"receipt_id": rc.ID, "reason": reason}, nil, nil)
	}
	now := time.Now()
	return s.advance(ctx, o.ID, order.EvVerify, actor, order.S4,
		"Receipt verified · releasing to them",
		map[string]string{"receipt_id": rc.ID}, nil,
		func(tx *sql.Tx, _ *order.Order) error {
			return store.MarkReceiptVerified(tx, rc.ID, now)
		})
}
```

- [ ] **Step 4: 写 handler**

`api/orders.go` 末尾新增：

```go
func (h *Handler) VerifyReceipt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, err)
		return
	}
	o, err := h.Svc.VerifyReceipt(r.Context(), h.actorID(r), chi.URLParam(r, "id"), req.OK, req.Reason)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	ok(w, h.toOrder(r.Context(), h.actorID(r), o, true))
}
```

若 `httpx.Decode` 名称不同，先跑 `grep -n 'func Decode\|func Bind' internal/httpx/httpx.go` 对齐。

- [ ] **Step 5: 挂路由**

`router.go` 的 `/orders/{id}` 分组里，`r.Post("/receipt", h.Receipt)` 之后加：

```go
				r.Post("/verify-receipt", h.VerifyReceipt)
```

- [ ] **Step 6: 构建并跑全量测试**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
git add internal/app/orders.go internal/store/orders.go internal/api/ 
git commit -m "OTC 加核验回执端点，上传者不能自己核自己的"
```

---

## Task 6: 候选对手方

**Files:**
- Modify: `internal/store/offers.go`
- Create: `internal/store/offers_eligible_test.go`
- Modify: `internal/app/offers.go`（`Match`/`Quote` 加 `counterparty_id`）
- Modify: `internal/api/orders.go`、`internal/api/router.go`

**Interfaces:**
- Consumes: Task 2 的 `users.hue`、`users.avatar_url`
- Produces:
  - `store.EligiblePeer{UserID, Name, PeerCode string; Hue int; AvatarURL string; TrustScore, Deals int; BestPrice, AvailableQty decimal.Decimal}`
  - `(*Store).EligibleCounterparties(ctx, viewerID, side, asset, fiat string, amount decimal.Decimal) ([]EligiblePeer, error)`
  - `GET /api/v1/orders/eligible-counterparties`
  - `Match`/`Quote` 的请求体新增可选字段 `counterparty_id`

- [ ] **Step 1: 先写失败的测试**

新建 `internal/store/offers_eligible_test.go`，覆盖五条排除规则各自的路径：

```go
package store

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
)

// 「真能吃下这单」有五条判定。每条都要有一个被它挡下来的用例，
// 否则规则写错了测试也发现不了。
func TestEligibleCounterparties(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	d := func(s string) decimal.Decimal { v, _ := decimal.NewFromString(s); return v }

	// viewer 想买 1000 USDT，付 CNY。能吃下的必须是 sell 方向的活跃挂单。
	seedOffer(t, st, "o-ok", "u2", "sell", "USDT", "CNY", "1000", "100", "active")
	seedOffer(t, st, "o-delisted", "u3", "sell", "USDT", "CNY", "1000", "100", "delisted")
	seedOffer(t, st, "o-samedir", "u4", "buy", "USDT", "CNY", "1000", "100", "active")
	seedOffer(t, st, "o-small", "u5", "sell", "USDT", "CNY", "10", "1", "active")
	seedOffer(t, st, "o-minlot", "u6", "sell", "USDT", "CNY", "5000", "3000", "active")
	seedOffer(t, st, "o-fiat", "u7", "sell", "USDT", "HKD", "1000", "100", "active")
	seedOffer(t, st, "o-self", "u1", "sell", "USDT", "CNY", "1000", "100", "active")

	got, err := st.EligibleCounterparties(ctx, "u1", "buy", "USDT", "CNY", d("1000"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].UserID != "u2" {
		ids := []string{}
		for _, p := range got {
			ids = append(ids, p.UserID)
		}
		t.Fatalf("命中 = %v, want [u2]", ids)
	}
}
```

同一文件里加 `seedOffer` 夹具，并在 `openTestStore` 的种子里把用户扩到 `u1`–`u7`
（连同 `merchant_profiles` 行，否则信誉字段 join 不到）。

- [ ] **Step 2: 跑测试确认它失败**

Run: `go test ./internal/store/ -run TestEligible -v`
Expected: FAIL，`undefined: (*Store).EligibleCounterparties`

- [ ] **Step 3: 写查询**

`store/offers.go` 末尾新增。反方向的映射是：viewer 要 `buy` 就找 `sell` 挂单。

```go
// EligiblePeer 是「能吃下这单」的对手方，带前端列表要的头像与信誉。
type EligiblePeer struct {
	UserID       string          `json:"user_id"`
	Name         string          `json:"display_name"`
	PeerCode     string          `json:"peer_code"`
	Hue          int             `json:"hue"`
	AvatarURL    string          `json:"avatar_url"`
	TrustScore   int             `json:"trust_score"`
	Deals        int             `json:"deals"`
	BestPrice    decimal.Decimal `json:"best_price"`
	AvailableQty decimal.Decimal `json:"available_qty"`
}

// EligibleCounterparties 列出真能接下这笔的人。
// 五条判定：方向相反、挂单活跃、余量够、起投额不超、法币与资产都对上；
// 外加排除自己。挡不住其中任何一条，前端就会把不能成交的人摆进列表。
func (s *Store) EligibleCounterparties(ctx context.Context,
	viewerID, side, asset, fiat string, amount decimal.Decimal) ([]EligiblePeer, error) {
	want := "sell"
	if side == "sell" {
		want = "buy"
	}
	rows, err := s.db.QueryContext(ctx,
		`select u.id, u.display_name, coalesce(m.peer_code,''), u.hue, u.avatar_url,
		        coalesce(m.trust_score,0), coalesce(m.deals,0),
		        o.unit_price, o.remaining_qty
		   from offers o
		   join users u on u.id = o.maker_id
		   left join merchant_profiles m on m.user_id = o.maker_id
		  where o.status = 'active'
		    and o.side = ?
		    and o.asset_code = ?
		    and o.fiat_code = ?
		    and o.maker_id <> ?
		    and cast(o.remaining_qty as real) >= ?
		    and cast(o.min_lot as real) <= ?
		  order by cast(o.unit_price as real) asc`,
		want, asset, fiat, viewerID, amount.InexactFloat64(), amount.InexactFloat64())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 一个 maker 可能有多条挂单，只留价格最优的那条。
	seen := map[string]bool{}
	var out []EligiblePeer
	for rows.Next() {
		var p EligiblePeer
		var price, qty string
		if err := rows.Scan(&p.UserID, &p.Name, &p.PeerCode, &p.Hue, &p.AvatarURL,
			&p.TrustScore, &p.Deals, &price, &qty); err != nil {
			return nil, err
		}
		if seen[p.UserID] {
			continue
		}
		seen[p.UserID] = true
		p.BestPrice, _ = decimal.NewFromString(price)
		p.AvailableQty, _ = decimal.NewFromString(qty)
		out = append(out, p)
	}
	return out, rows.Err()
}
```

注意 `cast(... as real)` 的比较：金额在库里是 TEXT，字符串比较会把 "9" 排在 "10" 后面。
这里只用于筛选不用于记账，精度损失可接受；**任何写账的地方仍必须走 decimal**。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -run TestEligible -v`
Expected: PASS

- [ ] **Step 5: 加 handler 与路由**

`api/orders.go` 新增：

```go
func (h *Handler) EligibleCounterparties(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	amount, err := decimal.NewFromString(q.Get("amount"))
	if err != nil {
		httpx.Error(w, httpx.Fail(http.StatusUnprocessableEntity, "BAD_AMOUNT", "amount",
			"amount must be a decimal number"))
		return
	}
	peers, err := h.St.EligibleCounterparties(r.Context(), h.actorID(r),
		q.Get("side"), q.Get("asset"), q.Get("fiat"), amount)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	if peers == nil {
		peers = []store.EligiblePeer{}
	}
	ok(w, map[string]any{"counterparties": peers})
}
```

`router.go` 的 `/orders` 分组里，`r.Post("/match", h.Match)` 之后加：

```go
			r.Get("/eligible-counterparties", h.EligibleCounterparties)
```

- [ ] **Step 6: 给 Match / Quote 加对手方过滤**

`app/offers.go` 的撮合请求结构体加字段：

```go
	// 空表示 Any。指定了就只在这个人的挂单里撮合，撮不到就明确失败，
	// 不静默回退到 Any——用户点了「跟他交易」，成交对象却是别人，是最坏的结果。
	CounterpartyID string `json:"counterparty_id"`
```

在挂单筛选处加上 `maker_id = ?` 的条件；撮不到时返回：

```go
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "NO_MATCH_WITH_COUNTERPARTY",
			"counterparty_id", "that counterparty has no offer that can fill this order right now")
```

- [ ] **Step 7: 构建并跑全量测试**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 8: 提交**

```bash
git add internal/store/offers.go internal/store/offers_eligible_test.go \
        internal/app/offers.go internal/api/
git commit -m "下单可指定对手方，候选列表只给真能吃下这单的人"
```

---

## Task 7: 收款方与提现

**Files:**
- Create: `internal/store/payees.go`
- Create: `internal/store/payees_test.go`
- Create: `internal/app/payees.go`
- Create: `internal/api/payees.go`
- Modify: `internal/api/router.go`

**Interfaces:**
- Consumes: Task 2 的 `payees`、`withdrawals` 表
- Produces:
  - `store.Payee{ID, OwnerID, Label, Chain, Address string; CreatedAt time.Time}`
  - `store.Withdrawal{ID, OwnerID, PayeeID, Asset, Purpose, DocUploadID, TxHash, State string; Amount decimal.Decimal; CreatedAt, UpdatedAt time.Time}`
  - `(*Store).Payees(ctx, ownerID) ([]Payee, error)` / `AddPayee(ctx, p Payee) error` / `DeletePayee(ctx, ownerID, id string) error`
  - `(*Store).Withdrawals(ctx, ownerID) ([]Withdrawal, error)` / `InsertWithdrawal(ctx, w Withdrawal) error`
  - `(*Service).CreateWithdrawal(ctx, ownerID, confirmToken string, req WithdrawReq) (*store.Withdrawal, error)`
  - 端点：`GET/POST /payees`、`DELETE /payees/{id}`、`GET/POST /withdrawals`

- [ ] **Step 1: 先写失败的测试**

新建 `internal/store/payees_test.go`：

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// 地址簿按 (owner, chain, address) 去重：同一个人不该把同一个地址存两遍。
func TestPayeeRoundTripAndDedup(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	p := Payee{ID: "p1", OwnerID: "u1", Label: "Ops wallet",
		Chain: "TRON", Address: "TXm...9f", CreatedAt: time.Now()}
	if err := st.AddPayee(ctx, p); err != nil {
		t.Fatalf("add: %v", err)
	}
	dup := p
	dup.ID = "p2"
	if err := st.AddPayee(ctx, dup); err == nil {
		t.Fatal("同一地址被存了第二遍")
	}
	got, err := st.Payees(ctx, "u1")
	if err != nil || len(got) != 1 || got[0].Label != "Ops wallet" {
		t.Fatalf("payees = %+v, err = %v", got, err)
	}
}

// 提现只记意图与合规材料，金额必须原样往返，不能被 float 改掉尾数。
func TestWithdrawalPreservesAmount(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.AddPayee(ctx, Payee{ID: "p1", OwnerID: "u1", Label: "L",
		Chain: "ETH", Address: "0xabc", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("payee: %v", err)
	}
	amt, _ := decimal.NewFromString("3.600000000000000001")
	w := Withdrawal{ID: "w1", OwnerID: "u1", PayeeID: "p1", Asset: "ETH",
		Amount: amt, Purpose: "OTC settlement", State: "submitted",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.InsertWithdrawal(ctx, w); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := st.Withdrawals(ctx, "u1")
	if err != nil || len(got) != 1 {
		t.Fatalf("withdrawals = %+v, err = %v", got, err)
	}
	if got[0].Amount.String() != amt.String() {
		t.Fatalf("amount = %s, want %s", got[0].Amount, amt)
	}
}
```

- [ ] **Step 2: 跑测试确认它失败**

Run: `go test ./internal/store/ -run 'TestPayee|TestWithdrawal' -v`
Expected: FAIL，`undefined: Payee`

- [ ] **Step 3: 写 store 层**

新建 `internal/store/payees.go`，按上面的 Interfaces 实现。
金额列用 `w.Amount.String()` 写入、`decimal.NewFromString` 读出，全程不经 float。
`AddPayee` 依赖表上的 `unique (owner_id, chain, address)` 报冲突，不在 Go 里查重
——查后写会给并发留缝。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -run 'TestPayee|TestWithdrawal' -v`
Expected: PASS

- [ ] **Step 5: 写 app 层用例**

新建 `internal/app/payees.go`：

```go
// WithdrawReq 是前端四步提现一次性提交的内容：
// 地址（收款方）→ 金额 → 用途 → 凭证。
type WithdrawReq struct {
	PayeeID     string `json:"payee_id"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
	Purpose     string `json:"purpose"`
	DocUploadID string `json:"doc_upload_id"`
}

// CreateWithdrawal 记下一次提现意图。非托管下链上转账由用户自己签，
// 平台不代持也不代发；这里存的是用途与凭证这类合规材料，加一个待回填的 tx_hash。
// 动钱必确认：即便平台不动手，这一步仍要签名档的确认令牌。
func (s *Service) CreateWithdrawal(ctx context.Context, ownerID, confirmToken string,
	req WithdrawReq) (*store.Withdrawal, error) {
```

必填校验：`payee_id`、`asset`、`amount`、`purpose` 缺一即 422，
错误码分别为 `PAYEE_REQUIRED`、`ASSET_REQUIRED`、`AMOUNT_REQUIRED`、`PURPOSE_REQUIRED`。
金额必须 `> 0`，否则 `AMOUNT_INVALID`。
确认令牌走 `s.Confirm.Consume(ctx, confirmToken, ownerID, Digest("withdraw", req.PayeeID, req.Asset, amt.String()), auth.GradeSignature)`。

- [ ] **Step 6: 写 handler 与路由**

新建 `internal/api/payees.go`，五个 handler：`Payees`、`AddPayee`、`DeletePayee`、
`Withdrawals`、`CreateWithdrawal`。`router.go` 在 `/uploads` 之后加：

```go
		r.Route("/payees", func(r chi.Router) {
			r.Get("/", h.Payees)
			r.Post("/", h.AddPayee)
			r.Delete("/{id}", h.DeletePayee)
		})
		r.Get("/withdrawals", h.Withdrawals)
		r.Post("/withdrawals", h.CreateWithdrawal)
```

- [ ] **Step 7: 构建并跑全量测试**

Run: `go build ./... && go test ./...`
Expected: PASS

- [ ] **Step 8: 提交**

```bash
git add internal/store/payees.go internal/store/payees_test.go \
        internal/app/payees.go internal/api/payees.go internal/api/router.go
git commit -m "提现与收款方入库，用途和凭证这类合规材料有地方存了"
```

---

## Task 8: Discover 纵向目录

**Files:**
- Create: `internal/api/discover.go`
- Modify: `internal/api/router.go`

**Interfaces:**
- Consumes: 无
- Produces: `GET /api/v1/discover/markets`。Task 10 复用同一文件放 Tasks handler。

- [ ] **Step 1: 写实现**

Discover 内容近乎静态，且与资产目录一样不入库（spec 已定），所以直接是 Go 常量。
新建 `internal/api/discover.go`：

```go
package api

import "net/http"

// 三个纵向照抄前端 DISCOVER 常量。只有 OTC 上线，另两个是 Coming。
// 与资产目录同样不入库——它们随版本走，不随数据走。
type marketJSON struct {
	Key  string     `json:"key"`
	Name string     `json:"name"`
	Live bool       `json:"live"`
	Desc string     `json:"desc,omitempty"`
	Map  [][2]string `json:"map,omitempty"`
}

var markets = []marketJSON{
	{Key: "otc", Name: "OTC pool", Live: true},
	{Key: "api", Name: "Compute & APIs", Live: false,
		Desc: "Where agents buy inference, data or compute — settled per call or per unit.",
		Map: [][2]string{
			{"Counterparty", "Providers — unfamiliar, high frequency"},
			{"Condition", "Call succeeds and returns"},
			{"Evidence", "API callback · usage reconciliation"},
		}},
	{Key: "shop", Name: "Merchants", Live: false,
		Desc: "Goods and services with an acceptance condition — receive first, pay after.",
		Map: [][2]string{
			{"Counterparty", "Merchants — unfamiliar, one-off"},
			{"Condition", "Signed for, no dispute in window"},
			{"Evidence", "Logistics API · 7-day auto-release"},
		}},
}

func (h *Handler) DiscoverMarkets(w http.ResponseWriter, _ *http.Request) {
	ok(w, map[string]any{"markets": markets})
}
```

- [ ] **Step 2: 挂路由**

`router.go` 的 `/catalog` 分组之后加：

```go
		r.Get("/discover/markets", h.DiscoverMarkets)
```

- [ ] **Step 3: 构建并手工验一次**

Run:
```bash
go build ./... && (go run ./cmd/atara-pay &) && sleep 3
curl -s -H 'X-Atara-User: demo' localhost:8080/api/v1/discover/markets
pkill -f atara-pay
```
Expected: 三个纵向，`otc` 的 `live` 为 `true`，另两个为 `false`

- [ ] **Step 4: 提交**

```bash
git add internal/api/discover.go internal/api/router.go
git commit -m "Discover 纵向目录，只有 OTC 上线"
```

---

## Task 9: Maker 申请与真实审核

**Files:**
- Create: `internal/store/maker.go`
- Create: `internal/store/maker_test.go`
- Create: `internal/app/maker.go`
- Create: `internal/api/maker.go`
- Modify: `internal/auth/auth.go`（加 `RequireRole`）
- Modify: `internal/api/router.go`

**Interfaces:**
- Consumes: Task 2 的 `maker_applications` 表与 `users.role`
- Produces:
  - `store.MakerApp{UserID, Phase, FormJSON, RejectReason, ReviewerID string; KYCDone, KYCOk, ListingDone, Approved bool; SubmittedAt, ReviewedAt *time.Time; UpdatedAt time.Time}`
  - `(*Store).MakerApp(ctx, userID) (*MakerApp, error)` / `UpsertMakerApp(ctx, a MakerApp) error` / `PendingMakerApps(ctx) ([]MakerApp, error)` / `ReviewMakerApp(ctx, userID, stage, decision, reason, reviewerID string, at time.Time) error`
  - `auth.RequireRole(role string) func(http.Handler) http.Handler`
  - 端点：`GET/POST /maker/application`、`GET /admin/maker/applications`、`POST /admin/maker/applications/{user_id}/review`

- [ ] **Step 1: 先写失败的测试**

新建 `internal/store/maker_test.go`，覆盖两段审核的状态位流转与拒绝后重提：

```go
package store

import (
	"context"
	"testing"
	"time"
)

// 两段审核各自置位：kyc 过了才进 listing 段，listing 过了才算 approved。
// 跳段会让没审身份的人直接挂单。
func TestMakerReviewStages(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "kyc",
		KYCDone: true, FormJSON: `{"kind":"Individual"}`, UpdatedAt: now}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := st.ReviewMakerApp(ctx, "u1", "kyc", "approve", "", "u2", now); err != nil {
		t.Fatalf("review kyc: %v", err)
	}
	a, _ := st.MakerApp(ctx, "u1")
	if !a.KYCOk || a.Phase != "listing" || a.Approved {
		t.Fatalf("kyc 审过后 = %+v, 期望 kyc_ok=true phase=listing approved=false", a)
	}

	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "listing",
		KYCDone: true, KYCOk: true, ListingDone: true, UpdatedAt: now}); err != nil {
		t.Fatalf("upsert listing: %v", err)
	}
	if err := st.ReviewMakerApp(ctx, "u1", "listing", "approve", "", "u2", now); err != nil {
		t.Fatalf("review listing: %v", err)
	}
	a, _ = st.MakerApp(ctx, "u1")
	if !a.Approved {
		t.Fatalf("listing 审过后 approved = false, 期望 true")
	}
}

// 拒绝要写明理由并把对应位清零，否则前端会一直显示 Under review。
func TestMakerReviewReject(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "kyc",
		KYCDone: true, UpdatedAt: now}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.ReviewMakerApp(ctx, "u1", "kyc", "reject", "ID 照片看不清", "u2", now); err != nil {
		t.Fatalf("reject: %v", err)
	}
	a, _ := st.MakerApp(ctx, "u1")
	if a.KYCOk || a.KYCDone || a.RejectReason != "ID 照片看不清" {
		t.Fatalf("拒绝后 = %+v, 期望 kyc_done/kyc_ok 都为 false 且有理由", a)
	}
}
```

- [ ] **Step 2: 跑测试确认它失败**

Run: `go test ./internal/store/ -run TestMaker -v`
Expected: FAIL，`undefined: MakerApp`

- [ ] **Step 3: 写 store 层**

新建 `internal/store/maker.go`。`ReviewMakerApp` 按 stage 分支：

- `stage=kyc` + `approve`：置 `kyc_ok=1`、`phase='listing'`、清 `reject_reason`
- `stage=kyc` + `reject`：清 `kyc_ok=0` 与 `kyc_done=0`、写 `reject_reason`
- `stage=listing` + `approve`：置 `approved=1`、清 `reject_reason`
- `stage=listing` + `reject`：清 `approved=0` 与 `listing_done=0`、写 `reject_reason`

四个分支都写 `reviewed_at` 与 `reviewer_id`。stage 或 decision 取值非法时返回错误，
不要静默忽略——静默忽略会让审核动作看起来成功却什么都没变。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -run TestMaker -v`
Expected: PASS

- [ ] **Step 5: 加角色中间件**

`auth.go` 末尾新增：

```go
// RequireRole 挡住没有该角色的人。用在审核这类真人动作上——
// maker 审核不是 agent 共识，不能由系统自动放行，也不能人人都能点。
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := Actor(r.Context())
			if u == nil || u.Role != role {
				httpx.Error(w, httpx.Fail(http.StatusForbidden, "ROLE_REQUIRED", "",
					"this action needs the "+role+" role"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

- [ ] **Step 6: 写 app 与 handler**

新建 `internal/app/maker.go` 与 `internal/api/maker.go`。
`POST /maker/application` 按当前 `phase` 分段接收：`phase=kyc` 时置 `kyc_done=1`，
`phase=listing` 时置 `listing_done=1`，两者都写 `form_json` 与 `submitted_at`。
后端只校验 `form_json` 是合法 JSON 且非空，**不校验九步表单的业务语义**
——字段还在改，后端跟着改会一直破。

`router.go` 加：

```go
		r.Get("/maker/application", h.MakerApplication)
		r.Post("/maker/application", h.SubmitMakerApplication)

		r.Route("/admin/maker", func(r chi.Router) {
			r.Use(auth.RequireRole("reviewer"))
			r.Get("/applications", h.PendingMakerApplications)
			r.Post("/applications/{user_id}/review", h.ReviewMakerApplication)
		})
```

- [ ] **Step 7: 构建、跑测试、手工验审核链路**

Run:
```bash
go build ./... && go test ./...
make clean && (go run ./cmd/atara-pay &) && sleep 3
curl -s -X POST -H 'X-Atara-User: demo' -H 'Content-Type: application/json' \
  -d '{"phase":"kyc","form":{"kind":"Individual"}}' \
  localhost:8080/api/v1/maker/application
curl -s -H 'X-Atara-User: demo' localhost:8080/api/v1/admin/maker/applications
curl -s -H 'X-Atara-User: reviewer' localhost:8080/api/v1/admin/maker/applications
pkill -f atara-pay
```
Expected: 测试全绿；`demo` 拿到 403 `ROLE_REQUIRED`，`reviewer` 拿到待审列表

- [ ] **Step 8: 提交**

```bash
git add internal/store/maker.go internal/store/maker_test.go \
        internal/app/maker.go internal/api/maker.go \
        internal/auth/auth.go internal/api/router.go
git commit -m "Maker 申请与两段审核，审核是真人动作不走共识"
```

---

## Task 10: Tasks 投影

**Files:**
- Modify: `internal/api/discover.go`
- Modify: `internal/api/router.go`

**Interfaces:**
- Consumes: Task 4 的 `(*Order).PhaseFor`
- Produces: `GET /api/v1/tasks`

- [ ] **Step 1: 写实现**

Tasks 是订单的派生视图，不建表。`api/discover.go` 追加：

```go
// taskJSON 对齐前端 TKST 的三个键。Tasks 是订单的投影，不是独立实体——
// 前端注释写得很清楚：「每笔交易开单即入列，状态跟着 advance() 走」。
type taskJSON struct {
	ID       string    `json:"id"`
	OrderRef string    `json:"order_ref"`
	Title    string    `json:"title"`
	State    string    `json:"state"` // run | you | done
	At       time.Time `json:"at"`
}

func (h *Handler) Tasks(w http.ResponseWriter, r *http.Request) {
	me := h.actorID(r)
	orders, err := h.St.OrdersFor(r.Context(), me)
	if err != nil {
		httpx.Error(w, err)
		return
	}
	out := []taskJSON{}
	for _, o := range orders {
		t := taskJSON{ID: o.ID, OrderRef: o.Ref, At: o.UpdatedAt}
		switch {
		case o.IsTerminal():
			t.State, t.Title = "done", "Settled"
		default:
			p, a, okp := o.PhaseFor(me)
			if !okp {
				continue // 没有阶段的单不进待办列表
			}
			t.Title = phaseTitles[p]
			if a == order.ViewerYou {
				t.State = "you"
			} else {
				t.State = "run"
			}
		}
		out = append(out, t)
	}
	// 该你动手的排最前，其次进行中，最后已完成；同组按更新时间倒序。
	rank := map[string]int{"you": 0, "run": 1, "done": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].State] != rank[out[j].State] {
			return rank[out[i].State] < rank[out[j].State]
		}
		return out[i].At.After(out[j].At)
	})
	ok(w, map[string]any{"tasks": out})
}

// 标题用前端 OSTATE 的第二个元素，保持两边措辞一致。
var phaseTitles = map[order.Phase]string{
	order.PhasePay:    "Send the transfer",
	order.PhaseVerify: "Verify their receipt",
	order.PhaseWait:   "Waiting on their transfer",
	order.PhaseLock:   "Locking into escrow",
	order.PhaseRel:    "Releasing to them",
}
```

若 `OrdersFor` 不存在，先跑 `grep -n 'func (s \*Store) Orders' internal/store/orders.go`
用已有的列表方法（`ListOrders` 的底层查询）替代，签名对齐即可。

- [ ] **Step 2: 挂路由**

`router.go` 加：

```go
		r.Get("/tasks", h.Tasks)
```

- [ ] **Step 3: 构建、跑测试、手工验**

Run:
```bash
go build ./... && go test ./...
make clean && (go run ./cmd/atara-pay &) && sleep 3
curl -s -H 'X-Atara-User: demo' localhost:8080/api/v1/tasks
pkill -f atara-pay
```
Expected: 返回 `tasks` 数组，`state` 只出现 `you`/`run`/`done` 三种值，且 `you` 排在最前

- [ ] **Step 4: 提交**

```bash
git add internal/api/discover.go internal/api/router.go
git commit -m "Tasks 做成订单的投影，该你动手的排最前"
```

---

## Task 11: 冒烟脚本补核验步骤

**Files:**
- Modify: `scripts/smoke.py`

**Interfaces:**
- Consumes: Task 5 的 `POST /orders/{id}/verify-receipt`、Task 4 的 `phase` 字段
- Produces: 无（终点任务）

- [ ] **Step 1: 读现有脚本的 OTC 段**

Run: `grep -n 'receipt\|s4\|s5\|OTC' scripts/smoke.py`
把上传回执之后直接断言终态的那一段找出来。

- [ ] **Step 2: 插入核验步骤**

上传回执之后、断言终态之前，插入：

1. 断言订单 `state == "s3v"`
2. 断言**上传方**看到的 `phase == "lock"`、`actor == "auto"`
3. 断言**对手方**看到的 `phase == "verify"`、`actor == "you"`
4. 以上传方身份调 `verify-receipt`，断言拿到 403 `NOT_YOUR_CALL`
5. 以对手方身份调 `verify-receipt` 传 `{"ok": true}`，断言进入 `s4`
6. 等到终态，断言 `terminal == "completed"`

第 4 步是这条链路的要害：放行的依据是回执被**对方**核过，
自己核自己等于回到「等对方点确认」，那正是 V1 要甩掉的东西。

- [ ] **Step 3: 跑冒烟**

Run: `make clean && make smoke`
Expected: 两条主流程都走到终态，脚本退出码 0

- [ ] **Step 4: 最终全量验证**

Run: `go build ./... && go test ./... && make smoke`
Expected: 全绿

- [ ] **Step 5: 提交**

```bash
git add scripts/smoke.py
git commit -m "冒烟脚本补核验步骤，含自己不能核自己的断言"
```

---

## 自查记录

**Spec 覆盖：** spec 七节各自对应 —— 第 1 节 → Task 4；第 2 节 → Task 1 + Task 5；
第 3 节（指定对手方与头像）→ Task 2 + Task 6；提现与收款方 → Task 2 + Task 7；
第 4 节 → Task 2 + Task 8 + Task 9；第 5 节 → Task 10；第 6 节 → Task 2 + Task 3；
第 7 节测试要求分散在各任务的测试步骤与 Task 11。无遗漏。

**已知偏离：** `s1` 的阶段映射由 `pay`/`wait` 改为 `lock`/`auto`，理由见开头，
Task 4 的测试固化。spec 需同步更新。

**类型一致性：** `order.Phase`/`order.Viewer`（Task 4 定义）在 Task 10 复用；
`store.ConfirmRow`（Task 3）与 `auth.ConfirmRow` 同形但分属两包，是有意为之，
避免 `auth` 反向依赖 `store`；`PhaseFor` 的三返回值签名在 Task 4 与 Task 10 一致。
