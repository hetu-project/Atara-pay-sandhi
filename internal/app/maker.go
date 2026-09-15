package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
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
	if errors.Is(err, sql.ErrNoRows) {
		// Never applied is not an error — the button should read "Become a maker →".
		return &store.MakerApp{UserID: userID, Phase: "kyc", FormJSON: "{}"}, nil
	}
	if err != nil {
		/* Anything else is a real failure and must say so.

		   This used to swallow every error as "never applied". A missing column
		   after a schema change then surfaced as "application not found" — which
		   sends whoever is debugging it looking for the wrong thing entirely.
		   Only ErrNoRows means "no such application". */
		return nil, err
	}
	return s.withIssues(ctx, a), nil
}

// withIssues 把最近一次预审的逐项问题挂上。
//
// 只在还挂着问题的时候挂（reject_reason 非空）：通过之后再把上一次被打回的
// 那几条发出去，界面会把已经改好的字段又标红一遍。
//
// 取不到就算了——逐项是锦上添花，摘要那一串已经把话说清楚了，
// 为了标红失败让整个请求挂掉是把轻重弄反。
func (s *Service) withIssues(ctx context.Context, a *store.MakerApp) *store.MakerApp {
	if a == nil || a.RejectReason == "" {
		return a
	}
	rs, err := s.St.MakerReviews(ctx, a.UserID, 1)
	if err != nil || len(rs) == 0 || rs[0].IssuesJSON == "" {
		return a
	}
	a.ReviewIssues = json.RawMessage(rs[0].IssuesJSON)
	a.ReviewSource = rs[0].Source
	a.ReviewModel = rs[0].ModelID
	return a
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
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// First submission: nothing on file yet, start from a blank one.
		cur = &store.MakerApp{UserID: userID, Phase: "kyc"}
	case err != nil:
		// Anything else is a real failure. Treating it as "first submission"
		// would quietly wipe the stage flags of an application that does exist.
		return nil, err
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
			/* 模型读不了不是「你材料有问题」，也不该为此惊动人——绝大多数是
			   一次抖动。材料照收（申请停在「审核中」，这句话此刻是实话：
			   我们确实还没审完），排一次重试，由 sweep 回来重跑。
			   连着失败到上限才转人工。 */
			log.Printf("maker review: 模型层 %s/%s: %v", userID, req.Phase, err)
			if err := s.St.UpsertMakerApp(ctx, next); err != nil {
				return nil, err
			}
			s.deferAIReview(ctx, userID, req.Phase, cur.AIAttempts, err)
			return s.MakerApplication(ctx, userID)
		default:
			issues, verdict, source = r.Issues, r.Verdict, makerreview.SourceAI
			modelID = s.Cfg.Desk.Model
			// 跑成了就把上一次留下的重试摘掉。
			if e := s.St.ClearAIRetry(ctx, userID); e != nil {
				log.Printf("maker review: 清重试 %s: %v", userID, e)
			}
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
	return s.MakerApplication(ctx, userID)
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

// ── 模型层的重试 ────────────────────────────────────────────────────

/*
aiMaxAttempts 是连着失败几次之后转人工。

为什么不无限重试：到了这个次数它已经不是一次抖动，是真出事了——密钥失效、
额度耗尽、上游改了返回格式。那时候该有人知道，而不是让一队申请人在
「审核中」里静静地排到天亮。
*/
const aiMaxAttempts = 5

/*
aiGaveUpMessage 是试满一轮之后写给申请人的那句话。

它必须说明「不用你改」。技术故障和材料问题在申请人那里是两件完全不同的事：
混成一句，他会回到表单里去改一个没有错的地方，改完再交，再被同一个故障
挡回来——而真正出问题的是我们。
*/
const aiGaveUpMessage = "We could not finish the automatic check on this submission. " +
	"A person will review it — there is nothing for you to change."

// aiBackoff 是第 n 次失败之后等多久再试：30s、1m、2m、4m、8m 封顶。
func aiBackoff(attempts int) time.Duration {
	d := 30 * time.Second
	for i := 0; i < attempts && d < 8*time.Minute; i++ {
		d *= 2
	}
	return d
}

// deferAIReview 排一次重试；已经试到上限就转人工。
func (s *Service) deferAIReview(ctx context.Context, userID, stage string, attempts int, cause error) {
	if attempts+1 >= aiMaxAttempts {
		log.Printf("maker review: %s/%s 连续 %d 次跑不成，转人工：%v",
			userID, stage, attempts+1, cause)
		if err := s.St.ClearAIRetry(ctx, userID); err != nil {
			log.Printf("maker review: 清重试 %s: %v", userID, err)
		}
		/* 转人工也不编一条指摘：这一刻出问题的是我们，不是他的材料。
		   说清楚「没能自动审完，有人会看」，比让他去改一个没有错的地方诚实。 */
		if err := s.St.ReviewMakerApp(ctx, userID, stage, "reject",
			aiGaveUpMessage, ""); err != nil {
			log.Printf("maker review: 转人工 %s: %v", userID, err)
		}
		return
	}
	if err := s.St.ScheduleAIRetry(ctx, userID, time.Now().Add(aiBackoff(attempts))); err != nil {
		log.Printf("maker review: 排重试 %s: %v", userID, err)
	}
}

// SweepAIRetries 重跑那些模型层没跑成的。由调度器每秒调一次。
//
// 一份失败不能挡住其它份——记下来接着走，跟另外两条 sweep 一个规矩。
func (s *Service) SweepAIRetries(ctx context.Context, now time.Time) error {
	if s.MakerAI == nil {
		return nil
	}
	due, err := s.St.DueAIRetries(ctx, now)
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
		form := stageForm(a.FormJSON, stage)
		if stage == "" || len(form) == 0 {
			// 已经有结论了（人点过，或者换了阶段），重试没有对象。
			if err := s.St.ClearAIRetry(ctx, a.UserID); err != nil {
				log.Printf("maker review: 清重试 %s: %v", a.UserID, err)
			}
			continue
		}
		t0 := time.Now()
		r, err := s.MakerAI.Review(ctx, stage, form)
		if errors.Is(err, makerreview.ErrOff) {
			/* Nothing for the model to do on this stage (or the layer was
			   switched off since it was queued). Drop it from the queue —
			   retrying would grind to the attempt limit and then wake a
			   person for something that was never going to run. */
			if err := s.St.ClearAIRetry(ctx, a.UserID); err != nil {
				log.Printf("maker review: clear retry %s: %v", a.UserID, err)
			}
			continue
		}
		if err != nil {
			s.deferAIReview(ctx, a.UserID, stage, a.AIAttempts, err)
			continue
		}
		if err := s.St.ClearAIRetry(ctx, a.UserID); err != nil {
			log.Printf("maker review: 清重试 %s: %v", a.UserID, err)
		}
		if err := s.St.InsertMakerReview(ctx, store.MakerReview{
			UserID: a.UserID, Stage: stage,
			Source: makerreview.SourceAI, Verdict: string(r.Verdict),
			IssuesJSON: issuesJSON(r.Issues), InputHash: store.HashInput(form),
			ModelID: s.Cfg.Desk.Model, LatencyMs: int(time.Since(t0).Milliseconds()),
		}); err != nil {
			log.Printf("maker review: 留痕 %s/%s: %v", a.UserID, stage, err)
		}
		if r.Verdict == makerreview.Pass {
			// 通过了就走提交那条路：上闹钟（演示档）或什么都不做（生产档）。
			if d := s.Cfg.T.MakerReview; d > 0 {
				if err := s.St.SetMakerAutoReview(ctx, a.UserID, time.Now().Add(d)); err != nil {
					log.Printf("maker review: 上闹钟 %s: %v", a.UserID, err)
				}
			}
			continue
		}
		if err := s.St.ReviewMakerApp(ctx, a.UserID, stage, "reject",
			makerreview.Summary(r.Issues), ""); err != nil {
			log.Printf("maker review: 落裁决 %s: %v", a.UserID, err)
		}
	}
	return nil
}

// stageForm 从合起来存的那份里取出某一段。取不到就返回空。
func stageForm(raw, stage string) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m[stage]
}

// ── 申诉 ────────────────────────────────────────────────────────────

// MakerAppealReq 是申请人对预审结论的异议。
type MakerAppealReq struct {
	// Note 是他的申辩。必填——空着的申诉在人工那头没有任何可读的东西，
	// 等于只是把同一份材料再排一次队。
	Note string `json:"note"`
}

/*
AppealMakerApplication 收下一次申诉，把这份申请转给人。

为什么必须有这条路：预审判错了而没有任何路径能推翻它，这个商户就被永久
锁在门外——他改也没用，因为他本来就没错。这不是流程问题，是系统里必须
存在一个能推翻机器的出口。它可以一个月零次，但不能不存在。

**申诉不重跑模型。** 同一份材料再问一次多半得到同一个答案，那只会让人
以为自己被敷衍了。申辩是新的信息，而读懂一段申辩、决定要不要采信，
正是我们一开始就没交给模型的那类判断。
*/
func (s *Service) AppealMakerApplication(ctx context.Context, userID string,
	req MakerAppealReq) (*store.MakerApp, error) {
	note := strings.TrimSpace(req.Note)
	if note == "" {
		return nil, httpx.Fail(http.StatusUnprocessableEntity, "NOTE_REQUIRED", "note",
			"tell us what you think we got wrong — without it there is nothing for a person to read")
	}
	if len(note) > 2000 {
		note = note[:2000]
	}
	cur, err := s.St.MakerApp(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, httpx.NotFound("application")
	}
	if err != nil {
		return nil, err
	}
	// 没被打回的没什么可申诉。放过去的话，队列里会混进一批没有对象的条目。
	stage := ""
	switch {
	case cur.KYCDone && !cur.KYCOk:
		stage = "kyc"
	case cur.ListingDone && !cur.Approved:
		stage = "listing"
	}
	if stage == "" || cur.RejectReason == "" {
		return nil, httpx.Fail(http.StatusConflict, "NOTHING_TO_APPEAL", "",
			"there is no decision on this application to appeal")
	}

	/* 申辩连同它针对的那个结论一起留痕。
	   人来看的时候要能同时读到两样：我们当时说了什么，他说我们哪里错了。
	   只存申辩的话，看的人还得自己去翻上一行。 */
	if err := s.St.InsertMakerReview(ctx, store.MakerReview{
		UserID: userID, Stage: stage,
		Source: makerreview.SourceHuman, Verdict: string(makerreview.Escalate),
		IssuesJSON: issuesJSON([]makerreview.Issue{{
			Fields: []string{"*"},
			Says:   "The applicant disagrees with the decision: " + note,
			Ask:    "Needs a person to look at it.",
			Route:  makerreview.ToEscalate,
		}}),
	}); err != nil {
		log.Printf("maker appeal: 留痕 %s/%s: %v", userID, stage, err)
	}

	/* 排进人工队列：清掉自动放行的闹钟，免得钟在人看之前先把它放了。
	   申请状态不动——它仍然是「被打回、等处理」，只是现在多了一个人在等
	   着看。骗他说「已通过」比不理他更糟。 */
	if err := s.St.ClearMakerAutoReview(ctx, userID); err != nil {
		log.Printf("maker appeal: 清闹钟 %s: %v", userID, err)
	}
	if err := s.St.ClearAIRetry(ctx, userID); err != nil {
		log.Printf("maker appeal: 清重试 %s: %v", userID, err)
	}
	if err := s.St.MarkMakerAppealed(ctx, userID, note); err != nil {
		return nil, err
	}
	return s.MakerApplication(ctx, userID)
}
