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
func (c *Client) Stream(ctx context.Context, msgs []Msg, onDelta func(string) error) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":      c.Model,
		"messages":   msgs,
		"stream":     true,
		"max_tokens": c.MaxTokens,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach the model: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		/* 错误体读一截就够，而且必须截断：上游 502 时回的是一整页 HTML，
		   整个塞进日志没人看得下去。 */
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("model returned %d: %s", resp.StatusCode,
			strings.TrimSpace(string(snippet)))
	}

	return c.read(resp.Body, onDelta)
}

// read 解 SSE。格式固定：一行 `data: {json}`，空行分隔，最后一行 `data: [DONE]`。
func (c *Client) read(r io.Reader, onDelta func(string) error) (string, error) {
	var full strings.Builder
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
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue // 解不开的那一帧跳过，别为一帧毁掉整段回答
		}
		if ev.Error != nil {
			return full.String(), fmt.Errorf("model: %s", ev.Error.Message)
		}
		for _, ch := range ev.Choices {
			if ch.Delta.Content == "" {
				continue
			}
			full.WriteString(ch.Delta.Content)
			if err := onDelta(ch.Delta.Content); err != nil {
				return full.String(), err
			}
		}
	}
	if err := sc.Err(); err != nil {
		/* 已经吐出去的字要带回去：上层据此把「答了一半」存下来，
		   而不是让用户看见的内容和库里存的对不上。 */
		return full.String(), fmt.Errorf("read the model stream: %w", err)
	}
	return full.String(), nil
}
