# 按 V1 前端对齐后端接口

日期：2026-09-02
状态：待评审
对齐目标：`advaita-web` 分支 `v1`，末位提交 `b801bdf`（2026-09-01 17:48）

## 背景

后端上一次对齐是 `bfd9016`（08-31 15:40），对的是前端 `8aec622`（08-31 15:09）。
此后前端多了三个提交，全在 09-01：

| 提交 | 标题 | 后端影响 |
|---|---|---|
| `fff4710` | Point the console at the rest of the product | 无。导航加两个外链，纯前端 |
| `0092919` | Cut the first version down to OTC, and let the keys leave | 大 |
| `b801bdf` | Stop offering what this version does not do | 中 |

一个前提必须写在前面：`console.html` 里 `fetch` 调用数为 0。前端至今是静态原型，
数据写死在页面里。因此本设计的接口契约是**从 UI 的写死数据结构反推的**，
没有任何真实调用能验证它对不对。第一次前后端联调时出现字段级出入是预期内的。

## 范围

做：

1. 订单增加按观察者视角计算的 `phase` / `actor`
2. OTC 放行链路补人工核验步骤
3. 下单可指定对手方（撮合侧过滤 + 候选对手方端点）
4. 提现流程与收款方入库
5. Discover 纵向目录 + Maker 申请（含真实审核入口）
6. Tasks 投影端点
7. passkey 确认令牌从内存搬进数据库

不做（已与需求方确认）：

- **不删 Conditional。** `internal/domain/condition`、`order_conditional`、
  `order_conditions`、`/orders/parse`、`CreateConditional` 全部原样保留，
  只是 V1 前端不再调用。V2 恢复条件支付时直接接回来。
- **目录不入库。** 资产、法币、网络继续硬编码在 `internal/money/catalog.go`。
  这一条与「除 agent 共识外都用数据库」有出入，是需求方的明确选择。
  副作用：汇率仍是编译期常量，改汇率要改代码重新发布。
- **不做助记词备份状态。** `users.wallet_kind`（`atara` | `ext`）已存在，
  「自带钱包 vs Atara 生成」后端已能区分；缺的只是「已抄写助记词」这一位。
  前端那颗勾选框的状态本次不落库，刷新即丢。

## 迁移方式

`store.Open` 启动时整份执行 `schema.sql`，全部是 `create table if not exists`，
仓库里零 `alter table`，没有版本化迁移。

因此本次新增字段直接写进建表语句，**已有的 `atara.db` 必须 `make clean` 重建**。
需求方已确认可接受。不引入版本化迁移。

## 1. 订单视角阶段

### 前端要什么

`console.html` 的 `OSTATE` 定义了 OTC 五个阶段，每个阶段绑定一个行动方：

```
pay    : ['you',  'Send the transfer']
verify : ['you',  'Verify their receipt']
wait   : ['them', 'Waiting on their transfer']
lock   : ['auto', 'Locking into escrow']
rel    : ['auto', 'Releasing to them']
```

这是**按观察者视角**的：同一张单，付法币的一方看到 `pay`，另一方同时看到 `wait`。

### 后端现状

状态机是中性的：`match → s1 → s3 → s4 → s5`，不区分观察者。

### 设计

订单 JSON 增两个只读派生字段，由后端按当前调用者身份计算：

```json
{
  "phase": "pay | verify | wait | lock | rel",
  "actor": "you | them | auto"
}
```

派生输入：`orders.state`、`order_otc.side`、调用者是 owner 还是 counterparty、
`fiat_receipts` 中双方回执的存在与 `verified_at`。

OTC 只有一条法币腿：买币的一方付法币，卖币的一方收法币并核验回执。
五个阶段是这条腿在两个视角下的投影。

派生规则（OTC）：

| state | 我的角色 | 条件 | phase | actor |
|---|---|---|---|---|
| `s1` | 双方 | 币还在往托管里注资 | `lock` | `auto` |
| `s3` | 法币付方 | 回执未上传 | `pay` | `you` |
| `s3` | 法币收方 | 对方回执未到 | `wait` | `them` |
| `s3v` | 法币付方 | 回执已上传，等对方核验 | `lock` | `auto` |
| `s3v` | 法币收方 | 对方回执已到，`verified_at` 为空 | `verify` | `you` |
| `s4` | 双方 | 回执已核过，正在放款 | `rel` | `auto` |

`s5` 不出现在表里：状态机里 `s4 → s5` 这条边总是同时把订单置为终态，
所以任何处于 `s5` 的订单在判断阶段之前就已经被终态短路，走不到这张表——
放款这一段能被外部观察到的状态就是 `s4`。

**`s1` 为什么不是 `pay`。** 写实施计划时发现的：taker 卖币时 `s1` 是 taker
往合约注资的阶段，**币还没锁进托管**。此时让法币付方（maker）看到 `pay`，
等于在托管成立之前就催他打钱。所以 `s1` 归入 `lock`（正在锁仓，无人需动手），
`pay` / `wait` 从 `s3` 才开始。前端 `lock` 的文案 "Locking into escrow" 正好吻合。

终态（`s5` / `cancelled` / `expired` / `disputed`）不产出 phase，
`phase` 为 `null`，前端按 `terminal` 字段渲染。

Conditional 订单不产出 `phase`（V1 前端不展示它们），字段为 `null`。

### 代价

接口带上了展示层语义。前端改文案不需要动后端（文案在前端），
但前端若改变阶段划分，后端必须跟着改。这是需求方在两个选项中选定的。

## 2. OTC 人工核验

### 前端要什么

`0092919` 的提交信息："Release never waits on the other side's word —
that is the point of verifying the receipt rather than asking them."

放行的依据是**核验对方的银行回执**，不是等对方点确认。
而 `verify` 在 `OSTATE` 里 actor 是 `you` —— 它是一个人工动作。

### 后端现状

`otcEdges` 中：

```
{OTCTake, S3, EvReceipt, both, S4, TermNone}   // 谁付法币谁传回执
{OTCTake, S4, EvTick,    sys,  S5, TermCompleted}
```

回执一上传就进 `s4`，随后定时器自动完成。**没有核验这一步。**

### 设计

在 `s3` 与 `s4` 之间插入等待核验的中间态 `s3v`：

```
{OTCTake, S3,  EvReceipt, both, S3V, TermNone}   // 回执上传，待对方核验
{OTCTake, S3V, EvVerify,  both, S4,  TermNone}   // 核验通过 → 进入锁仓
{OTCTake, S3V, EvDispute, both, Disputed, TermDisputed}  // 核验不通过
{OTCTake, S4,  EvTick,    sys,  S5,  TermCompleted}
```

新增事件 `EvVerify`。新增端点：

```
POST /api/v1/orders/{id}/verify-receipt
  body: {"receipt_id": "...", "ok": true, "reason": ""}
  权限：只有回执的对手方能核验，上传者不能自己核验
  ok=false → 转 Disputed，reason 落 order_events.payload
```

核验成功时写 `fiat_receipts.verified_at`（字段已存在）。

`s3v` 需要 `state_deadline`：核验超时的处理沿用现有 `EvTick` 语义，
超时转 `Expired`。具体时长走 `ATARA_DEMO_TIMING` 的两套口径。

### 影响面

`internal/domain/order/machine.go` 与 `machine_test.go` 必须改。
这是本次唯一动状态机的改动，风险最高，实施时先写测试。

## 3. 指定对手方

### 前端要什么

Buy / Sell 面板可以选「跟谁交易」，默认 `Any`。
选定后，列表只显示「真能吃下这单」的对手方，带头像。
在某个对手方的会话线程里下单时，对手方固定为他，不可改。

### 后端现状

- `orders.counterparty_id` 列**已存在**。OTC 下单时由 `app/offers.go:203`
  从被吃的挂单的 maker 填入。
- `offers` 表**没有**对手方限定列 —— 挂单对所有人开放。

所以「指定对手方」不是给挂单加限制，而是**收窄我匹配哪些挂单**。
不需要新增列。

### 设计

新增候选对手方端点：

```
GET /api/v1/orders/eligible-counterparties
    ?side=buy|sell &asset=USDT &fiat=CNY &amount=3000
  →  [{"user_id","display_name","hue","avatar_url","peer_code",
       "trust_score","deals","best_price","available_qty"}]
```

「真能吃下这单」的判定：

1. 该用户有 `status='active'` 且方向相反的挂单
2. `remaining_qty >= amount` 换算后的数量
3. `min_lot <= amount`
4. `fiat_code` 与 `asset_code` 匹配
5. 排除自己

现有 `/orders/match` 与 `/orders/quote` 增加可选参数 `counterparty_id`，
传了就只在该用户的挂单里撮合，撮不到返回明确错误
（`NO_MATCH_WITH_COUNTERPARTY`），不静默回退到 Any。

### 头像

前端消息与对手方列表要 `hue` 与头像。`users` 表当前没有这两个字段。
新增：

```sql
users.hue        integer not null default 0    -- 0 表示按 id 哈希取色
users.avatar_url text    not null default ''
```

`hue` 为 0 时由前端按 id 派生，与前端现有 `PAV_HUES` 逻辑一致。

## 4. Discover 与 Maker 申请

### Discover

前端 `DISCOVER` 是三个纵向，只有 `otc` 上线：

```
otc  → OTC pool（live）
api  → Compute & APIs（Coming）
shop → Merchants（Coming）
```

内容接近静态。设计为只读端点，数据来自 Go 常量（与目录同样不入库，保持一致）：

```
GET /api/v1/discover/markets
  → [{"key","name","live","desc","map":[["Counterparty","..."],...]}]
```

### Maker 申请

Discover 上那颗按钮的三种文案由真实状态驱动：

```
approved            → "Create a listing →"
done && !approved   → "Under review…"
otherwise           → "Become a maker →"
```

前端 `SELLER` 有四个状态位与两份表单：

- `kycDone` 身份已提交 / `kycOk` 身份已审过
- `done` 挂单配置已提交 / `approved` 配置已审过
- 九步 KYC 表单（个人与公司两条路径）
- 挂单配置（方向、币种、区间、网络、通道、定价、点差）

现有 `merchant_profiles` 只有信誉字段（`trust_score`、`deals`、`disputes`、
`fill_rate`、`median_release_secs`、`docs`），装不下申请流程。

新表：

```sql
create table if not exists maker_applications (
  user_id      text primary key references users(id),
  phase        text not null default 'kyc' check (phase in ('kyc','listing')),
  kyc_done     integer not null default 0,
  kyc_ok       integer not null default 0,
  listing_done integer not null default 0,
  approved     integer not null default 0,
  form_json    text not null default '{}',
  reject_reason text not null default '',
  submitted_at text,
  reviewed_at  text,
  reviewer_id  text references users(id),
  updated_at   text not null
);
```

表单整体存 JSON blob：九步字段过于零碎且前端仍在改，逐列建模的维护成本
远高于收益。后端只校验必填项存在，不校验业务语义。

端点：

```
GET  /api/v1/maker/application            读自己的申请
POST /api/v1/maker/application            提交/更新（幂等，按 phase 分段）
```

### 审核入口

需求方明确：**maker 审核不算 agent 共识**，因此不能用 mock 自动放行，
必须有真实审核动作。

```
GET  /api/v1/admin/maker/applications?state=pending    列出待审
POST /api/v1/admin/maker/applications/{user_id}/review
     body: {"stage":"kyc|listing", "decision":"approve|reject", "reason":""}
```

权限：`users` 增列

```sql
users.role text not null default 'user' check (role in ('user','reviewer'))
```

`auth.Middleware` 之后加一层 `requireRole("reviewer")`。
种子数据里给一个 `reviewer` handle，便于演示时切换身份审核。

审核动作按 `stage` 分别置位：`kyc` 审过置 `kyc_ok` 并把 `phase` 推到 `listing`；
`listing` 审过置 `approved`。拒绝时写 `reject_reason`，对应位清零。
每次审核在 `order_events` 之外单独记录不必要，`reviewed_at` + `reviewer_id` 足够。

## 5. Tasks

前端 `const TASKS=[]` 是空数组，由 `taskUpsert()` 在开单时写入、
随 `advance()` 更新。注释写明「每笔交易开单即入列」。

它是订单的派生视图，不是独立实体。**不建表**，只加投影端点：

```
GET /api/v1/tasks
  → [{"id","order_ref","title","state":"run|you|done","at"}]
```

`state` 由第 1 节的 `actor` 推出：

| actor / 终态 | task state |
|---|---|
| `you` | `you` |
| `them` / `auto` | `run` |
| 订单已终态 | `done` |

排序：`you` 在前，其次 `run`，最后 `done`；同组按 `updated_at` 倒序。
与前端 `TKST` 的三个键一致。

## 6. 确认令牌入库

`internal/auth/auth.go:82` 的 `Confirmations` 用 `sync.Mutex` 守一个
`map[string]token`，进程重启即丢。这是全仓唯一的内存态。
（`mockchain` 有互斥锁但落自己的 SQLite；`mockagent` 是纯函数，保持 mock。）

新表：

```sql
create table if not exists confirmations (
  token       text primary key,
  user_id     text not null references users(id),
  digest      text not null,
  grade       text not null,
  expires_at  text not null,
  consumed_at text
);
```

一次性消费用条件 UPDATE 保证原子，不靠读后写：

```sql
update confirmations set consumed_at = ?
 where token = ? and user_id = ? and digest = ?
   and consumed_at is null and expires_at > ?
```

`RowsAffected() == 0` 即失败，错误码沿用现有的区分（过期 / 已用 / 摘要不匹配）
需要额外一次查询来判定具体原因，仅用于错误信息，不影响判定结果。

过期行的清理挂在现有 `internal/scheduler` 上，与其他定时任务同一个循环。

`Confirmations` 的方法签名要加 `ctx` 与 `*sql.Tx`，调用点在
`app/orders.go`、`app/offers.go` 共 4 处，随事务边界走。

## 7. 测试

| 改动 | 测试 |
|---|---|
| 状态机新增 `s3v` 与 `EvVerify` | `machine_test.go` 补迁移用例，含核验拒绝转 Disputed、核验超时转 Expired |
| `phase` / `actor` 派生 | 表驱动单测，覆盖买卖两侧 × 五个阶段 × owner/counterparty 两种视角 |
| 候选对手方筛选 | store 层用例，覆盖五条判定规则各自的排除路径 |
| 提现与收款方 | store 层往返测试 |
| Maker 申请与审核 | 两段审核的状态位流转，含拒绝后重新提交 |
| 确认令牌入库 | 并发消费同一 token 只能成功一次 |
| 全流程 | `scripts/smoke.py` 补一条 OTC 完整链路，含核验步骤，走到终态 |

## 待验证的假设

以下几条是从写死数据反推的，联调时最可能出错：

1. `phase` 五个取值与 `actor` 三个取值的字符串字面量，取自前端 `OSTATE` 的键名
2. **买币方上传回执之后看到什么。** 前端五个种子单没有覆盖这个状态：
   `wait` 的文案是「Waiting on their transfer」，说的是等对方打法币，
   不适用于「我已打款、等对方核验」。本设计暂时映射为 `lock`，
   理由是这一步之后就是锁仓。这是纯猜测，联调时优先确认。
   若前端需要区分，要么加第六个阶段，要么放宽 `wait` 的文案。
3. 候选对手方的「真能吃下这单」判定规则，前端只说了结果没说规则
4. Maker 九步 KYC 的字段名，本设计不逐个建模，靠 JSON blob 回避了这个风险
5. Discover 三个纵向的 `map` 数组结构，直接照抄前端常量
