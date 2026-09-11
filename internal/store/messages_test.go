package store

import (
	"context"
	"testing"

	"github.com/advaita/atara-pay/internal/domain/model"
)

// 一条消息要落进双方的会话。只写发送方那一行的话，对方的界面上什么都没有，
// 而两个人都以为自己在跟对方说话。
func TestPostBothLandsOnBothSides(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	m := &model.Message{Author: "me", Kind: "chat", Body: "receipt is up"}
	if err := st.PostBoth(ctx, "u1", "u2", m); err != nil {
		t.Fatal(err)
	}

	mine, err := st.Thread(ctx, "u1", "u2")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Author != "me" || mine[0].Body != "receipt is up" {
		t.Fatalf("发送方那一侧不对：%+v", mine)
	}

	theirs, err := st.Thread(ctx, "u2", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(theirs) != 1 {
		t.Fatalf("接收方那一侧看不到：%+v", theirs)
	}
	// 同一句话，在对方眼里作者是「他」而不是「我」。
	if theirs[0].Author != "them" {
		t.Fatalf("接收方看到的作者是 %q，应该是 them", theirs[0].Author)
	}
	if theirs[0].Body != "receipt is up" {
		t.Fatalf("正文对不上：%q", theirs[0].Body)
	}
	// 两行是两条记录，id 不能共用——id 是主键。
	if theirs[0].ID == mine[0].ID {
		t.Fatal("两侧共用了同一个 id")
	}
}

// 系统播报两边都是 system：它不是谁说的话，没有「我」和「他」之分。
func TestPostBothKeepsSystemAuthor(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	m := &model.Message{Author: "system", Kind: "order", Body: "Matched"}
	if err := st.PostBoth(ctx, "u1", "u2", m); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"u1", "u2"}, {"u2", "u1"}} {
		got, _ := st.Thread(ctx, c[0], c[1])
		if len(got) != 1 || got[0].Author != "system" {
			t.Fatalf("%s 那一侧：%+v", c[0], got)
		}
	}
}
