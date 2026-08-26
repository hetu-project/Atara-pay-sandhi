// Package mockagent 是三处 agent 能力的确定性实现。
//
// 「确定性」是刻意的：demo 里同一句话必须每次解析成同一张单，
// 同一个对手方必须每次给出同一组票，否则没法演示也没法测试。
package mockagent

import (
	"context"
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

// 七个 agent 各查一件事。共识门槛 6/7——与前端的共识环一致。
var riskAgents = []string{
	"Sanctions screening", "Source of funds", "Counterparty history",
	"Document integrity", "Chain provenance", "Dispute record", "Velocity check",
}

const threshold = 6

func (Suite) Assess(_ context.Context, in agent.AssessInput) (agent.Assessment, error) {
	votes := make([]agent.Vote, 0, len(riskAgents))
	flag := func(name, note string) { votes = append(votes, agent.Vote{Agent: name, Verdict: "flag", Note: note}) }
	pass := func(name, note string) { votes = append(votes, agent.Vote{Agent: name, Verdict: "pass", Note: note}) }

	pass(riskAgents[0], "No sanctions list match")

	if in.Docs["sow"] || in.Docs["pof"] {
		pass(riskAgents[1], "Source of wealth on file")
	} else {
		flag(riskAgents[1], "No proof of funds shared")
	}
	if in.Deals >= 20 {
		pass(riskAgents[2], fmt.Sprintf("%d settled trades", in.Deals))
	} else {
		flag(riskAgents[2], fmt.Sprintf("Only %d settled trades", in.Deals))
	}
	if in.Docs["kyc"] {
		pass(riskAgents[3], "Identity verified by the platform")
	} else {
		flag(riskAgents[3], "Identity not verified")
	}
	if in.Docs["chain"] {
		pass(riskAgents[4], "On-chain history traced")
	} else {
		flag(riskAgents[4], "No on-chain provenance shared")
	}
	if in.Disputes == 0 {
		pass(riskAgents[5], "No disputes on record")
	} else {
		flag(riskAgents[5], fmt.Sprintf("%d disputes on record", in.Disputes))
	}
	pass(riskAgents[6], "Trade frequency within normal range")

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
	return agent.Assessment{
		Score: in.TrustScore, Passed: passed, Total: len(votes),
		Votes: votes, Summary: summary, Threshold: threshold,
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
