package store

import (
	"context"
	"testing"
)

// 挂单必须过审批闸门。没有这道门，做市准入流程做完了也是装饰——
// 任何注册用户直接调 POST /offers 就能挂单，前端隐藏按钮拦不住 curl。
func TestMakerApprovedGate(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// 从没申请过
	if st.MakerApproved(ctx, "u1") {
		t.Fatal("没申请过的人被判为已审批")
	}

	// 提交了身份但没审
	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "kyc", KYCDone: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if st.MakerApproved(ctx, "u1") {
		t.Fatal("只提交未审批就被放行")
	}

	// 身份审过，但挂单配置还没审
	if err := st.ReviewMakerApp(ctx, "u1", "kyc", "approve", "", "u2"); err != nil {
		t.Fatalf("review kyc: %v", err)
	}
	if st.MakerApproved(ctx, "u1") {
		t.Fatal("只审过身份就被放行——跳段等于让没审配置的人挂单")
	}

	// 两段都审过
	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "listing",
		KYCDone: true, KYCOk: true, ListingDone: true}); err != nil {
		t.Fatalf("upsert listing: %v", err)
	}
	if err := st.ReviewMakerApp(ctx, "u1", "listing", "approve", "", "u2"); err != nil {
		t.Fatalf("review listing: %v", err)
	}
	if !st.MakerApproved(ctx, "u1") {
		t.Fatal("两段都审过却没放行")
	}

	// 事后被撤回审批，闸门要立刻关上
	if err := st.ReviewMakerApp(ctx, "u1", "listing", "reject", "材料过期", "u2"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if st.MakerApproved(ctx, "u1") {
		t.Fatal("撤回审批后闸门没关")
	}
}
