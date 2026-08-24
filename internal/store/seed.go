package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Seed 灌入演示数据，全部来自 console.html 的 CPS / ASSETS / CARDS / POOL——
// 前端把 mock 换成本服务后，画面应该和现在一模一样。
// 已经灌过就跳过：重启不该复制一份池子出来。
func (s *Store) Seed(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `select count(*) from users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	now := ts(Now())

	return s.Tx(ctx, func(tx *sql.Tx) error {
		user := func(id, handle, name, kind string) error {
			_, err := tx.Exec(`insert into users(id,handle,email,display_name,kind,created_at) values(?,?,?,?,?,?)`,
				id, handle, handle+"@atara.example", name, kind, now)
			return err
		}
		wallet := func(uid, asset, avail, esc string) error {
			_, err := tx.Exec(`insert into wallets(id,user_id,asset_code,available,escrowed) values(?,?,?,?,?)`,
				NewID(), uid, asset, avail, esc)
			return err
		}

		// ── demo 用户 ──
		if err := user(demoID, DemoHandle, "Demo Account", "person"); err != nil {
			return err
		}
		for _, w := range [][4]string{
			{demoID, "USDT", "34500", "5400"},
			{demoID, "USDC", "1200", "0"},
			{demoID, "BTC", "0.42", "0"},
			{demoID, "ETH", "3.6", "0"},
		} {
			if err := wallet(w[0], w[1], w[2], w[3]); err != nil {
				return err
			}
		}

		// 授权卡：人自己也是一张卡，agent 各是一张
		cards := []struct {
			id, name, kind, cycle, quota, used, per, tpl, note string
			on                                                 int
		}{
			{"card-me", "Me", "person", "weekly", "25000", "8400", "10000", "Any",
				"Hitting the limit stops autopay, not incoming funds", 1},
			{"card-pa", "Procurement agent", "agent", "weekly", "2000", "340", "500", "On delivery · 7-day window",
				"Over-limit returns for your approval", 1},
			{"card-da", "Data agent", "agent", "monthly", "800", "612", "200", "On successful call",
				"High frequency, low ceiling", 1},
			{"card-ta", "Travel agent", "agent", "monthly", "5000", "0", "1500", "On ticketing · refundable",
				"Set an allowlist before enabling", 0},
		}
		for _, c := range cards {
			if _, err := tx.Exec(
				`insert into authorization_cards(id,owner_id,name,kind,cycle,quota,used,per_deal_cap,allowlist,template,enabled,note)
				 values(?,?,?,?,?,?,?,?,'',?,?,?)`,
				c.id, demoID, c.name, c.kind, c.cycle, c.quota, c.used, c.per, c.tpl, c.on, c.note); err != nil {
				return err
			}
		}

		// ── 联系人：条件支付的对手方 ──
		contacts := []struct{ id, handle, name, kind string }{
			{"cp-hc", "huachuang", "Huachuang", "firm"},
			{"cp-kj", "kenji", "Kenji M.", "person"},
			{"cp-ar", "aria", "Aria Studio", "firm"},
			{"cp-pa", "procurement", "Procurement agent", "agent"},
		}
		for _, c := range contacts {
			if err := user(c.id, c.handle, c.name, c.kind); err != nil {
				return err
			}
			// 对手方也要有余额：条件支付放款要打进他们的钱包
			for _, a := range []string{"USDT", "USDC", "BTC", "ETH"} {
				if err := wallet(c.id, a, "0", "0"); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(
				`insert into merchant_profiles(user_id,peer_code,trust_score,deals,disputes,fill_rate,median_release_secs,docs)
				 values(?,?,?,?,?,?,?,?)`,
				c.id, "ATR-"+strings.ToUpper(c.handle[:4]), 90, 12, 0, "97", 180, `{"kyc":true}`); err != nil {
				return err
			}
		}

		// ── 挂单池：10 条，对齐前端 POOL ──
		for _, p := range pool {
			mid := "mk-" + p.id
			if err := user(mid, "maker-"+p.id, p.name, "firm"); err != nil {
				return err
			}
			docs, _ := json.Marshal(p.docs)
			if _, err := tx.Exec(
				`insert into merchant_profiles(user_id,peer_code,trust_score,deals,disputes,fill_rate,median_release_secs,docs)
				 values(?,?,?,?,?,?,?,?)`,
				mid, p.peer, p.score, p.deals, p.disputes, p.fillRate, p.releaseSecs, string(docs)); err != nil {
				return err
			}
			// 卖单挂出即锁币：种子里直接把这部分放进 escrowed，
			// 与运行时 CreateOffer 走 offer_lock 得到的账面一致。
			esc := "0"
			if p.side == "sell" {
				esc = p.qty
			}
			if err := wallet(mid, p.asset, p.reserve, esc); err != nil {
				return err
			}
			if _, err := tx.Exec(
				`insert into offers(id,maker_id,side,asset_code,network,networks,fiat_code,
					unit_price,qty,remaining_qty,min_lot,status,created_at,updated_at)
				 values(?,?,?,?,?,?,?,?,?,?,?,'active',?,?)`,
				p.id, mid, p.side, p.asset, p.nets[0], strings.Join(p.nets, ","), p.fiat,
				p.price, p.qty, p.qty, p.minLot, now, now); err != nil {
				return err
			}
		}
		return nil
	})
}

const (
	demoID     = "user-demo"
	DemoHandle = "demo"
)

func (s *Store) DemoUserID() string { return demoID }

type seedOffer struct {
	id, name, peer, side, asset, fiat   string
	nets                                []string
	price, qty, minLot, reserve         string
	score, deals, disputes, releaseSecs int
	fillRate                            string
	docs                                map[string]bool
}

func dset(keys ...string) map[string]bool {
	m := map[string]bool{"kyc": false, "pof": false, "stm": false, "poa": false, "sow": false, "chain": false}
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// 数值逐条对齐 console.html 的 POOL。
// reserve 是挂单方的可用余额，给注资托管留出空间。
var pool = []seedOffer{
	{"p1", "CrabWalk Trading", "D118500", "sell", "USDT", "CNY", []string{"TRON", "ETH"}, "7.31", "108015", "5000", "200000", 66, 70, 0, 320, "98.7", dset("kyc", "chain")},
	{"p2", "Harbor Desk", "D137037", "sell", "USDT", "HKD", []string{"TRON"}, "7.81", "45211", "3000", "100000", 82, 78, 0, 180, "99.5", dset("kyc", "pof", "chain")},
	{"p3", "Nova OTC", "D118537", "buy", "USDT", "USD", []string{"TRON", "ETH"}, "1.00", "180923", "1000", "400000", 83, 170, 0, 145, "99.6", dset("kyc", "pof", "stm", "sow", "chain")},
	{"p4", "Pacific Bridge", "D118574", "sell", "USDC", "SGD", []string{"POLYGON", "ETH"}, "1.35", "8682", "500", "20000", 84, 113, 0, 160, "99.6", dset("kyc", "pof", "stm", "poa", "chain")},
	{"p5", "Blockstone", "D118611", "sell", "USDT", "JPY", []string{"ETH"}, "157", "118034", "100000", "250000", 79, 142, 1, 210, "99.1", dset("kyc", "chain")},
	{"p6", "Eastwind Desk", "D118648", "buy", "USDC", "CNY", []string{"POLYGON"}, "7.35", "114832", "5000", "250000", 77, 24, 0, 260, "99.0", dset("kyc", "pof")},
	{"p7", "Silver Oak", "D118685", "sell", "BTC", "AED", []string{"BTC"}, "343100", "2.004", "20000", "10", 74, 25, 0, 290, "98.9", dset("kyc", "chain")},
	{"p8", "Mint Street", "D118722", "sell", "ETH", "EUR", []string{"ETH"}, "2880", "18.23", "2000", "60", 75, 120, 3, 240, "98.5", dset("kyc", "pof", "stm")},
	{"p9", "Lotus Capital", "D118759", "buy", "USDT", "CNY", []string{"TRON", "ETH"}, "7.28", "31134", "5000", "80000", 97, 125, 4, 62, "99.2", dset("kyc", "pof", "stm", "poa", "sow", "chain")},
	{"p10", "Golden Gate", "D118796", "sell", "USDT", "CNY", []string{"TRON"}, "7.32", "18826", "3000", "50000", 90, 124, 0, 95, "99.8", dset("kyc", "pof", "stm", "sow", "chain")},
}

var _ = time.Now
