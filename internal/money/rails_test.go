package money

import "testing"

// 只发能结算的。渠道菜单曾经列了三档后端根本不结算的法币,选了的人配置照样
// 审过,然后永远撮合不到单——而他不会收到任何报错。
func TestRailsOnlyListSettleableFiats(t *testing.T) {
	for _, g := range Rails() {
		if !Tradable(g.Fiat) {
			t.Fatalf("%s 的法币 %s 不可结算,不该出现在菜单里", g.Group, g.Fiat)
		}
		for _, r := range g.Rails {
			if r.Fiat != g.Fiat {
				t.Fatalf("%s 的币种 %s 跟所在组 %s 对不上", r.Name, r.Fiat, g.Fiat)
			}
		}
	}
	// 反面：不可结算的那几档必须真的被挡掉,否则这个测试什么都没验。
	for _, name := range []string{"DBS", "Emirates NBD", "Wise"} {
		if _, ok := RailFiat(name); ok {
			t.Fatalf("%s 收的是不可结算的法币,RailFiat 不该说它可用", name)
		}
	}
}

func TestRailFiatKnowsTheTable(t *testing.T) {
	if f, ok := RailFiat("ICBC"); !ok || f != "CNY" {
		t.Fatalf("ICBC = %q %v, 期望 CNY true", f, ok)
	}
	if f, ok := RailFiat("HSBC"); !ok || f != "HKD" {
		t.Fatalf("HSBC = %q %v, 期望 HKD true", f, ok)
	}
	// 名字不在表里就是不在表里,不猜。
	if _, ok := RailFiat("Bank of Nowhere"); ok {
		t.Fatalf("认不出的渠道不该被当成可用")
	}
}
