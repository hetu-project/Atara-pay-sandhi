# 部署

前后端同源，一台机器：nginx 发静态文件 + 反代 `/api`，后端只监听回环。

## 部署前必须先定的三件事

这套系统现在可以作为**受控演示**上线，但不能承接真实资金或公开注册。三处不是配置问题，是还没实现：

| | 现状 | 后果 |
|---|---|---|
| **鉴权** | `X-Atara-User` 头直接注入身份，零校验 | 任何人 `curl -H 'X-Atara-User: Demo'` 就是 Demo。不带头默认就是 demo 用户 |
| **Passkey** | 令牌的签发、绑定摘要、一次性、过期都是真的，**但没有 WebAuthn 验签** | 「动钱要签名」这道门形同虚设 |
| **托管合约** | 单签名方、阈值 1 | 那把私钥丢了，合约里的钱能被放走 |

在这三件事解决之前：

- nginx 里那道 Basic Auth **不要删**，它是唯一挡在外面的东西
- 或者用 IP 白名单 / Cloudflare Access 换掉它，但必须有一层
- 不要往托管合约里放真钱

## 一、准备机器

```bash
adduser --system --group --home /srv/atara atara
mkdir -p /srv/atara/{bin,dist,data/uploads}
chown -R atara:atara /srv/atara
```

## 二、构建

在开发机上构建，把产物拷过去——服务器上不装 Go 和 Node。

```bash
# 后端（交叉编译到 Linux）
cd atara-pay
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/atara-pay ./cmd/atara-pay
scp bin/atara-pay  server:/srv/atara/bin/

# 前端
cd advaita-web/app
npm ci && npm run build
scp -r dist/*      server:/srv/atara/dist/
```

`CGO_ENABLED=0` 能成立是因为 SQLite 驱动是纯 Go 的（modernc），产物是静态二进制，不依赖服务器上的任何库。

前端不需要配 API 地址：默认走相对路径 `/api/v1`，同源由 nginx 转发。前后端**不同源**时才需要
`VITE_API_BASE=https://api.example.com/api/v1 npm run build`，那种部署要靠后端 CORS 放行，比反代麻烦也更容易配错。

## 三、配置

```bash
# .env 里有链的签名私钥
install -m 0600 -o atara -g atara .env /srv/atara/.env

cp deploy/atara-pay.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now atara-pay

cp deploy/nginx.conf /etc/nginx/sites-available/atara
ln -s /etc/nginx/sites-available/atara /etc/nginx/sites-enabled/
htpasswd -c /etc/nginx/.htpasswd atara      # 设访问密码
nginx -t && systemctl reload nginx
```

证书用 certbot：`certbot --nginx -d atara.example.com`。

## 四、选链

`.env` 里的 `ATARA_CHAIN_IMPL` 决定托管走哪条路：

- **`mock`** —— 没有真链，确认数按墙钟推算。演示业务流程用这个最省事，也没有私钥风险
- **`evm`** —— 真链。服务器上要有可达的 RPC。**anvil 不适合**：它进程一重启链上状态全没了，而 SQLite 里还记着那些仓位，两边立刻对不上。要么用 BSC 测试网，要么用带持久化的节点

换链或删库时两边要一起重来，否则会出现「平台说这单锁着钱，链上查无此仓位」：

```bash
systemctl stop atara-pay
rm -f /srv/atara/data/atara.db*
# 真链还要重新部署合约并更新 .env 里的地址
systemctl start atara-pay
```

## 五、验证

```bash
curl -u atara:PASS https://atara.example.com/api/v1/../healthz   # 后端活着
curl -sI https://atara.example.com/ | head -3                     # 前端能开
ss -lntp | grep 8080                                             # 应当只有 127.0.0.1
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
