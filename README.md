# atara-pay

Atara 控制台 `New order` 与 `Trade` 两页的后端。**非托管**模型，Demo 用。

## 跑起来

```bash
go run ./cmd/atara-pay      # 或 make run
```

没有别的步骤：SQLite 单文件库自动建表灌种子数据，不需要 Docker、不需要装 Postgres。
默认 `:8080`，库落在 `./atara.db`。

```bash
make test     # 领域层单测
make smoke    # 端到端跑两条主流程 + 非托管的每一处分叉
make clean    # 删库重来
```

## 最重要的一件事：平台不持有资金

这不是一句口号，是代码结构上的约束：

- **本项目没有 `wallets` 表。** 余额与托管仓位属于链，由 `chain_*` 表持有，
  只能隔着 `chain.Chain` 接口读。平台自己记一笔余额，就等于又变回托管了。
- **钱直接进托管合约**，从不经过 Atara。放款与退回都是合约动作。
- **法币不入账**：法币腿点对点走银行，平台只核验回执。
- **地址就是账户**。邮箱只是通知渠道。

由此带来一个初看像 bug 的现象：从**外部钱包**入金的订单，付款方在本平台记录的
链上余额不会减少——因为钱本来就在别处，我们只看到它到了合约。这是对的。

## 两处外部依赖，两个可插拔 Mock

| | 接口 | 一期实现 | 接真时 |
|---|---|---|---|
| 链 | `internal/chain` | `mockchain`：确认数按墙钟推算，自带 `chain_*` 账本 | 换成 loka-chain |
| AI | `internal/agent` | `mockagent`：确定性规则，同一句话每次解析成同一张单 | 换成模型 |

两者返回结构与真实实现一致，换实现不改路由与 DTO。**其余全部是真实实现。**

## 两条主流程

```
条件支付   POST /orders/parse → /orders/quote → /passkey/assert → POST /orders
           → POST /orders/{id}/fund {via: wallet|external}
           fund ──入金确认数走满──→ locked → awaiting_counterparty
                → awaiting_me → releasing → released

OTC       GET /offers → POST /orders/match（先撮合，给 3 个候选）
          → POST /offers/{id}/take → POST /orders/{id}/accept
          match → s1 → s3 → s4 → s5
```

**OTC 的 s1 按方向分叉**，这是非托管下最关键的不对称：

| taker 方向 | s1 是什么 | 快慢 |
|---|---|---|
| 买币 | 验证对方**挂单时就锁好**的仓位，绑到这笔订单 | 秒级，没有新的资金动作 |
| 卖币 | 自己的币从钱包出去进合约 | 要等 6 个确认 |

## 确认分级

前端把这件事说得很清楚，后端不能把两者当成一回事：

| 档位 | 什么时候 | 前端长什么样 |
|---|---|---|
| `signature` | 动钱：入金、卖单挂出、签发额度、卖方向接单 | Passkey 签名 |
| `commit` | 只承诺不动钱：建单、买方向接单、「我已经打款了」 | 普通按钮 |

`POST /passkey/assert` 带 `grade` 换令牌。签名档满足承诺档的要求，反过来不行。

## 接口

| | |
|---|---|
| 目录 | `GET /catalog/{assets,fiats,conditions,intents}` |
| 账户 | `GET /me` · `POST /auth/connect` · `GET /wallet` · `POST /passkey/assert` · `POST /uploads` |
| 额度 | `GET /allowances` · `POST /allowances` · `POST /allowances/{id}` · `DELETE /allowances/{id}` |
| 联系人 | `GET /contacts` · `POST /contacts`（一个字段收名字或地址） |
| 线程 | `GET /threads` · `GET /threads/{peer}` · `POST /threads/{peer}/messages` |
| New order | `POST /orders/{parse,quote,match}` · `POST /orders` · `GET /orders[/{id}]` · `GET /orders/{id}/{events,escrow,release-consensus}` · `POST /orders/{id}/{fund,confirm,evidence,cancel,dispute}` |
| Trade | `GET /offers` · `POST /offers` · `DELETE /offers/{id}` · `GET /offers/mine` · `GET /offers/{id}/{dossier,assessment}` · `POST /offers/{id}/take` · `POST /orders/{id}/{accept,receipt}` |

全部挂在 `/api/v1` 下。`GET /orders/{id}/escrow` 给前端画那个链上观察窗：
合约地址、确认数、tx、浏览器链接。

## 四种终态

| 终态 | 触发 | 资金 | 履约回写 |
|---|---|---|---|
| `completed` | 条件成立且放行共识通过 | 合约放款给收款方 | 正向 |
| `cancelled` | 条件成立前主动撤，或吃单后未确认 | 合约原路退回 | 不回写 |
| `expired` | 承诺后到期未履约 | 合约原路退回 | **负向** |
| `disputed` | 窗口内提出异议 | **留在合约里**等裁决 | 待裁决 |

`cancelled` 与 `expired` 刻意分开：没成交不是违约，都记成超时会让履约率无故变差。

## 约定

- **鉴权是 mock**：`X-Atara-User` 传地址或展示名，不带就落到 demo 账户。
  可用：`Demo`、`Huachuang`、`Kenji M.`、`Aria Studio`、`CrabWalk Trading`、`Lotus Capital` …
- **登录**：`POST /auth/connect` 四种方式（passkey / wallet / google / email），
  落点都是一个地址。连外部钱包的账户 `wallet_kind=ext`，额度走对支出合约的 approve；
  Atara 钱包写进账户合约策略。
- **演示时长**：`ATARA_DEMO_TIMING=true`（默认）状态机走秒级；
  `false` 换真实口径（30min / 4h / 2h / 14d）。

## 与设计文档的偏差

原 spec 定的是 PostgreSQL + sqlc + goose 与托管账本。实际：

1. **SQLite（modernc，纯 Go）取代 Postgres** —— 本机没有 Docker 也没有 PG，目标是 `go run` 直接起。
   分层与 SQL 结构不变，只换方言；`schema.sql` 启动时执行。
2. **金额底层用 `decimal` 而非 int64 最小单位** —— 18 位精度下 int64 最多表示 9.2 ETH，
   种子数据里就有 3.6 ETH。
3. **托管账本整体删除** —— 前端在 `Make the account non-custodial` 之后改成非托管，
   `wallets` 表与 `ledger` 包随之下线，换成 `chain.Chain` 接口 + 链上观察日志。

## 不在本期范围

KYC / KYB、法币收款账户管理、History 页、二度关系、真实 WebAuthn 验签、真实 LLM、真实链。
