package app

import (
	"testing"

	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/shopspring/decimal"
)

// 分数必须落在 60–99：低于 60 界面上是红的、等于劝退；100 分等于说零风险。
func TestScoreRange(t *testing.T) {
	peers := []*model.Merchant{
		nil,
		{Deals: 0},
		{Deals: 500, Docs: map[string]bool{"kyc": true, "pof": true, "stm": true, "poa": true, "sow": true, "chain": true}},
		{Deals: 500, Disputes: 100},
		{Disputes: 999},
	}
	amounts := []string{"0", "1000", "50000", "5000000"}
	for i := 0; i < 3000; i++ {
		id := "order-" + decimal.NewFromInt(int64(i)).String()
		for _, p := range peers {
			for _, a := range amounts {
				got := ScoreOrder(id, p, decimal.RequireFromString(a))
				if got < 60 || got > 99 {
					t.Fatalf("score %d out of range (id=%s peer=%+v amt=%s)", got, id, p, a)
				}
			}
		}
	}
}

// 同一笔单任何时候都是同一个分。不稳定的话，列表页和详情页会显示成两个数。
func TestScoreStable(t *testing.T) {
	p := &model.Merchant{Deals: 12, Disputes: 1, Docs: map[string]bool{"kyc": true}}
	a := decimal.RequireFromString("5000")
	first := ScoreOrder("abc-123", p, a)
	for i := 0; i < 100; i++ {
		if got := ScoreOrder("abc-123", p, a); got != first {
			t.Fatalf("same order scored %d then %d", first, got)
		}
	}
}

// 不同工单要分散开，不能挤成一个数。
func TestScoreSpread(t *testing.T) {
	seen := map[int]int{}
	for i := 0; i < 400; i++ {
		seen[ScoreOrder("o"+decimal.NewFromInt(int64(i)).String(), nil, decimal.Zero)]++
	}
	if len(seen) < 10 {
		t.Fatalf("only %d distinct scores across 400 orders: %v", len(seen), seen)
	}
}

// 纠纷要真的把分拉下来。
func TestDisputesLower(t *testing.T) {
	clean := &model.Merchant{Deals: 40, Docs: map[string]bool{"kyc": true}}
	dirty := &model.Merchant{Deals: 40, Disputes: 5, Docs: map[string]bool{"kyc": true}}
	a := decimal.Zero
	worse, better := 0, 0
	for i := 0; i < 200; i++ {
		id := "d" + decimal.NewFromInt(int64(i)).String()
		if ScoreOrder(id, dirty, a) < ScoreOrder(id, clean, a) {
			worse++
		} else {
			better++
		}
	}
	if worse < 150 {
		t.Fatalf("disputes barely moved the score: worse=%d better=%d", worse, better)
	}
}
