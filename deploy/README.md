# 部署

前后端同源，一台机器：nginx 发静态文件 + 反代 `/api`，后端只监听回环。

## 部署前必须先定的三件事

这套系统现在可以作为**受控演示**上线，但不能承接真实资金或公开注册。三处不是配置问题，是还没实现：

| | 现状 | 后果 |
|---|---|---|
| **鉴权** | `X-Atara-User` 头直接注入身份，零校验 | 任何人 `curl -H 'X-Atara-User: Demo'` 就是 Demo。不带头默认就是 demo 用户 |
| **Passkey** | 令牌的签发、绑定摘要、一次性、过期都是真的，**但没有 WebAuthn 验签** | 「动钱要签名」这道门形同虚设 |
| **托管合约** | 单签名方、阈值 1 | 那把私钥丢了，合约里的钱能被放走 |

这次上线选的是 mock 链（见「四、选链」），第三行因此不适用：没有合约、没有私钥，
也就不可能有真钱在里面。**前两行照旧成立**——鉴权和验签都还是假的，所以：

- nginx 里那道 Basic Auth **不要删**，它是唯一挡在外面的东西
- 或者用 IP 白名单 / Cloudflare Access 换掉它，但必须有一层
- 将来换真链时，第三行重新生效：不要往托管合约里放真钱

## 一、准备机器

```bash
adduser --system --group --home /srv/atara atara
mkdir -p /srv/atara/{bin,dist,data/uploads}
chown -R atara:atara /srv/atara
```

## 二、构建

代码在服务器上拉、在服务器上编。两个仓库分开，**分支不是默认的那个，务必确认**：

| 仓库 | 分支 | 说明 |
|---|---|---|
| `hetu-project/Atara-pay-sandhi` | `v1-alignment` | 后端。这就是它的默认分支，clone 下来即是 |
| `hetu-project/atara` | **`v1`** | 前端。默认分支 `main` **里没有 `app/` 目录**，整个控制台只在 `v1` 上 |

### 工具链

发行版自带的版本大概率太旧：`go.mod` 要 Go 1.25.6，vite 6 要 Node 18+。

```bash
# Go
curl -fsSL https://go.dev/dl/go1.25.6.linux-amd64.tar.gz -o /tmp/go.tgz
rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile.d/go.sh && . /etc/profile.d/go.sh

# Node 20
curl -fsSL https://deb.nodesource.com/setup_20.x | bash - && apt install -y nodejs

go version && node -v      # 应为 go1.25.6+ 与 v20+
```

### 编译

```bash
# 后端 → 静态二进制
cd ~/src/Atara-pay-sandhi && git pull
CGO_ENABLED=0 go build -o /srv/atara/bin/atara-pay ./cmd/atara-pay

# 前端 → 静态文件
cd ~/src/atara && git checkout v1 && git pull
cd app && npm ci && npm run build
rm -rf /srv/atara/dist/* && cp -r dist/. /srv/atara/dist/

chown -R atara:atara /srv/atara
```

`CGO_ENABLED=0` 能成立是因为 SQLite 驱动是纯 Go 的（modernc），产物是静态二进制，不依赖机器上的任何库。

前端不需要配 API 地址：默认走相对路径 `/api/v1`，同源由 nginx 转发。前后端**不同源**时才需要
`VITE_API_BASE=https://api.example.com/api/v1 npm run build`，那种部署要靠后端 CORS 放行，比反代麻烦也更容易配错。

> 小机器上 `npm ci` 和 Go 编译都可能吃满内存。1G 内存的机器建议先挂 2G swap，
> 否则会看到编译进程被 OOM killer 无声杀掉（`dmesg | tail` 才看得到原因）。

## 三、配置

这一版跑 mock 链，**没有 `.env`**——不连 RPC、不签真交易、不需要私钥，
所有配置都在 systemd 单元的 `Environment=` 里。

```bash
cp deploy/atara-pay.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now atara-pay

cp deploy/nginx.conf /etc/nginx/sites-available/atara
ln -s /etc/nginx/sites-available/atara /etc/nginx/sites-enabled/
htpasswd -c /etc/nginx/.htpasswd atara      # 设访问密码
nginx -t && systemctl reload nginx
```

证书用 certbot：`certbot --nginx -d atara.example.com`。

## 四、选链

这次上线选的是 **`mock`**（单元里 `ATARA_CHAIN_IMPL=mock` 已经写死）。

`ATARA_CHAIN_IMPL` 决定托管走哪条路：

- **`mock`** —— 没有真链，确认数按墙钟推算。业务链路和真链完全一样：挂单锁仓、
  入托管、放款、退款、超时回写，一步都不少，差别只在链上那一笔是模拟的。
  **不需要 RPC、不需要合约地址、不需要私钥**——整个 `.env` 连同私钥保管的问题一起消失
- **`evm`** —— 真链。服务器上要有可达的 RPC，还要 `.env`（0600，属主 atara）装
  `ATARA_SIGNER_KEY` 等。**anvil 不适合**：它进程一重启链上状态全没了，而 SQLite 里
  还记着那些仓位，两边立刻对不上。要么用 BSC 测试网，要么用带持久化的节点

mock 下地址全是 EVM 格式（`0x` + 20 字节），和只保留 MetaMask 的登录方式对得上；
`ExplorerURL` 返回空串——这条链不存在，给一个 etherscan 链接只会把人点到
「查无此地址」，比不给链接更让人怀疑是不是坏了。

换链或删库时两边要一起重来，否则会出现「平台说这单锁着钱，链上查无此仓位」：

```bash
systemctl stop atara-pay
rm -f /srv/atara/data/atara.db*
# 换成真链才需要重新部署合约并更新 .env 里的地址
systemctl start atara-pay
```

### 节奏

单元里是 `ATARA_DEMO_TIMING=true`，秒级推进。真实时长（`false`）下付款窗口 4 小时、
平台核验 2 小时——当着人演示时一笔单子根本走不完，屏幕上只会停在「等待中」。
要按真实时长跑再改回 `false`。

## 五、验证

```bash
# 后端活着（在服务器上直连；/healthz 挂在后端根路径，nginx 只反代 /api/，
# 从公网访问 /healthz 会被 SPA 回退接走返回 index.html，验不出任何东西）
curl -s 127.0.0.1:8080/healthz

# 反代通了：这是一条真的后端接口，返回 USDT / USDC 才算对
curl -su atara:PASS https://atara.example.com/api/v1/catalog/assets

# 前端能开
curl -sI https://atara.example.com/ | head -3

# 后端只监听回环
ss -lntp | grep 8080
```

最后一条最重要：如果 `:8080` 绑在 `0.0.0.0`，公网可以直连后端、绕过 nginx 的全部访问控制。

## 备份

要备份的是两样：

- `/srv/atara/data/` —— 数据库与上传的凭证
- `.env` —— 丢了签名私钥，托管合约里的钱就取不出来了

SQLite 用 WAL，热备份要用 `sqlite3 atara.db ".backup /path/out.db"`，直接 `cp` 会拿到不一致的快照。

## 什么时候该换掉 SQLite

现在是单文件 SQLite，单写者。演示和内部试用够用；一旦有并发下单，写会开始互相阻塞。
迁 Postgres 改的是 `internal/store/`，上面几层不动——SQL 是照 PG 的方言写的，
只在 SQLite 上换了类型映射（uuid/decimal/timestamp 一律 TEXT，enum 用 CHECK）。
