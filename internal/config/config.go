package config

import (
	"os"
	"strconv"
	"time"
)

// Timings 是状态机各站的停留时长。
// demo 用短值，真实口径写在注释里——两套值出自 console.html:4978。
type Timings struct {
	OTCMatch    time.Duration // 吃单后的软预留窗口（真实 10m）
	OTCBind     time.Duration // 买方向查挂单锁仓并绑单——瞬时，不是一段等待
	OTCS1       time.Duration // 对手方注资托管（真实 30m）
	OTCS3       time.Duration // 你的法币转账窗口（真实 4h）
	OTCTheirPay time.Duration // 卖方向：等对方打法币。到点是对方付款，不是你逾期
	// OTCVerify 是对方核验你回执的窗口。演示口径下它比别的站长得多（90s 而非几秒）：
	// 核验是这条链路上唯一必须由人做的动作，控制台要能切到对手方身份点它。
	// 到点没人核时调度器会代种子商家核（见 tickOTC 的 S3V 分支），那是无人值守的兜底。
	OTCVerify   time.Duration // 对方核验你的回执（真实 2h）
	OTCS4       time.Duration // 平台核验回执（真实 2h）
	Dispute     time.Duration // 凭证档的异议窗口（真实 72h）
	Fallback    time.Duration // 超时兜底转人工（真实 14d）
	CondSettle  time.Duration // 条件支付里对手方交付的模拟时长
}

func demoTimings() Timings {
	return Timings{
		OTCMatch: 20 * time.Second, OTCBind: 2 * time.Second, OTCS1: 10 * time.Second, OTCS3: 24 * time.Second, OTCTheirPay: 10 * time.Second,
		OTCVerify: 90 * time.Second,
		OTCS4:     4 * time.Second, Dispute: 15 * time.Second, Fallback: 60 * time.Second,
		CondSettle: 5 * time.Second,
	}
}

func realTimings() Timings {
	return Timings{
		OTCMatch: 10 * time.Minute, OTCBind: 5 * time.Second, OTCS1: 30 * time.Minute, OTCS3: 4 * time.Hour, OTCTheirPay: 90 * time.Minute,
		OTCVerify: 2 * time.Hour,
		OTCS4:     2 * time.Hour, Dispute: 72 * time.Hour, Fallback: 14 * 24 * time.Hour,
		CondSettle: 30 * time.Minute,
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
}

func Load() Config {
	c := Config{
		Addr:        env("ATARA_HTTP_ADDR", ":8080"),
		DBPath:      env("ATARA_DB_PATH", "./atara.db"),
		AgentImpl:   env("ATARA_AGENT_IMPL", "mock"),
		DemoTiming:  envBool("ATARA_DEMO_TIMING", true),
		UploadDir:   env("ATARA_UPLOAD_DIR", "./var/uploads"),
		CORSOrigins: env("ATARA_CORS_ORIGINS", "*"),
	}
	if c.DemoTiming {
		c.T = demoTimings()
	} else {
		c.T = realTimings()
	}
	return c
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
