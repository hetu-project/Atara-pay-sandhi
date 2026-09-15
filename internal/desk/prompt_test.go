package desk

import (
	"strings"
	"testing"
)

// 三种准入状态在界面上不是一回事。合并成「未通过」会让等着审核的人
// 以为自己被拒了——这是这个函数存在的全部理由。
func TestStageTellsSubmittedFromRejected(t *testing.T) {
	cases := []struct {
		submitted, approved bool
		want                string
	}{
		{false, false, "not submitted yet"},
		{true, false, "submitted, under review"},
		{true, true, "approved"},
	}
	for _, c := range cases {
		if got := stage(c.submitted, c.approved); got != c.want {
			t.Errorf("stage(%v,%v) = %q，想要 %q", c.submitted, c.approved, got, c.want)
		}
	}
}

func TestSnapshotCarriesTheNumbers(t *testing.T) {
	s := Snapshot{
		Name: "Demo", Address: "0xabc", WalletKind: "atara",
		IDState: "accept", KycOK: true, ListingDone: true, Approved: false,
		TotalUSD: "34500", EscrowUSD: "1200",
		Balances: []Balance{{Asset: "USDT", Network: "BSC", OnChain: "34500", InEscrow: "0", USD: "34500"}},
		Orders: []OrderLine{{Ref: "ATR-8F42C1", Amount: "5000", Asset: "USDT",
			Counterparty: "Golden Gate", State: "s3", Phase: "Send the transfer",
			Actor: "waiting on you", Updated: "3 minutes ago"}},
		Allowances: []string{"agent-x — up to 100 USDT per payment"},
	}
	txt := s.Text()

	for _, want := range []string{
		"Demo", "0xabc", "34500", "1200", "USDT", "BSC",
		"ATR-8F42C1", "Golden Gate", "Send the transfer", "waiting on you", "3 minutes ago",
		"agent-x",
		"submitted, under review", // listing 交了没过
		"passed",                  // 身份核验过了
		"cannot post listings",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("快照里没有 %q：模型答不出来的东西，多半是这里就没给\n%s", want, txt)
		}
	}
}

// 空账户不能让模型以为「我看不到」——空和看不到是两回事。
func TestSnapshotSaysEmptyOutLoud(t *testing.T) {
	txt := Snapshot{Name: "New", WalletKind: "atara"}.Text()
	for _, want := range []string{"Empty wallet", "No orders yet", "None issued", "None posted",
		"not submitted yet"} {
		if !strings.Contains(txt, want) {
			t.Errorf("空账户的快照里少了 %q\n%s", want, txt)
		}
	}
}

// 法币那句必须无条件出现：钱包里从来没有法币行，而「我的人民币余额」
// 是必然会被问到的。不写死这一句，模型只会说「我看不到」。
func TestSnapshotAlwaysExplainsFiat(t *testing.T) {
	for _, s := range []Snapshot{{}, {Balances: []Balance{{Asset: "USDT"}}}} {
		if !strings.Contains(s.Text(), "No fiat is ever held here") {
			t.Error("快照没有说明法币不入账")
		}
	}
}

// 快照要排在历史之前。排在最后模型会把它当成用户刚说的话，
// 回一句「收到你的账户信息」。
func TestBuildPutsSnapshotBeforeHistory(t *testing.T) {
	msgs := Build(Snapshot{Name: "D"}, []Msg{{Role: "user", Content: "hi"}})
	if len(msgs) != 3 {
		t.Fatalf("想要 系统提示 + 快照 + 1 条历史，得到 %d 条", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[1].Role != "system" {
		t.Errorf("前两条应当都是 system，得到 %q / %q", msgs[0].Role, msgs[1].Role)
	}
	if !strings.Contains(msgs[1].Content, "ACCOUNT SNAPSHOT") {
		t.Error("第二条不是快照")
	}
	if msgs[2].Content != "hi" {
		t.Errorf("历史被挪位了：%q", msgs[2].Content)
	}
}

// 系统提示里那几条硬规矩不能被改没了——它们是这个功能能不能上线的前提。
func TestSystemPromptKeepsTheHardRules(t *testing.T) {
	for _, want := range []string{
		"must come from", // 数字只能来自快照
		"cannot perform", // 不能替人动手
		"same language",  // 跟着用户的语言
		"private key",    // 不许问私钥
		"non-custodial",  // 不许叫人把钱打给 Atara
		"illustrative",   // 演示数据要说明
		"plain text",     // 气泡是纯文本渲染的，markdown 会原样印出来
	} {
		if !strings.Contains(strings.ToLower(system), strings.ToLower(want)) {
			t.Errorf("系统提示里丢了 %q 这条规矩", want)
		}
	}
}

// 身份核验的五种状态必须各说各的。
//
// 这条测试盯的是一次真实的漂移：快照原来读 maker_applications 那两个布尔，
// 而它们只在 accept 时才被写。于是 pending / review / reject 三种状态在模型
// 眼里都是「还没提交」——人刚拍完证件做完活体、正卡在人工复核，问一句
// 「我的核验怎么样了」，得到的是「你还没提交身份验证」。
func TestIdPhraseSeparatesAllFiveStates(t *testing.T) {
	seen := map[string]string{}
	for _, st := range []string{"none", "pending", "accept", "review", "reject"} {
		got := idPhrase(st)
		if got == "" {
			t.Fatalf("%q 没有说法", st)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%q 和 %q 说的是同一句话：%q", st, prev, got)
		}
		seen[got] = st
	}
	/* 三种「不是通过、也不是没开始」的状态，一个都不能被说成没提交。 */
	for _, st := range []string{"pending", "review", "reject"} {
		if strings.Contains(strings.ToLower(idPhrase(st)), "not started") {
			t.Errorf("%q 被说成了「没开始」", st)
		}
	}
	/* 认不出来的状态退回「没开始」，不能是空字符串——空的会让模型自己编。 */
	if idPhrase("something-new") != idPhrase("none") {
		t.Error("没见过的状态该退回「没开始」")
	}
}

// review 的措辞要说清「没你的事了，等着就行」。
// 说成「还有一步要做」的话，人会一遍遍重开核验，而每重开一次就是一次上游计费。
func TestReviewSaysThereIsNothingToDo(t *testing.T) {
	p := strings.ToLower(idPhrase("review"))
	if !strings.Contains(p, "wait") {
		t.Errorf("review 没说清要等：%q", p)
	}
	if !strings.Contains(p, "read") {
		t.Errorf("review 没说清文件已经读过了：%q", p)
	}
}

// 没通过时得说出哪几项没过，并且要说清「还不能下单」。
func TestSnapshotCarriesRejectReasons(t *testing.T) {
	txt := Snapshot{
		Name: "D", WalletKind: "atara", IDState: "reject", KycOK: false,
		IDWarnings: []string{"Face does not match the document photo", "Document expired"},
	}.Text()
	for _, want := range []string{
		"did not pass",
		"Face does not match the document photo",
		"Document expired",
		"cannot place orders",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("快照里少了 %q\n%s", want, txt)
		}
	}
}

// 通过之后不该再挂着「不能下单」那句。
func TestSnapshotDropsTheBlockOnceVerified(t *testing.T) {
	txt := Snapshot{Name: "D", WalletKind: "atara", IDState: "accept", KycOK: true}.Text()
	if strings.Contains(txt, "cannot place orders") {
		t.Errorf("已经验过了还说不能下单\n%s", txt)
	}
}
