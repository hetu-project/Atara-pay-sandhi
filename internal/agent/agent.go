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
}

type Vote struct {
	Agent   string `json:"agent"`
	Verdict string `json:"verdict"` // pass | flag
	Note    string `json:"note"`
}

type Assessment struct {
	Score     int    `json:"score"`
	Passed    int    `json:"passed"`
	Total     int    `json:"total"`
	Votes     []Vote `json:"votes"`
	Summary   string `json:"summary"`
	Threshold int    `json:"threshold"`
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
