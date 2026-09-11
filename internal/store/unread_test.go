package store

import (
	"context"
	"testing"
)

// 未读只数对方说的话。
func TestUnreadCountsOnlyTheirMessages(t *testing.T) {
	st := openTestStore(t)

	// u2 说了两句，我说了一句。
	post(t, st, "u1", "u2", "them", "chat", "first")
	post(t, st, "u1", "u2", "them", "chat", "second")
	post(t, st, "u1", "u2", "me", "chat", "mine")
	// 对方那一侧：我说的那句在他眼里是 them
	post(t, st, "u2", "u1", "them", "chat", "mine")

	if got := unread(t, st, "u1", "u2"); got != 2 {
		t.Fatalf("未读 = %d，应该是 2", got)
	}
	// 对方那一侧只有我说的那一句是「them」。
	if got := unread(t, st, "u2", "u1"); got != 1 {
		t.Fatalf("对方那一侧未读 = %d，应该是 1", got)
	}
}

// 系统播报不算未读：「Matched with X」常常是我自己下单触发的，给自己的
// 动作挂角标，人点进去会发现没有任何新东西。
func TestUnreadIgnoresSystemMessages(t *testing.T) {
	st := openTestStore(t)
	post(t, st, "u1", "u2", "system", "order", "Matched with U2")
	if got := unread(t, st, "u1", "u2"); got != 0 {
		t.Fatalf("系统播报被算成了未读：%d", got)
	}
}

// 读过之后清零；之后再来的消息重新计数。
func TestMarkThreadReadClearsThenRecounts(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	post(t, st, "u1", "u2", "them", "chat", "before")
	if got := unread(t, st, "u1", "u2"); got != 1 {
		t.Fatalf("标记前 = %d", got)
	}
	if err := st.MarkThreadRead(ctx, "u1", "u2"); err != nil {
		t.Fatal(err)
	}
	if got := unread(t, st, "u1", "u2"); got != 0 {
		t.Fatalf("标记后应该清零，实际 = %d", got)
	}

	post(t, st, "u1", "u2", "them", "chat", "after")
	if got := unread(t, st, "u1", "u2"); got != 1 {
		t.Fatalf("读过之后再来的消息应该重新计数，实际 = %d", got)
	}
}

// 从没读过的会话，全部算未读——没有 thread_reads 那一行不等于「都读过了」。
func TestUnreadWithoutAnyReadRecord(t *testing.T) {
	st := openTestStore(t)
	for i := 0; i < 3; i++ {
		post(t, st, "u1", "u2", "them", "chat", "hi")
	}
	if got := unread(t, st, "u1", "u2"); got != 3 {
		t.Fatalf("没有读记录时 = %d，应该是 3", got)
	}
}

// post 直接写一行消息。owner 是「这一行属于谁的会话」，peer 是对面那个人——
// 同一句话在库里是两行，各自站在一侧，author 也就不同（me / them）。
func post(t *testing.T, st *Store, owner, peer, author, kind, body string) {
	t.Helper()
	if _, err := st.DB().Exec(
		`insert into messages(id,owner_id,peer_id,author,kind,body,payload,created_at)
		 values(?,?,?,?,?,?,'{}',?)`,
		NewID(), owner, peer, author, kind, body, ts(Now())); err != nil {
		t.Fatal(err)
	}
}

func unread(t *testing.T, st *Store, owner, peer string) int {
	t.Helper()
	ts, err := st.Threads(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		if x.PeerID == peer {
			return x.Unread
		}
	}
	return 0
}
