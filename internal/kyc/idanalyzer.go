// Package kyc 接 ID Analyzer 的 DocuPass —— 托管的证件 + 活体核验流程。
//
// 这一层只负责跟 ID Analyzer 讲话：建会话、拉结果、把它那套字段读成我们的
// 结构。放行与否、写不写库，是 app 层的事。
//
// ── 为什么整件事必须在服务端 ──
//
// APIKey 是账户级凭据：拿到它就能替我们建会话、读任何一次核验的完整人像与
// 证件号。它一旦进了浏览器产物就是公开文件。ID Analyzer 自己的文档把这条写
// 成硬规矩——应用只应持有短效的 reference，绝不可直接调 POST /docupass 或
// GET /docupass/{reference}。所以这个包没有任何一处把 APIKey 交出去，
// 前端拿到的只有 reference 和那条 URL。
//
// 同理，DocuPass 结束时前端那个回调只是「用户点完了」的 UI 信号，不是结论。
// 结论只有两个来源：ID Analyzer 打给我们的 webhook，或者我们自己去拉。
package kyc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 两个区域的接入点。个人信息只存放在提交去的那一侧，不跨区同步——
// 选哪个既是延迟问题也是合规问题（EU 那侧是为 GDPR 留的）。
const (
	BaseUS = "https://api2.idanalyzer.com"
	BaseEU = "https://api2-eu.idanalyzer.com"
)

// BaseFor 把区域名解析成接入点。认不出来的一律当 US——
// 这个值配错时退回默认比启动失败合理：它只影响连哪一侧，不影响对错，
// 而真配错了第一次调用就会因为 API key 不属于该区而报错，跑不远。
func BaseFor(region string) string {
	if strings.EqualFold(strings.TrimSpace(region), "eu") {
		return BaseEU
	}
	return BaseUS
}

// APIError 是上游回的一个错。
//
// 带着状态码是因为调用方要分两类：4xx 是配置问题（key 的权限不对、配置档
// 不存在、区域不匹配），重试一万次也是同一个结果；5xx 和网络错误才是
// 「过一会儿再试」。混成一句话，界面就会对着一个永远好不了的问题说「稍后重试」。
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("idanalyzer HTTP %d %s: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("idanalyzer HTTP %d: %s", e.Status, e.Msg)
}

// Permanent 说这个错重试没有意义。
func (e *APIError) Permanent() bool { return e.Status >= 400 && e.Status < 500 }

// Client 是一个 ID Analyzer 账户。零值不可用，必须走 New。
type Client struct {
	base    string
	apiKey  string
	profile string
	hc      *http.Client
}

func New(base, apiKey, profile string) *Client {
	if base == "" {
		base = BaseUS
	}
	return &Client{
		base: strings.TrimRight(base, "/"), apiKey: apiKey, profile: profile,
		// 建会话和拉结果都是小请求，但对方偶尔会慢。给一个明确的上限，
		// 免得一次卡住的调用把我们的 handler 一起拖住。
		hc: &http.Client{Timeout: 20 * time.Second},
	}
}

// Session 是一次 DocuPass 会话。
//
// Reference 是这次会话的短效标识，也是唯一可以下发给浏览器的东西。
// URL 是托管流程的地址；QRCode 是同一条 URL 的二维码（PNG data URL），
// 桌面端没摄像头时让人用手机扫。
type Session struct {
	Reference string `json:"reference"`
	URL       string `json:"url"`
	QRCode    string `json:"qrCode"`
}

// CreateOpts 是建会话时那几个每链接可覆盖的选项。
type CreateOpts struct {
	// Mode 决定让用户做什么：0 证件+人脸，1 只证件，2 只人脸，3 只签署。
	// 我们要的是 0——OTC 的法币腿要认人，只读证件不做人脸等于没认。
	Mode int
	// CustomData 会原样出现在 webhook 和拉取结果里。我们放自己的 user id，
	// 结果回来才知道是谁——ID Analyzer 不认识我们的用户。
	CustomData string
	// Language 空表示让 DocuPass 按浏览器语言自己选。
	Language string
}

// CreateDocuPass 建一次会话。
//
// version 固定 3：v3 是当前的 DocuPass，而且每条链接单次有效——
// 可复用链接在 v3 已废弃，也不该要：一条能重复用的核验链接等于
// 谁拿到谁都能顶着这个人的名义走一遍。
func (c *Client) CreateDocuPass(ctx context.Context, o CreateOpts) (*Session, error) {
	body := map[string]any{
		"version": 3,
		"mode":    o.Mode,
		"profile": c.profile,
	}
	if o.CustomData != "" {
		body["customData"] = o.CustomData
	}
	if o.Language != "" {
		body["language"] = o.Language
	}
	var s Session
	if err := c.do(ctx, http.MethodPost, "/docupass", body, &s); err != nil {
		return nil, err
	}
	if s.Reference == "" {
		return nil, fmt.Errorf("idanalyzer: 建会话成功但没给 reference")
	}
	s.URL = withLanguage(s.URL, o.Language)
	return &s, nil
}

// withLanguage 把界面语言钉在链接上。
//
// 为什么不能只靠建会话时那个 language 参数：它是写进会话数据里的，托管页面
// 要等会话加载完才 setOnce() 覆盖过来。而页面一开始是按 navigator.language
// 铺的——中文系统上就是先铺一遍中文，再看会话说什么。加载慢或者会话没取到，
// 停住的那一版就是中文。
//
// 他们前端读的是查询串里的 l：
//
//	let paramLanguage = new URLSearchParams(location.search).get('l');
//	if (paramLanguage !== "" && paramLanguage in this.languageTable) return paramLanguage;
//
// 这一步在挑语言的最前面，先于浏览器猜，所以钉上去就一定是它。
//
// 关键是位置：reference 在 **井号后面**（.../#USABC…），查询串必须放在井号
// 之前，否则它整段落进 hash 里，location.search 是空的，等于没写。
func withLanguage(raw, lang string) string {
	if raw == "" || lang == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		// 解析不了就原样返回：宁可语言不对，也不能把一条本来能用的链接改坏。
		return raw
	}
	q := u.Query()
	if q.Get("l") != "" {
		return raw // 对方已经写了，不覆盖
	}
	q.Set("l", lang)
	u.RawQuery = q.Encode()
	return u.String()
}

// GetDocuPass 拉一次会话的当前结果。
//
// 这是权威读法之一（另一个是 webhook），两条路回来的是同一份 JSON。
// 返回原始字节是有意的：ID Analyzer 的字段随 profile 配置而变，我们只解析
// 认得的那几个，其余整份存下来——出了纠纷要查的是它当时到底说了什么，
// 而不是我们当时解析出了什么。
func (c *Client) GetDocuPass(ctx context.Context, reference string) (*Result, []byte, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/docupass/"+reference, nil, &raw); err != nil {
		return nil, nil, err
	}
	r, err := ParseResult(raw)
	if err != nil {
		return nil, raw, err
	}
	return r, raw, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-KEY", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("idanalyzer %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	// 限一下读取量：对方回的是我们没法预判大小的 JSON，
	// 不设上限的话一份异常响应就能把内存吃掉。
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// 把对方的原话带上——「HTTP 403」查不出是 key 的权限不够、配置档不存在，
		// 还是 key 不属于这个区。这三件事的处理方式完全不同。
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &body)
		msg := body.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(b))
		}
		return &APIError{Status: res.StatusCode, Code: body.Error.Code, Msg: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("idanalyzer %s %s: 响应不是预期的 JSON: %w", method, path, err)
	}
	return nil
}
