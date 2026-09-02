package store

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// 地址簿按 (owner, chain, address) 去重：同一个人不该把同一个地址存两遍。
// 靠表上的 unique 约束报冲突，不在 Go 里查后写——查后写会给并发留缝。
func TestPayeeRoundTripAndDedup(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	p := Payee{ID: "p1", OwnerID: "u1", Label: "Ops wallet",
		Chain: "TRON", Address: "TXm9f", CreatedAt: Now()}
	if err := st.AddPayee(ctx, p); err != nil {
		t.Fatalf("add: %v", err)
	}
	dup := p
	dup.ID = "p2"
	if err := st.AddPayee(ctx, dup); err == nil {
		t.Fatal("同一地址被存了第二遍")
	}
	// 换条链，同一个地址是另一回事
	other := p
	other.ID, other.Chain = "p3", "ETH"
	if err := st.AddPayee(ctx, other); err != nil {
		t.Fatalf("换链应可存: %v", err)
	}
	got, err := st.Payees(ctx, "u1")
	if err != nil || len(got) != 2 {
		t.Fatalf("payees = %d 条, err = %v", len(got), err)
	}
}

// 别人的地址簿删不掉。
func TestDeletePayeeIsScopedToOwner(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.AddPayee(ctx, Payee{ID: "p1", OwnerID: "u1", Label: "L",
		Chain: "ETH", Address: "0xabc", CreatedAt: Now()}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.DeletePayee(ctx, "u2", "p1"); err == nil {
		t.Fatal("u2 删掉了 u1 的收款方")
	}
	if err := st.DeletePayee(ctx, "u1", "p1"); err != nil {
		t.Fatalf("owner 自己删应成功: %v", err)
	}
	got, _ := st.Payees(ctx, "u1")
	if len(got) != 0 {
		t.Fatalf("删完还剩 %d 条", len(got))
	}
}

// 提现只记意图与合规材料，金额必须原样往返——18 位精度下经过 float
// 会悄悄改掉尾数，那是这个项目一开始就拒绝 int64/float 的原因。
func TestWithdrawalPreservesAmount(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.AddPayee(ctx, Payee{ID: "p1", OwnerID: "u1", Label: "L",
		Chain: "ETH", Address: "0xabc", CreatedAt: Now()}); err != nil {
		t.Fatalf("payee: %v", err)
	}
	amt, _ := decimal.NewFromString("3.600000000000000001")
	w := Withdrawal{ID: "w1", OwnerID: "u1", PayeeID: "p1", Asset: "ETH",
		Amount: amt, Purpose: "OTC settlement", State: "submitted",
		CreatedAt: Now(), UpdatedAt: Now()}
	if err := st.InsertWithdrawal(ctx, w); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := st.Withdrawals(ctx, "u1")
	if err != nil || len(got) != 1 {
		t.Fatalf("withdrawals = %d 条, err = %v", len(got), err)
	}
	if got[0].Amount.String() != amt.String() {
		t.Fatalf("amount = %s, want %s", got[0].Amount, amt)
	}
	if got[0].PayeeAddress != "0xabc" || got[0].PayeeLabel != "L" {
		t.Fatalf("收款方信息没带上: %+v", got[0])
	}
	var _ time.Time = got[0].CreatedAt
}

// 提现必须挂在自己的收款方上：拿别人的 payee_id 会被外键之外的归属校验挡住。
func TestWithdrawalRejectsForeignPayee(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.AddPayee(ctx, Payee{ID: "p1", OwnerID: "u2", Label: "L",
		Chain: "ETH", Address: "0xabc", CreatedAt: Now()}); err != nil {
		t.Fatalf("payee: %v", err)
	}
	if _, ok := st.Payee(ctx, "u1", "p1"); ok {
		t.Fatal("u1 读到了 u2 的收款方")
	}
}
