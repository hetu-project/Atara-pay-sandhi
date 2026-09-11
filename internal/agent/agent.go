// Package agent 是三处 agent 能力的接口层。
//
// 一期用确定性 mock 实现，但返回结构与真实实现完全一致——
// 后期接 LLM 只换实现，路由与 DTO 不动。
package agent

import (
	"context"

	"github.com/advaita/atara-pay/internal/domain/condition"
)

// ── 自然语言解析 ──

type ParseInput struct {
	Text     string
	Assets   []string
	Fiats    []string
	Contacts []Contact
}

type Contact struct{ ID, Name string }

// Draft 是解析出的订单草稿。Guessed 列出「系统推断而非用户明说」的槽位——
// 前端把这些槽标成虚线，有虚线未确认就提交时，把问题标在问题上。
type Draft struct {
	Intent     string            `json:"intent"` // buy | sell | transfer
	Amount     string            `json:"amount"`
	AmountKind string            `json:"amount_kind"` // coin | fiat
	Asset      string            `json:"asset"`
	Fiat       string            `json:"fiat"`
	PeerID     string            `json:"counterparty_id"`
	PeerName   string            `json:"counterparty_name"`
	Note       string            `json:"note"`
	Conditions []condition.Atom  `json:"conditions"`
	Guessed    []string          `json:"guessed"`
	Extra      map[string]string `json:"extra,omitempty"`
}

type Parser interface {
	Parse(ctx context.Context, in ParseInput) (Draft, error)
}

// ── 对手方风控共识 ──

type AssessInput struct {
	PeerName   string
	TrustScore int
	Deals      int
	Disputes   int
	Docs       map[string]bool
	// Seed 让同一个对手方在不同的单上得到不同的分。
	//
	// 不给的话，同一条挂单被谁吃、吃几次，七个 agent 都报同一组数字——
	// 界面上就成了「这套评分跟这一单无关」。传工单号：同一单任何时候看
	// 都是同一组数（评估是对下单那一刻的判断，判断做完就固定），不同单
	// 之间又互不相同。
	Seed string
}

type Vote struct {
	Agent   string `json:"agent"`
	Verdict string `json:"verdict"` // pass | flag
	Note    string `json:"note"`
	// Score 是这个 agent 单独给的分，0 表示它没给分。
	// 界面上候命排每个名字下面的那个数字用它。
	Score int `json:"score,omitempty"`
}

type Assessment struct {
	Score     int    `json:"score"`
	Passed    int    `json:"passed"`
	Total     int    `json:"total"`
	Votes     []Vote `json:"votes"`
	Summary   string `json:"summary"`
	Threshold int    `json:"threshold"`

	// Sources / Records 是这次评估读了多少个数据源、多少条记录。
	// 界面上「Read 23 sources · 447 records」那一行用它。
	//
	// 由评估器自己报，不由上层猜：这句话是在向用户交代「凭什么」，
	// 让接口层按票数编一个好看的数，就是拿一句假话去支撑一个判断。
	Sources int `json:"sources"`
	Records int `json:"records"`

	// TookMs 是这次评估真正花了多少毫秒，由调用方在跑完之后填。
	//
	// 界面上「Assessed in 13s」那一行用它。不填就只显示「Assessed」——
	// 参照那边的秒数是动画自己跑掉的时间，我们这边评估是瞬时算完的，
	// 编一个像样的秒数等于拿一句假话去撑「我们认真查过」这个印象。
	TookMs int64 `json:"took_ms,omitempty"`
}

type RiskAssessor interface {
	Assess(ctx context.Context, in AssessInput) (Assessment, error)
}

// ── 放行共识 ──

type Outcome string

const (
	OutcomeRelease       Outcome = "release"
	OutcomeHoldForReview Outcome = "hold_for_review"
)

type ReleaseInput struct {
	OrderID       string
	Asset         string
	AmountUSD     string
	PeerName      string
	PeerDisputes  int
	ConditionText string
}

// Decision 的出口只有两个。放行共识没有裁量权：只能放行或拦下转人工，
// 不能改判条件——否则「条件成立即放款」的确定性就没了。
// 用类型钉死这个边界，而不是靠注释。
type Decision struct {
	Outcome   Outcome `json:"outcome"`
	Votes     []Vote  `json:"votes"`
	Rationale string  `json:"rationale"`
}

type ReleaseConsensus interface {
	Vote(ctx context.Context, in ReleaseInput) (Decision, error)
}

// Suite 把三处能力打包，方便整体替换实现。
type Suite struct {
	Parser
	RiskAssessor
	ReleaseConsensus
}
