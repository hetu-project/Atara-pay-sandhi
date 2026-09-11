package mockagent

import (
	"context"
	"testing"

	"github.com/advaita/atara-pay/internal/agent"
)

func in(seed string) agent.AssessInput {
	return agent.AssessInput{
		PeerName: "Golden Gate", TrustScore: 90, Deals: 124, Seed: seed,
		Docs: map[string]bool{"kyc": true, "sow": true, "chain": true},
	}
}

// 名字必须是界面上那七个。对不上的话前端只能按下标去配，而两边顺序一旦不同，
// 每一句理由都会挂在错误的标题下——而读的人无从发现。
func TestAgentNamesMatchTheConsole(t *testing.T) {
	a, err := Suite{}.Assess(context.Background(), in("o1"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Identity", "Provenance", "Graph", "Sanctions", "Behavior", "Pricing", "Velocity"}
	if len(a.Votes) != len(want) {
		t.Fatalf("%d 票，应该是 %d", len(a.Votes), len(want))
	}
	for i, n := range want {
		if a.Votes[i].Agent != n {
			t.Fatalf("第 %d 票是 %q，应该是 %q", i, a.Votes[i].Agent, n)
		}
	}
}

// 同一单任何时候看都是同一组分——否则「当时评了多少」就不成立。
func TestScoresAreStableForTheSameOrder(t *testing.T) {
	a, _ := Suite{}.Assess(context.Background(), in("order-1"))
	b, _ := Suite{}.Assess(context.Background(), in("order-1"))
	for i := range a.Votes {
		if a.Votes[i].Score != b.Votes[i].Score {
			t.Fatalf("%s 两次不一样：%d vs %d", a.Votes[i].Agent, a.Votes[i].Score, b.Votes[i].Score)
		}
	}
}

// 不同单之间要散开，否则七个数字看着像写死的。
func TestScoresDifferBetweenOrders(t *testing.T) {
	a, _ := Suite{}.Assess(context.Background(), in("order-1"))
	b, _ := Suite{}.Assess(context.Background(), in("order-2"))
	same := 0
	for i := range a.Votes {
		if a.Votes[i].Score == b.Votes[i].Score {
			same++
		}
	}
	if same == len(a.Votes) {
		t.Fatal("两单七个分完全相同——种子没起作用")
	}
}

// 每一票都要有分：候命排里每个名字下面都印一个数，缺一个就是一个空洞。
// 过与没过分在两段不重叠的区间，扫一眼数字就知道哪个出了问题。
func TestEveryVoteScoredAndBanded(t *testing.T) {
	bad := in("order-3")
	bad.Docs = map[string]bool{} // 什么都没交，好让几票落到 flag
	bad.Deals, bad.Disputes = 2, 3
	a, _ := Suite{}.Assess(context.Background(), bad)
	for _, v := range a.Votes {
		if v.Score == 0 {
			t.Fatalf("%s 没有分", v.Agent)
		}
		if v.Verdict == "pass" && (v.Score < 82 || v.Score > 97) {
			t.Fatalf("%s 过了却是 %d 分，落在 82–97 之外", v.Agent, v.Score)
		}
		if v.Verdict == "flag" && (v.Score < 58 || v.Score > 73) {
			t.Fatalf("%s 没过却是 %d 分，落在 58–73 之外", v.Agent, v.Score)
		}
	}
}
