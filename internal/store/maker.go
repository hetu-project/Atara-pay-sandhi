// Maker 申请与审核。审核不是 agent 共识——它是真人动作，所以既不能由系统
// 自动放行，也不能人人都能点。放行的两段各自置位，跳段会让没审身份的人直接挂单。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type MakerApp struct {
	UserID       string     `json:"user_id"`
	Phase        string     `json:"phase"` // kyc | listing
	KYCDone      bool       `json:"kyc_done"`
	KYCOk        bool       `json:"kyc_ok"`
	ListingDone  bool       `json:"listing_done"`
	Approved     bool       `json:"approved"`
	FormJSON     string     `json:"form"`
	RejectReason string     `json:"reject_reason,omitempty"`
	SubmittedAt  *time.Time `json:"submitted_at,omitempty"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
	ReviewerID   string     `json:"reviewer_id,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`

	// AutoReviewAt 到点后由 scheduler 放行。演示用——真实环境这里是人。
	AutoReviewAt *time.Time `json:"-"`

	// 待审列表要显示是谁在申请。
	DisplayName string `json:"display_name,omitempty"`
}

const makerCols = `user_id,phase,kyc_done,kyc_ok,listing_done,approved,form_json,
	reject_reason,submitted_at,reviewed_at,coalesce(reviewer_id,''),updated_at,auto_review_at`

func scanMakerApp(scan func(...any) error, extra ...any) (*MakerApp, error) {
	var a MakerApp
	var submitted, reviewed sql.NullString
	var updated string
	var auto sql.NullString
	dest := []any{&a.UserID, &a.Phase, &a.KYCDone, &a.KYCOk, &a.ListingDone, &a.Approved,
		&a.FormJSON, &a.RejectReason, &submitted, &reviewed, &a.ReviewerID, &updated, &auto}
	dest = append(dest, extra...)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	if submitted.Valid {
		t := parseTS(submitted.String)
		a.SubmittedAt = &t
	}
	if reviewed.Valid {
		t := parseTS(reviewed.String)
		a.ReviewedAt = &t
	}
	if auto.Valid {
		t := parseTS(auto.String)
		a.AutoReviewAt = &t
	}
	a.UpdatedAt = parseTS(updated)
	return &a, nil
}

func (s *Store) MakerApp(ctx context.Context, userID string) (*MakerApp, error) {
	return scanMakerApp(s.db.QueryRowContext(ctx,
		`select `+makerCols+` from maker_applications where user_id=?`, userID).Scan)
}

// UpsertMakerApp 收下一次提交。重新提交要清掉上一轮的拒绝理由——
// 用户已经改过了，还挂着旧理由会让他以为没提交成功。
func (s *Store) UpsertMakerApp(ctx context.Context, a MakerApp) error {
	if a.Phase == "" {
		a.Phase = "kyc"
	}
	now := ts(Now())
	var submitted any
	if a.KYCDone || a.ListingDone {
		submitted = now
	}
	var auto any
	if a.AutoReviewAt != nil {
		auto = ts(*a.AutoReviewAt)
	}
	_, err := s.db.ExecContext(ctx,
		`insert into maker_applications
		   (user_id,phase,kyc_done,kyc_ok,listing_done,approved,form_json,
		    reject_reason,submitted_at,auto_review_at,updated_at)
		 values(?,?,?,?,?,?,?,'',?,?,?)
		 on conflict(user_id) do update set
		   phase=excluded.phase,
		   kyc_done=excluded.kyc_done,
		   kyc_ok=excluded.kyc_ok,
		   listing_done=excluded.listing_done,
		   approved=excluded.approved,
		   form_json=excluded.form_json,
		   reject_reason='',
		   submitted_at=excluded.submitted_at,
		   auto_review_at=excluded.auto_review_at,
		   updated_at=excluded.updated_at`,
		a.UserID, a.Phase, a.KYCDone, a.KYCOk, a.ListingDone, a.Approved,
		nz(a.FormJSON), submitted, auto, now)
	return err
}

func nz(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

// ReviewMakerApp 是真人审核动作。stage 与 decision 取值非法一律报错——
// 静默忽略会让审核看起来成功却什么都没变，那比报错危险得多。
func (s *Store) ReviewMakerApp(ctx context.Context, userID, stage, decision, reason, reviewerID string) error {
	var set string
	switch {
	case stage == "kyc" && decision == "approve":
		set = `kyc_ok=1, phase='listing', reject_reason=''`
	case stage == "kyc" && decision == "reject":
		// kyc_done 保持 1。它记的是「交过了」，不是「审过了」——两件事
		// 用同一位表示，打回之后这份申请就跟从没交过一模一样，而 reject_reason
		// 在前端唯一的落点挂在「交过了」那一段里，于是理由存了却没人看得见，
		// 用户被丢回一张空表单，不知道哪里要改。
		//
		// 三态由这两位加理由读出来：
		//   done=0                          还没交
		//   done=1 ok=0 reason=''           在审
		//   done=1 ok=0 reason!=''          打回，等你改
		//   ok=1                            过了
		set = `kyc_ok=0, reject_reason=?`
	case stage == "listing" && decision == "approve":
		set = `approved=1, reject_reason=''`
	case stage == "listing" && decision == "reject":
		// 同上：listing_done 保持 1
		set = `approved=0, reject_reason=?`
	default:
		return fmt.Errorf("bad review: stage=%q decision=%q", stage, decision)
	}
	args := []any{}
	if decision == "reject" {
		args = append(args, reason)
	}
	/* 不是人出的票就存 NULL。
	
	   reviewer_id 有外键指向 users，塞一个 "system:rule" 这样的假 id 进去
	   会撞约束；而「谁出的这一票」本来也不该记在这儿——maker_reviews.source
	   分得清规则层、模型和人，那才是它的位置。这一列只回答「哪个人审的」，
	   没有人审就是没有。 */
	var reviewer any
	if reviewerID != "" {
		reviewer = reviewerID
	}
	args = append(args, ts(Now()), reviewer, ts(Now()), userID)
	// 审过了就把闹钟摘掉。不摘的话，打回之后那份申请仍然满足 sweep 的条件
	// （done=1、ok=0），十秒后被自动放行——人刚驳回的东西，钟把它放了过去。
	res, err := s.db.ExecContext(ctx,
		`update maker_applications set `+set+`, reviewed_at=?, reviewer_id=?,
		        auto_review_at=null, updated_at=?
		  where user_id=?`, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no application for %s", userID)
	}
	return nil
}

// PendingMakerApps 列出提交了、当前这一段还没审过的申请。
func (s *Store) PendingMakerApps(ctx context.Context) ([]MakerApp, error) {
	rows, err := s.db.QueryContext(ctx,
		`select a.user_id,a.phase,a.kyc_done,a.kyc_ok,a.listing_done,a.approved,a.form_json,
		        a.reject_reason,a.submitted_at,a.reviewed_at,coalesce(a.reviewer_id,''),a.updated_at,
		        a.auto_review_at,u.display_name
		   from maker_applications a
		   join users u on u.id = a.user_id
		  where (a.kyc_done=1 and a.kyc_ok=0) or (a.listing_done=1 and a.approved=0)
		  order by a.submitted_at asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MakerApp{}
	for rows.Next() {
		var name string
		a, err := scanMakerApp(rows.Scan, &name)
		if err != nil {
			return nil, err
		}
		a.DisplayName = name
		out = append(out, *a)
	}
	return out, rows.Err()
}

// ReviewedMakerApps 列出审过的申请（有 reviewed_at 的），最近审的在前。
//
// 注意这是审核历史的近似，不是完整时间线：每份申请在库里只有一行，重审会
// 覆盖上一次的结果，所以这里给的是「当前状态 + 最近一次审核的人和时间」。
// 真正的逐次留痕要一张 append-only 的审计表——那是下一步，先不动写路径。
func (s *Store) ReviewedMakerApps(ctx context.Context, limit int) ([]MakerApp, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`select a.user_id,a.phase,a.kyc_done,a.kyc_ok,a.listing_done,a.approved,a.form_json,
		        a.reject_reason,a.submitted_at,a.reviewed_at,coalesce(a.reviewer_id,''),a.updated_at,
		        a.auto_review_at,u.display_name
		   from maker_applications a
		   join users u on u.id = a.user_id
		  where a.reviewed_at is not null
		  order by a.reviewed_at desc
		  limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MakerApp{}
	for rows.Next() {
		var name string
		a, err := scanMakerApp(rows.Scan, &name)
		if err != nil {
			return nil, err
		}
		a.DisplayName = name
		out = append(out, *a)
	}
	return out, rows.Err()
}

// DueMakerApps 找出到点该自动放行的申请。
//
// 时间戳存在库里，不是起一个睡 5 秒的 goroutine：进程重启后 goroutine 就没了，
// 申请会永远停在「审核中」，而重启是演示机上最常发生的事。
func (s *Store) DueMakerApps(ctx context.Context, now time.Time) ([]MakerApp, error) {
	rows, err := s.db.QueryContext(ctx,
		`select `+makerCols+` from maker_applications
		  where auto_review_at is not null and auto_review_at <= ?`, ts(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MakerApp{}
	for rows.Next() {
		a, err := scanMakerApp(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// AutoApproveMakerApp 到点放行当前这一段。
//
// reviewer_id 留空:没有人看过件,记一个假的审核人比不记更糟——
// 事后查「谁批的」时,空值至少是诚实的。
func (s *Store) AutoApproveMakerApp(ctx context.Context, userID, stage string) error {
	var set string
	switch stage {
	case "kyc":
		set = `kyc_ok=1, phase='listing'`
	case "listing":
		set = `approved=1`
	default:
		return fmt.Errorf("bad stage %q", stage)
	}
	now := ts(Now())
	// 带上 auto_review_at is not null:两个 sweep 撞上时只有一个能改到行,
	// 另一个看到 0 行,不会重复放行。
	_, err := s.db.ExecContext(ctx,
		`update maker_applications
		    set `+set+`, reject_reason='', reviewed_at=?, auto_review_at=null, updated_at=?
		  where user_id=? and auto_review_at is not null`, now, now, userID)
	return err
}

// ClearMakerAutoReview 撤掉自动放行的约定——真人已经先审过了。
func (s *Store) ClearMakerAutoReview(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`update maker_applications set auto_review_at=null where user_id=?`, userID)
	return err
}

// MakerApproved 是挂单的闸门：两段审核都过了才算。
//
// 查不到申请、只提交未审、只审过身份，一律不放行。撤回审批后立刻关门——
// 这就是为什么它每次都查库而不缓存。
func (s *Store) MakerApproved(ctx context.Context, userID string) bool {
	var approved bool
	err := s.db.QueryRowContext(ctx,
		`select approved from maker_applications where user_id=?`, userID).Scan(&approved)
	return err == nil && approved
}
