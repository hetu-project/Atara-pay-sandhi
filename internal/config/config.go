package config

import (
	"os"
	"strconv"
	"time"
)

// Timings 是状态机各站的停留时长。
// demo 用短值，真实口径写在注释里——两套值出自 console.html:4978。
type Timings struct {
	OTCMatch   time.Duration // 吃单后的软预留窗口（真实 10m）
	OTCS1      time.Duration // 对手方注资托管（真实 30m）
	OTCS3      time.Duration // 你的法币转账窗口（真实 4h）
	OTCS4      time.Duration // 平台核验回执（真实 2h）
	Dispute    time.Duration // 凭证档的异议窗口（真实 72h）
	Fallback   time.Duration // 超时兜底转人工（真实 14d）
	CondSettle time.Duration // 条件支付里对手方交付的模拟时长
}

func demoTimings() Timings {
	return Timings{
		OTCMatch: 20 * time.Second, OTCS1: 7 * time.Second, OTCS3: 24 * time.Second,
		OTCS4: 4 * time.Second, Dispute: 15 * time.Second, Fallback: 60 * time.Second,
		CondSettle: 5 * time.Second,
	}
}

func realTimings() Timings {
	return Timings{
		OTCMatch: 10 * time.Minute, OTCS1: 30 * time.Minute, OTCS3: 4 * time.Hour,
		OTCS4: 2 * time.Hour, Dispute: 72 * time.Hour, Fallback: 14 * 24 * time.Hour,
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
