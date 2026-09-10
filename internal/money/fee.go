package money

import "github.com/shopspring/decimal"

// 平台手续费。
//
// 按吃单方的法币金额收，8 个基点（0.08%）。存的是基点而不是百分数：
// 百分数写成小数在配置里迟早被人写成 0.08 和 8 混着用，基点是整数，
// 没有这个歧义。
//
// 费率会变，但**每一单收多少在下单那一刻就定死**——所以订单表里存的是
// 算好的金额，不是费率。改费率不该动到历史单上写的那个数。
const TakerFeeBps = 8

// Fee 按法币金额算手续费。四舍五入到该法币的最小单位。
func Fee(fiatAmount decimal.Decimal, fiat string) decimal.Decimal {
	f := fiatAmount.Mul(decimal.NewFromInt(TakerFeeBps)).Div(decimal.NewFromInt(10000))
	return f.Round(int32(Scale(fiat)))
}
