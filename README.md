# atara-pay

Atara 控制台 `New order` 与 `Trade` 两页的后端。一期，Demo 用。

设计文档：`advaita-web/docs/superpowers/specs/2026-08-27-atara-pay-backend-design.md`

## 跑起来

```bash
go run ./cmd/atara-pay      # 或 make run
```

没有别的步骤：SQLite 单文件库自动建表并灌种子数据，不需要 Docker、不需要装 Postgres。
默认监听 `:8080`，库落在 `./atara.db`。

```bash
make test     # 领域层单测
make smoke    # 端到端跑一遍两条主流程到终态
make clean    # 删库重来
```

## 与设计文档的两处偏差

1. **SQLite 而非 PostgreSQL。** 本机没有 Docker 也没有 Postgres，而目标是「能起来看一下」。
   分层与 SQL 结构与 Postgres 版一致，换的只是方言：`uuid`/`decimal`/`timestamp` 一律 `TEXT`，
   enum 用 `CHECK`，`SELECT ... FOR UPDATE SKIP LOCKED` 换成单实例调度器。
   迁移是启动时执行的 `internal/store/schema.sql`，没有 goose；SQL 是手写的，没有 sqlc。
2. **金额底层是 `decimal` 而非 `int64` 最小单位。** 18 位精度下 1 ETH = 10¹⁸，
   `int64` 上限约 9.2×10¹⁸，最大只能表示 9.2 ETH——种子数据里就有 3.6 ETH。
   语义不变（精确十进制、绝不用 float），底层换成任意精度。

## 约定

- **鉴权是 mock**：`X-Atara-User: <handle>` 直接注入身份，不带就落到 `demo`。
  可用 handle：`demo`、`huachuang`、`kenji`、`aria`、`procurement`、`maker-p1` … `maker-p10`。
- **动钱必须带确认令牌**：`POST /passkey/assert` 换 `X-Atara-Confirmation`，
  120 秒、一次性、绑定操作摘要。真实 WebAuthn 验签没做，令牌本身的生命周期是真的。
- **法币不入账**：钱包只有数字资产。法币腿点对点走银行，平台只核验回执。
- **演示时长**：`ATARA_DEMO_TIMING=true`（默认）下状态机用秒级时长；
  设为 `false` 换成真实口径（30min / 4h / 2h / 14d）。

## 接口

| | |
|---|---|
| 目录 | `GET /catalog/{assets,fiats,conditions,intents}` |
| 账户 | `GET /wallet` · `GET /authorization-cards` · `GET /counterparties` · `POST /passkey/assert` · `POST /uploads` |
| New order | `POST /orders/parse` · `POST /orders/quote` · `POST /orders` · `GET /orders[/{id}]` · `GET /orders/{id}/events` · `POST /orders/{id}/{confirm,evidence,cancel,dispute}` |
| Trade | `GET /offers` · `POST /offers` · `DELETE /offers/{id}` · `GET /offers/mine` · `GET /offers/{id}/{dossier,assessment}` · `POST /offers/{id}/take` · `POST /orders/{id}/{accept,fund,receipt}` |
| Agent | `GET /orders/{id}/release-consensus` |

全部挂在 `/api/v1` 下。

### 两条主流程

```
New order   POST /orders/parse  →  POST /orders/quote  →  POST /passkey/assert  →  POST /orders
            locked → awaiting_counterparty → awaiting_me → releasing → released

Trade       GET /offers  →  POST /offers/{id}/take  →  POST /passkey/assert
            →  POST /orders/{id}/accept  →  POST /orders/{id}/receipt
            match → s1 → s3 → s4 → s5
```

系统步（对方注资、平台核验、日期到期、异议窗口静默）由 `internal/scheduler` 每秒扫
`state_deadline` 推进；人的步走上面的显式接口。前端轮询 `GET /orders/{id}` 即可。

### 四种终态

| 终态 | 触发 | 资金 | 履约回写 |
|---|---|---|---|
| `completed` | 条件成立且放行共识通过 | 给收款方 | 正向 |
| `cancelled` | 条件成立前主动撤，或吃单后未确认 | 原路退回 | 不回写 |
| `expired` | 承诺后到期未履约 | 原路退回 | **负向** |
| `disputed` | 窗口内提出异议 | **保持锁定** | 待裁决 |

`cancelled` 与 `expired` 刻意分开：没成交不是违约，把两者都记成超时会让履约率无故变差。

## 留桩的地方

`internal/agent` 定义了 `Parser` / `RiskAssessor` / `ReleaseConsensus` 三个接口，
一期由 `internal/agent/mockagent` 用确定性规则实现——同一句话每次解析成同一张单，
同一个对手方每次给出同一组票。返回结构与真实实现一致，后期接 LLM 只换实现，路由与 DTO 不动。

放行共识的出口只有 `release` 与 `hold_for_review` 两个，这个边界写在类型里：
它没有裁量权，不能改判条件，否则「条件成立即放款」的确定性就没了。

## 不在本期范围

注册 / KYC / KYB、入金、出金、收款账户管理、Contacts 增删改、History 页、会话消息、
二度关系、真实 Passkey 验签、真实 LLM。这些要么留桩，要么是种子数据。
