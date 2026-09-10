package app

import (
	"testing"

	"github.com/advaita/atara-pay/internal/snowflake"
)

// 工单号要唯一。旧的 6 位随机十六进制约 4800 单就有一半概率撞上，
// 而 orders.ref 上有 unique 约束——撞上就是一次下单失败。
func TestRefUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200000; i++ {
		r := Ref()
		if seen[r] {
			t.Fatalf("duplicate ref %s at %d", r, i)
		}
		seen[r] = true
	}
}

// 形状：ATR- 加 13 位。这个号要打进银行附言，长度得是稳定的。
func TestRefShape(t *testing.T) {
	r := Ref()
	if len(r) != 4+13 {
		t.Fatalf("ref %q has length %d, want 17", r, len(r))
	}
	if r[:4] != "ATR-" {
		t.Fatalf("ref %q lost the prefix", r)
	}
	for _, c := range r[4:] {
		if c == 'I' || c == 'L' || c == 'O' || c == 'U' {
			t.Fatalf("ref %q contains a look-alike character", r)
		}
	}
}

// 后发的号排在后面：按号排序就是按时间排序。
func TestRefOrdered(t *testing.T) {
	prev := Ref()
	for i := 0; i < 10000; i++ {
		cur := Ref()
		if cur <= prev {
			t.Fatalf("ref went backwards: %s then %s", prev, cur)
		}
		prev = cur
	}
}

var _ = snowflake.TimeOf
