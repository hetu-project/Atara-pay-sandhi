package store

import (
	"context"
	"testing"
	"time"
)

// 一次性消费：并发下同一枚令牌只能成功一次。
// 这是「动钱必确认」里最关键的一条——重放一笔已确认的支付不该再通过。
func TestConsumeConfirmationIsOnceOnly(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	exp := time.Now().Add(2 * time.Minute)
	if err := st.InsertConfirmation(ctx, "tok1", "u1", "dig1", "signature", exp); err != nil {
		t.Fatalf("insert: %v", err)
	}

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, _, err := st.ConsumeConfirmation(ctx, "tok1", "u1", time.Now())
			errs <- err
		}()
	}
	okCount := 0
	for i := 0; i < n; i++ {
		if <-errs == nil {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("成功次数 = %d, want 1", okCount)
	}
}

// 过期的令牌不能消费。
func TestConsumeConfirmationRejectsExpired(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Second)
	if err := st.InsertConfirmation(ctx, "tok2", "u1", "dig1", "signature", past); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := st.ConsumeConfirmation(ctx, "tok2", "u1", time.Now()); err == nil {
		t.Fatal("过期令牌被接受了")
	}
}

// 别人的令牌不能消费。
func TestConsumeConfirmationRejectsOtherUser(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.InsertConfirmation(ctx, "tok3", "u1", "dig1", "signature",
		time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := st.ConsumeConfirmation(ctx, "tok3", "u2", time.Now()); err == nil {
		t.Fatal("他人令牌被接受了")
	}
}

// openTestStore 开一个临时库，跑完即弃。
//
// 这是共用测试夹具，不只服务本文件——本仓库此前没有 store 层测试，
// 这是第一个，后面几个任务（清算、对账之类）的 store 测试也会开同一种库、
// 复用/扩充这份种子数据，所以种子刻意给了两个用户（u1、u2）而不是只给
// 确认令牌测试够用的一个。
//
// 外键打开、连接池为 1，与生产一致——否则并发用例测不出真实行为。
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(
		`insert into users(id,address,display_name,created_at) values
		 ('u1','0xu1','U1',datetime('now')),('u2','0xu2','U2',datetime('now'))`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return st
}
