package kyc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const secret = "whsec_test"

func headers(t time.Time, body []byte) (string, string) {
	ts := strconv.FormatInt(t.Unix(), 10)
	return Sign(secret, ts, body), ts
}

func TestVerifyWebhookAcceptsGenuine(t *testing.T) {
	body := []byte(`{"event":"docupass_conclusive","decision":"accept"}`)
	now := time.Now()
	sig, ts := headers(now, body)
	if err := VerifyWebhook(secret, sig, ts, body, now); err != nil {
		t.Fatalf("真回调被拒了: %v", err)
	}
}

// 签名是按原始字节算的。一个空格的差别就该拒——正是这一点决定了
// handler 里必须拿 raw body 去验，不能先反序列化再重新编码。
func TestVerifyWebhookRejectsTamperedBody(t *testing.T) {
	body := []byte(`{"decision":"review"}`)
	now := time.Now()
	sig, ts := headers(now, body)
	if err := VerifyWebhook(secret, sig, ts, []byte(`{"decision":"accept"}`), now); err == nil {
		t.Fatal("改过的 body 居然验过了")
	}
}

func TestVerifyWebhookRejectsWrongSecret(t *testing.T) {
	body := []byte(`{"a":1}`)
	now := time.Now()
	sig, ts := headers(now, body)
	if err := VerifyWebhook("whsec_other", sig, ts, body, now); err != ErrBadSignature {
		t.Fatalf("换了密钥还是过了: %v", err)
	}
}

// 抓到过的那一份签名永远有效，能挡住重放的只有时间。
func TestVerifyWebhookRejectsReplay(t *testing.T) {
	body := []byte(`{"a":1}`)
	sent := time.Now().Add(-30 * time.Minute)
	sig, ts := headers(sent, body)
	if err := VerifyWebhook(secret, sig, ts, body, time.Now()); err != ErrStale {
		t.Fatalf("半小时前的回调被放过了: %v", err)
	}
}

// 没配密钥时必须报错，不能当成「不用验」。
func TestVerifyWebhookRefusesWithoutSecret(t *testing.T) {
	if err := VerifyWebhook("", "v1=x", "1", []byte(`{}`), time.Now()); err == nil {
		t.Fatal("没配密钥却放行了")
	}
}

func TestVerifyWebhookRejectsMissingHeaders(t *testing.T) {
	if err := VerifyWebhook(secret, "", "", []byte(`{}`), time.Now()); err != ErrNoSignature {
		t.Fatal("没带头的请求被放过了")
	}
}

// 同一个键正反面各有一项时，按置信度挑——不是按证件的排版顺序碰运气。
func TestDataMapPicksHighestConfidence(t *testing.T) {
	d := DataMap{"lastName": {
		{Value: "LIU", Confidence: 0.62, Source: "visual"},
		{Value: "Liu", Confidence: 0.98, Source: "MRZ"},
	}}
	if got := d.Get("lastName"); got != "Liu" {
		t.Fatalf("挑错了: %q", got)
	}
}

func TestDataMapFirstFallsThrough(t *testing.T) {
	d := DataMap{"personalNumber": {{Value: "A123", Confidence: 1}}}
	if got := d.First("documentNumber", "personalNumber"); got != "A123" {
		t.Fatalf("没退到第二个键: %q", got)
	}
	if got := d.First("nope"); got != "" {
		t.Fatalf("凭空造了个值: %q", got)
	}
}

func TestIdentityMasksDocumentNumber(t *testing.T) {
	r := &Result{Data: DataMap{
		"documentNumber": {{Value: "H12345678", Confidence: 1}},
		"dob":            {{Value: "1992/04/16", Confidence: 1}},
		"documentType":   {{Value: "P", Confidence: 1}},
		"sex":            {{Value: "F", Confidence: 1}},
	}}
	id := r.Identity()
	if id.DocNumber != "•••••5678" {
		t.Fatalf("证件号没打码: %q", id.DocNumber)
	}
	if id.DOB != "1992-04-16" {
		t.Fatalf("日期没转成表单的格式: %q", id.DOB)
	}
	if id.DocType != "Passport" || id.Sex != "Female" {
		t.Fatalf("代码没翻成人话: %q %q", id.DocType, id.Sex)
	}
}

// 认不出来的代码原样留着：编一个看着像样的说法，比显示一个看不懂的原文更糟。
func TestIdentityKeepsUnknownCodes(t *testing.T) {
	r := &Result{Data: DataMap{"documentType": {{Value: "Z", Confidence: 1}}}}
	if got := r.Identity().DocType; got != "Z" {
		t.Fatalf("把不认识的类型编成了别的: %q", got)
	}
}

func TestBaseForRegion(t *testing.T) {
	if BaseFor("eu") != BaseEU || BaseFor("EU") != BaseEU {
		t.Fatal("eu 没解析成法兰克福那一侧")
	}
	if BaseFor("") != BaseUS || BaseFor("nonsense") != BaseUS {
		t.Fatal("认不出的区域没退回 US")
	}
}

// 上游那句原话必须带回来：「HTTP 403」查不出是 key 的权限不够、配置档不存在，
// 还是 key 不属于这个区——三件事的处理方式完全不同。
func TestAPIErrorCarriesUpstreamReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"success":false,"error":{"status":403,` +
			`"code":"ERROR_KEY_FORBIDDEN","message":"Your api key does not have permission."}}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k", "security_medium").
		CreateDocuPass(context.Background(), CreateOpts{Mode: 0})
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("没拿到 APIError: %v", err)
	}
	if ae.Code != "ERROR_KEY_FORBIDDEN" || !strings.Contains(ae.Msg, "permission") {
		t.Fatalf("上游的原话丢了: %+v", ae)
	}
	// 4xx 是配错了，不是上游在抖——照着它说「稍后重试」会让人一直重试
	// 一个永远好不了的东西。
	if !ae.Permanent() {
		t.Fatal("403 被当成了可重试的错")
	}
}

func TestAPIErrorTreatsServerErrorsAsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "k", "p").CreateDocuPass(context.Background(), CreateOpts{})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Permanent() {
		t.Fatalf("502 应当算可重试: %v", err)
	}
}

// reference 在井号后面，查询串必须放在井号之前——放后面的话整段落进 hash，
// location.search 是空的，他们前端那句 urlParams.get('l') 读不到，等于没写。
func TestWithLanguagePutsParamBeforeTheHash(t *testing.T) {
	got := withLanguage("https://v.idanalyzer.com/#USABC123", "en")
	want := "https://v.idanalyzer.com/?l=en#USABC123"
	if got != want {
		t.Fatalf("链接拼错了:\n 得到 %s\n 期望 %s", got, want)
	}
}

func TestWithLanguageKeepsUpstreamChoice(t *testing.T) {
	in := "https://v.idanalyzer.com/?l=cn#USABC123"
	if got := withLanguage(in, "en"); got != in {
		t.Fatalf("把对方已经写好的语言覆盖了: %s", got)
	}
}

// 语言没配就不该动这条链接。
func TestWithLanguageNoopWhenUnset(t *testing.T) {
	in := "https://v.idanalyzer.com/#USABC123"
	if got := withLanguage(in, ""); got != in {
		t.Fatalf("不该动: %s", got)
	}
}

// 解析不了宁可语言不对，也不能把一条本来能用的链接改坏。
func TestWithLanguageKeepsUnparseableURL(t *testing.T) {
	in := "://nonsense"
	if got := withLanguage(in, "en"); got != in {
		t.Fatalf("把坏链接改得更坏了: %s", got)
	}
}
