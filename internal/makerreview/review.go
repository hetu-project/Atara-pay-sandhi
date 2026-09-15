// Package makerreview 是商家准入的预审：规则层 + （后续）模型层。
//
// 设计与依据见 docs/MAKER-REVIEW-AI.md。三条不能放宽的规矩：
//
//  1. 出具体问题，不出结论。每条指摘必须指到表单字段——指不到字段的
//     意见一律丢弃。依据：具体到字段的指摘是可验证的，用户一眼就知道
//     说得对不对，说错了会立刻炸；而「材料不符合要求」这种黑箱结论
//     判错了没人发现，等发现已经错拒了三个月。
//
//  2. 裁决由代码从 issues 推出，不由出票的人自己宣布（见 VerdictOf）。
//     模型只负责找出哪几项对不上，判不判过是代码的事。
//
//  3. 能写成规则的绝不交给模型。规则层稳定、可测试、不会变笨，模型
//     故障时它照常工作（PRD §8.4「规则模板与模型判断的双层结构」）。
package makerreview

// Route 说这条问题该往哪走。
//
// 分两档是因为「你能改的」和「你改不了的」是两件事：地址证明过期你能补，
// 被列入制裁名单你补不了。把后者也写成「请修改」，等于让人对着一个他
// 无论如何都满足不了的要求反复提交。
type Route string

const (
	// ToRevise：申请人自己能改的。打回，表单带着原内容重开。
	ToRevise Route = "revise"
	// ToEscalate：申请人改不了的，得有人看。制裁、PEP、高风险辖区。
	ToEscalate Route = "escalate"
)

// Issue 是一条具体的指摘。
type Issue struct {
	// Fields 是表单里的字段 key，前端照着它高亮。必填。
	Fields []string `json:"fields"`
	// Says 说「哪里不对」——陈述事实，不下判词。
	Says string `json:"says"`
	// Ask 说「要你做什么」。
	Ask string `json:"ask"`
	// Route 决定这条往哪走，默认 revise。
	Route Route `json:"route,omitempty"`
}

// Verdict 是一次预审的出口。三个，没有终局驳回。
//
// 依据（MAKER-REVIEW-AI.md §4）：误拒和误放的代价不对称——误拒是吵闹的、
// 可恢复的（申请人会投诉，改了能再交）；误放的代价落在陌生买家身上，
// 出事才知道，且不可挽回。所以自动出来的「有问题」必须落在可恢复的那一侧。
type Verdict string

const (
	Pass     Verdict = "pass"
	Revise   Verdict = "revise"
	Escalate Verdict = "escalate"
)

// Source 说这次裁决是谁出的。留痕要能回答「从哪天开始判得不对的」。
const (
	SourceRule  = "rule"
	SourceAI    = "ai"
	SourceHuman = "human"
)

// Result 是一次预审的完整结果。
type Result struct {
	Verdict Verdict `json:"verdict"`
	Source  string  `json:"source"`
	Issues  []Issue `json:"issues"`
}

// VerdictOf 由 issues 推出裁决。
//
// 谁都不许自己宣布结论——规则层不许，模型更不许。这样模型即使被诱导着
// 说「我批准了」，也改变不了代码按它找出的问题算出来的那个结果。
func VerdictOf(issues []Issue) Verdict {
	out := Pass
	for _, i := range issues {
		if i.Route == ToEscalate {
			// 一条转人工就整份转人工：改不了的那条挡在前面，
			// 让人先去改别的没有意义。
			return Escalate
		}
		out = Revise
	}
	return out
}

// Summary 把 issues 压成一句话，存进 maker_applications.reject_reason。
//
// 逐项原文存在 maker_reviews 里；这一列留一句摘要，是为了兼容已有的
// admin 后台和 AI 聊天窗（desk 快照会读它）。
func Summary(issues []Issue) string {
	switch len(issues) {
	case 0:
		return ""
	case 1:
		return issues[0].Says
	}
	s := issues[0].Says
	for _, i := range issues[1:] {
		s += " " + i.Says
	}
	return s
}
