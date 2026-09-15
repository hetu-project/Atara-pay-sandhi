package money

/*
法币收款渠道：对手方把钱打到哪里。

**这张表在后端,前端从 /catalog/rails 取。** 理由跟隔壁 catalog.Chain 那段
一模一样——写死在前端的目录会跟后端的能力漂开,而且要等到很晚才发现：

	渠道菜单原来在前端写死,列了 SGD / AED / EUR 三档,而后端只结算
	CNY / HKD / USD。只勾了 SGD 渠道的商户配置照样审过,然后永远撮合不到
	任何一单——他没收到任何报错,只是没有生意。

所以这里只发**能结算的那些**：不可交易的法币,连同它下面的渠道一起不出现。
选不到,就构造不出那种配置。
*/

// Rail 是一个收款渠道。
type Rail struct {
	// Name 是渠道名,也是挂单配置里存的值。
	Name string `json:"name"`
	// Fiat 是这条渠道收的币种。
	Fiat string `json:"fiat"`
}

// RailGroup 按走廊分组,对齐界面上那个分组多选菜单。
type RailGroup struct {
	Group string `json:"group"`
	Fiat  string `json:"fiat"`
	Rails []Rail `json:"rails"`
}

// railTable 是全集,含暂时不可交易的法币——法币重新开放时只要改 tradableFiat,
// 不必回来补这张表。过滤在 Rails() 里做。
var railTable = []struct {
	group string
	fiat  string
	names []string
}{
	{"Mainland China · CNY", "CNY", []string{
		"ICBC", "China Merchants Bank", "Bank of China", "CCB",
		"Agricultural Bank", "Alipay", "WeChat Pay"}},
	{"Hong Kong · HKD", "HKD", []string{
		"HSBC", "Bank of China (HK)", "Hang Seng", "ZA Bank", "FPS"}},
	{"Singapore · SGD", "SGD", []string{"DBS", "OCBC", "UOB", "PayNow"}},
	{"UAE · AED", "AED", []string{"Emirates NBD", "FAB", "Mashreq"}},
	{"Europe · EUR", "EUR", []string{"SEPA transfer", "Wise", "Revolut"}},
}

// Rails 列出当前能用的收款渠道,按走廊分组。
//
// 只发可结算法币下面的那些。这一版是 CNY 与 HKD——**USD 虽然可结算,
// 但一条渠道都还没有**,所以想用 USD 结算的商户在界面上无处可选。
// 那是产品缺口,不是代码缺陷：补一行渠道数据就有了,但编几个银行名出来
// 不是这里该做的事。
func Rails() []RailGroup {
	out := make([]RailGroup, 0, len(railTable))
	for _, g := range railTable {
		if !Tradable(g.fiat) {
			continue
		}
		rs := make([]Rail, 0, len(g.names))
		for _, n := range g.names {
			rs = append(rs, Rail{Name: n, Fiat: g.fiat})
		}
		out = append(out, RailGroup{Group: g.group, Fiat: g.fiat, Rails: rs})
	}
	return out
}

// RailFiat 说这条渠道收的是什么币,以及它现在能不能用。
//
// 认不出来的渠道返回 ok=false——名字不在表里就是不在表里,不猜。
func RailFiat(name string) (fiat string, ok bool) {
	for _, g := range railTable {
		for _, n := range g.names {
			if n == name {
				return g.fiat, Tradable(g.fiat)
			}
		}
	}
	return "", false
}
