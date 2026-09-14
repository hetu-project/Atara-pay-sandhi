package config

import (
	"os"
	"strconv"
	"time"
)

// Timings 是状态机各站的停留时长。
// demo 用短值，真实口径写在注释里——两套值出自 console.html:4978。
type Timings struct {
	OTCMatch time.Duration // 吃单后的软预留窗口（真实 10m）
	OTCBind  time.Duration // 买方向查挂单锁仓并绑单——瞬时，不是一段等待
	OTCS1    time.Duration // 对手方注资托管（真实 30m）
	// OTCS3 是你去银行把法币打出去的窗口。这是整条链路上唯一要人离开屏幕
	// 去做一件事的地方，所以演示口径也不能压到几十秒——压了就只能演示「超时」。
	OTCS3       time.Duration // 你的法币转账窗口（真实 4h）
	OTCTheirPay time.Duration // 卖方向：等对方打法币。到点是对方付款，不是你逾期
	// OTCVerify 是对方核验你回执的窗口。演示口径下它比别的站长得多（90s 而非几秒）：
	// 核验是这条链路上唯一必须由人做的动作，控制台要能切到对手方身份点它。
	// 到点没人核时调度器会代种子商家核（见 tickOTC 的 S3V 分支），那是无人值守的兜底。
	OTCVerify  time.Duration // 对方核验你的回执（真实 2h）
	OTCS4      time.Duration // 平台核验回执（真实 2h）
	Dispute    time.Duration // 凭证档的异议窗口（真实 72h）
	Fallback   time.Duration // 超时兜底转人工（真实 14d）
	CondSettle time.Duration // 条件支付里对手方交付的模拟时长
	// MakerReview 是提交准入材料后到自动放行之间的间隔。
	// 0 表示不自动放行——真实环境这一步是人在看件，钟不该替他点。
	MakerReview time.Duration
}

func demoTimings() Timings {
	return Timings{
		OTCMatch: 20 * time.Second, OTCBind: 2 * time.Second, OTCS1: 10 * time.Second,
		OTCS3: 10 * time.Minute, OTCTheirPay: 10 * time.Second,
		OTCVerify: 90 * time.Second,
		OTCS4:     4 * time.Second, Dispute: 15 * time.Second, Fallback: 60 * time.Second,
		CondSettle: 5 * time.Second, MakerReview: 10 * time.Second,
	}
}

func realTimings() Timings {
	return Timings{
		OTCMatch: 10 * time.Minute, OTCBind: 5 * time.Second, OTCS1: 30 * time.Minute, OTCS3: 4 * time.Hour, OTCTheirPay: 90 * time.Minute,
		OTCVerify: 2 * time.Hour,
		OTCS4:     2 * time.Hour, Dispute: 72 * time.Hour, Fallback: 14 * 24 * time.Hour,
		CondSettle: 30 * time.Minute, MakerReview: 0,
	}
}

type Config struct {
	Addr        string
	DBPath      string
	AgentImpl   string
	DemoTiming  bool
	UploadDir   string
	CORSOrigins string
	T           Timings

	// Seed 说要不要灌那套演示数据（10 家做市方、10 条挂单、联系人、
	// 余额、额度）。默认不灌：拿真实流程做验收时，写死评分和成交记录的
	// 假商家混在自己挂的单里根本分不清哪个是真的。
	Seed bool

	// SchedTick 是调度器多久扫一次到期的工单。
	//
	// 接了真链时这就是「多久去链上确认一次状态」。每秒扫一遍在真链上是浪费：
	// 一次状态推进本身就要等区块（十几秒起），而且每次扫描都是若干次 RPC
	// 往返，公共节点会限流。mock 链没有网络往返，链就是本地那张表，
	// 所以那边保持每秒——演示要的是当场看到状态走完。
	SchedTick time.Duration

	// ChainImpl 选链实现：mock | evm。
	// evm 需要下面这一组配得完整，缺一个就在启动时炸——
	// 配错链参数比不配更危险，不能让它悄悄退回 mock 继续跑。
	ChainImpl string
	Chain     ChainConfig

	// Voice 是语音听写的接入参数。没配就只是这一个功能不可用，
	// 不影响别的——所以不在启动时炸，由接口自己报 VOICE_NOT_CONFIGURED。
	Voice VoiceConfig

	// KYC 是 ID Analyzer / DocuPass 的接入参数。同样不在启动时炸：
	// 没配就只是身份核验这一件事不可用。
	KYC KYCConfig

	Desk DeskConfig
}

type DeskConfig struct {
	APIKey  string
	BaseURL string
	Model   string
	// MaxTokens 限的是单次回答的长度，不是次数。没有它一次跑飞的回答
	// 能一直吐到超时，界面上是一屏停不下来的字。
	MaxTokens int
}

func (d DeskConfig) Configured() bool { return d.APIKey != "" }

// KYCConfig 是 ID Analyzer（DocuPass）的接入参数。
//
// APIKey 绝不下发到浏览器——ID Analyzer 自己的文档把这条写成硬规矩：
// 应用只应持有短效的 reference，绝不可直接调 POST /docupass 或
// GET /docupass/{reference}。拿到这把 key 就能替我们建会话、读任何一次
// 核验的完整人像和证件号。
type KYCConfig struct {
	APIKey string
	// Profile 是门户里建的 KYC 配置档 ID。也可以直接用内置预设
	// security_high / security_medium / security_low / security_none——
	// 不用先去门户建档就能跑通，代价是没有 webhook（webhook URL 配在档上）。
	Profile string
	// Region: us | eu。个人信息只存放在提交去的那一侧，不跨区同步。
	Region string
	// WebhookSecret 是门户里 API Keys → Webhook Signing Secret 那一条。
	// 没配就拒收所有回调——「没配置」和「验过了」是两件事，
	// 混成一件等于这个接口谁都能打。
	WebhookSecret string
	// Mode 决定让用户做什么：0 证件+人脸，1 只证件，2 只人脸，3 只签署。
	// 默认 0：OTC 的法币腿要认人，只读证件不做人脸等于没认。
	Mode int
	// Language 是托管页面的界面语言。空表示让 DocuPass 按浏览器语言自己挑。
	//
	// 默认 en：不定的话，一台中文系统打开的是中文界面，而这个控制台整体是
	// 英文的——同一个弹窗里外两种语言。语言该跟着产品走，不跟着操作系统走。
	//
	// 取值是 DocuPass 自己那套键，不是 BCP-47：英文 en，简中 cn，繁中 tw，
	// 日文 ja……（完整列表见 v.idanalyzer.com/asset/language.json）。
	// 写成 zh-CN 是认不出来的，那会悄悄退回按浏览器猜。
	Language string
}

// Configured 说这套参数齐不齐。没有 key 就什么都做不了；
// Profile 可以是内置预设，所以也必须有一个值。
func (k KYCConfig) Configured() bool { return k.APIKey != "" && k.Profile != "" }

// VoiceConfig 是科大讯飞实时语音听写（IAT）的密钥。
//
// APIKey / APISecret 绝不下发到浏览器：前端只拿后端签好的、带时效的 WSS URL。
// 密钥一旦进了 JS 产物就是公开文件，任何人都能拿去刷我们的讯飞额度。
type VoiceConfig struct {
	AppID     string
	APIKey    string
	APISecret string
}

// Configured 说这套密钥齐不齐。三个缺一个都签不出 URL。
func (v VoiceConfig) Configured() bool {
	return v.AppID != "" && v.APIKey != "" && v.APISecret != ""
}

// ChainConfig 是真实链的接入参数。
type ChainConfig struct {
	RPCURL string
	Escrow string
	// Spending 是支配权策略合约。空表示额度不上链。
	Spending string
	// SignerKey 是 Demo 里唯一的私钥。绝不写进代码或提交进仓库——
	// 只从环境变量读，本地开发放 .env（已在 .gitignore 里）。
	SignerKey    string
	USDT         string
	USDC         string
	Network      string
	ExplorerBase string
}

func Load() Config {
	// 先把 .env 读进环境，再逐个取。命令行上显式给的值不会被文件盖掉。
	loadDotenv()
	c := Config{
		Addr:        env("ATARA_HTTP_ADDR", ":8080"),
		DBPath:      env("ATARA_DB_PATH", "./atara.db"),
		AgentImpl:   env("ATARA_AGENT_IMPL", "mock"),
		DemoTiming:  envBool("ATARA_DEMO_TIMING", true),
		UploadDir:   env("ATARA_UPLOAD_DIR", "./var/uploads"),
		CORSOrigins: env("ATARA_CORS_ORIGINS", "*"),
		ChainImpl:   env("ATARA_CHAIN_IMPL", "mock"),
		Seed:        envBool("ATARA_SEED", false),
		SchedTick:   envDur("ATARA_SCHED_TICK", 0),
		Chain: ChainConfig{
			RPCURL:       env("ATARA_RPC_URL", "http://127.0.0.1:8545"),
			Escrow:       env("ATARA_ESCROW_ADDR", ""),
			Spending:     env("ATARA_SPENDING_ADDR", ""),
			SignerKey:    env("ATARA_SIGNER_KEY", ""),
			USDT:         env("ATARA_TOKEN_USDT", ""),
			USDC:         env("ATARA_TOKEN_USDC", ""),
			Network:      env("ATARA_NETWORK", "BSC-TESTNET"),
			ExplorerBase: env("ATARA_EXPLORER", "https://testnet.bscscan.com"),
		},
		Voice: VoiceConfig{
			AppID:     env("IFLYTEK_APPID", ""),
			APIKey:    env("IFLYTEK_API_KEY", ""),
			APISecret: env("IFLYTEK_API_SECRET", ""),
		},
		KYC: KYCConfig{
			APIKey:        env("IDANALYZER_API_KEY", ""),
			Profile:       env("IDANALYZER_PROFILE", "security_medium"),
			Region:        env("IDANALYZER_REGION", "us"),
			WebhookSecret: env("IDANALYZER_WEBHOOK_SECRET", ""),
			Mode:          envInt("IDANALYZER_MODE", 0),
			Language:      env("IDANALYZER_LANGUAGE", "en"),
		},
		Desk: DeskConfig{
			APIKey:    env("DEEPSEEK_API_KEY", ""),
			BaseURL:   env("DEEPSEEK_BASE_URL", "https://api.deepseek.com"),
			Model:     env("DEEPSEEK_MODEL", "deepseek-chat"),
			MaxTokens: envPosInt("DEEPSEEK_MAX_TOKENS", 800),
		},
	}
	if c.DemoTiming {
		c.T = demoTimings()
	} else {
		c.T = realTimings()
	}
	if c.SchedTick <= 0 {
		c.SchedTick = time.Second
		if c.ChainImpl != "mock" {
			c.SchedTick = time.Minute
		}
	}
	return c
}

// envDur 读一个时长，如 30s / 2m。解析不了就当没配——
// 配错了悄悄用默认值比启动失败好：这个值只影响快慢，不影响对错。
func envDur(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// NodeID 是这个实例在发号器里的编号（0–1023）。
//
// 多实例部署时每个实例必须给不同的号：两个实例用同一个号会发出重复的工单号，
// 而 orders.ref 上有 unique 约束——表现是下单在随机时刻失败。
func NodeID() int {
	v := os.Getenv("ATARA_NODE_ID")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 1023 {
		return 0
	}
	return n
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// envInt 读一个整数。任何整数都收，包括 0 和负数。
//
// 给「0 本身就是一个合法取值」的字段用——比如 IDANALYZER_MODE，
// 0 是证件+人脸，还正好是默认值。这种字段不能拿 0 当「没配」。
func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// envPosInt 读一个正整数。0、负数、解析不了，一律当没配。
//
// 给「0 没有意义」的字段用——比如 DEEPSEEK_MAX_TOKENS：配成 0 会被原样传给
// 上游，每次回答都截成空，而界面上只会看到 AI 一句话不说。这种字段宁可
// 退回默认值，也不能把一个说不通的数发出去。
//
// 和 envInt 分开是有意的：这两条规则对同一个函数是矛盾的，合成一个就必然
// 有一边是错的。名字里写清楚，调用处才不会挑错。
func envPosInt(k string, def int) int {
	if n := envInt(k, def); n > 0 {
		return n
	}
	return def
}
