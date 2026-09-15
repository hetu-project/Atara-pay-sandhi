package makerreview

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/advaita/atara-pay/internal/desk"
)

/*
模型层：读自述字段，找规则层穷举不了的矛盾。

它判的只有一类东西——**自由文本与结构化字段对不对得上**。「业务描述说是跨境
电商代收，营业额档位选了 Under 5M，却申请单笔上限 50 万 USDT」这种话没法写成
规则，因为它依赖读懂那段文字。除此之外的一切都该留在规则层。

三条约束落在代码里，不靠提示词自觉：

  1. 只有白名单里的字段出网（redact.go）
  2. 指不到字段的意见直接丢弃（parse）
  3. 裁决由 VerdictOf 算，模型说了不算——它即使被诱导着写
     "verdict":"pass"，也改变不了代码按它找出的问题算出来的结果
*/

// AI 是模型层。Client 为 nil 表示这一层关着——规则层照常工作。
type AI struct {
	Client  *desk.Client
	ModelID string
	// Timeout 是单次调用的上限。超时不是「材料有问题」，是「我读不了」。
	Timeout time.Duration
}

// ErrOff 表示模型层没开。调用方据此跳过，而不是当成失败。
var ErrOff = errors.New("makerreview: model layer is off")

const systemPrompt = `You review merchant onboarding applications for a payments platform.

You are given ONE JSON object of self-declared fields. Identity documents have already
been verified by a separate service — do NOT comment on document authenticity, identity,
or anything not present in the JSON.

Your only job: find places where the declared fields CONTRADICT EACH OTHER or are
implausible together. Typical examples:
- registration country, operating city and payment rails that do not line up
- a stated source of wealth or income band that cannot support the requested limits
- a free-text business description that does not match the declared industry or turnover

Rules you must follow:
1. Report ONLY problems you can point at specific field keys from the input.
   Never invent a field key. Never comment on a field that is absent.
2. State what is inconsistent as a fact. Do NOT give verdicts, risk ratings,
   approvals or rejections. Do NOT say things like "high risk" or "suspicious".
3. If everything lines up, return an empty issues array. Finding nothing is a
   normal and common outcome — do not manufacture a problem to look useful.
4. Write "says" and "ask" in plain English, one or two sentences each, addressed
   to the applicant as "you".

Reply with ONLY a JSON object, no prose and no code fences:
{"issues":[{"fields":["key1","key2"],"says":"...","ask":"..."}]}`

// Review 让模型看一遍。
//
// 返回的 Result 只带 issues 与由代码算出的 verdict；出错一律 Escalate——
// 技术故障不能表达成「你材料有问题」（MAKER-REVIEW-AI.md §6）。
func (a *AI) Review(ctx context.Context, stage string, form json.RawMessage) (Result, error) {
	if a == nil || a.Client == nil {
		return Result{}, ErrOff
	}
	/* Only identity material goes to the model.

	   The listing stage is entirely structured — the rules decide all of it,
	   and there is no free text to read. Running the model there would spend
	   a call and a few seconds of someone's time to repeat what the rule
	   layer already said. See the note in redact.go. */
	if stage != "kyc" {
		return Result{}, ErrOff
	}
	safe, err := RedactKYC(form)
	if err != nil {
		// 读不出来就别发：发一份我们自己都没解析成功的东西出去，
		// 等于不知道到底发了什么。
		return escalated("We could not read what you submitted."), nil
	}
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}
	raw, err := a.Client.Stream(ctx, []desk.Msg{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: string(safe)},
	}, func(string) error { return nil })
	if err != nil {
		return escalated(""), err
	}
	issues, ok := parse(raw, safe)
	if !ok {
		return escalated(""), errors.New("makerreview: model returned something we could not parse")
	}
	return Result{Verdict: VerdictOf(issues), Source: SourceAI, Issues: issues}, nil
}

// escalated 是「我读不了」的那个出口。
//
// says 为空时不给用户看细节——这一刻出问题的是我们，不是他的材料，
// 编一条指摘出来只会让他去改一个没有错的地方。
func escalated(says string) Result {
	if says == "" {
		says = "We could not finish the automatic check on this submission."
	}
	return Result{
		Verdict: Escalate,
		Source:  SourceAI,
		Issues: []Issue{{
			Fields: []string{"*"},
			Says:   says,
			Ask:    "A person will review this — there is nothing for you to change.",
			Route:  ToEscalate,
		}},
	}
}

// parse 读模型的回话。
//
// 第二个返回值是「读懂了没有」，不是「有没有问题」——空 issues 是一个完全
// 正常的结果（没找到矛盾），跟「返回了一坨读不出的东西」必须分开。
func parse(raw string, sent json.RawMessage) ([]Issue, bool) {
	var out struct {
		Issues []Issue `json:"issues"`
	}
	if err := json.Unmarshal([]byte(trimFence(raw)), &out); err != nil {
		return nil, false
	}
	// 只留指得到字段、而且指的是我们真发出去过的字段的那几条。
	//
	// 后半句是关键：模型可以编一个 "passport_number" 出来说它不一致——
	// 而那个字段根本没发给它。放过去的话，用户会收到一条指着一个他在这张表上
	// 找不到的字段的意见，而那正是「不可验证的指摘」的最坏形态。
	known := keysOf(sent)
	keep := []Issue{}
	for _, i := range out.Issues {
		fs := []string{}
		for _, f := range i.Fields {
			if known[f] {
				fs = append(fs, f)
			}
		}
		if len(fs) == 0 || strings.TrimSpace(i.Says) == "" || strings.TrimSpace(i.Ask) == "" {
			continue
		}
		// 模型不出终局：它找出来的东西一律是「你去改」，改不了的那几类
		// 由规则层认（制裁、PEP、高风险辖区），不归它判。
		keep = append(keep, Issue{Fields: fs, Says: i.Says, Ask: i.Ask, Route: ToRevise})
	}
	return keep, true
}

// trimFence 去掉模型有时候自己加的 ```json 围栏。
func trimFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
}

func keysOf(raw json.RawMessage) map[string]bool {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
