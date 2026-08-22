package money

import "github.com/shopspring/decimal"

func d(s string) decimal.Decimal { v, _ := decimal.NewFromString(s); return v }

// 数字资产目录。scale 与网络对齐前端 console.html 的 ASSETS / NETS_OF。
var cryptos = []Asset{
	{Code: "USDT", Kind: KindCrypto, Name: "Tether USD", Symbol: "₮", Scale: 6, Networks: []string{"TRON", "ETH"}, USDRate: d("1")},
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
func Cryptos() []Asset                 { return cryptos }
func Fiats() []Asset                   { return fiats }

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
		for _, f := range fiats {
			if f.Corridor == g {
				list = append(list, f)
			}
		}
		out = append(out, struct {
			Group  string  `json:"group"`
			Assets []Asset `json:"assets"`
		}{g, list})
	}
	return out
}
