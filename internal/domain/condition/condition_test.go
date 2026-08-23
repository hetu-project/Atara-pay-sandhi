package condition

import "testing"

// main_branch 与 waiting_on 必须能分开：API data 编译到 on_date 分支，
// 但它等的是指标不是日期。合并这两个字段会让轨道站名说谎。
func TestCompileSeparatesBranchFromWaiting(t *testing.T) {
	c := Compile([]Atom{{Type: Data, Params: map[string]string{
		"src": "Logistics API", "metric": "Delivered", "target": "= 1"}}}, 14)
	if c.Main != OnDate {
		t.Fatalf("main = %q, want %q", c.Main, OnDate)
	}
	if c.Waiting != WaitData {
		t.Fatalf("waiting = %q, want %q — the rail must say it waits on the metric", c.Waiting, WaitData)
	}
}

func TestEmptyMeansImmediate(t *testing.T) {
	c := Compile(nil, 14)
	if c.Main != Immediate || c.Waiting != WaitNow {
		t.Fatalf("empty condition set must mean immediate release, got %q/%q", c.Main, c.Waiting)
	}
}

func TestApprovalWinsTheBranch(t *testing.T) {
	c := Compile([]Atom{
		{Type: Evidence, Params: map[string]string{"proof": "Bank receipt"}},
		{Type: Approve, Params: map[string]string{"who": "Both sides confirm"}},
	}, 14)
	if c.Main != OnConfirm {
		t.Fatalf("main = %q, want %q — an explicit approval outranks evidence", c.Main, OnConfirm)
	}
	if c.Text != "Bank receipt uploaded + Both sides confirm" {
		t.Fatalf("text = %q", c.Text)
	}
}

func TestValidateRejects(t *testing.T) {
	four := []Atom{{Type: Approve}, {Type: Evidence}, {Type: Time}, {Type: Data}}
	if err := Validate(four); err == nil {
		t.Fatal("more than 3 atoms must be rejected")
	}
	dup := []Atom{{Type: Approve}, {Type: Approve}}
	if err := Validate(dup); err == nil {
		t.Fatal("duplicate atom types must be rejected")
	}
	badMetric := []Atom{{Type: Data, Params: map[string]string{"src": "Logistics API", "metric": "Clicks"}}}
	if err := Validate(badMetric); err == nil {
		t.Fatal("a metric that does not belong to the source must be rejected")
	}
}
