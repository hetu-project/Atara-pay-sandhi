package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestKycCheckRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.StartKycCheck(ctx, "REF1", "u1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	k, err := st.KycCheck(ctx, "REF1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if k.Status != "pending" || k.Concluded() {
		t.Fatalf("刚建的会话应当是 pending: %+v", k)
	}

	if err := st.SaveKycResult(ctx, "REF1", KycResult{
		Status: "accept", TransactionID: "txn1", DocupassID: "REF1",
		Event: "docupass_conclusive", IdentityJSON: `{"last_name":"Liu"}`,
		RawJSON: `{"decision":"accept"}`, Source: "webhook",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	k, err = st.KycCheck(ctx, "REF1")
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	if !k.Concluded() || k.Status != "accept" || k.ConcludedAt == nil {
		t.Fatalf("落结论后 = %+v", k)
	}
}

// 回调会重投（自动重试 4 次，门户里还能手动重发 48 小时）。
// concluded_at 记的是"这个人什么时候走完的"——每次重投都刷新的话，
// 这个时间会一路漂到最后一次重投，那就不是通过的时间了。
func TestSaveKycResultKeepsFirstConcludedAt(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.StartKycCheck(ctx, "REF1", "u1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := KycResult{Status: "accept", TransactionID: "txn1", Event: "docupass_conclusive"}
	if err := st.SaveKycResult(ctx, "REF1", r); err != nil {
		t.Fatalf("save: %v", err)
	}
	first, _ := st.KycCheck(ctx, "REF1")

	r.Event = "update"
	if err := st.SaveKycResult(ctx, "REF1", r); err != nil {
		t.Fatalf("resave: %v", err)
	}
	again, _ := st.KycCheck(ctx, "REF1")
	if !again.ConcludedAt.Equal(*first.ConcludedAt) {
		t.Fatalf("重投把通过时间改了: %v → %v", first.ConcludedAt, again.ConcludedAt)
	}
	if !again.UpdatedAt.After(first.UpdatedAt) && !again.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("updated_at 应当跟着动: %v → %v", first.UpdatedAt, again.UpdatedAt)
	}
}

// 认不出的会话不能凭空建行——那正是伪造回调想做的事。
func TestSaveKycResultUnknownReference(t *testing.T) {
	st := openTestStore(t)
	err := st.SaveKycResult(context.Background(), "NOPE", KycResult{Status: "accept"})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("给一次没开过的会话落了结论: %v", err)
	}
}

// 一个已经通过的人又开了一次新会话时，他的身份不该在那一刻变回未验证。
func TestLatestPassedSurvivesANewSession(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.StartKycCheck(ctx, "REF1", "u1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := st.SaveKycResult(ctx, "REF1", KycResult{Status: "accept"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 换了本护照，又开一次。
	if err := st.StartKycCheck(ctx, "REF2", "u1"); err != nil {
		t.Fatalf("start 2: %v", err)
	}

	latest, err := st.LatestKycCheck(ctx, "u1")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.Reference != "REF2" || latest.Status != "pending" {
		t.Fatalf("最近一次应该是新开那次: %+v", latest)
	}
	passed, err := st.LatestPassedKycCheck(ctx, "u1")
	if err != nil {
		t.Fatalf("passed: %v", err)
	}
	if passed.Reference != "REF1" {
		t.Fatalf("通过过的那次丢了: %+v", passed)
	}
}

func TestLatestPassedIgnoresReviewAndReject(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for ref, status := range map[string]string{"R1": "review", "R2": "reject"} {
		if err := st.StartKycCheck(ctx, ref, "u1"); err != nil {
			t.Fatalf("start %s: %v", ref, err)
		}
		if err := st.SaveKycResult(ctx, ref, KycResult{Status: status}); err != nil {
			t.Fatalf("save %s: %v", ref, err)
		}
	}
	if _, err := st.LatestPassedKycCheck(ctx, "u1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("review/reject 被当成通过了: %v", err)
	}
}

// 很多人是先被下单拦下来才去验身份的，他从没走过做市申请——
// 没有那一行，kyc_ok 就无处可写。
func TestMarkKycOkCreatesTheApplication(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.MarkKycOk(ctx, "u1"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	a, err := st.MakerApp(ctx, "u1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !a.KYCOk || !a.KYCDone {
		t.Fatalf("身份没置位: %+v", a)
	}
	if a.Approved {
		t.Fatalf("身份通过不该顺带给挂单资格: %+v", a)
	}
}

// 已经在走做市申请的人，验过身份只动身份那一段，不能把挂单那一段抹掉。
func TestMarkKycOkKeepsListingProgress(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.UpsertMakerApp(ctx, MakerApp{UserID: "u1", Phase: "listing",
		KYCDone: true, KYCOk: true, ListingDone: true, FormJSON: `{"kyc":{"a":1}}`}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.MarkKycOk(ctx, "u1"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	a, err := st.MakerApp(ctx, "u1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !a.ListingDone || a.FormJSON != `{"kyc":{"a":1}}` {
		t.Fatalf("挂单那一段被冲掉了: %+v", a)
	}
}
