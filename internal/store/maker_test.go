package store

import (
	"context"
	"testing"
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

// 拒绝要写明理由并把对应位清零，否则前端会一直显示 Under review。
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
	if a.KYCOk || a.KYCDone || a.RejectReason != "ID 照片看不清" {
		t.Fatalf("拒绝后 = %+v, 期望 kyc_done/kyc_ok 都为 false 且有理由", a)
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
