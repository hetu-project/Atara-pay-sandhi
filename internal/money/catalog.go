package money

import "github.com/shopspring/decimal"

func d(s string) decimal.Decimal { v, _ := decimal.NewFromString(s); return v }

// 数字资产目录。scale 与网络对齐前端 console.html 的 ASSETS / NETS_OF。
var cryptos = []Asset{
	// 登录只剩 MetaMask，用户手里是 0x 地址；再给 USDT 挂一条 TRON，
	// 同一张订单上就会出现 T 开头的网络名配 0x 的托管合约。这一版全走 EVM。
	{Code: "USDT", Kind: KindCrypto, Name: "Tether USD", Symbol: "₮", Scale: 6, Networks: []string{"ETH", "POLYGON"}, USDRate: d("1")},
	{Code: "USDC", Kind: KindCrypto, Name: "USD Coin", Symbol: "$", Scale: 6, Networks: []string{"POLYGON", "ETH"}, USDRate: d("1")},
	{Code: "BTC", Kind: KindCrypto, Name: "Bitcoin", Symbol: "₿", Scale: 8, Networks: []string{"BTC"}, USDRate: d("93600")},
	{Code: "ETH", Kind: KindCrypto, Name: "Ether", Symbol: "Ξ", Scale: 18, Networks: []string{"ETH"}, USDRate: d("3130")},
}

// 法币目录按走廊分组，对齐前端 FIATS。法币不入账——它们只出现在目录、
// 挂单价格与回执里，wallets 表永远没有法币行。
var fiats = []Asset{
	{Code: "CNY", Name: "Chinese Yuan", Symbol: "¥", Corridor: "Greater China", USDRate: d("0.137")},
	{Code: "HKD", Name: "Hong Kong Dollar", Symbol: "HK$", Corridor: "Greater China", USDRate: d("0.128")},
	{Code: "TWD", Name: "New Taiwan Dollar", Symbol: "NT$", Corridor: "Greater China", USDRate: d("0.031")},
	{Code: "SGD", Name: "Singapore Dollar", Symbol: "S$", Corridor: "Asia Pacific", USDRate: d("0.74")},
	{Code: "JPY", Name: "Japanese Yen", Symbol: "¥", Corridor: "Asia Pacific", USDRate: d("0.0064")},
	{Code: "KRW", Name: "South Korean Won", Symbol: "₩", Corridor: "Asia Pacific", USDRate: d("0.00072")},
	{Code: "THB", Name: "Thai Baht", Symbol: "฿", Corridor: "Asia Pacific", USDRate: d("0.029")},
	{Code: "VND", Name: "Vietnamese Dong", Symbol: "₫", Corridor: "Asia Pacific", USDRate: d("0.000039")},
	{Code: "IDR", Name: "Indonesian Rupiah", Symbol: "Rp", Corridor: "Asia Pacific", USDRate: d("0.000061")},
	{Code: "PHP", Name: "Philippine Peso", Symbol: "₱", Corridor: "Asia Pacific", USDRate: d("0.017")},
	{Code: "MYR", Name: "Malaysian Ringgit", Symbol: "RM", Corridor: "Asia Pacific", USDRate: d("0.23")},
	{Code: "INR", Name: "Indian Rupee", Symbol: "₹", Corridor: "Asia Pacific", USDRate: d("0.012")},
	{Code: "AUD", Name: "Australian Dollar", Symbol: "A$", Corridor: "Asia Pacific", USDRate: d("0.65")},
	{Code: "AED", Name: "UAE Dirham", Symbol: "د.إ", Corridor: "Middle East", USDRate: d("0.272")},
	{Code: "SAR", Name: "Saudi Riyal", Symbol: "﷼", Corridor: "Middle East", USDRate: d("0.267")},
	{Code: "TRY", Name: "Turkish Lira", Symbol: "₺", Corridor: "Middle East", USDRate: d("0.029")},
	{Code: "EUR", Name: "Euro", Symbol: "€", Corridor: "Europe", USDRate: d("1.08")},
	{Code: "GBP", Name: "British Pound", Symbol: "£", Corridor: "Europe", USDRate: d("1.27")},
	{Code: "CHF", Name: "Swiss Franc", Symbol: "Fr", Corridor: "Europe", USDRate: d("1.13")},
	{Code: "RUB", Name: "Russian Ruble", Symbol: "₽", Corridor: "Europe", USDRate: d("0.011")},
	{Code: "USD", Name: "US Dollar", Symbol: "$", Corridor: "Americas", USDRate: d("1")},
	{Code: "CAD", Name: "Canadian Dollar", Symbol: "C$", Corridor: "Americas", USDRate: d("0.73")},
	{Code: "BRL", Name: "Brazilian Real", Symbol: "R$", Corridor: "Americas", USDRate: d("0.17")},
	{Code: "MXN", Name: "Mexican Peso", Symbol: "Mex$", Corridor: "Americas", USDRate: d("0.049")},
	{Code: "NGN", Name: "Nigerian Naira", Symbol: "₦", Corridor: "Africa", USDRate: d("0.00065")},
	{Code: "ZAR", Name: "South African Rand", Symbol: "R", Corridor: "Africa", USDRate: d("0.055")},
	{Code: "KES", Name: "Kenyan Shilling", Symbol: "KSh", Corridor: "Africa", USDRate: d("0.0077")},
}

var byCode = map[string]Asset{}

func init() {
	for i := range fiats {
		fiats[i].Kind = KindFiat
		fiats[i].Scale = 2
	}
	for _, a := range cryptos {
		byCode[a.Code] = a
	}
	for _, a := range fiats {
		byCode[a.Code] = a
	}
}

func Lookup(code string) (Asset, bool) { a, ok := byCode[code]; return a, ok }

// ══ V1 交易范围 ══
//
// 注册表保留全部币种（历史工单要用它查精度），但**能开新单的只有这几种**。
// 范围由这里声明、由目录端点对外发布——放在前端过滤的话，
// 别的客户端拿到的还是全集，范围就成了一句口头约定。
var tradableCrypto = map[string]bool{"USDT": true, "USDC": true}
var tradableFiat = map[string]bool{"CNY": true, "HKD": true, "USD": true}

// Tradable 说这个币现在能不能开新单。撮合、挂单、目录都认它。
func Tradable(code string) bool { return tradableCrypto[code] || tradableFiat[code] }

// Cryptos / Fiats 只列 V1 能交易的——目录、撮合、挂单校验都认它。
func Cryptos() []Asset { return filterTradable(cryptos) }
func Fiats() []Asset   { return filterTradable(fiats) }

// AllCryptos 是注册表全集。钱包要列的是「你持有什么」，
// 不是「你能交易什么」——历史留下的 BTC/ETH 余额不能因为下架就看不见了。
func AllCryptos() []Asset { return cryptos }

func filterTradable(in []Asset) []Asset {
	out := make([]Asset, 0, len(in))
	for _, a := range in {
		if Tradable(a.Code) {
			out = append(out, a)
		}
	}
	return out
}

func IsCrypto(code string) bool { a, ok := Lookup(code); return ok && a.Kind == KindCrypto }
func IsFiat(code string) bool   { a, ok := Lookup(code); return ok && a.Kind == KindFiat }

// Corridors 把法币按走廊分组，对齐前端的分组下拉。
func Corridors() []struct {
	Group  string  `json:"group"`
	Assets []Asset `json:"assets"`
} {
	order := []string{"Greater China", "Asia Pacific", "Middle East", "Europe", "Americas", "Africa"}
	out := make([]struct {
		Group  string  `json:"group"`
		Assets []Asset `json:"assets"`
	}, 0, len(order))
	for _, g := range order {
		var list []Asset
		for _, f := range Fiats() {
			if f.Corridor == g {
				list = append(list, f)
			}
		}
		if len(list) == 0 {
			continue // 一个都不剩的走廊不发，免得前端画一个空分组
		}
		out = append(out, struct {
			Group  string  `json:"group"`
			Assets []Asset `json:"assets"`
		}{g, list})
	}
	return out
}
