package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/shopspring/decimal"
)

// Funder 是种子数据往链上灌余额与锁仓的口子。
// 它刻意不在 chain.Chain 接口里——真链上没有"凭空记一笔余额"这种动作。
type Funder interface {
	Credit(ctx context.Context, address, asset string, amt decimal.Decimal) error
	LockListing(ctx context.Context, offerID, owner, asset string, amt decimal.Decimal) (string, error)
}

// Seed 灌演示数据，来自 console.html 的 CPS / ASSETS / CARDS / POOL。
//
// 注意余额走 Funder 灌到链上，不写进本库——平台没有余额表，
// 种子数据也不能例外，否则第一行代码就把非托管的边界破了。
func (s *Store) Seed(ctx context.Context, ch Funder) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `select count(*) from users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	now := ts(Now())

	type seeded struct{ id, addr, asset, amount string }
	var credits []seeded
	type listing struct{ offerID, addr, asset, qty string }
	var locks []listing

	err := s.Tx(ctx, func(tx *sql.Tx) error {
		// pavHues 对齐前端 PAV_HUES：没有头像图的用户，前端按这组色相之一渲染头像底色。
		// 种子用户挨个循环取色，纯粹为了让 demo 列表看起来色彩不单一。
		pavHues := [...]int{221, 190, 266, 152, 36, 320}
		hueIdx := 0
		nextHue := func() int {
			h := pavHues[hueIdx%len(pavHues)]
			hueIdx++
			return h
		}

		user := func(id, addr, name, kind, walletKind, login string) error {
			_, err := tx.Exec(
				`insert into users(id,address,display_name,email,kind,wallet_kind,login_method,hue,created_at)
				 values(?,?,?,?,?,?,?,?,?)`,
				id, addr, name, "", kind, walletKind, login, nextHue(), now)
			return err
		}

		// ── demo 用户。身份就是地址，邮箱只是通知渠道。 ──
		if err := user(demoID, DemoAddress, "Demo", "person", "atara", "passkey"); err != nil {
			return err
		}
		if _, err := tx.Exec(`update users set email=? where id=?`, "demo@atara.example", demoID); err != nil {
			return err
		}
		for _, w := range [][2]string{{"USDT", "34500"}, {"USDC", "1200"}, {"BTC", "0.42"}, {"ETH", "3.6"}} {
			credits = append(credits, seeded{demoID, DemoAddress, w[0], w[1]})
		}

		// ── 审核账号：role=reviewer，供 Task 9 的审核端点演示用。
		// 审核是真人动作，不走 agent 共识，所以必须有这么一个能登录的角色。
		if _, err := tx.Exec(
			`insert into users(id,address,display_name,email,kind,wallet_kind,login_method,hue,role,created_at)
			 values(?,?,?,?,?,?,?,?,?,?)`,
			reviewerID, ReviewerAddress, "Reviewer", "reviewer@atara.example",
			"person", "atara", "passkey", nextHue(), "reviewer", now); err != nil {
			return err
		}

		// ── 额度：本人一份，三个 agent 各一份 ──
		allowances := []struct {
			id, spender, kind, cycle, per, cap_, used, recipients, tpl, note, exp string
			status                                                                string
		}{
			{"al-me", "Me", "person", "weekly", "10000", "25000", "8400", "Any", "Any",
				"Hitting the limit stops autopay, not incoming funds", "", "live"},
			{"al-pa", "Procurement agent", "agent", "weekly", "500", "2000", "340",
				"3 verified providers", "On delivery · 7-day window",
				"Over-limit returns for your approval", "2026-10-31T00:00:00Z", "live"},
			{"al-da", "Data agent", "agent", "monthly", "200", "800", "612",
				"2 data sources", "On successful call", "High frequency, low ceiling",
				"2026-09-30T00:00:00Z", "live"},
			{"al-ta", "Travel agent", "agent", "monthly", "1500", "5000", "0",
				"Platform merchants only", "On ticketing · refundable",
				"Set recipients before using it", "", "revoked"},
		}
		for _, a := range allowances {
			var exp any
			if a.exp != "" {
				exp = a.exp
			}
			if _, err := tx.Exec(
				`insert into allowances(id,owner_id,spender,kind,asset,per_payment,window_cap,used,cycle,
					expires_at,recipients,template,wallet_kind,chain_tx,status,note)
				 values(?,?,?,?,'USDT',?,?,?,?,?,?,?,'atara','',?,?)`,
				a.id, demoID, a.spender, a.kind, a.per, a.cap_, a.used, a.cycle,
				exp, a.recipients, a.tpl, a.status, a.note); err != nil {
				return err
			}
		}

		// ── 联系人：条件支付的对手方 ──
		contacts := []struct{ id, addr, name, kind, label string }{
			{"cp-hc", "THuaChuang7xk2m9wq4vLpR3dNf6bZaU8kQ", "Huachuang", "firm", "Supplier"},
			{"cp-kj", "TKenjiM4pXvL3wR9dHf7bN6tZaU8kQ5n2Y", "Kenji M.", "person", "Client"},
			{"cp-ar", "TAriaStudio9pVv7NcGL2sYxk4mQeq3hxK", "Aria Studio", "firm", "Supplier"},
			{"cp-pa", "TProcureAgent2mK8pXvL3wR9dHf4bN6tZa", "Procurement agent", "agent", "My agent"},
		}
		for _, c := range contacts {
			if err := user(c.id, c.addr, c.name, c.kind, "atara", "passkey"); err != nil {
				return err
			}
			if _, err := tx.Exec(
				`insert into contacts(owner_id,contact_id,label,nickname,created_at) values(?,?,?,'',?)`,
				demoID, c.id, c.label, now); err != nil {
				return err
			}
			// 对手方也要有画像：放款后要往上回写履约
			if _, err := tx.Exec(
				`insert into merchant_profiles(user_id,peer_code,trust_score,deals,disputes,fill_rate,median_release_secs,docs)
				 values(?,?,?,?,?,?,?,?)`,
				c.id, strings.ToUpper(c.id), 90, 12, 0, "97", 180, `{"kyc":true}`); err != nil {
				return err
			}
		}

		// ── 挂单池：10 条，对齐前端 POOL ──
		for _, p := range pool {
			mid := "mk-" + p.id
			if err := user(mid, p.addr, p.name, "firm", "atara", "passkey"); err != nil {
				return err
			}
			docs, _ := json.Marshal(p.docs)
			if _, err := tx.Exec(
				`insert into merchant_profiles(user_id,peer_code,trust_score,deals,disputes,fill_rate,median_release_secs,docs)
				 values(?,?,?,?,?,?,?,?)`,
				mid, p.peer, p.score, p.deals, p.disputes, p.fillRate, p.releaseSecs, string(docs)); err != nil {
				return err
			}
			credits = append(credits, seeded{mid, p.addr, p.asset, p.reserve})
			if _, err := tx.Exec(
				`insert into offers(id,maker_id,side,asset_code,network,networks,fiat_code,
					unit_price,qty,remaining_qty,min_lot,lock_tx,status,created_at,updated_at)
				 values(?,?,?,?,?,?,?,?,?,?,?,'','active',?,?)`,
				p.id, mid, p.side, p.asset, p.nets[0], strings.Join(p.nets, ","), p.fiat,
				p.price, p.qty, p.qty, p.minLot, now, now); err != nil {
				return err
			}
			// 种子做市方视为已过两段审核。它们有活跃挂单，没有审批记录就不自洽——
			// 而且装上闸门后它们连新挂单都发不出来。
			if _, err := tx.Exec(
				`insert or ignore into maker_applications
				   (user_id,phase,kyc_done,kyc_ok,listing_done,approved,form_json,updated_at)
				 values(?,'listing',1,1,1,1,'{"seeded":true}',?)`, mid, now); err != nil {
				return err
			}
			// 卖单挂出即锁币——锁进合约，所以留到事务外走链
			if p.side == "sell" {
				locks = append(locks, listing{p.id, p.addr, p.asset, p.qty})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 链上动作在事务之外：跟链之间没有分布式事务。
	for _, c := range credits {
		if err := ch.Credit(ctx, c.addr, c.asset, dec(c.amount)); err != nil {
			return err
		}
	}
	for _, l := range locks {
		tx, err := ch.LockListing(ctx, l.offerID, l.addr, l.asset, dec(l.qty))
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `update offers set lock_tx=? where id=?`, tx, l.offerID); err != nil {
			return err
		}
	}
	return nil
}

const (
	demoID = "user-demo"
	// DemoAddress 是 demo 账户的地址。地址就是账户——
	// X-Atara-User 传它，或者传 "Demo" 也认。
	DemoAddress = "TDemo8F42C1kQm2vL9xW3cHf7bN6tZaU5p"
	DemoHandle  = DemoAddress

	reviewerID = "user-reviewer"
	// ReviewerAddress 是审核账号的地址；X-Atara-User 传它，或者传 "reviewer" 也认
	// （落到 UserByHandle 的 display_name 兜底匹配，跟 Demo 是同一套路）。
	ReviewerAddress = "TReviewer2C1kQm9wq4vLpR3dNf6bZaU8k5"
)

func (s *Store) DemoUserID() string { return demoID }

type seedOffer struct {
	id, name, peer, addr, side, asset, fiat string
	nets                                    []string
	price, qty, minLot, reserve             string
	score, deals, disputes, releaseSecs     int
	fillRate                                string
	docs                                    map[string]bool
}

func dset(keys ...string) map[string]bool {
	m := map[string]bool{"kyc": false, "pof": false, "stm": false, "poa": false, "sow": false, "chain": false}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// 数值逐条对齐 console.html 的 POOL。
// reserve 是挂单方钱包里的链上余额，挂单锁的量从这里出。
var pool = []seedOffer{
	{"p1", "CrabWalk Trading", "D118500", "TCrabWalk5n7Yc2mK8pXvL3wR9dHf4bN6t", "sell", "USDT", "CNY", []string{"TRON", "ETH"}, "7.31", "108015", "5000", "308015", 66, 70, 0, 320, "98.7", dset("kyc", "chain")},
	{"p2", "Harbor Desk", "D137037", "THarborDesk2mK8pXvL3wR9dHf4bN6tZaU", "sell", "USDT", "HKD", []string{"TRON"}, "7.81", "45211", "3000", "145211", 82, 78, 0, 180, "99.5", dset("kyc", "pof", "chain")},
	{"p3", "Nova OTC", "D118537", "TNovaOTC7xk2m9wq4vLpR3dNf6bZaU8kQ5", "buy", "USDT", "USD", []string{"TRON", "ETH"}, "1.00", "180923", "1000", "400000", 83, 170, 0, 145, "99.6", dset("kyc", "pof", "stm", "sow", "chain")},
	{"p4", "Pacific Bridge", "D118574", "TPacificBr9pVv7NcGL2sYxk4mQeq3hxKD", "sell", "USDC", "SGD", []string{"POLYGON", "ETH"}, "1.35", "8682", "500", "28682", 84, 113, 0, 160, "99.6", dset("kyc", "pof", "stm", "poa", "chain")},
	{"p5", "Blockstone", "D118611", "TBlockstone4pXvL3wR9dHf7bN6tZaU8kQ", "sell", "USDT", "JPY", []string{"ETH"}, "157", "118034", "100000", "318034", 79, 142, 1, 210, "99.1", dset("kyc", "chain")},
	{"p6", "Eastwind Desk", "D118648", "TEastwind3dNf6bZaU8kQ5n7Yc2mK8pXvL", "buy", "USDC", "CNY", []string{"POLYGON"}, "7.35", "114832", "5000", "250000", 77, 24, 0, 260, "99.0", dset("kyc", "pof")},
	{"p7", "Silver Oak", "D118685", "TSilverOak2sYxk4mQeq9pVv7NcGL3hxKD", "sell", "BTC", "AED", []string{"BTC"}, "343100", "2.004", "20000", "12.004", 74, 25, 0, 290, "98.9", dset("kyc", "chain")},
	{"p8", "Mint Street", "D118722", "TMintStreet8kQ5n7Yc2mK4pXvL3wR9dHf", "sell", "ETH", "EUR", []string{"ETH"}, "2880", "18.23", "2000", "78.23", 75, 120, 3, 240, "98.5", dset("kyc", "pof", "stm")},
	{"p9", "Lotus Capital", "D118759", "TLotusCap6bZaU8kQ5n7Yc2mK8pXvL3wR9", "buy", "USDT", "CNY", []string{"TRON", "ETH"}, "7.28", "31134", "5000", "80000", 97, 125, 4, 62, "99.2", dset("kyc", "pof", "stm", "poa", "sow", "chain")},
	{"p10", "Golden Gate", "D118796", "TGoldenGate7NcGL2sYxk4mQeq9pVv3hxK", "sell", "USDT", "CNY", []string{"TRON"}, "7.32", "18826", "3000", "118826", 90, 124, 0, 95, "99.8", dset("kyc", "pof", "stm", "sow", "chain")},
}
