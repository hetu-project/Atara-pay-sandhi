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
也就不可能有真钱在里面。**前两行照旧成立**——鉴权和验签都还是假的。

当前这套部署**没有加任何访问控制**（`nginx-http-ip.conf` 里的 Basic Auth 已按需求关掉，
也没有 TLS）。也就是说：拿到 IP 和端口的任何人都能进、能以任何身份下单、能往
`/api/v1/uploads` 传文件。这是明知代价后的选择——里面的钱和身份都是假的。

由此而来的三条纪律：

- 不要往里放任何真实证件、凭证或客户资料
- 不要拿这个地址当任何真实业务的入口，也不要写进对外材料
- 将来换真链时，第三行重新生效：不要往托管合约里放真钱

要加回访问控制的话，`nginx-http-ip.conf` 里留了两种写法（密码 / 按来源 IP 放行），
都是解开两行注释的事。

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
# Go：机器上如果已经有 apt 装的 go，先卸掉。
# 不卸也能装，但 apt 的 go 在 /usr/bin/go，PATH 里排在 /usr/local/go/bin 前面，
# 装完 `go version` 还是老版本——这个坑很难自己看出来。
which go && dpkg -S "$(readlink -f "$(which go)")"   # 报出包名就是 apt 装的
apt remove -y golang-go golang-1.22-go && apt autoremove -y

curl -fsSL https://go.dev/dl/go1.25.6.linux-amd64.tar.gz -o /tmp/go.tgz
rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
# 前置而不是追加，才压得住残留的老 go
echo 'export PATH=/usr/local/go/bin:$PATH' > /etc/profile.d/go.sh && . /etc/profile.d/go.sh
hash -r

# Node 20
curl -fsSL https://deb.nodesource.com/setup_20.x | bash - && apt install -y nodejs

go version && node -v      # 应为 go1.25.6 与 v20+
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
journalctl -u atara-pay -n 20 --no-pager        # 确认起来了
```

nginx 有两份配置，按有没有域名选一份：

| 文件 | 用在什么时候 | 访问地址 |
|---|---|---|
| `nginx-http-ip.conf` | 裸 IP + 端口，没有 TLS | `http://<IP>:8090` |
| `nginx-domain.conf` | 有域名，配合 certbot | `https://你的域名` |

```bash
cp deploy/nginx-http-ip.conf /etc/nginx/sites-available/atara   # 或 nginx-domain.conf
ln -sf /etc/nginx/sites-available/atara /etc/nginx/sites-enabled/
nginx -t && systemctl reload nginx
```

访问控制已按需求关掉，不需要 `htpasswd`。

### 上 HTTPS：不只是安全问题

**Privy 的托管钱包只能在安全上下文里跑。** 明文 HTTP（localhost 除外）下
它会在初始化时抛 `Embedded wallet is only available over HTTPS`，异常从
PrivyProvider 里出来，整棵 React 树跟着挂——线上表现就是整屏全黑。

前端因此有一条按 `isSecureContext` 的降级分支：不开托管钱包，Twitter 登录
不显示，Google 登录的地址改由后端按邮箱派生。能用，但不等价。所以 HTTPS
不是锦上添花，是这个登录方案的前提。

**有域名**（域名先加一条 A 记录指向服务器 IP，`dig +short 你的域名` 能解析出来）：

```bash
# nginx-domain.conf 里的 server_name 改成你的域名，装好并 reload 之后
certbot --nginx -d atara.example.com
```

`nginx-domain.conf` 只监听 80，443 块、证书路径和跳转由 certbot 自己加进去。
不要手写 443：证书还没签出来时 `ssl_certificate` 指向的文件不存在，
`nginx -t` 会直接失败，而 certbot 又需要 nginx 活着才能做校验。

**没有域名**也行。Let's Encrypt 从 2026 年 1 月起支持 IP 地址证书：

```bash
snap install --classic certbot     # 发行版自带的版本会直接拒绝 IP 请求
certbot --nginx --ip-address 62.146.236.64 --preferred-profile shortlived
```

代价是证书**只有 6 天**有效期（IP 证书强制短期；certbot 的定时器一天跑两次，
够用，但别把它关了）。另外 **80 端口必须能从公网访问**，ACME 的 HTTP-01
校验走那里——防火墙记得放行 80 和 443。

### 换到 HTTPS 之后要记得两件事

- 去 Privy 后台把新地址加进 allowed origins。不加的话登录弹窗照弹，但登不
  进去，而且界面上没有任何提示，只有控制台一条 `frame-ancestors` 报错
- Google 登录拿到的地址会变（从「后端按邮箱派生」换成「Privy 托管钱包」），
  同一个 Google 账号会落到另一个账户上。演示数据无所谓，但别在 HTTP 和
  HTTPS 两种模式之间来回切着演示同一条链路

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
curl -su atara:PASS http://<服务器IP>:8090/api/v1/catalog/assets

# 前端能开
curl -sI -u atara:PASS http://<服务器IP>:8090/ | head -3

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
