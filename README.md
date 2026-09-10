# atara-pay

```bash
make run        # 起服务，保留现有数据
make fresh      # 删掉全部历史数据再起——每次从零开始
```

`make fresh` 会先停掉占着端口的旧进程，再删库和上传文件，然后启动。
顺序不能反：SQLite 那个进程还开着文件句柄的时候，删掉的只是目录项，
它照样读写同一个 inode——不重启的话，界面上看到的还是老数据。

库和上传目录的路径在 Makefile 顶上写死一份（`DB` / `UPLOADS`），
`run` 和 `clean` 共用。要换位置就在命令行覆盖：

```bash
make fresh DB=./atara.db ADDR=:9000
```

```bash
make test   
make smoke 
make clean      # 只删不起
```

## 接真链

默认跑 mock 链：合约动作记在本地 SQLite 里，不发交易、不需要钱包。
要让钱真的进托管合约，得先把合约部到一条链上。

```bash
cp .env.example .env                 # 填 RPC、私钥、已有的代币地址
make chain-deploy                    # 演练：只检查，一笔交易都不发
make chain-deploy A="send write"     # 真的部署，并把地址写回 .env
make fresh-chain                     # 删数据 + 连真链起
```

用 Hardhat + viem（`contracts/` 里自带 package.json，第一次会自动 npm install），
**不需要 foundry**。合约的 Foundry 测试还留着，装了 forge 的人照跑。

**默认是演练**：这个命令会往链上发不可撤销的交易、花掉真的 gas，默认发交易
意味着敲错一次就多一份没人管的合约。演练会把该问的都问清楚：节点通不通、
chainId 是几、部署者余额够不够、给的代币地址上到底有没有合约、精度是多少。
**地址填错照样能部署成功，等到挂单锁币那一刻才炸，那时错误信息只会说
call reverted。**

`ATARA_TOKEN_USDT` / `ATARA_TOKEN_USDC` 填了就用现成的，留空就新部一个测试币。

**`run` / `fresh` 不读 `.env`，`run-chain` / `fresh-chain` 才读。** 这是故意的：
前两个是演示用的 mock 链，读了 `.env` 就会去连真链，RPC 一不通后端直接起不来，
而人往往只是想开个演示。

合约地址存在库里（`chain_deployments` + `chain_tokens`），由
`GET /api/v1/catalog/chain` 发下去：

```json
{ "impl": "evm", "network": "BSC-TESTNET", "chain_id": 97,
  "rpc_url": "…", "explorer": "…",
  "escrow": "0x…", "spending": "0x…",
  "tokens": { "USDT": { "address": "0x…", "decimals": 18 } } }
```

前端据此知道该在哪条网络上、approve 给谁、锁进哪个合约。写死在前端的话，
合约换一次地址，前端就会把钱 approve 给一个旧合约，而且要等到锁币那一刻
才发现。mock 下这些地址一律为空——那条链上没有合约可调，前端据此知道
这一版不发交易。

调度器接了真链是**每分钟**扫一次到期工单（也就是每分钟去链上确认一次状态），
mock 是每秒。真链上每秒扫毫无意义：一次状态推进本身就要等区块，而且每次
扫描都是若干次 RPC 往返，公共节点会限流。要改用 `ATARA_SCHED_TICK=30s`。

**库里那一行是后端启动时按自己实际在用的配置写进去的**，所以「前端看到的
地址」与「后端真正在说话的合约」是同一份，不会漂。地址来源的优先级：

- 环境变量给了 → 用它，并写回库（刚部署完就是这条路）
- 环境变量空着 → 从库里读上次那条记录（`.env` 丢了也照样起得来）

真链上**不灌种子余额**：那些余额是「凭空发币」，链上没有这回事。币不是
我们发的就一路 revert，是我们发的就等于每次重启白铸一遍，而且每笔都要等
一次 RPC 往返——实测接 BSC 测试网时光灌种子就三分钟还没走完。所以种子只
建账户与挂单，演示做市方的挂单会显示可成交量 0。

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
