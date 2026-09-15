package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/kyc"
	"github.com/advaita/atara-pay/internal/store"
)

// 身份核验：托管的 DocuPass 流程接进来。
//
// ── 谁说了算 ──
//
// 浏览器里那个流程跑完时会回调一次「完成了」。那**不是**结论——ID Analyzer
// 自己的文档把这条写得很直白：onFinish 只是 UI 信号。任何一个打开控制台的人
// 都能在控制台里手敲一句"我完成了"。所以放行只认两个来源：
//
//   1. ID Analyzer 打给我们的 webhook（带 HMAC 签名，见 internal/kyc/webhook.go）
//   2. 我们拿自己的 API key 去 GET /docupass/{reference}
//
// 这两条路回来的是同一份 JSON，所以落库走同一个函数（ingest）。
//
// ── 为什么拉取是主路径而不是兜底 ──
//
// webhook URL 配在门户的 KYC 配置档上，要求公网可达的 HTTPS。本地开发根本
// 收不到；用内置预设（security_medium 那几个）时压根没有配置档可配。
// 把放行绑死在回调上，等于这套东西只有在生产环境、且门户配置正确时才工作。
// 所以前端每次问状态，我们顺手去拉一次——回调到了就是白拉一次，没到也不耽误。

// ErrKycUnknownSession 说这条回调指向一次我们没开过的会话。
//
// 单独拎出来是为了让 handler 回 200 而不是 500：这种情况重投多少次都是
// 同一个结果，而 ID Analyzer 会按 10 分钟 / 1 小时 / 12 小时 / 24 小时
// 重试四轮。让它重试等于给自己攒一队永远消化不掉的回调。
var ErrKycUnknownSession = errors.New("回调指向一次我们没开过的会话")

// kycClient 按当前配置造一个客户端。没配就返回 nil。
func (s *Service) kycClient() *kyc.Client {
	if !s.Cfg.KYC.Configured() {
		return nil
	}
	return kyc.New(kyc.BaseFor(s.Cfg.KYC.Region), s.Cfg.KYC.APIKey, s.Cfg.KYC.Profile)
}

var errKycNotConfigured = httpx.Fail(http.StatusInternalServerError, "KYC_NOT_CONFIGURED", "",
	"identity verification is not configured on this server")

// KycSession 是下发给浏览器的那一份。
//
// 只有 reference、URL 和二维码——API key 留在这里。前端拿 reference 去开
// 托管流程，拿不到任何可以替我们调 ID Analyzer 的东西。
type KycSession struct {
	Reference string `json:"reference"`
	URL       string `json:"url"`
	QRCode    string `json:"qr_code,omitempty"`
}

// StartKyc 给这个用户开一次核验会话。
//
// customData 放我们的 user id：ID Analyzer 不认识我们的用户，结果回来时
// 全靠它认人。不放的话，一份回调到手只知道"有人验过了"。
func (s *Service) StartKyc(ctx context.Context, userID string) (*KycSession, error) {
	if s.Cfg.KYC.Simulated() {
		return s.simulateKyc(ctx, userID)
	}
	c := s.kycClient()
	if c == nil {
		return nil, errKycNotConfigured
	}
	sess, err := c.CreateDocuPass(ctx, kyc.CreateOpts{
		Mode: s.Cfg.KYC.Mode, CustomData: userID, Language: s.Cfg.KYC.Language,
	})
	if err != nil {
		log.Printf("kyc: 建会话 %s: %v", userID, err)
		// 4xx 是这台机器配错了，不是上游在抖：让人「稍后重试」只会让他一直重试
		// 一个永远好不了的东西。把上游的原话带回去——那句话里写着到底缺什么。
		var ae *kyc.APIError
		if errors.As(err, &ae) && ae.Permanent() {
			return nil, httpx.Fail(http.StatusInternalServerError, "KYC_MISCONFIGURED", "",
				"identity verification is misconfigured on this server: "+ae.Msg)
		}
		return nil, httpx.Fail(http.StatusBadGateway, "KYC_UPSTREAM", "",
			"could not reach the verification service — try again in a moment")
	}
	if err := s.St.StartKycCheck(ctx, sess.Reference, userID); err != nil {
		return nil, err
	}
	return &KycSession{Reference: sess.Reference, URL: sess.URL, QRCode: sess.QRCode}, nil
}

/*
simulateKyc 是本地开发用的核验：不打上游，当场判过。

走的是**跟真核验同一条落库路径**（StartKycCheck → SaveKycResult → MarkKycOk），
不是在别处另开一个「假装通过」的分支。这样模拟和真实产生的库状态完全一样，
本地测出来的行为就是线上的行为；另写一条路的话，两边迟早会长得不一样，
而差异只会在生产上暴露。

签发的证件号写死成 SIMULATED-xxxx，而且姓名是 Test Applicant——
任何一眼扫过数据库或界面的人都该立刻看出这不是一份真的核验。
*/
func (s *Service) simulateKyc(ctx context.Context, userID string) (*KycSession, error) {
	ref := "sim-" + store.NewID()
	if err := s.St.StartKycCheck(ctx, ref, userID); err != nil {
		return nil, err
	}
	id := kyc.Identity{
		FirstName: "Test", LastName: "Applicant", FullName: "Test Applicant",
		DocType: "Passport", DocNumber: "SIMULATED-" + ref[len(ref)-4:],
		DOB: "1990-01-01", Issued: "2020-01-01", Expiry: "2030-01-01",
		Sex: "Male", Nationality: "Hong Kong", Country: "HKG",
	}
	idJSON, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	if err := s.St.SaveKycResult(ctx, ref, store.KycResult{
		Status: "accept", Event: "simulated", Source: "simulated",
		IdentityJSON: string(idJSON), WarningsJSON: "[]", RawJSON: "{}",
	}); err != nil {
		return nil, err
	}
	if err := s.St.MarkKycOk(ctx, userID); err != nil {
		return nil, err
	}
	log.Printf("kyc: 模拟核验（ATARA_KYC=false）user=%s ref=%s", userID, ref)
	// URL 留空：没有东西可开。前端据此不去弹窗口，直接显示已通过。
	return &KycSession{Reference: ref}, nil
}

// KycStatus 是前端那个查询接口的返回。
type KycStatus struct {
	// State: none | pending | accept | review | reject
	// none 表示这个人从没开过会话——跟"开了还没走完"不是一回事，
	// 界面上一个该显示"开始核验"，一个该显示"继续"。
	State     string        `json:"state"`
	Reference string        `json:"reference,omitempty"`
	Identity  *kyc.Identity `json:"identity,omitempty"`
	Warnings  []kyc.Warning `json:"warnings,omitempty"`
	// KycOk 是这个账户当前能不能下单。它看的是历史上有没有通过过，
	// 不是最近这一次的状态——见 store.LatestPassedKycCheck 的说明。
	KycOk       bool       `json:"kyc_ok"`
	ConcludedAt *time.Time `json:"concluded_at,omitempty"`
	// Configured 说这台机器配没配 ID Analyzer。没配时前端要说
	// "这台机器没开身份核验"，而不是显示一颗按不动的按钮。
	//
	// 模拟模式下它是 true：流程是通的，按钮该能按。真假由下面那个字段说。
	Configured bool `json:"configured"`
	// Simulated 说刚才那一步没有真的验过任何东西。
	//
	// 必须单独发出来、而且界面上必须显式说出来：一个「已通过」的绿勾背后
	// 是真核验还是本地开关，看的人有权知道。藏起来的话，谁截个图就能拿去
	// 当作「我们验过了」。
	Simulated bool `json:"simulated,omitempty"`
}

// KycStatusFor 查这个用户的当前状态，顺带去上游拉一次。
//
// refresh=false 时只读库——列表页之类的地方不该每次渲染都打一次上游。
func (s *Service) KycStatusFor(ctx context.Context, userID string, refresh bool) (*KycStatus, error) {
	sim := s.Cfg.KYC.Simulated()
	out := &KycStatus{
		State:      "none",
		Configured: s.Cfg.KYC.Configured() || sim,
		Simulated:  sim,
	}

	// 能不能下单看的是"有没有通过过"，不是最近这一次。一个已经验过的人
	// 又开了一次新会话时，他的身份不该在那一刻变回未验证。
	if passed, err := s.St.LatestPassedKycCheck(ctx, userID); err == nil && passed != nil {
		out.KycOk = true
	} else if !errors.Is(err, sql.ErrNoRows) && err != nil {
		return nil, err
	}

	cur, err := s.St.LatestKycCheck(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}

	// 还没有结论就去拉一次。已经有结论的不再拉：那份结果不会再变，
	// 每次进页面都打一次上游是白花钱（DocuPass 按次计费）。
	// 模拟模式不拉上游：那边没有这条会话，拉一次只会拿回一个 404
	// 并在日志里刷一行看着像故障的错。
	if refresh && !cur.Concluded() && !sim {
		if c := s.kycClient(); c != nil {
			if updated, err := s.pull(ctx, c, cur); err != nil {
				// 拉不到不是错误：人可能还没走完，上游也可能正好在抖。
				// 记一笔，把库里那一份照实返回。
				log.Printf("kyc: 拉取 %s: %v", cur.Reference, err)
			} else if updated != nil {
				cur = updated
			}
		}
	}

	out.State = cur.Status
	out.Reference = cur.Reference
	out.ConcludedAt = cur.ConcludedAt
	if cur.Status == "accept" {
		out.KycOk = true
	}
	if cur.IdentityJSON != "" && cur.IdentityJSON != "{}" {
		var id kyc.Identity
		if err := json.Unmarshal([]byte(cur.IdentityJSON), &id); err == nil {
			out.Identity = &id
		}
	}
	if cur.WarningsJSON != "" {
		var w []kyc.Warning
		if err := json.Unmarshal([]byte(cur.WarningsJSON), &w); err == nil {
			out.Warnings = w
		}
	}
	return out, nil
}

// pull 去上游读一次这次会话的结果，有结论就落库。
func (s *Service) pull(ctx context.Context, c *kyc.Client, cur *store.KycCheck) (*store.KycCheck, error) {
	r, raw, err := c.GetDocuPass(ctx, cur.Reference)
	if err != nil {
		return nil, err
	}
	if err := s.ingest(ctx, cur.Reference, cur.UserID, r, raw, "pull"); err != nil {
		return nil, err
	}
	return s.St.KycCheck(ctx, cur.Reference)
}

// IngestWebhook 收下一条回调。签名在 handler 里已经验过。
//
// 回调会重投（失败自动重试 4 次，门户里还能手动重发 48 小时），而且可能乱序。
// 所以这里必须幂等，也必须认得出"这条比库里那份旧"。
func (s *Service) IngestWebhook(ctx context.Context, r *kyc.Result, raw []byte) error {
	// 只有 conclusive 是"这个人走完了"。new 只说明他刚传了一张照片，
	// 按它放行等于没核。update 是事后改判，要跟。
	if r.Event != kyc.EventConclusive && r.Event != kyc.EventUpdate {
		return nil
	}
	ref := r.DocuPass
	if ref == "" {
		return ErrKycUnknownSession
	}
	cur, err := s.St.KycCheck(ctx, ref)
	if errors.Is(err, sql.ErrNoRows) {
		// 认不出的会话不建行。customData 是我们自己写进去的 user id，
		// 但一条我们没开过的会话说自己属于某个用户时，没有理由信它——
		// 那正是伪造回调想做的事。签名挡住了大部分，这里是第二道。
		return ErrKycUnknownSession
	}
	if err != nil {
		return err
	}
	// 乱序保护：已经有结论的会话，只接受 update（事后改判），
	// 不接受迟到的 conclusive 把它盖回去。
	if cur.Concluded() && r.Event != kyc.EventUpdate {
		return nil
	}
	return s.ingest(ctx, ref, cur.UserID, r, raw, "webhook")
}

// ingest 是两条路共用的落库口：webhook 和主动拉取回来的是同一份 JSON。
func (s *Service) ingest(ctx context.Context, reference, userID string,
	r *kyc.Result, raw []byte, source string) error {
	status := r.Decision
	switch status {
	case kyc.DecisionAccept, kyc.DecisionReview, kyc.DecisionReject:
	default:
		// 还没有决策就是还没走完。不要把空字符串写成一个状态。
		status = "pending"
	}

	id := r.Identity()
	idJSON, _ := json.Marshal(id)
	warnJSON, _ := json.Marshal(r.Warnings)

	if err := s.St.SaveKycResult(ctx, reference, store.KycResult{
		Status:        status,
		TransactionID: r.TransactionID,
		DocupassID:    r.DocuPass,
		ProfileID:     r.ProfileID,
		ReviewScore:   r.ReviewScore,
		RejectScore:   r.RejectScore,
		Event:         r.Event,
		IdentityJSON:  string(idJSON),
		WarningsJSON:  string(warnJSON),
		RawJSON:       string(raw),
		Source:        source,
	}); err != nil {
		return err
	}

	// 只有 accept 放行。review 是"要人看一眼"，不是"过了"——
	// 把它当通过，这套核验就只剩一个装饰作用。
	if status == kyc.DecisionAccept {
		if err := s.St.MarkKycOk(ctx, userID); err != nil {
			return err
		}
	}
	return nil
}
