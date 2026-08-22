// Package money 提供资金值对象与资产目录。
//
// 金额一律用 decimal，禁止 float64 参与任何资金运算——
// int64 最小单位在 ETH 上会溢出（18 位精度下 1 ETH = 1e18，int64 上限约 9.2e18）。
package money

import (
	"fmt"

	"github.com/shopspring/decimal"
)

type Kind string

const (
	KindCrypto Kind = "crypto"
	KindFiat   Kind = "fiat"
)

// Asset 是目录里的一种资产。数字资产与法币同构，用 Kind 区分。
type Asset struct {
	Code     string          `json:"code"`
	Kind     Kind            `json:"kind"`
	Name     string          `json:"name"`
	Symbol   string          `json:"symbol"`
	Scale    int32           `json:"scale"`
	Networks []string        `json:"networks,omitempty"` // crypto
	Corridor string          `json:"corridor,omitempty"` // fiat 走廊分组
	USDRate  decimal.Decimal `json:"-"`                  // 折算 USD 用于授权卡额度口径
}

// Amount 是「数值 + 资产」的值对象。序列化成字符串，不用 number，
// 避免 JSON 解析端的 float 精度损失。
type Amount struct {
	Value decimal.Decimal
	Asset string
}

func New(v decimal.Decimal, asset string) Amount { return Amount{Value: v, Asset: asset} }

func Parse(s, asset string) (Amount, error) {
	v, err := decimal.NewFromString(s)
	if err != nil {
		return Amount{}, fmt.Errorf("not a number: %q", s)
	}
	return Amount{Value: v, Asset: asset}, nil
}

func (a Amount) IsZero() bool        { return a.Value.IsZero() }
func (a Amount) IsPositive() bool    { return a.Value.GreaterThan(decimal.Zero) }
func (a Amount) Add(b Amount) Amount { return Amount{a.Value.Add(b.Value), a.Asset} }
func (a Amount) Sub(b Amount) Amount { return Amount{a.Value.Sub(b.Value), a.Asset} }
func (a Amount) Cmp(b Amount) int    { return a.Value.Cmp(b.Value) }
func (a Amount) String() string      { return a.Value.String() }

// Display 按资产精度向下取整。舍入只发生在序列化边界，且一律 ROUND_DOWN——
// 展示层不得凭空多出资金。
func (a Amount) Display() string {
	return a.Value.RoundDown(Scale(a.Asset)).String()
}

// USD 把金额折成 USD 口径，用于授权卡的单笔上限与周期额度。
func (a Amount) USD() decimal.Decimal {
	as, ok := Lookup(a.Asset)
	if !ok {
		return a.Value
	}
	return a.Value.Mul(as.USDRate)
}

func Scale(code string) int32 {
	if a, ok := Lookup(code); ok {
		return a.Scale
	}
	return 2
}

func Zero(asset string) Amount { return Amount{decimal.Zero, asset} }
