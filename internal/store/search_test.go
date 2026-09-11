package store

import (
	"context"
	"testing"
)

// 名字模糊、地址精确——搜索的全部契约就这两条。
func TestSearchAccountsNameFuzzyAddressExact(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	got, err := st.SearchAccounts(ctx, "u1", "u2", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "u2" {
		t.Fatalf("按名字模糊查没找到 U2：%v", got)
	}

	if got, _ := st.SearchAccounts(ctx, "u1", "0xu2", 8); len(got) != 1 || got[0].ID != "u2" {
		t.Fatalf("按完整地址没找到 U2：%v", got)
	}
	// 地址少一个字符就该是查无此人。地址做前缀匹配等于开放一个可以按前缀
	// 遍历全部账户的接口。
	if got, _ := st.SearchAccounts(ctx, "u1", "0xu", 8); len(got) != 0 {
		t.Fatalf("地址前缀不该匹配到任何人：%v", got)
	}
}

// 自己不出现在结果里——「添加我自己为联系人」不是一件有意义的事。
func TestSearchAccountsExcludesSelf(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if got, _ := st.SearchAccounts(ctx, "u1", "u1", 8); len(got) != 0 {
		t.Fatalf("搜到了自己：%v", got)
	}
	if got, _ := st.SearchAccounts(ctx, "u1", "0xu1", 8); len(got) != 0 {
		t.Fatalf("按地址搜到了自己：%v", got)
	}
}

// 已经是联系人的照样返回，并且带上关系——两条路（名字 / 地址）必须给
// 同一个答案。早先按名字会把已加的人排掉、按地址却返回，同一个人两种结果。
func TestSearchAccountsReportsRelationOnBothPaths(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.AddContact(ctx, "u1", "u2", "Client", "", "pending"); err != nil {
		t.Fatal(err)
	}

	byName, _ := st.SearchAccounts(ctx, "u1", "u2", 8)
	byAddr, _ := st.SearchAccounts(ctx, "u1", "0xu2", 8)
	if len(byName) != 1 || len(byAddr) != 1 {
		t.Fatalf("已加的联系人被排掉了：name=%v addr=%v", byName, byAddr)
	}
	if byName[0].Relation != "pending" || byAddr[0].Relation != "pending" {
		t.Fatalf("关系没带出来：name=%q addr=%q", byName[0].Relation, byAddr[0].Relation)
	}

	if err := st.AcceptContact(ctx, "u2", "u1"); err != nil {
		t.Fatal(err)
	}
	byName, _ = st.SearchAccounts(ctx, "u1", "u2", 8)
	byAddr, _ = st.SearchAccounts(ctx, "u1", "0xu2", 8)
	if byName[0].Relation != "accepted" || byAddr[0].Relation != "accepted" {
		t.Fatalf("接受之后关系没更新：name=%q addr=%q", byName[0].Relation, byAddr[0].Relation)
	}
}

// users 和 contacts 都有 created_at：join 时列名不加表前缀会 ambiguous，
// 而那个错只在运行时出现。空结果和报错在接口上长得一样，所以专门测一次。
func TestSearchAccountsJoinDoesNotAmbiguate(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.SearchAccounts(context.Background(), "u1", "U", 8); err != nil {
		t.Fatalf("按名字查报错：%v", err)
	}
}

// EVM 地址的大小写是 EIP-55 校验和，不是地址的一部分。按小写查不到的话，
// 前端会拿到 UNKNOWN_ACTOR、清掉本机身份——一次大小写差异表现成莫名其妙
// 被登出。
func TestUserByAddressIgnoresEVMCase(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if _, err := st.DB().Exec(
		`insert into users(id,address,display_name,created_at) values
		 ('ue','0xAbCdEf0123456789012345678901234567890123','Checksummed',datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{
		"0xAbCdEf0123456789012345678901234567890123",
		"0xabcdef0123456789012345678901234567890123",
		"0xABCDEF0123456789012345678901234567890123",
	} {
		u, err := st.UserByAddress(ctx, a)
		if err != nil || u.ID != "ue" {
			t.Fatalf("按 %q 查不到：%v", a, err)
		}
	}
}

// base58 的地址不能忽略大小写——那会把两个不同的地址当成同一个。
func TestUserByAddressKeepsBase58Case(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if _, err := st.DB().Exec(
		`insert into users(id,address,display_name,created_at) values
		 ('ut','TQ5n7YabcdEFGHjkmnPQRstuvwxyz1234','Tron',datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserByAddress(ctx, "tq5n7yABCDefghJKMNPqrSTUVWXYZ1234"); err == nil {
		t.Fatal("base58 地址被当成大小写不敏感了")
	}
}
