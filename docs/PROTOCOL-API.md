# Atara 结算协议 · 接口规范

**版本** v1 · **状态** Draft · **Base** `/api/v1` · **修订日** 2026-09-02

---

## 0. 这是什么

Atara 是一套**非托管的条件结算协议**。它不是一个代管用户资金的支付网关——协议本身不持有、不代收、不代发任何资金。

协议定义四件事：

| | |
|---|---|
| **托管的成立与解除** | 资金锁进链上托管合约，由状态机而非平台意志决定何时解除 |
| **结算条件的表达与判定** | 条件是可判定的原子集合，不是一句自然语言承诺 |
| **放行依据** | 放行由**可核验的凭证**触发，不由对手方的口头确认触发 |
| **支配权的授予** | 额度是签进链上的支配权，可撤销、有窗口、有单笔上限 |

三条不变量贯穿全部接口，理解它们比理解任何单个端点都重要：

**I1 · 平台不持有资金。** 账户即链上地址。余额从链上读，不从平台账本读。托管合约地址、交易哈希、确认数全部通过接口暴露给对接方自行复核。

**I2 · 法币不入账。** 协议只结算数字资产。法币腿点对点走银行，协议仅核验回执。任何接口都不会返回法币余额,也没有法币充值/提现端点。

**I3 · 一笔一工单，终态后只读。** 每次结算发起即建一条工单，走一条不可跳站的状态机，四种终态互斥（`completed` / `cancelled` / `expired` / `disputed`）。到达终态后工单只读。转移表是这两条规则的**唯一**执行点。

### 实现成熟度

对接方必须先读这一节，否则会对协议当前能力产生误判。

| 组件 | 状态 | 对接影响 |
|---|---|---|
| 状态机与转移表 | **完整实现** | 可依赖。不许跳站、终态只读由代码强制 |
| 结算确认令牌 | **完整实现** | 签发、摘要绑定、一次性消费、过期，全生命周期为真 |
| 账本与工单流水 | **完整实现** | 每次状态转移都有不可变事件记录 |
| 业务规则（额度、起投额、余量预留、归属校验） | **完整实现** | 可依赖 |
| **链** | **模拟层** | `internal/chain/mockchain`。锁仓、确认数、余额均真实记账于独立库，但**未接入任何真实网络**。`tx_hash` 不可在区块浏览器查证 |
| **身份鉴权** | **模拟层** | 请求头 `X-Atara-User: <handle>` 直接注入身份。**无会话、无签名验签**。生产接入需替换此层 |
| **Agent 共识** | **模拟层** | 放行共识与对手方评估由确定性 mock 产出 |
| Passkey 验签 | **未实现** | 令牌生命周期为真，签名验证本身待接入 WebAuthn |

一期为演示构建，SQLite 单文件库。分层与 SQL 结构与 PostgreSQL 版一致，方言差异见 `README.md`。

---

## 1. 传输约定

### 1.1 请求

所有请求 `Content-Type: application/json`。

| 头 | 必需 | 说明 |
|---|---|---|
| `X-Atara-User` | 是 | 身份注入。当前为模拟鉴权，取 handle 或地址。缺省落到 `demo` |
| `X-Atara-Confirmation` | 视操作 | 结算确认令牌。见 §3 |

### 1.2 成功响应

`200 OK`（新建额度为 `201 Created`，删除类返回 `{"status": "..."}`）。

### 1.3 错误响应

统一信封，HTTP 状态码 + 机器可读 `code`：

```json
{
  "error": {
    "code": "BELOW_MIN_LOT",
    "field": "amount",
    "message": "3000 CNY is below CrabWalk Trading's smallest lot",
    "remedy": {
      "action": "set_amount",
      "value": "5000",
      "label": "Use the smallest lot — 5000 CNY"
    }
  }
}
```

| 字段 | 说明 |
|---|---|
| `code` | 稳定标识，**对接方应据此分支**，不要匹配 `message` |
| `field` | 出错的请求字段。为空表示整体性错误 |
| `message` | 面向终端用户的英文文案，可能变化 |
| `remedy` | 可选。**前置拦截**机制：协议判定此请求后续必然失败时，同时给出一条可直接采用的替代值。`action` 是语义动作名，`value` / `values` 是建议值 |

`remedy` 是协议的一个设计取向：后续必然失败的请求在提交前拦下，并给出出路，而不是让对接方自行猜测边界。

### 1.4 金额表示

**所有金额一律为十进制字符串，主单位。**

```json
{ "amount": "3.600000000000000001", "asset": "ETH", "scale": 18 }
```

协议内部使用任意精度十进制，**从不使用浮点数,也不使用最小单位整数**。18 位精度下 1 ETH = 10^18，`int64` 上限约 9.2×10^18，最大仅能表示 9.2 ETH——种子数据里就有 3.6 ETH。对接方解析时必须使用十进制库，用 IEEE 754 浮点会静默改变尾数。

### 1.5 时间

RFC 3339,UTC。工单同时给出 `state_deadline`（绝对时刻）与 `seconds_left`（相对秒数），后者供 UI 倒计时，不要用它做业务判定。

---

## 2. 账户模型

### 2.1 地址即身份

协议没有独立的用户标识体系。**账户等于一个链上地址。**

```
POST /auth/connect
```

```json
{ "method": "passkey | wallet | google | email", "address": "...", "email": "...", "name": "..." }
```

四条接入路径最终都归到一个地址。地址已存在则直接返回该账户,不建重复账户——因此此端点同时承担注册与登录。

| `method` | `wallet_kind` | 含义 |
|---|---|---|
| `wallet` | `ext` | 对接方自带钱包。**协议从未持有其私钥**，故额度只能通过链上 approve 授予。`address` 必填 |
| `passkey` / `google` / `email` | `atara` | 由协议派生地址,私钥由 passkey 持有 |

`wallet_kind` 决定额度的签发方式,对接方需据此分支。

### 2.2 账户视图

```
GET /me      → 身份：id · address · wallet_kind · role · display_name · hue
GET /wallet  → 资金视图
```

`GET /wallet` 响应：

```json
{
  "address": "TDemo...",
  "wallet_kind": "atara",
  "custody": "self",
  "on_chain_usd": "34500.00",
  "in_escrow_usd": "5400.00",
  "total_usd": "39900.00",
  "assets": [
    { "asset": "USDT", "on_chain": "34500", "in_escrow": "5400",
      "usd_value": "39900.00", "networks": ["TRON", "ETH"] }
  ],
  "escrow_contract": { "address": "TEscrow...", "network": "TRON" },
  "spending_contract": "TSpend..."
}
```

`custody` 恒为 `"self"`。`on_chain` 读自链,`in_escrow` 是该账户当前锁在托管合约中的量。**`assets` 中永不出现法币**（不变量 I2）。合约地址暴露出来供对接方自行复核。

### 2.3 支配权（额度）

额度不是信用卡限额,是**签进链上的支配权**：可撤销、有周期窗口、有单笔上限、可限定收款方。

```
GET    /allowances
POST   /allowances            签发（201）
POST   /allowances/{id}       修改（200）
DELETE /allowances/{id}       撤销
```

```json
{
  "spender": "Ops agent",
  "kind": "agent | person",
  "per_payment": "300",
  "window_cap": "1200",
  "cycle": "weekly",
  "expires": "30 days | 90 days | \"\"",
  "recipients": "..."
}
```

签发与修改**均需签名档令牌**（§3）——授予支配权本身就是一次授权动作。单笔上限不得超过窗口总额，否则返回 `CAP_ABOVE_WINDOW`。

签发路径随 `wallet_kind` 分叉：`atara` 钱包写进账户合约策略,`ext` 钱包是对支出合约的 approve。

---

## 3. 结算确认：分级令牌

协议要求**每一笔资金流出都经过一次显式确认**,无金额豁免。确认以短时、一次性、绑定操作摘要的令牌形式表达。

```
POST /passkey/assert
```

```json
{ "scope": "withdraw", "parts": ["payee-id", "USDT", "250.5"], "grade": "signature" }
```

响应：

```json
{
  "confirmation": "a1b2c3...",
  "expires_at": "2026-09-02T05:44:05Z",
  "grade": "signature",
  "header": "X-Atara-Confirmation"
}
```

令牌随后通过 `X-Atara-Confirmation` 头递给目标端点。

### 3.1 令牌属性

| 属性 | 值 | 意义 |
|---|---|---|
| 有效期 | 120 秒 | 过期即失效 |
| 消费次数 | 一次 | 重放一笔已确认的结算不会再通过 |
| 绑定 | `scope` + `parts` 的摘要 | 换了金额或对手方,旧令牌不再认可 |
| 持久化 | 是 | 进程重启不影响未过期令牌 |

摘要绑定是关键：一枚为「向 A 支付 100」签发的令牌，无法用于「向 B 支付 100」或「向 A 支付 1000」。摘要不匹配时令牌**同时被作废**——它本就不该用于这次操作,留着只会给重放留口子。

### 3.2 两个档位

| 档位 | 语义 | 适用操作 |
|---|---|---|
| `signature` | **动钱**。签的是那笔链上转账本身 | 挂卖单（锁币）· 卖方向入金 · 提现 · 签发额度 |
| `commit` | **仅承诺**,不动钱 | 挂买单 · 买方向接单 · 声明「已完成法币转账」 |

承诺档**不能**冒充签名档；反向可以（签名强于承诺）。以承诺档令牌调用要求签名档的端点，返回 `SIGNATURE_REQUIRED`。

这个分级是刻意的：若两者等同，要么签名成了摆设，要么接一笔单也要摸指纹。

### 3.3 相关错误码

| `code` | HTTP | 含义 |
|---|---|---|
| `CONFIRMATION_REQUIRED` | 401 | 未携带令牌 |
| `CONFIRMATION_INVALID` | 401 | 已过期 / 已使用 / 属于他人 / 摘要不匹配 |
| `SIGNATURE_REQUIRED` | 401 | 该操作需签名档,递来的是承诺档 |

---

## 4. 流动性：挂单

挂单是**做市方对协议作出的可执行承诺**,不是一条广告。

### 4.1 承诺的成本按方向不对称

这是非托管模型下最重要的一处设计,对接方必须理解：

| 方向 | 挂出时发生什么 |
|---|---|
| `side: "sell"`（做市方卖币） | 校验链上确有该数量 → **上链把币锁进托管合约** → 才入库。需 `signature` 档令牌 |
| `side: "buy"`（做市方买币） | **不锁定任何资产**。法币腿走银行,协议不代收法币,故此为纯承诺。需 `commit` 档令牌 |

卖单**挂出即锁币**——对接方在池子里看到的可成交量是真的在托管合约里,不是一个数字。锁仓凭证以 `lock_tx` 暴露。

链上动作先于入库执行,因为链动作不可回滚：先成功上链,再记账。

### 4.2 端点

```
GET    /offers?side=&asset=&fiat=      浏览池子（side 缺省为 buy）
GET    /offers/mine                     自己的挂单
POST   /offers                          挂单
GET    /offers/{id}
DELETE /offers/{id}                     下架
GET    /offers/{id}/dossier             做市方资质件明细
GET    /offers/{id}/assessment          多 agent 评估（模拟层）
POST   /offers/{id}/take                吃单 → 建工单
```

`POST /offers` 请求：

```json
{
  "side": "sell",
  "asset": "USDT",
  "fiat": "CNY",
  "unit_price": "7.31",
  "qty": "108015",
  "min_lot": "5000",
  "network": "TRON",
  "networks": ["TRON", "ETH"]
}
```

**单位约定（易错点）：** `qty` / `remaining_qty` 为**币**单位；`min_lot` / `fiat_ceiling` 为**法币**单位。二者不可混比。

挂单响应含做市方信誉：`trust_score` · `deals` · `disputes` · `fill_rate` · `median_release_secs` · `docs`。**资质件缺项照常公开**——协议不隐藏缺口,由对接方自行为缺口定价。

### 4.3 做市准入

```
GET  /maker/application                 读申请状态
POST /maker/application                 提交（分两段）
```

两段准入,各自独立置位：

```
提交身份材料 → kyc_done → 【审核】→ kyc_ok → 可提挂单配置
提交挂单配置 → listing_done → 【审核】→ approved
```

身份未审通过而提交挂单配置,返回 `KYC_NOT_APPROVED`——跳段等于让未核验身份者直接做市。

审核为**真人动作,不经 agent 共识**,挂在角色门后：

```
GET  /admin/maker/applications                        待审列表【需 reviewer 角色】
POST /admin/maker/applications/{user_id}/review       裁决【需 reviewer 角色】
```

```json
{ "stage": "kyc | listing", "decision": "approve | reject", "reason": "..." }
```

拒绝必须给出 `reason`（否则 `REASON_REQUIRED`）。重新提交时旧理由被清空。

> **当前实现缺口：** `POST /offers` 尚未校验 `approved`。准入流程完整,但挂单端点上的闸门未接。对接方不应依赖协议侧的做市准入强制。

---

## 5. 结算工单

### 5.1 工单类型

| `kind` | 含义 | v1 状态 |
|---|---|---|
| `otc_take` | OTC 成交：数字资产 ⇄ 法币,点对点 | **v1 主流程** |
| `conditional_transfer` | 条件支付：自定义放行规则 | 实现完整,**v1 不启用** |

以下均描述 `otc_take`。条件支付的端点保留可用但 v1 契约不覆盖,一并列出以免对接方误以为它们不存在：
`POST /orders/parse`（自然语言解析成条件原子）· `POST /orders`（建条件工单）·
`POST /orders/{id}/evidence`（对手方提交交付凭证）· `POST /orders/{id}/confirm`（放行确认）。
条件支付的状态机是另一条链路（`fund → locked → awaiting_counterparty → awaiting_me → releasing → released`）,
V2 启用时另行补充规范。

### 5.2 状态机

```
match ──→ s1 ──→ s3 ──→ s3v ──→ s4 ──→ s5 (completed)
  │        │       │       │
  │        │       │       └─ 核验不通过 ──→ disputed  （资金保持锁定）
  │        │       └─ 超时 ──→ expired
  │        └─ 撤单 ──→ cancelled
  └─ 撤单 / 预留过期 ──→ cancelled
```

| 状态 | 含义 |
|---|---|
| `match` | 已撮合,软预留可成交量,等吃单方确认。**尚未动钱** |
| `s1` | 数字资产进入托管。买方向为验证既有锁仓（瞬时）;卖方向为等待链上确认数 |
| `s3` | 法币腿。买币的一方点对点转账 |
| `s3v` | 回执已提交,**待收款方核验** |
| `s4` | 回执已核验,放款中 |
| `s5` | 终态 `completed` |

**终态语义（影响履约率回写）：**

| 终态 | 资金处置 | 履约率 |
|---|---|---|
| `completed` | 放给收款方 | 正向回写 |
| `cancelled` | 原路退回 | **不回写**——未成交不是违约 |
| `expired` | 原路退回 | 负向回写 |
| `disputed` | **保持锁定**,待裁决 | 不回写 |

`match` 站超时归 `cancelled` 而非 `expired`,是刻意的：两者都记 expired 会让做市方履约率无故变差。

### 5.3 放行规则

> **协议的核心取向：放行由可核验的凭证触发,不由对手方的口头确认触发。**

法币腿完成后,付款方提交回执 → 工单进入 `s3v` → **收款方核验回执** → 放行。

约束：

- 核验只能由**收取法币的一方**执行
- **回执提交者不得核验自己提交的回执**（返回 `NOT_YOUR_CALL`）
- 核验通过写入 `verified_at`,与状态转移同一事务
- 核验不通过转 `disputed`,资金保持锁定

若允许自核,协议就退化成「等对方点确认」——那正是它要取代的东西。

### 5.4 生命周期端点

```
POST /offers/{id}/take                  吃单 → 工单（无需令牌）
POST /orders/{id}/accept                承诺点 → s1
POST /orders/{id}/fund                  入金（仅当仍欠入金）
POST /orders/{id}/receipt               提交法币回执 → s3v
POST /orders/{id}/verify-receipt        核验回执 → s4 或 disputed
POST /orders/{id}/cancel                撤单（match / s1 / s3 可撤）
POST /orders/{id}/dispute               提出异议
```

**吃单** `POST /offers/{id}/take`：

```json
{ "amount": "73100", "amount_kind": "coin | fiat", "network": "TRON", "card_id": "..." }
```

吃单只建工单,**不动钱,故不需要令牌**。但事务内会预留可成交量——并发吃同一挂单时这是唯一守门人,抢不到返回 `ABOVE_AVAILABLE_QTY`。方向在此确定：做市方卖 → 吃单方买,反之亦然。吃自己的单返回 `SELF_TRADE`。

`card_id` 为额度标识（历史命名）。

**承诺** `POST /orders/{id}/accept`：

```json
{ "via": "wallet | external" }
```

令牌档位按方向分叉：

| 情形 | 档位 | 原因 |
|---|---|---|
| 吃单方买币 | `commit` | 对方的币早已锁定,自己无需出资 |
| 吃单方卖币 + `wallet` | `signature` | 需签署真实的链上转账 |
| 吃单方卖币 + `external` | `commit` | 协议无该钱包私钥,只能等待扫链 |

卖方向在此步同时校验额度并发起入金。

**核验** `POST /orders/{id}/verify-receipt`：

```json
{ "ok": true, "reason": "" }
```

`ok: false` 时 `reason` 落入工单事件流水。

### 5.5 工单视图

```
GET /orders?kind=&state=&terminal=&open=true
GET /orders/{id}
GET /orders/{id}/events              状态流水
GET /orders/{id}/escrow              链上事实
GET /orders/{id}/release-consensus   放行共识过程
GET /tasks                           待办投影
```

工单响应（节选）：

```json
{
  "id": "...", "ref": "ATR-8F42C1", "kind": "otc_take",
  "state": "s3v", "terminal": "",
  "phase": "lock", "actor": "auto",
  "amount": { "amount": "10000", "asset": "USDT", "scale": 6 },
  "counterparty_id": "mk-p1", "counterparty_name": "CrabWalk Trading",
  "state_deadline": "2026-09-02T05:44:11Z", "seconds_left": 6,
  "otc": { "offer_id": "p1", "side": "buy", "unit_price": "7.31",
           "fiat_code": "CNY", "fiat_amount": "73100",
           "network": "TRON", "receipt_ref": "..." },
  "escrow": { "contract": "TEscrow...", "network": "TRON",
              "explorer": "https://...", "funding_via": "wallet",
              "tx_hash": "0x...", "confirmations": 12, "required": 12,
              "needs_funding": false },
  "rail": [ { "key": "match", "label": "Matched", "state": "done" }, "..." ],
  "created_at": "..."
}
```

#### `phase` / `actor`：按观察者计算

**同一张工单,两方看到不同的 `phase`。** 协议按当前请求者的身份计算这两个字段,对接方直接渲染即可,无需自行重建状态机。

| `phase` | `actor` | 含义 |
|---|---|---|
| `pay` | `you` | 该你转出法币 |
| `wait` | `them` | 等对方转出法币 |
| `verify` | `you` | 对方回执已到,该你核验 |
| `lock` | `auto` | 资产进入托管中,无人需动手 |
| `rel` | `auto` | 已核验,放款中 |

终态工单、条件支付工单、以及非交易双方发起的查询,`phase` 与 `actor` 均为 `null`。

`escrow` 一节是**链的事实,不是协议的账**：合约地址、交易哈希、确认数、区块浏览器链接全部给出,供对接方独立复核。

`GET /tasks` 是工单的派生投影（不建表）：`state` 为 `you` / `run` / `done`,`you` 排最前。

### 5.6 停留时长

`ATARA_DEMO_TIMING` 切换两套口径,默认演示口径。

| 窗口 | 演示 | 真实 | 到点行为 |
|---|---|---|---|
| `match` 软预留 | 20 秒 | 10 分钟 | → `cancelled` |
| `s1` 买方向绑单 | 2 秒 | 5 秒 | 验证锁仓并绑定 |
| `s1` 卖方向入金 | 10 秒 | 30 分钟 | 等确认数 |
| `s3` 本方转账窗口 | 24 秒 | 4 小时 | → `expired` |
| `s3` 等对方转账 | 10 秒 | 90 分钟 | 对方提交回执 |
| `s3v` 核验窗口 | 6 秒 | 2 小时 | → `expired` |
| `s4` 放款 | 4 秒 | 2 小时 | → `s5` |
| 异议窗口 | 15 秒 | 72 小时 | 静默即放行 |

---

## 6. 撮合

```
POST /orders/match
GET  /orders/eligible-counterparties?side=&asset=&fiat=&amount=&amount_kind=
POST /orders/quote
```

`POST /orders/match`：

```json
{ "intent": "buy | sell", "amount": "1000", "amount_kind": "coin",
  "asset": "USDT", "fiat": "CNY", "counterparty_id": "" }
```

**先撮合,后评估。** 顺序是刻意的：先评估是逻辑倒置——对手方尚未出现,评的是谁?返回信誉最优的三个候选,排序即默认选择。

`counterparty_id` 非空时仅在该做市方的挂单内撮合。撮不到返回 `NO_MATCH_WITH_COUNTERPARTY`,**不静默回退到全池**——指定了对手方却成交给别人,是最坏的结果。

`GET /orders/eligible-counterparties` 返回真正能承接该笔的做市方。五条判定：方向相反、挂单活跃、余量足（币单位）、起投额不超（法币单位）、资产与法币匹配,外加排除自己。金额按各挂单单价逐条换算,不混单位。

---

## 7. 提现

协议**不代持、不代发**。链上转账由账户持有者自行签署,协议记录意图、合规材料与待回填的交易哈希。

```
GET    /payees          地址簿
POST   /payees
DELETE /payees/{id}
GET    /withdrawals
POST   /withdrawals
```

```json
{ "label": "Ops wallet", "chain": "TRON", "address": "TXm..." }
```

按 `(owner, chain, address)` 唯一——同一串字符在另一条链上是另一个账户,故 `chain` 必填。重复返回 `PAYEE_EXISTS`。

```json
{ "payee_id": "...", "asset": "USDT", "amount": "250.5",
  "purpose": "OTC settlement", "doc_upload_id": "..." }
```

需 `signature` 档令牌。`purpose` 必填——收款银行会追问。**仅支持数字资产**（不变量 I2）,提交法币返回 `ASSET_REQUIRED`。

`state` 取值：`draft` → `submitted` → `broadcast` → `confirmed` / `failed`。

---

## 8. 辅助端点

```
GET  /healthz                        存活探针（无鉴权）
GET  /catalog/assets                 数字资产目录：code · name · symbol · scale · networks · usd_rate
GET  /catalog/fiats                  法币目录,按走廊分组
GET  /catalog/conditions             可用的结算条件类型
GET  /catalog/intents                意图类型
GET  /discover/markets               协议纵向：otc（live）· api（Coming）· shop（Coming）
GET  /contacts                       对手方名录
POST /contacts                       单字段收名字或地址
GET  /threads                        对手方会话索引
GET  /threads/{peer}                 单个对手方：消息 + 工单同流
POST /threads/{peer}/messages
POST /uploads                        上传凭证 → file_ref
GET  /uploads/*
```

> 目录数据当前为编译期常量,含 `usd_rate`。汇率变更需重新部署。

---

## 9. 错误码索引

### 确认与授权

| `code` | HTTP | 含义 |
|---|---|---|
| `CONFIRMATION_REQUIRED` | 401 | 未携带结算确认令牌 |
| `CONFIRMATION_INVALID` | 401 | 令牌过期 / 已用 / 属他人 / 摘要不匹配 |
| `SIGNATURE_REQUIRED` | 401 | 需签名档,递来承诺档 |
| `UNKNOWN_ACTOR` | 401 | 身份不存在 |
| `ROLE_REQUIRED` | 403 | 缺少所需角色 |
| `NOT_YOURS` | 403 | 该对象属于其他账户 |
| `NOT_YOUR_CALL` | 403 | 回执提交者不得自核 |

### 挂单与撮合

| `code` | HTTP | 含义 |
|---|---|---|
| `INVALID_SIDE` / `UNKNOWN_ASSET` / `UNKNOWN_FIAT` | 400/422 | 参数非法 |
| `INVALID_PRICE` / `INVALID_AMOUNT` / `INVALID_MIN_LOT` | 422 | 数值非法 |
| `OFFER_CLOSED` | 409 | 挂单已不开放 |
| `SELF_TRADE` | 422 | 不得吃自己的单 |
| `BELOW_MIN_LOT` | 422 | 低于起投额（带 `remedy`） |
| `ABOVE_AVAILABLE_QTY` | 422/409 | 超出可成交量,或并发被抢（带 `remedy`） |
| `NETWORK_UNSUPPORTED` | 422 | 该做市方不在此网络结算（带 `remedy`） |
| `NO_COUNTERPARTY` | 422 | 该方向无活跃挂单 |
| `NO_FIAT_CORRIDOR` | 422 | 该法币走廊无可用流动性 |
| `NO_MATCH_WITH_COUNTERPARTY` | 422 | 指定对手方无法承接此笔 |
| `INSUFFICIENT_BALANCE` | 422 | 链上余额不足（带 `remedy`：改用外部钱包出资） |

### 工单

| `code` | HTTP | 含义 |
|---|---|---|
| `INVALID_TRANSITION` | 409 | 转移表不允许 |
| `ORDER_TERMINAL` | 409 | 工单已终态,只读 |
| `NOTHING_TO_FUND` | 409 | 该工单未在等待入金 |
| `RECEIPT_REQUIRED` | 422 | 未附回执 |
| `NO_RECEIPT` | 422 | 尚无回执可核验 |
| `CHAIN_REJECTED` | 422 | 链上动作被拒（余额、锁仓状态等） |
| `TOO_MANY_CONDITIONS` | 422 | 条件原子数超限（条件支付） |

### 额度与做市准入

| `code` | HTTP | 含义 |
|---|---|---|
| `CAP_ABOVE_WINDOW` | 422 | 单笔上限超过窗口总额 |
| `OVER_CAP` | 422 | 单笔超过额度上限 |
| `OVER_QUOTA` | 422 | 超过周期窗口余量 |
| `ALLOWANCE_EXPIRED` | 422 | 额度已过期 |
| `ALLOWANCE_REVOKED` | 422 | 额度已撤销 |
| `ALLOWANCE_FOREIGN` | 422 | 该额度不属于本账户 |
| `KYC_NOT_APPROVED` | 409 | 身份未审通过,不得提交挂单配置 |
| `BAD_PHASE` / `FORM_REQUIRED` | 422 | 申请参数非法 |
| `REASON_REQUIRED` | 422 | 拒绝未给理由 |
| `BAD_REVIEW` | 422 | 审核参数非法或申请不存在 |

### 提现

| `code` | HTTP | 含义 |
|---|---|---|
| `PAYEE_EXISTS` | 409 | 该链上此地址已在地址簿 |
| `PAYEE_REQUIRED` | 422 | 收款方缺失或不属于本账户 |
| `ADDRESS_REQUIRED` / `CHAIN_REQUIRED` | 422 | 地址簿参数缺失 |
| `ASSET_REQUIRED` | 422 | 资产缺失,或非数字资产 |
| `AMOUNT_REQUIRED` / `AMOUNT_INVALID` | 422 | 金额非法或非正 |
| `PURPOSE_REQUIRED` | 422 | 未声明用途 |

### 通用

| `code` | HTTP |
|---|---|
| `BAD_REQUEST` | 400 |
| `BAD_AMOUNT` | 422 |
| `NOT_FOUND` | 404 |
| `INTERNAL` | 500 |

### 名录与会话

| `code` | HTTP | 含义 |
|---|---|---|
| `UNKNOWN_METHOD` | 400 | `auth/connect` 的 `method` 非法 |
| `NO_SUCH_ACCOUNT` | 422 | 按名字或地址找不到该账户 |
| `SELF_CONTACT` | 422 | 不能把自己加为对手方 |
| `CONTACT_REQUIRED` | 422 | 未指定对手方 |
| `EMPTY_MESSAGE` | 422 | 消息体为空 |

---

## 10. 一次完整结算

以吃单方买入数字资产为例。

```bash
BASE=http://localhost:8080/api/v1
ME='-H Content-Type:application/json -H X-Atara-User:demo'
```

```bash
# 1 · 接入
curl -sX POST $BASE/auth/connect -d '{"method":"passkey","name":"Acme Desk"}'

# 2 · 浏览池子
curl -s "$BASE/offers?side=sell&asset=USDT&fiat=CNY"

# 3 · 撮合
curl -sX POST $BASE/orders/match \
  -d '{"intent":"buy","amount":"10000","amount_kind":"coin","asset":"USDT","fiat":"CNY"}'

# 4 · 吃单 → 工单，state=match（无需令牌）
curl -sX POST $BASE/offers/p1/take \
  -d '{"amount":"73100","amount_kind":"fiat","network":"TRON"}'

# 5 · 换承诺档令牌
curl -sX POST $BASE/passkey/assert \
  -d '{"scope":"accept","parts":["<order-id>"],"grade":"commit"}'

# 6 · 承诺 → state=s1，随后自动进 s3
curl -sX POST $BASE/orders/<order-id>/accept \
  -H "X-Atara-Confirmation: <token>" -d '{}'

# 7 · 线下完成法币转账，提交回执 → state=s3v
curl -sX POST $BASE/orders/<order-id>/receipt -d '{"file_ref":"receipt.pdf"}'

# 8 · 轮询：此刻自己 phase=lock/auto，对手方 phase=verify/you
curl -s $BASE/orders/<order-id>

# 9 · 对手方核验 → s4 → s5，terminal=completed
#     （以收取法币一方的身份调用；提交者自核将得到 NOT_YOUR_CALL）
```

`make smoke` 覆盖买卖双向的完整链路,可作为对接参考实现。

---

## 11. 对接注意事项

**必须做**

- 金额一律以十进制字符串解析,使用任意精度库
- 错误分支基于 `code`,不匹配 `message`
- 直接使用 `phase` / `actor` 渲染,不自行重建状态机
- 每次动钱操作前重新签发令牌（120 秒、一次性、绑定摘要）
- 独立复核 `escrow` 中的链上事实,不完全依赖协议自述

**不要做**

- 不要缓存令牌复用
- 不要混用币单位与法币单位（`qty` 是币,`min_lot` 是法币）
- 不要假设 `wait` 与 `pay` 是同一状态的两种文案——它们是同一条法币腿在两个视角下的投影
- 不要依赖协议侧的做市准入强制（见 §4.3 缺口）
- 不要在生产环境依赖当前的身份鉴权层与链层（均为模拟,见 §0）

**变更策略**

`code` 与 `phase` / `actor` / `state` 的取值集合视为契约的一部分,不做破坏性变更。`message` 文案可能变化。

---

## 附:与 v1 前端的契约来源

本规范的字段与取值,部分反推自控制台前端 `console.html` 中的写死数据结构——该前端当前尚未接入后端（`fetch` 调用数为 0）。首次联调时出现字段级出入属预期,以本文档与代码为准。

其中一项尚未验证：**付款方提交回执后应观察到的 `phase`**。前端演示数据未覆盖该状态,当前映射为 `lock`。
