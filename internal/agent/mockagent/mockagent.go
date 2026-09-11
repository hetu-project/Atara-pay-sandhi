// Package mockagent 是三处 agent 能力的确定性实现。
//
// 「确定性」是刻意的：demo 里同一句话必须每次解析成同一张单，
// 同一个对手方必须每次给出同一组票，否则没法演示也没法测试。
package mockagent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"

	"github.com/advaita/atara-pay/internal/agent"
	"github.com/advaita/atara-pay/internal/domain/condition"
)

type Suite struct{}

func New() agent.Suite {
	s := Suite{}
	return agent.Suite{Parser: s, RiskAssessor: s, ReleaseConsensus: s}
}

var (
	reAmount = regexp.MustCompile(`([0-9][0-9,]*(?:\.[0-9]+)?)\s*([kKmM])?`)
	reWord   = regexp.MustCompile(`[A-Za-z][A-Za-z.\- ]{1,30}`)
)

// Parse 做确定性槽位抽取：抽到的槽实心，抽不到的给合理默认值并列进 Guessed。
func (Suite) Parse(_ context.Context, in agent.ParseInput) (agent.Draft, error) {
	t := in.Text
	low := strings.ToLower(t)
	d := agent.Draft{AmountKind: "coin", Extra: map[string]string{}, Guessed: []string{}}

	switch {
	case strings.Contains(low, "sell"):
		d.Intent = "sell"
	case strings.Contains(low, "buy"):
		d.Intent = "buy"
	default:
		d.Intent = "transfer"
	}

	if m := reAmount.FindStringSubmatch(t); m != nil {
		d.Amount = strings.ReplaceAll(m[1], ",", "")
		switch strings.ToLower(m[2]) {
		case "k":
			d.Amount += "000"
		case "m":
			d.Amount += "000000"
		}
	} else {
		d.Amount = "1000"
		d.Guessed = append(d.Guessed, "amount")
	}

	d.Asset = pick(low, in.Assets, "USDT", &d.Guessed, "asset")
	d.Fiat = pick(low, in.Fiats, "CNY", &d.Guessed, "fiat")

	// 对手方：先按联系人名字精确匹配，匹配不到就取第一个并标成推断
	for _, c := range in.Contacts {
		if strings.Contains(low, strings.ToLower(strings.Fields(c.Name)[0])) {
			d.PeerID, d.PeerName = c.ID, c.Name
			break
		}
	}
	if d.PeerID == "" && len(in.Contacts) > 0 {
		d.PeerID, d.PeerName = in.Contacts[0].ID, in.Contacts[0].Name
		d.Guessed = append(d.Guessed, "counterparty")
	}

	// 条件关键词 → 条件原子。抽不到条件就是空集，空集 = 立即释放。
	switch {
	case strings.Contains(low, "on delivery"), strings.Contains(low, "收货"), strings.Contains(low, "delivered"):
		d.Conditions = []condition.Atom{{Type: condition.Evidence, Params: map[string]string{"proof": "Delivery record"}}}
	case strings.Contains(low, "receipt"), strings.Contains(low, "回执"):
		d.Conditions = []condition.Atom{{Type: condition.Evidence, Params: map[string]string{"proof": "Bank receipt"}}}
	case strings.Contains(low, "approve"), strings.Contains(low, "confirm"), strings.Contains(low, "验收"):
		d.Conditions = []condition.Atom{{Type: condition.Approve, Params: map[string]string{"who": "Both sides confirm"}}}
	}
	if d.Intent == "transfer" && len(d.Conditions) == 0 {
		d.Guessed = append(d.Guessed, "conditions")
	}

	// 用途只是备注，不影响资金——所以抽不到就留空，不瞎猜。
	if i := strings.Index(low, " for "); i >= 0 {
		if w := reWord.FindString(t[i+5:]); w != "" {
			d.Note = strings.TrimSpace(w)
		}
	}
	return d, nil
}

// pick 按词边界匹配，不是子串匹配——"USDT" 里含 "USD"，
// 子串匹配会把「买 USDT」读成「用美元结算」。
func pick(low string, opts []string, def string, guessed *[]string, slot string) string {
	words := strings.FieldsFunc(low, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	for _, o := range opts {
		want := strings.ToLower(o)
		for _, w := range words {
			if w == want {
				return o
			}
		}
	}
	*guessed = append(*guessed, slot)
	return def
}

// 七个 agent 各查一件事，名字与界面上候命排那七个一一对应。共识门槛 6/7。
//
// 名字必须跟前端那套一致。以前后端叫「Sanctions screening / Source of funds /
// Counterparty history…」，前端叫「Identity / Provenance / Graph…」，两边都是
// 七个、顺序还不同——前端只能按下标去配，于是 Identity 那一格印的是制裁那一票
// 的理由。每句话都挂在错误的标题下，而读的人无从发现。名字对齐之后，前端按
// 名字取，错位这一整类问题就没有了。
var riskAgents = []string{
	"Identity", "Provenance", "Graph", "Sanctions", "Behavior", "Pricing", "Velocity",
}

const threshold = 6

// agentScore 给一个 agent 在这一单上打分。
//
// 跟 app.ScoreOrder 同一套办法：从种子哈希出一个稳定的伪随机数，再按这一票
// 的结论调整。稳定是必须的——同一单翻回去看必须还是那个数，否则「当时评了
// 多少」就不成立；而不同单之间要散开，不然七个数字看着像写死的。
//
// 过的落在 82–97，没过的落在 58–73：两段不重叠，扫一眼数字就知道哪个出了问题，
// 不用去读那一行字。
func agentScore(seed, agent string, ok bool) int {
	h := sha256.Sum256([]byte("atara-agent|" + seed + "|" + agent))
	n := int(binary.BigEndian.Uint32(h[:4]))
	if ok {
		return 82 + n%16
	}
	return 58 + n%16
}

func (Suite) Assess(_ context.Context, in agent.AssessInput) (agent.Assessment, error) {
	votes := make([]agent.Vote, 0, len(riskAgents))
	// 种子缺省退回对手方名字：至少不同对手方不一样，比七个常数强。
	seed := in.Seed
	if seed == "" {
		seed = in.PeerName
	}
	put := func(name, note string, ok bool) {
		v := "flag"
		if ok {
			v = "pass"
		}
		votes = append(votes, agent.Vote{
			Agent: name, Verdict: v, Note: note, Score: agentScore(seed, name, ok),
		})
	}

	// Identity —— 身份件在不在。
	if in.Docs["kyc"] {
		put(riskAgents[0], "Government ID and liveness on file", true)
	} else {
		put(riskAgents[0], "Identity not verified", false)
	}

	// Provenance —— 钱从哪儿来。
	if in.Docs["sow"] || in.Docs["pof"] {
		put(riskAgents[1], "Source of wealth on file", true)
	} else {
		put(riskAgents[1], "No proof of funds shared", false)
	}

	// Graph —— 地址聚类与关联方。
	//
	// 这一版没有真的图数据：AssessInput 里只有成交、纠纷和资质件。所以它退回
	// 去看链上溯源那一件，说的也只是「交上来了/没交」，不声称聚类过什么。
	// 接真模型时这里换成地址聚类的结果。
	if in.Docs["chain"] {
		put(riskAgents[2], "On-chain history shared and traced", true)
	} else {
		put(riskAgents[2], "No on-chain provenance shared", false)
	}

	// Sanctions —— 名单筛查。
	put(riskAgents[3], "No hits across OFAC, UN or EU lists", true)

	// Behavior —— 成交史与纠纷。这两件本来就是一回事的两面，合成一票。
	switch {
	case in.Disputes > 0:
		put(riskAgents[4], fmt.Sprintf("%d disputes on record", in.Disputes), false)
	case in.Deals >= 20:
		put(riskAgents[4], fmt.Sprintf("%d settled trades, no disputes", in.Deals), true)
	default:
		put(riskAgents[4], fmt.Sprintf("Only %d settled trades", in.Deals), false)
	}

	// Pricing —— 报价偏离。
	//
	// 同样没有数据：这一版的 AssessInput 不带报价，也不带指数价。它只说这一
	// 项没有可疑之处，不编一个具体的偏离幅度——编出来的百分比会被人当真。
	put(riskAgents[5], "Quote inside the normal band", true)

	// Velocity —— 频次与金额异常。
	put(riskAgents[6], "Trade frequency within normal range", true)

	passed := 0
	for _, v := range votes {
		if v.Verdict == "pass" {
			passed++
		}
	}
	summary := fmt.Sprintf("Passed %d of %d checks", passed, len(votes))
	if passed < threshold {
		summary += " — below the 6/7 consensus threshold"
	}
	// 读了多少来源、多少记录。
	//
	// mock 里这是按输入推出来的：每个 agent 手上有自己那几个源，记录数随
	// 对手方的成交与纠纷量涨。它不是真的去数过——但至少是可解释的、稳定的，
	// 而且随对手方变化。接真模型时这两个数由模型如实报。
	sources := len(votes) * 3
	records := len(votes) * 12
	for _, d := range []bool{in.Docs["kyc"], in.Docs["pof"], in.Docs["sow"], in.Docs["chain"]} {
		if d {
			sources += 2
			records += 35
		}
	}
	records += in.Deals*3 + in.Disputes*17

	return agent.Assessment{
		Score: in.TrustScore, Passed: passed, Total: len(votes),
		Votes: votes, Summary: summary, Threshold: threshold,
		Sources: sources, Records: records,
	}, nil
}

// Vote 是放行共识。出口只有 release 与 hold_for_review。
func (Suite) Vote(_ context.Context, in agent.ReleaseInput) (agent.Decision, error) {
	votes := []agent.Vote{
		{Agent: "Buyer conditions", Verdict: "pass", Note: "Conditions met as written: " + in.ConditionText},
		{Agent: "Seller terms", Verdict: "pass", Note: "No outstanding obligation on the seller side"},
		{Agent: "Protocol screening", Verdict: "pass", Note: "No screening hit at release time"},
	}
	if in.PeerDisputes >= 4 {
		votes[2] = agent.Vote{Agent: "Protocol screening", Verdict: "flag",
			Note: fmt.Sprintf("Counterparty carries %d open disputes", in.PeerDisputes)}
		return agent.Decision{
			Outcome: agent.OutcomeHoldForReview, Votes: votes,
			Rationale: "Held for human review — the counterparty's dispute record crossed the threshold. Funds stay locked.",
		}, nil
	}
	return agent.Decision{
		Outcome: agent.OutcomeRelease, Votes: votes,
		Rationale: "All three checks agree the transfer matches what both sides agreed. Releasing.",
	}, nil
}
