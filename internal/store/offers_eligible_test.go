package store

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
)

// seedEligibleUser 补一个带信誉档的用户。候选列表要 join merchant_profiles，
// 没有档的人查出来信誉是零值——所以两张表都要种。
func seedEligibleUser(t *testing.T, st *Store, id, name string) {
	t.Helper()
	if _, err := st.DB().Exec(
		`insert or ignore into users(id,address,display_name,hue,created_at)
		 values(?,?,?,?,datetime('now'))`, id, "0x"+id, name, 221); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
	if _, err := st.DB().Exec(
		`insert or ignore into merchant_profiles(user_id,peer_code,trust_score,deals)
		 values(?,?,?,?)`, id, "D"+id, 90, 12); err != nil {
		t.Fatalf("seed merchant %s: %v", id, err)
	}
}

func seedTestOffer(t *testing.T, st *Store, id, maker, side, asset, fiat, price, remaining, minLot, status string) {
	t.Helper()
	seedEligibleUser(t, st, maker, "M-"+maker)
	if _, err := st.DB().Exec(
		`insert into offers(id,maker_id,side,asset_code,network,networks,fiat_code,
		                    unit_price,qty,remaining_qty,min_lot,status,created_at,updated_at)
		 values(?,?,?,?,'TRON','["TRON"]',?,?,?,?,?,?,datetime('now'),datetime('now'))`,
		id, maker, side, asset, fiat, price, remaining, remaining, minLot, status); err != nil {
		t.Fatalf("seed offer %s: %v", id, err)
	}
}

// 「真能吃下这单」有五条判定。每条都要有一个被它挡下来的用例，
// 否则规则写错了测试也发现不了——列表里摆着不能成交的人，是最坏的结果。
func TestEligibleCounterparties(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	d := func(s string) decimal.Decimal { v, _ := decimal.NewFromString(s); return v }

	// viewer(u1) 想买 1000 USDT 付 CNY，能吃下的必须是 sell 方向的活跃挂单。
	seedTestOffer(t, st, "o-ok", "m-ok", "sell", "USDT", "CNY", "7.3", "5000", "100", "active")
	seedTestOffer(t, st, "o-delisted", "m-delisted", "sell", "USDT", "CNY", "7.3", "5000", "100", "delisted")
	seedTestOffer(t, st, "o-samedir", "m-samedir", "buy", "USDT", "CNY", "7.3", "5000", "100", "active")
	seedTestOffer(t, st, "o-small", "m-small", "sell", "USDT", "CNY", "7.3", "10", "1", "active")
	// 起投额是法币口径：买 1000 USDT @7.3 = 7300 CNY，挡不住 7300 的才该出现
	seedTestOffer(t, st, "o-minlot", "m-minlot", "sell", "USDT", "CNY", "7.3", "5000", "99999", "active")
	seedTestOffer(t, st, "o-fiat", "m-fiat", "sell", "USDT", "HKD", "7.3", "5000", "100", "active")
	seedTestOffer(t, st, "o-asset", "m-asset", "sell", "BTC", "CNY", "7.3", "5000", "100", "active")
	seedTestOffer(t, st, "o-self", "u1", "sell", "USDT", "CNY", "7.3", "5000", "100", "active")

	got, err := st.EligibleCounterparties(ctx, "u1", "buy", "USDT", "CNY", "coin", d("1000"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var ids []string
	for _, p := range got {
		ids = append(ids, p.UserID)
	}
	if len(got) != 1 || got[0].UserID != "m-ok" {
		t.Fatalf("命中 = %v, want [m-ok]", ids)
	}
	if got[0].Hue != 221 || got[0].TrustScore != 90 || got[0].PeerCode != "Dm-ok" {
		t.Fatalf("头像与信誉字段没带上: %+v", got[0])
	}
	if !got[0].AvailableQty.Equal(d("5000")) {
		t.Fatalf("余量 = %s, want 5000", got[0].AvailableQty)
	}
}

// 同一个 maker 挂了多条能吃的单，只应出现一次，且取价格最优的那条。
func TestEligibleCounterpartiesDedupesByMaker(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	d := func(s string) decimal.Decimal { v, _ := decimal.NewFromString(s); return v }

	seedEligibleUser(t, st, "m1", "M1")
	for _, c := range []struct{ id, price string }{{"a", "7.50"}, {"b", "7.10"}, {"c", "7.90"}} {
		if _, err := st.DB().Exec(
			`insert into offers(id,maker_id,side,asset_code,network,networks,fiat_code,
			                    unit_price,qty,remaining_qty,min_lot,status,created_at,updated_at)
			 values(?,'m1','sell','USDT','TRON','["TRON"]','CNY',?,'5000','5000','100','active',
			        datetime('now'),datetime('now'))`, c.id, c.price); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	got, err := st.EligibleCounterparties(ctx, "u1", "buy", "USDT", "CNY", "coin", d("1000"))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("同一个 maker 出现了 %d 次，应只出现 1 次", len(got))
	}
	if !got[0].BestPrice.Equal(d("7.10")) {
		t.Fatalf("取的不是最优价: %s, want 7.10", got[0].BestPrice)
	}
}

// 起投额是法币口径，余量是币口径（见 app.checkLot）。拿同一个数去比两者，
// 就会把撮不动的人放进列表——这个功能存在的意义正是防这件事。
func TestEligibleCounterpartiesUnitsDoNotMix(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	d := func(s string) decimal.Decimal { v, _ := decimal.NewFromString(s); return v }

	// 单价 7.3，余量 5000 USDT，起投额 3000 CNY（≈411 USDT）。
	seedTestOffer(t, st, "o1", "m1", "sell", "USDT", "CNY", "7.3", "5000", "3000", "active")

	// 买 1000 USDT = 7300 CNY ≥ 3000 起投额，余量也够 → 应命中。
	// 若把 1000 直接当法币去比 3000，会被误判为低于起投额而漏掉。
	got, err := st.EligibleCounterparties(ctx, "u1", "buy", "USDT", "CNY", "coin", d("1000"))
	if err != nil || len(got) != 1 {
		t.Fatalf("币口径 1000 应命中: got=%d err=%v", len(got), err)
	}

	// 按法币口径买 3650 CNY = 500 USDT，余量够、也过起投额 → 应命中。
	got, err = st.EligibleCounterparties(ctx, "u1", "buy", "USDT", "CNY", "fiat", d("3650"))
	if err != nil || len(got) != 1 {
		t.Fatalf("法币口径 3650 应命中: got=%d err=%v", len(got), err)
	}

	// 法币 2000 < 起投额 3000 → 应被挡。
	got, err = st.EligibleCounterparties(ctx, "u1", "buy", "USDT", "CNY", "fiat", d("2000"))
	if err != nil || len(got) != 0 {
		t.Fatalf("低于起投额应被挡: got=%d err=%v", len(got), err)
	}

	// 币 6000 > 余量 5000 → 应被挡。
	got, err = st.EligibleCounterparties(ctx, "u1", "buy", "USDT", "CNY", "coin", d("6000"))
	if err != nil || len(got) != 0 {
		t.Fatalf("超出余量应被挡: got=%d err=%v", len(got), err)
	}
}
