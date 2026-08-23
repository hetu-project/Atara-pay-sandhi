// Package condition 实现释放条件：最多 3 个原子的 AND 组合。
//
// 不支持 OR——任一满足即放，等于对手方只需攻破最弱条件。
// 空集 = 立即释放。
package condition

import (
	"errors"
	"fmt"
	"strings"
)

type AtomType string

const (
	Approve  AtomType = "approve"  // 谁确认
	Evidence AtomType = "evidence" // 凭证类型
	Data     AtomType = "data"     // API 数据源 + 指标 + 目标值
	Time     AtomType = "time"     // 绝对日期
)

const Max = 3

type Atom struct {
	Type   AtomType          `json:"atom_type"`
	Params map[string]string `json:"params"`
}

// MainBranch 是托管状态机认的四条主分支。
type MainBranch string

const (
	Immediate   MainBranch = "immediate"
	OnConfirm   MainBranch = "on_confirm"
	ProofWindow MainBranch = "proof_window"
	OnDate      MainBranch = "on_date"
)

// WaitingOn 是「实际在等什么」，给轨道站名与文案用。
// 它与 MainBranch 刻意分开：API data 编译到 on_date 分支，
// 但它等的是指标不是日期。合并会让 UI 上的站名说谎。
type WaitingOn string

const (
	WaitNow      WaitingOn = "now"
	WaitApprove  WaitingOn = "approve"
	WaitEvidence WaitingOn = "evidence"
	WaitData     WaitingOn = "data"
	WaitTime     WaitingOn = "time"
)

type Compiled struct {
	Main    MainBranch
	Waiting WaitingOn
	Text    string
	Params  map[string]string
}

var dataMetrics = map[string][]string{
	"Ad platform API":     {"Clicks", "Conversions", "Impressions"},
	"Logistics API":       {"Delivered", "Signed for", "Out for delivery"},
	"Payment gateway API": {"Payment settled", "Chargeback rate"},
	"On-chain oracle":     {"Transfer confirmed", "Balance"},
}

var proofTypes = []string{"Bank receipt", "Delivery record", "Work delivered"}
var approvers = []string{"Both sides confirm", "I confirm"}

func DataMetrics() map[string][]string { return dataMetrics }
func ProofTypes() []string             { return proofTypes }
func Approvers() []string              { return approvers }

var ErrTooMany = errors.New("at most 3 conditions")

func Validate(atoms []Atom) error {
	if len(atoms) > Max {
		return ErrTooMany
	}
	seen := map[AtomType]bool{}
	for _, a := range atoms {
		switch a.Type {
		case Approve, Evidence, Data, Time:
		default:
			return fmt.Errorf("unknown condition type %q", a.Type)
		}
		if seen[a.Type] {
			return fmt.Errorf("duplicate condition %q", a.Type)
		}
		seen[a.Type] = true
		if a.Type == Data {
			src := a.Params["src"]
			metrics, ok := dataMetrics[src]
			if !ok {
				return fmt.Errorf("unknown data source %q", src)
			}
			if !contains(metrics, a.Params["metric"]) {
				return fmt.Errorf("metric %q is not available on %q", a.Params["metric"], src)
			}
		}
	}
	return nil
}

// Compile 把原子组合编译成状态机认的主分支 + 展示文本。
// 对齐前端 console.html 的 compileConds。
func Compile(atoms []Atom, fallbackDays int) Compiled {
	if len(atoms) == 0 {
		return Compiled{Main: Immediate, Waiting: WaitNow, Text: "immediately", Params: map[string]string{}}
	}
	find := func(t AtomType) *Atom {
		for i := range atoms {
			if atoms[i].Type == t {
				return &atoms[i]
			}
		}
		return nil
	}
	parts := make([]string, 0, len(atoms))
	for _, a := range atoms {
		parts = append(parts, text(a))
	}
	p := map[string]string{}
	main := OnDate
	switch {
	case find(Approve) != nil:
		main = OnConfirm
	case find(Evidence) != nil:
		main = ProofWindow
	}
	waiting := WaitTime
	switch {
	case find(Approve) != nil:
		waiting = WaitApprove
	case find(Evidence) != nil:
		waiting = WaitEvidence
	case find(Data) != nil:
		waiting = WaitData
	}
	if e := find(Evidence); e != nil {
		p["proof"] = e.Params["proof"]
	}
	if t := find(Time); t != nil {
		p["date"] = t.Params["date"]
	} else if find(Data) != nil {
		p["date"] = "the data target"
	}
	if find(Approve) != nil {
		p["within"] = fmt.Sprintf("%d days", fallbackDays)
	}
	return Compiled{Main: main, Waiting: waiting, Text: strings.Join(parts, " + "), Params: p}
}

func text(a Atom) string {
	switch a.Type {
	case Approve:
		if w := a.Params["who"]; w != "" {
			return w
		}
		return "Both sides confirm"
	case Evidence:
		return a.Params["proof"] + " uploaded"
	case Data:
		return strings.TrimSpace(fmt.Sprintf("%s · %s %s", a.Params["src"], a.Params["metric"], a.Params["target"]))
	case Time:
		return "On " + a.Params["date"]
	}
	return string(a.Type)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
