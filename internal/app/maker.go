package app

import (
	"context"
	"encoding/json"
	"net/http"

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
	next := store.MakerApp{UserID: userID, Phase: req.Phase, FormJSON: string(req.Form),
		KYCDone: cur.KYCDone, KYCOk: cur.KYCOk, ListingDone: cur.ListingDone, Approved: cur.Approved}
	if req.Phase == "kyc" {
		next.KYCDone = true
	} else {
		next.ListingDone = true
	}
	if err := s.St.UpsertMakerApp(ctx, next); err != nil {
		return nil, err
	}
	return s.St.MakerApp(ctx, userID)
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
