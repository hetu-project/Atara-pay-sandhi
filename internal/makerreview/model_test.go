package makerreview

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/advaita/atara-pay/internal/desk"
)

/*
白名单钉死。

这个测试的作用不是「验证代码对」，是**让任何一次增删都必须显式改这里**。
往外发一个新字段应当是一个需要有人点头的动作，不是一次顺手的 append。
*/
func TestOutboundWhitelistIsPinned(t *testing.T) {
	want := strings.Join([]string{
		"amlpolicy", "bizindustry", "bizscope", "city", "csow", "empstatus",
		"headcount", "highrisk", "income", "industry", "kind", "mainrev",
		"nationality", "province", "regcountry", "sanction", "sow",
		"taxcountry", "turnover",
	}, ",")
	if got := strings.Join(Outbound(), ","); got != want {
		t.Fatalf("身份材料出网字段变了。\n现在: %s\n钉的: %s\n"+
			"改这张表要先回答：模型少了它，具体判不了哪一条？", got, want)
	}
}

// 身份标识一个都不许出网。白名单漏写一条只是少一项判断依据，
// 而黑名单漏写一条会把身份证号发出去——所以这里正面钉一遍。
func TestNoIdentifiersEverLeave(t *testing.T) {
	full := json.RawMessage(`{
		"kind":"Corporate","company":"Golden Gate Ltd","regno":"CR-2841996",
		"regcountry":"Hong Kong","city":"Shenzhen","street":"1 Queens Rd","zip":"999077",
		"bizindustry":"Cross-border e-commerce","bizscope":"collections for merchants",
		"turnover":"Under 5M","headcount":"1–10","csow":["Operations"],"mainrev":"fees",
		"sanction":"No","highrisk":"No","amlpolicy":"Yes",
		"repname":"L Cheung","repid":"E12345678","repphone":"+852 9000 0000",
		"dirname":"M Fong","dirid":"K998877","ubo":"L Cheung","uboid":"E12345678",
		"csign":"signed","estdate":"2024-03-01"}`)
	safe, err := RedactKYC(full)
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	banned := []string{
		"Golden Gate Ltd", "CR-2841996", "1 Queens Rd", "999077",
		"L Cheung", "E12345678", "+852 9000 0000", "M Fong", "K998877",
		"signed", "2024-03-01",
	}
	for _, b := range banned {
		if strings.Contains(string(safe), b) {
			t.Fatalf("身份信息出网了: %q\n发出去的是: %s", b, safe)
		}
	}
	// 该留的要留下，否则模型什么也判不了。
	for _, k := range []string{"Shenzhen", "Hong Kong", "Cross-border e-commerce", "Under 5M"} {
		if !strings.Contains(string(safe), k) {
			t.Fatalf("判断依据被误删: %q", k)
		}
	}
}

// 白名单之外的键要**不存在**，不是置空——置空会让模型看见「这里有东西被
// 藏起来了」，进而开始猜。
func TestRedactedKeysAreAbsentNotBlank(t *testing.T) {
	safe, _ := RedactKYC(json.RawMessage(`{"kind":"Individual","idno":"E1"}`))
	var m map[string]any
	_ = json.Unmarshal(safe, &m)
	if _, ok := m["idno"]; ok {
		t.Fatalf("idno 不该出现在发出去的对象里: %s", safe)
	}
}

// 模型编一个没发给它的字段出来，这条必须被丢掉。
//
// 放过去的话，用户会收到一条指着他在表上找不到的字段的意见——那是
// 「不可验证的指摘」最坏的形态，而可验证正是这一层唯一的防退化手段。
func TestInventedFieldsAreDropped(t *testing.T) {
	sent := json.RawMessage(`{"kind":"Individual","income":"Under 500k"}`)
	raw := `{"issues":[
		{"fields":["passport_number"],"says":"x","ask":"y"},
		{"fields":["income","passport_number"],"says":"a","ask":"b"}]}`
	is, ok := parse(raw, sent)
	if !ok {
		t.Fatalf("这份应当解析得出来")
	}
	if len(is) != 1 {
		t.Fatalf("编出来的字段没被丢掉: %+v", is)
	}
	if len(is[0].Fields) != 1 || is[0].Fields[0] != "income" {
		t.Fatalf("混在一条里的假字段没被剔掉: %+v", is[0])
	}
}

// 模型不出终局：它找出来的一律是「你去改」。改不了的那几类（制裁、PEP、
// 高风险辖区）由规则层认，不归它判。
func TestModelIssuesAlwaysRouteToRevise(t *testing.T) {
	sent := json.RawMessage(`{"income":"Under 500k"}`)
	raw := `{"issues":[{"fields":["income"],"says":"a","ask":"b","route":"escalate"}]}`
	is, _ := parse(raw, sent)
	if len(is) != 1 || is[0].Route != ToRevise {
		t.Fatalf("模型不该能自己要求转人工: %+v", is)
	}
}

// 说不清哪里不对、或者不说怎么改的，都留不下来。
func TestIncompleteIssuesAreDropped(t *testing.T) {
	sent := json.RawMessage(`{"income":"x"}`)
	raw := `{"issues":[
		{"fields":["income"],"says":"  ","ask":"b"},
		{"fields":["income"],"says":"a","ask":""},
		{"fields":[],"says":"a","ask":"b"}]}`
	if is, _ := parse(raw, sent); len(is) != 0 {
		t.Fatalf("残缺的指摘应当全部丢掉: %+v", is)
	}
}

// 没找到问题是一个完全正常的结果，跟「返回了一坨读不出的东西」必须分开。
func TestEmptyIssuesIsAValidAnswer(t *testing.T) {
	is, ok := parse(`{"issues":[]}`, json.RawMessage(`{"a":1}`))
	if !ok {
		t.Fatalf("空 issues 是有效回答")
	}
	if VerdictOf(is) != Pass {
		t.Fatalf("没找到问题就该放行")
	}
}

func TestUnparseableAnswerIsNotSilentlyPassed(t *testing.T) {
	for _, raw := range []string{"", "I think it looks fine", "{oops"} {
		if _, ok := parse(raw, json.RawMessage(`{"a":1}`)); ok {
			t.Fatalf("读不出的回答不该被当成「没问题」: %q", raw)
		}
	}
}

// 围栏是模型很常见的一个习惯，不该因此判成故障。
func TestCodeFenceIsTolerated(t *testing.T) {
	raw := "```json\n{\"issues\":[{\"fields\":[\"income\"],\"says\":\"a\",\"ask\":\"b\"}]}\n```"
	is, ok := parse(raw, json.RawMessage(`{"income":"x"}`))
	if !ok || len(is) != 1 {
		t.Fatalf("带围栏的回答应当能读出来: ok=%v is=%+v", ok, is)
	}
}

// 模型层没开的时候要说「没开」，不能返回一个看起来像结论的东西。
func TestOffReturnsErrOff(t *testing.T) {
	var a *AI
	if _, err := a.Review(nil, "kyc", nil); err != ErrOff {
		t.Fatalf("没配客户端时应当返回 ErrOff, got %v", err)
	}
	if _, err := (&AI{}).Review(nil, "kyc", nil); err != ErrOff {
		t.Fatalf("Client 为 nil 时应当返回 ErrOff, got %v", err)
	}
}

// 技术故障一律转人工，且不编一条指摘出来——这一刻出问题的是我们，
// 不是他的材料。
func TestFailureEscalatesWithoutBlamingTheForm(t *testing.T) {
	r := escalated("")
	if r.Verdict != Escalate {
		t.Fatalf("裁决 = %s", r.Verdict)
	}
	if !strings.Contains(r.Issues[0].Ask, "nothing for you to change") {
		t.Fatalf("不该让用户去改一个没有错的地方: %q", r.Issues[0].Ask)
	}
}

// The listing stage must never reach the model: every field on it is
// structured and the rules already decide all of it. A call there would spend
// money and a few seconds of the applicant's time to repeat what the rule
// layer had already told them.
func TestListingNeverReachesTheModel(t *testing.T) {
	a := &AI{Client: &desk.Client{}}
	_, err := a.Review(context.Background(), "listing", json.RawMessage(`{"coins":["USDT"]}`))
	if err != ErrOff {
		t.Fatalf("listing should be off, got %v", err)
	}
}
