package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/makerreview"
	"github.com/advaita/atara-pay/internal/store"
)

// MakerSubmitReq 是九步 KYC 或挂单配置的一次提交。
//
// 表单整体存 JSON blob，不逐列建模：九步字段过于零碎且前端仍在改，
// 后端跟着改会一直破。这里只校验它是合法 JSON 且非空，不校验业务语义。
type MakerSubmitReq struct {
	Phase string          `json:"phase"` // kyc | listing
	Form  json.RawMessage `json:"form"`
}

func (s *Service) MakerApplication(ctx context.Context, userID string) (*store.MakerApp, error) {
	a, err := s.St.MakerApp(ctx, userID)
	if err != nil {
		// 还没申请过不是错误——前端那颗按钮要显示「Become a maker →」。
		return &store.MakerApp{UserID: userID, Phase: "kyc", FormJSON: "{}"}, nil
	}
	return a, nil
}

func (s *Service) SubmitMakerApplication(ctx context.Context, userID string,
	req MakerSubmitReq) (*store.MakerApp, error) {
	if req.Phase != "kyc" && req.Phase != "listing" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "BAD_PHASE", "phase",
			"phase must be kyc or listing")
	}
	if len(req.Form) == 0 || !json.Valid(req.Form) {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "FORM_REQUIRED", "form",
			"send the form you filled in")
	}
	cur, err := s.St.MakerApp(ctx, userID)
	if err != nil {
		cur = &store.MakerApp{UserID: userID, Phase: "kyc"}
	}
	// 身份没审过就不能提挂单配置——跳段等于让没审身份的人直接挂单。
	if req.Phase == "listing" && !cur.KYCOk {
		return nil, httpx.Fail(http.StatusConflict, "KYC_NOT_APPROVED", "phase",
			"your identity check has not cleared yet")
	}
	next := store.MakerApp{UserID: userID, Phase: req.Phase, FormJSON: mergeForms(cur.FormJSON, req.Phase, req.Form),
		KYCDone: cur.KYCDone, KYCOk: cur.KYCOk, ListingDone: cur.ListingDone, Approved: cur.Approved}
	if req.Phase == "kyc" {
		next.KYCDone = true
	} else {
		next.ListingDone = true
	}

	/* 规则层当场审一遍。

	   能写成规则的绝不交给模型：这一层稳定、可测试、不会变笨，模型故障时
	   它照常工作（PRD §8.4「规则模板与模型判断的双层结构」）。规则层挑出
	   问题的，直接打回，不必再花一次模型调用——结论已经确定了。 */
	issues := makerreview.CheckKYC(req.Form)
	if req.Phase == "listing" {
		issues = makerreview.CheckListing(req.Form)
	}
	verdict := makerreview.VerdictOf(issues)
	source := makerreview.SourceRule
	modelID, latency := "", 0

	/* 规则层没话说的，才轮到模型层。

	   反过来不行：规则层已经确定的结论没有理由再花一次模型调用，而且
	   「必填项空着」那种话由模型来说既慢又可能说岔。 */
	if verdict == makerreview.Pass && s.MakerAI != nil {
		t0 := time.Now()
		r, err := s.MakerAI.Review(ctx, req.Phase, req.Form)
		latency = int(time.Since(t0).Milliseconds())
		switch {
		case errors.Is(err, makerreview.ErrOff):
			// 这一层关着：什么都没发生，照规则层的结论走。
		case err != nil:
			/* 模型挂了不是「你材料有问题」。转人工、保持不放行，
			   而且不编一条指摘出来——这一刻出问题的是我们。
			   （MAKER-REVIEW-AI.md §6） */
			log.Printf("maker review: 模型层 %s/%s: %v", userID, req.Phase, err)
			issues, verdict, source = r.Issues, r.Verdict, makerreview.SourceAI
			modelID = s.Cfg.Desk.Model
		default:
			issues, verdict, source = r.Issues, r.Verdict, makerreview.SourceAI
			modelID = s.Cfg.Desk.Model
		}
	}

	// 只有规则层没话说的才继续往下走。有话说就当场落定，闹钟不上——
	// 一份已经有结论的申请没有理由再等钟。
	if verdict == makerreview.Pass {
		/* 收下材料先进「审核中」，隔一会儿由调度器放行。

		   真实环境这一步是有人看件的，但那条路在演示里是个死胡同：没人去
		   审核台点一下，提交完的账户就永远停在审核中，后面全走不下去。

		   时间写进库、由每秒一次的 sweep 来推，不是起个睡 5 秒的 goroutine：
		   进程重启后 goroutine 就没了，申请会永远卡在审核中。 */
		if d := s.Cfg.T.MakerReview; d > 0 {
			t := time.Now().Add(d)
			next.AutoReviewAt = &t
		}
	}
	if err := s.St.UpsertMakerApp(ctx, next); err != nil {
		return nil, err
	}

	/* 留痕先写，再落裁决。

	   写失败不回给用户：留痕是给事后复盘用的，而申请人这一刻在等一个回复。
	   为了一行日志把他的提交判成失败，是把两件事的轻重弄反了。 */
	if err := s.St.InsertMakerReview(ctx, store.MakerReview{
		UserID: userID, Stage: req.Phase,
		Source: source, Verdict: string(verdict),
		IssuesJSON: issuesJSON(issues), InputHash: store.HashInput(req.Form),
		ModelID: modelID, LatencyMs: latency,
	}); err != nil {
		log.Printf("maker review: 留痕 %s/%s: %v", userID, req.Phase, err)
	}

	if verdict != makerreview.Pass {
		/* 打回与转人工都写进 reject_reason，由 ReviewMakerApp 走既有那条路：
		   它会把「交过了」那一位留着、把闹钟摘掉，前端据此把表单带着原内容
		   重新铺开。两种出口在库里长得一样，区别在 maker_reviews 那一行的
		   verdict 上——转人工的那些等人来点，打回的那些等用户改。 */
		if err := s.St.ReviewMakerApp(ctx, userID, req.Phase, "reject",
			makerreview.Summary(issues), ""); err != nil {
			return nil, err
		}
	}
	return s.St.MakerApp(ctx, userID)
}

// issuesJSON 把逐项问题序列化进留痕。序列化不出来也不能让提交失败。
func issuesJSON(issues []makerreview.Issue) string {
	b, err := json.Marshal(issues)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// SweepMakerReviews 放行到点的准入申请。由调度器每秒调一次。
//
// 一份失败不能挡住其它份——记下来接着走，跟工单那条 sweep 一个规矩。
func (s *Service) SweepMakerReviews(ctx context.Context, now time.Time) error {
	due, err := s.St.DueMakerApps(ctx, now)
	if err != nil {
		return err
	}
	for _, a := range due {
		stage := ""
		switch {
		case a.KYCDone && !a.KYCOk:
			stage = "kyc"
		case a.ListingDone && !a.Approved:
			stage = "listing"
		}
		if stage == "" {
			// 已经被人审过了（审核台先点了），到期时间留着没意义，清掉。
			if err := s.St.ClearMakerAutoReview(ctx, a.UserID); err != nil {
				log.Printf("maker review: clear %s: %v", a.UserID, err)
			}
			continue
		}
		if err := s.St.AutoApproveMakerApp(ctx, a.UserID, stage); err != nil {
			log.Printf("maker review: %s %s: %v", a.UserID, stage, err)
			continue
		}
		if err := s.St.EnsureMerchant(ctx, a.UserID, docsOf(a.FormJSON)); err != nil {
			log.Printf("maker review: %s 画像: %v", a.UserID, err)
		}
	}
	return nil
}

// mergeForms 把两段提交分别留着：{"kyc":{…},"listing":{…}}。
//
// 原来两段共用一个 blob，交完挂单配置，身份那份就被覆盖没了。那份材料是
// 用户交上来的记录——回执里「查看提交内容 · 25 项」要照着它渲染，覆盖掉
// 之后那条历史消息就只剩一句话。
func mergeForms(prev, phase string, form json.RawMessage) string {
	old := map[string]json.RawMessage{}
	_ = json.Unmarshal([]byte(prev), &old)
	out := map[string]json.RawMessage{}
	// 只认这两个键。老库里存的是单张表单（键是 kind/surname 那些），
	// 整个搬过来会把表单字段混成阶段名。
	for _, k := range []string{"kyc", "listing"} {
		if v, ok := old[k]; ok {
			out[k] = v
		}
	}
	out[phase] = form
	b, err := json.Marshal(out)
	if err != nil {
		return string(form)
	}
	return string(b)
}

// docsOf 说这个申请人到底提供了哪几样资质件。
//
// 六个标记的含义写在前端的 DOCS 里，这里必须跟它对上，而且**只认真收过的**：
// 挂单卡上那一排是给买家看的凭据，凭空点亮就是在替他做担保。所以
// PoF（资金证明）、Stmts（流水）、Chain（链上溯源）一律不点——这一版
// 根本没收过、也没跑过链上筛查。
func docsOf(formJSON string) map[string]bool {
	var forms struct {
		KYC map[string]any `json:"kyc"`
	}
	_ = json.Unmarshal([]byte(formJSON), &forms)
	k := forms.KYC
	// multi 类字段存的是数组，空数组不算填过——「点开看了一眼」跟
	// 「真的选了来源」是两回事。
	has := func(name string) bool {
		v, ok := k[name]
		if !ok || v == nil || v == "" {
			return false
		}
		if a, isArr := v.([]any); isArr {
			return len(a) > 0
		}
		return true
	}
	docs := map[string]bool{
		// 身份审过了，这一条才是这次流程真正证成的东西
		"kyc": true,
		// 财富来源：个人表是 sow，企业表是 csow / mainrev
		"sow": has("sow") || has("csow") || has("mainrev"),
		// 授权文件只对企业主体有意义
		"poa": k["kind"] == "Corporate",
	}
	return docs
}

type MakerReviewReq struct {
	Stage    string `json:"stage"`    // kyc | listing
	Decision string `json:"decision"` // approve | reject
	Reason   string `json:"reason"`
}

// ReviewMakerApplication 是真人审核。拒绝必须给理由——用户看不到理由就
// 不知道该改什么，只会反复提交同一份材料。
func (s *Service) ReviewMakerApplication(ctx context.Context, reviewerID, userID string,
	req MakerReviewReq) (*store.MakerApp, error) {
	if req.Decision == "reject" && req.Reason == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "REASON_REQUIRED", "reason",
			"say why — without it they will just resend the same thing")
	}
	if err := s.St.ReviewMakerApp(ctx, userID, req.Stage, req.Decision, req.Reason, reviewerID); err != nil {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "BAD_REVIEW", "",
			"stage must be kyc or listing, decision must be approve or reject, and the application must exist")
	}
	return s.St.MakerApp(ctx, userID)
}
