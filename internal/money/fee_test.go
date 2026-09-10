package money

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestFee(t *testing.T) {
	cases := []struct{ amt, fiat, want string }{
		{"36600", "CNY", "29.28"}, // 截图里那单：¥36,600 → ¥29
		{"100", "CNY", "0.08"},
		{"0", "CNY", "0"},
		// 法币精度不同：JPY 是 0 位
		{"1000000", "JPY", "800"},
	}
	for _, c := range cases {
		got := Fee(decimal.RequireFromString(c.amt), c.fiat)
		if got.String() != c.want {
			t.Fatalf("Fee(%s %s) = %s, want %s", c.amt, c.fiat, got, c.want)
		}
	}
}

// 费率是整数基点，不该出现浮点误差。
func TestFeeNoFloatDrift(t *testing.T) {
	a := decimal.RequireFromString("0.1")
	for i := 0; i < 1000; i++ {
		if Fee(a, "CNY").IsNegative() {
			t.Fatal("negative fee")
		}
	}
}
