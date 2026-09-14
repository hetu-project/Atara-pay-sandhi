package desk

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sse 起一个假模型：按给定的行吐 SSE，测试不出网。
func sse(t *testing.T, lines ...string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q，想要 Bearer k", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			fmt.Fprint(w, l)
		}
	}))
	t.Cleanup(srv.Close)
	return New("k", srv.URL, "deepseek-chat", 800)
}

func delta(s string) string {
	return fmt.Sprintf(`data: {"choices":[{"delta":{"content":%q}}]}`+"\n\n", s)
}

func TestStreamJoinsDeltas(t *testing.T) {
	c := sse(t, delta("你"), delta("好"), delta("，Atara"), "data: [DONE]\n\n")
	var got []string
	full, err := c.Stream(context.Background(), []Msg{{Role: "user", Content: "hi"}},
		func(s string) error { got = append(got, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if full != "你好，Atara" {
		t.Errorf("拼出来的整段 = %q", full)
	}
	if len(got) != 3 {
		t.Errorf("onDelta 调了 %d 次，想要 3 次——流式的意义就在于分次到达", len(got))
	}
}

// 心跳行和空行不能被当成内容。漏了这条，界面上会多出莫名的空白。
func TestStreamSkipsNoise(t *testing.T) {
	c := sse(t, ": keep-alive\n\n", "\n", delta("ok"), "data: [DONE]\n\n")
	full, err := c.Stream(context.Background(), nil, func(string) error { return nil })
	if err != nil || full != "ok" {
		t.Fatalf("full=%q err=%v", full, err)
	}
}

// 中间一帧坏掉不该毁掉整段回答。
func TestStreamSurvivesBadFrame(t *testing.T) {
	c := sse(t, delta("a"), "data: {not json\n\n", delta("b"), "data: [DONE]\n\n")
	full, _ := c.Stream(context.Background(), nil, func(string) error { return nil })
	if full != "ab" {
		t.Errorf("full = %q，想要 ab——一帧解不开就跳过它，不是整段作废", full)
	}
}

// onDelta 报错（客户端走了）时要立刻停，并且把已经说出口的带回来：
// 上层据此把「答了一半」存库，库里和屏幕上才一致。
func TestStreamStopsWhenClientLeaves(t *testing.T) {
	c := sse(t, delta("one"), delta("two"), delta("three"), "data: [DONE]\n\n")
	n := 0
	full, err := c.Stream(context.Background(), nil, func(string) error {
		n++
		if n == 2 {
			return fmt.Errorf("client gone")
		}
		return nil
	})
	if err == nil {
		t.Error("客户端走了却没报错")
	}
	if full != "onetwo" {
		t.Errorf("full = %q，想要 onetwo（停在出错那一段，含它自己）", full)
	}
}

// 上游返回 HTML 错误页时，错误信息要可读、且不能把整页塞进去。
func TestStreamReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()
	c := New("k", srv.URL, "m", 10)
	_, err := c.Stream(context.Background(), nil, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v，想要带上状态码", err)
	}
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("err = %v，想要带上上游说的原因", err)
	}
}

// 模型在流里报错（HTTP 200 但帧里是 error）也要认出来。
func TestStreamReportsInStreamError(t *testing.T) {
	c := sse(t, delta("partial"), `data: {"error":{"message":"rate limited"}}`+"\n\n")
	full, err := c.Stream(context.Background(), nil, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v", err)
	}
	if full != "partial" {
		t.Errorf("出错前说过的话要带回来，得到 %q", full)
	}
}
