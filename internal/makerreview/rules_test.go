package makerreview

import (
	"encoding/json"
	"strings"
	"testing"
)

func fieldsOf(issues []Issue) string {
	out := []string{}
	for _, i := range issues {
		out = append(out, strings.Join(i.Fields, "+"))
	}
	return strings.Join(out, ",")
}

// 每条指摘都必须指到字段。指不到字段的意见是不可验证的，而「可验证」正是
// 这一层唯一的防退化手段——模型哪天变笨了，用户会指着一条不存在的问题喊。
func TestEveryIssueNamesFields(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"kind":"Corporate"}`),
		json.RawMessage(`{"coins":["BTC"],"lo":"900","hi":"100"}`),
	}
	for _, c := range cases {
		for _, is := range [][]Issue{CheckKYC(c), CheckListing(c)} {
			for _, i := range is {
				if len(i.Fields) == 0 {
					t.Fatalf("有一条没指到字段: %+v", i)
				}
				if i.Says == "" || i.Ask == "" {
					t.Fatalf("说了哪里不对就要说怎么改: %+v", i)
				}
			}
		}
	}
}

// 干净的一份要放行。规则层最容易写坏的方向是「宁可多拦」，那会让每个人
// 都被打回一次。
func TestCleanListingPasses(t *testing.T) {
	ok := json.RawMessage(`{
		"dir":["Sell crypto"],"coins":["USDT","USDC"],"lo":"1000","hi":"50000",
		"nets":["BSC"],"pricing":"Float","spread":"0.8",
		"rails":["ICBC"],"agree":true}`)
	if is := CheckListing(ok); len(is) != 0 {
		t.Fatalf("干净的配置被拦了: %s", fieldsOf(is))
	}
	if v := VerdictOf(CheckListing(ok)); v != Pass {
		t.Fatalf("裁决 = %s, 期望 pass", v)
	}
}

// 表单能选 BTC/ETH，后端只结算 USDT/USDC。审的时候不说，人就会通过了准入
// 再去撞挂单那一步的墙——那堵墙在界面上是看不见的。
func TestUntradableCoinIsCaught(t *testing.T) {
	raw := json.RawMessage(`{
		"dir":["Sell crypto"],"coins":["BTC","ETH"],"lo":"1000","hi":"5000",
		"nets":["BSC"],"pricing":"Float","spread":"0.8",
		"rails":["ICBC"],"agree":true}`)
	is := CheckListing(raw)
	if len(is) != 1 || is[0].Fields[0] != "coins" {
		t.Fatalf("没抓到不可交易的币种: %s", fieldsOf(is))
	}
	if !strings.Contains(is[0].Says, "BTC") || !strings.Contains(is[0].Says, "ETH") {
		t.Fatalf("指摘要点名是哪几个币: %q", is[0].Says)
	}
	if VerdictOf(is) != Revise {
		t.Fatalf("选错币种是能改的，应当打回而不是转人工")
	}
}

// 部分不可交易时不要把整份否掉——其余币种是好的，说清楚就行。
func TestPartlyUntradableCoinsKeepsTheRest(t *testing.T) {
	raw := json.RawMessage(`{
		"dir":["Sell crypto"],"coins":["USDT","BTC"],"lo":"1000","hi":"5000",
		"nets":["BSC"],"pricing":"Float","spread":"0.8",
		"rails":["ICBC"],"agree":true}`)
	is := CheckListing(raw)
	if len(is) != 1 || !strings.Contains(is[0].Says, "The rest") {
		t.Fatalf("部分不可交易应当保留其余的: %+v", is)
	}
}

func TestLimitOrderIsCaught(t *testing.T) {
	raw := json.RawMessage(`{
		"dir":["Sell crypto"],"coins":["USDT"],"lo":"50000","hi":"1000",
		"nets":["BSC"],"pricing":"Float","spread":"0.8",
		"rails":["ICBC"],"agree":true}`)
	is := CheckListing(raw)
	if len(is) != 1 || is[0].Fields[0] != "lo" {
		t.Fatalf("没抓到下限高于上限: %s", fieldsOf(is))
	}
}

// 千分位是有效金额，不该被当成没填。
func TestThousandSeparatorIsAccepted(t *testing.T) {
	raw := json.RawMessage(`{
		"dir":["Buy crypto"],"coins":["USDT"],"lo":"1,000","hi":"50,000",
		"nets":["BSC"],"pricing":"Fixed","fixed":"7.28",
		"rails":["ICBC"],"agree":true}`)
	if is := CheckListing(raw); len(is) != 0 {
		t.Fatalf("带千分位的金额被拦了: %s", fieldsOf(is))
	}
}

func TestSpreadOutOfBand(t *testing.T) {
	raw := json.RawMessage(`{
		"dir":["Sell crypto"],"coins":["USDT"],"lo":"1000","hi":"5000",
		"nets":["BSC"],"pricing":"Float","spread":"40",
		"rails":["ICBC"],"agree":true}`)
	is := CheckListing(raw)
	if len(is) != 1 || is[0].Fields[1] != "spread" {
		t.Fatalf("没抓到越界的点差: %s", fieldsOf(is))
	}
}

// 缺项要一条说完，且点名是哪几项——十条「某某是空的」刷满一屏，
// 人反而找不到自己漏了什么。
func TestMissingFieldsAreOneIssue(t *testing.T) {
	is := CheckKYC(json.RawMessage(`{"kind":"Individual"}`))
	if len(is) != 1 {
		t.Fatalf("缺项应当合成一条: %s", fieldsOf(is))
	}
	if !strings.Contains(is[0].Says, "Nationality") || !strings.Contains(is[0].Says, "Signature") {
		t.Fatalf("要点名缺了哪几项: %q", is[0].Says)
	}
}

// 声明「是」的三件事申请人改不了，必须转人工，不能写成「请修改」——
// 那等于让人对着一个他永远满足不了的要求反复提交。
func TestUnfixableDeclarationsEscalate(t *testing.T) {
	base := `"company":"A","regno":"1","regcountry":"Hong Kong","bizindustry":"x",
	         "turnover":"Under 5M","csow":["Operations"],"ubo":"L","csign":"s",
	         "highrisk":"No","kind":"Corporate"`
	raw := json.RawMessage(`{` + base + `,"sanction":"Yes"}`)
	is := CheckKYC(raw)
	if VerdictOf(is) != Escalate {
		t.Fatalf("受制裁声明应当转人工, issues=%+v", is)
	}
	for _, i := range is {
		if i.Fields[0] != "sanction" {
			continue
		}
		if i.Route != ToEscalate {
			t.Fatalf("受制裁那条应当转人工, got route=%q", i.Route)
		}
		// 措辞也要说明这一点：让人去「改」一件他改不了的事，他只会反复重交。
		if !strings.Contains(i.Ask, "nothing for you to change") {
			t.Fatalf("不该要求申请人去改一件他改不了的事: %q", i.Ask)
		}
	}
}

func TestPepEscalates(t *testing.T) {
	raw := json.RawMessage(`{"kind":"Individual","nationality":"China","surname":"L",
		"firstname":"J","idtype":"Passport","idno":"E1","taxcountry":"China",
		"sow":["Salary"],"income":"500k–2M","sign":"s","pep":"Yes"}`)
	if v := VerdictOf(CheckKYC(raw)); v != Escalate {
		t.Fatalf("PEP 要加强尽调，裁决 = %s, 期望 escalate", v)
	}
}

// 一条转人工就整份转人工：改不了的那条挡在前面，让人先去改别的没有意义。
func TestEscalateBeatsRevise(t *testing.T) {
	v := VerdictOf([]Issue{
		{Fields: []string{"a"}, Route: ToRevise},
		{Fields: []string{"b"}, Route: ToEscalate},
	})
	if v != Escalate {
		t.Fatalf("裁决 = %s, 期望 escalate", v)
	}
}

// 读不出来的表单是「我读不了」，不是「你材料有问题」——技术故障不能
// 表达成打回（MAKER-REVIEW-AI.md §6）。
func TestUnreadableFormEscalates(t *testing.T) {
	for _, is := range [][]Issue{
		CheckKYC(json.RawMessage(`not json`)),
		CheckListing(json.RawMessage(`not json`)),
	} {
		if VerdictOf(is) != Escalate {
			t.Fatalf("读不出的表单应当转人工, got %+v", is)
		}
	}
}

func TestSummaryJoinsIssues(t *testing.T) {
	if Summary(nil) != "" {
		t.Fatalf("没有问题就没有摘要")
	}
	s := Summary([]Issue{{Says: "First."}, {Says: "Second."}})
	if s != "First. Second." {
		t.Fatalf("摘要 = %q", s)
	}
}
