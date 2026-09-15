// Package desk 是 Atara AI 对话台：把一句话连同这个账户的当前状态交给模型，
// 把回答一个字一个字地送回去。
//
// 它不是 internal/agent 那一套。那边三个接口（Parser / RiskAssessor /
// ReleaseConsensus）都是给风控用的，输入输出都是结构化的判定；这里要的是
// 自然语言对话，硬塞进那个接口只会让两件事都别扭。
package desk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Msg 是一轮对话里的一条。role 取 system / user / assistant——
// DeepSeek 兼容 OpenAI 的 chat completions 格式。
type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Usage 是一次调用的 token 用量。开启 stream_options.include_usage 后，
// 流的最后一帧（choices 为空）会带上它。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Client 是模型侧的最小封装。只做一件事：把消息发出去，把增量吐回来。
type Client struct {
	APIKey    string
	BaseURL   string
	Model     string
	MaxTokens int
	HTTP      *http.Client
}

func New(apiKey, baseURL, model string, maxTokens int) *Client {
	return &Client{
		APIKey: apiKey, BaseURL: strings.TrimRight(baseURL, "/"),
		Model: model, MaxTokens: maxTokens,
		/* 没有总超时：流式回答本来就要开着连接几十秒，一刀切会把正常的
		   长回答砍断。控制交给调用方传进来的 ctx——那样取消的理由是明确的
		   （用户关了页面 / 上层超时），而不是「连接活太久了」。 */
		HTTP: &http.Client{},
	}
}

// Stream 把一轮对话发给模型，每收到一段文字就调一次 onDelta。
//
// 返回完整文本。onDelta 返回 error 时立刻停止（用于「客户端断开了，别再算了」）。
// 这是保持原签名的薄封装；要 token 用量走 StreamUsage。
func (c *Client) Stream(ctx context.Context, msgs []Msg, onDelta func(string) error) (string, error) {
	full, _, err := c.StreamUsage(ctx, msgs, onDelta)
	return full, err
}

// StreamUsage 同 Stream，但多回一份 token 用量（供后台调用日志记账）。
//
// 靠 stream_options.include_usage 让上游在流末尾带上 usage——不额外发一次
// 非流式请求去问用量（那是重复计费）。上游若不支持这个开关，usage 会是零值，
// 记账那边把它当「没拿到」处理即可。
func (c *Client) StreamUsage(ctx context.Context, msgs []Msg, onDelta func(string) error) (string, Usage, error) {
	body, err := json.Marshal(map[string]any{
		"model":          c.Model,
		"messages":       msgs,
		"stream":         true,
		"max_tokens":     c.MaxTokens,
		"stream_options": map[string]any{"include_usage": true},
	})
	if err != nil {
		return "", Usage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", Usage{}, fmt.Errorf("reach the model: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		/* 错误体读一截就够，而且必须截断：上游 502 时回的是一整页 HTML，
		   整个塞进日志没人看得下去。 */
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", Usage{}, fmt.Errorf("model returned %d: %s", resp.StatusCode,
			strings.TrimSpace(string(snippet)))
	}

	return c.read(resp.Body, onDelta)
}

// read 解 SSE。格式固定：一行 `data: {json}`，空行分隔，最后一行 `data: [DONE]`。
func (c *Client) read(r io.Reader, onDelta func(string) error) (string, Usage, error) {
	var full strings.Builder
	var usage Usage
	// 结束原因留到最后看：一个字都没说的时候，它是唯一能说明原因的东西。
	finish := ""
	sc := bufio.NewScanner(r)
	/* 默认 64KB 的行上限对 SSE 不够稳妥：一段带长引用的 delta 就能超。
	   撞上限时 Scanner 会直接停，表现是「回答莫名其妙断在中间」。 */
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue // 心跳与注释行
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue // 解不开的那一帧跳过，别为一帧毁掉整段回答
		}
		if ev.Error != nil {
			return full.String(), usage, fmt.Errorf("model: %s", ev.Error.Message)
		}
		// usage 在最后一帧（choices 为空）才出现，留到这里覆盖。
		if ev.Usage != nil {
			usage = *ev.Usage
		}
		for _, ch := range ev.Choices {
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
			if ch.Delta.Content == "" {
				continue
			}
			full.WriteString(ch.Delta.Content)
			if err := onDelta(ch.Delta.Content); err != nil {
				return full.String(), usage, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		/* 已经吐出去的字要带回去：上层据此把「答了一半」存下来，
		   而不是让用户看见的内容和库里存的对不上。 */
		return full.String(), usage, fmt.Errorf("read the model stream: %w", err)
	}
	/*
		撞了 max_tokens 而且一个字没说——这必须是个错误，不能静静地返回空串。

		推理模型（deepseek-flash 这类）的 max_tokens 限的是**推理加回答的总和**。
		上限给小了，推理还没想完就到顶，答案永远没机会写出来：finish_reason
		是 length，content 是 0 字节，而 HTTP 一切正常、错误帧也没有。

		实测这一路的返回长这样：
		    finish_reason=length  content=0  reasoning_tokens=800/800

		返回 ("", nil) 的话，每个调用方都得自己记得「空串其实是失败」——
		而它看起来像一个正常的空回答。这一类静默失败最贵：它不报警，
		只是偶尔什么都不做，而且因为推理长度随机，它是时好时坏的。
	*/
	if full.Len() == 0 && finish == "length" {
		return "", usage, fmt.Errorf(
			"model hit the %d-token cap before writing an answer "+
				"(reasoning models spend the cap on thinking — raise DEEPSEEK_MAX_TOKENS)",
			c.MaxTokens)
	}
	return full.String(), usage, nil
}
