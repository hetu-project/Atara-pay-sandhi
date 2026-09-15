package store

import (
	"context"
	"testing"
	"time"
)

// 两段审核各自置位：kyc 过了才进 listing 段，listing 过了才算 approved。
// 跳段会让没审身份的人直接挂单。
func TestMakerReviewStages(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "kyc",
		KYCDone: true, FormJSON: `{"kind":"Individual"}`}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.ReviewMakerApp(ctx, "u1", "kyc", "approve", "", "u2"); err != nil {
		t.Fatalf("review kyc: %v", err)
	}
	a, err := st.MakerApp(ctx, "u1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !a.KYCOk || a.Phase != "listing" || a.Approved {
		t.Fatalf("kyc 审过后 = %+v, 期望 kyc_ok=true phase=listing approved=false", a)
	}
	if a.ReviewerID != "u2" || a.ReviewedAt == nil {
		t.Fatalf("审核留痕缺失: %+v", a)
	}

	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "listing",
		KYCDone: true, KYCOk: true, ListingDone: true}); err != nil {
		t.Fatalf("upsert listing: %v", err)
	}
	if err := st.ReviewMakerApp(ctx, "u1", "listing", "approve", "", "u2"); err != nil {
		t.Fatalf("review listing: %v", err)
	}
	a, _ = st.MakerApp(ctx, "u1")
	if !a.Approved {
		t.Fatalf("listing 审过后 approved = false, 期望 true")
	}
}

// 打回要写明理由，且「交过了」这一位要留着——理由是挂在那一段上的，
// 清零等于把理由一起抹掉，用户会被丢回一张空表单。
func TestMakerReviewReject(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "kyc", KYCDone: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.ReviewMakerApp(ctx, "u1", "kyc", "reject", "ID 照片看不清", "u2"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	a, _ := st.MakerApp(ctx, "u1")
	if a.KYCOk || !a.KYCDone || a.RejectReason != "ID 照片看不清" {
		t.Fatalf("打回后 = %+v, 期望 kyc_done=true、kyc_ok=false 且有理由", a)
	}
	// 闹钟要摘掉：不摘的话这份刚被打回的申请十秒后会被 sweep 自动放行。
	if a.AutoReviewAt != nil {
		t.Fatalf("打回后 auto_review_at = %v, 期望已清空", a.AutoReviewAt)
	}

	// 拒绝后重新提交：理由要被清掉，否则用户改完了还挂着旧的拒绝原因。
	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "kyc",
		KYCDone: true, FormJSON: `{"kind":"Company"}`}); err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	a, _ = st.MakerApp(ctx, "u1")
	if a.RejectReason != "" || !a.KYCDone {
		t.Fatalf("重新提交后 = %+v, 期望理由清空且 kyc_done=true", a)
	}
}

// 打回之后那份申请不能再被自动放行。
//
// 打回保留 kyc_done=1（理由挂在那一段上），于是它仍然满足 sweep 找的
// 「交过了、还没过」——闹钟不摘，十秒后钟会把人刚打回的东西放过去。
func TestRejectedAppIsNotDueForAutoReview(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	past := Now().Add(-time.Minute)
	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "kyc",
		KYCDone: true, AutoReviewAt: &past}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	due, err := st.DueMakerApps(ctx, Now())
	if err != nil || len(due) != 1 {
		t.Fatalf("打回前应当到期一份, got %d err=%v", len(due), err)
	}
	if err := st.ReviewMakerApp(ctx, "u1", "kyc", "reject", "地址证明过期", "u2"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	due, err = st.DueMakerApps(ctx, Now())
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("打回后仍有 %d 份等着自动放行，钟会把它放过去", len(due))
	}
}

// 系统出的票没有对应的人，reviewer_id 要存 NULL。
//
// 那一列有外键指向 users，塞 "system:rule" 这类假 id 会撞约束——而规则层
// 打回是最常走的一条路，撞上了整个提交都会失败。
func TestSystemReviewLeavesReviewerNull(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "kyc", KYCDone: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.ReviewMakerApp(ctx, "u1", "kyc", "reject", "缺少签名", ""); err != nil {
		t.Fatalf("系统打回失败（外键？）: %v", err)
	}
	a, err := st.MakerApp(ctx, "u1")
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if a.ReviewerID != "" || a.RejectReason != "缺少签名" {
		t.Fatalf("= %+v, 期望 reviewer 为空、理由留着", a)
	}
}

// 留痕一次审一行，不覆盖——模型变笨了要能答「从哪天开始的」。
func TestMakerReviewsAccumulate(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for _, v := range []string{"revise", "revise", "pass"} {
		if err := st.InsertMakerReview(ctx, MakerReview{
			UserID: "u1", Stage: "kyc", Source: "rule", Verdict: v,
			InputHash: HashInput([]byte(v)),
		}); err != nil {
			t.Fatalf("留痕 %s: %v", v, err)
		}
	}
	rs, err := st.MakerReviews(ctx, "u1", 0)
	if err != nil || len(rs) != 3 {
		t.Fatalf("留痕 %d 行 err=%v, 期望 3 行", len(rs), err)
	}
	if rs[0].IssuesJSON != "[]" {
		t.Fatalf("没给 issues 时要落成空数组, got %q", rs[0].IssuesJSON)
	}
}

// stage 或 decision 取值非法必须报错，不能静默什么都不做——
// 静默忽略会让审核动作看起来成功却什么都没变。
func TestMakerReviewRejectsBadInput(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "kyc", KYCDone: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, c := range []struct{ stage, decision string }{
		{"bogus", "approve"}, {"kyc", "bogus"}, {"", ""},
	} {
		if err := st.ReviewMakerApp(ctx, "u1", c.stage, c.decision, "", "u2"); err == nil {
			t.Fatalf("stage=%q decision=%q 被接受了", c.stage, c.decision)
		}
	}
}

// 待审列表只列真的提交了、还没审过的。
func TestPendingMakerApps(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	_ = st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "kyc", KYCDone: true})
	_ = st.UpsertMakerApp(ctx, MakerApp{UserID: "u2", Phase: "kyc"}) // 没提交
	got, err := st.PendingMakerApps(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(got) != 1 || got[0].UserID != "u1" {
		t.Fatalf("待审 = %+v, 期望只有 u1", got)
	}
	_ = st.ReviewMakerApp(ctx, "u1", "kyc", "approve", "", "u2")
	got, _ = st.PendingMakerApps(ctx)
	if len(got) != 0 {
		t.Fatalf("审过还留在待审里: %+v", got)
	}
}
