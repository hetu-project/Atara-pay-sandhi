package app

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/advaita/atara-pay/internal/httpx"
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
	// 收下材料先进「审核中」，隔一会儿由调度器放行——两段都一样。
	//
	// 真实环境这一步是有人看件的，但那条路在演示里是个死胡同：没人去审核台
	// 点一下，提交完的账户就永远停在审核中，后面的挂单配置、成交全都走不下去。
	// 所以放行留给钟，而不是当场置位——当场变「已通过」，用户看不到有人审过件
	// 这件事发生过，界面上那张「已收到，审核中」的卡片也就白做了。
	//
	// 时间写进库、由每秒一次的 sweep 来推，不是起个睡 5 秒的 goroutine：
	// 进程重启后 goroutine 就没了，申请会永远卡在审核中。
	if d := s.Cfg.T.MakerReview; d > 0 {
		t := time.Now().Add(d)
		next.AutoReviewAt = &t
	}
	if err := s.St.UpsertMakerApp(ctx, next); err != nil {
		return nil, err
	}
	return s.St.MakerApp(ctx, userID)
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
