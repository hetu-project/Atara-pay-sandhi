# atara-pay

```bash
go run ./cmd/atara-pay      # 或 make run
```

```bash
make test   
make smoke 
make clean
```

- **钱直接进托管合约**，从不经过 Atara。放款与退回都是合约动作。
- **法币不入账**：法币点对点走银行，平台只核验回执。


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
