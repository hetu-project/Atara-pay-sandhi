package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/shopspring/decimal"
)

type OfferFilter struct {
	Side   string // 买家视角想要的方向留给上层换算，这里是挂单自身的 side
	Asset  string
	Fiat   string
	Status string
	Maker  string
}

const offerCols = `o.id,o.maker_id,o.side,o.asset_code,o.network,o.networks,o.fiat_code,
	o.unit_price,o.qty,o.remaining_qty,o.min_lot,o.lock_tx,o.status,o.created_at`

func (s *Store) Offers(ctx context.Context, f OfferFilter) ([]*model.Offer, error) {
	q := `select ` + offerCols + ` from offers o where 1=1`
	var args []any
	add := func(cond string, v string) {
		if v != "" {
			q += cond
			args = append(args, v)
		}
	}
	add(` and o.side=?`, f.Side)
	add(` and o.asset_code=?`, f.Asset)
	add(` and o.fiat_code=?`, f.Fiat)
	add(` and o.status=?`, f.Status)
	add(` and o.maker_id=?`, f.Maker)
	q += ` order by o.created_at desc`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Offer
	for rows.Next() {
		o, err := scanOffer(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 挂单卡缺了信任分与履约数据就没法比价——一并带出来。
	for _, o := range out {
		o.Maker, _ = s.User(ctx, o.MakerID)
		o.Merchant, _ = s.Merchant(ctx, o.MakerID)
	}
	return out, nil
}

func (s *Store) Offer(ctx context.Context, id string) (*model.Offer, error) {
	row := s.db.QueryRowContext(ctx, `select `+offerCols+` from offers o where o.id=?`, id)
	o, err := scanOffer(row.Scan)
	if err != nil {
		return nil, err
	}
	o.Maker, _ = s.User(ctx, o.MakerID)
	o.Merchant, _ = s.Merchant(ctx, o.MakerID)
	return o, nil
}

func offerTx(tx *sql.Tx, id string) (*model.Offer, error) {
	row := tx.QueryRow(`select `+offerCols+` from offers o where o.id=?`, id)
	return scanOffer(row.Scan)
}

func scanOffer(scan func(...any) error) (*model.Offer, error) {
	var o model.Offer
	var nets, price, qty, rem, minLot, created string
	if err := scan(&o.ID, &o.MakerID, &o.Side, &o.Asset, &o.Network, &nets, &o.Fiat,
		&price, &qty, &rem, &minLot, &o.LockTx, &o.Status, &created); err != nil {
		return nil, err
	}
	o.Networks = strings.Split(nets, ",")
	o.UnitPrice, o.Qty, o.RemainingQty, o.MinLot = dec(price), dec(qty), dec(rem), dec(minLot)
	o.CreatedAt = parseTS(created)
	return &o, nil
}

func (s *Store) InsertOffer(tx *sql.Tx, o *model.Offer) error {
	_, err := tx.Exec(
		`insert into offers(id,maker_id,side,asset_code,network,networks,fiat_code,
			unit_price,qty,remaining_qty,min_lot,lock_tx,status,created_at,updated_at)
		 values(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.MakerID, o.Side, o.Asset, o.Network, strings.Join(o.Networks, ","), o.Fiat,
		decStr(o.UnitPrice), decStr(o.Qty), decStr(o.RemainingQty), decStr(o.MinLot),
		o.LockTx, o.Status, ts(o.CreatedAt), ts(o.CreatedAt))
	return err
}

// ReserveQty 扣减可成交量。买家看到的可成交量必须真的在托管里，
// 所以预留和回补都必须落到这一列上。
func ReserveQty(tx *sql.Tx, offerID string, delta decimal.Decimal) error {
	o, err := offerTx(tx, offerID)
	if err != nil {
		return err
	}
	next := o.RemainingQty.Add(delta)
	if next.IsNegative() {
		return ErrOversold
	}
	status := o.Status
	if next.IsZero() && o.Status == "active" {
		status = "filled"
	}
	if next.IsPositive() && o.Status == "filled" {
		status = "active"
	}
	_, err = tx.Exec(`update offers set remaining_qty=?, status=?, updated_at=? where id=?`,
		decStr(next), status, ts(Now()), offerID)
	return err
}

func SetOfferLockTx(tx *sql.Tx, offerID, tx2 string) error {
	_, err := tx.Exec(`update offers set lock_tx=? where id=?`, tx2, offerID)
	return err
}

func SetOfferStatus(tx *sql.Tx, offerID, status string) error {
	_, err := tx.Exec(`update offers set status=?, updated_at=? where id=?`, status, ts(Now()), offerID)
	return err
}

// EligiblePeer 是「能吃下这单」的对手方，带前端列表要的头像与信誉。
type EligiblePeer struct {
	UserID       string          `json:"user_id"`
	Name         string          `json:"display_name"`
	PeerCode     string          `json:"peer_code"`
	Hue          int             `json:"hue"`
	AvatarURL    string          `json:"avatar_url"`
	TrustScore   int             `json:"trust_score"`
	Deals        int             `json:"deals"`
	BestPrice    decimal.Decimal `json:"best_price"`
	AvailableQty decimal.Decimal `json:"available_qty"`
}

// EligibleCounterparties 列出真能接下这笔的人。
//
// 五条判定：方向相反、挂单活跃、余量够、起投额不超、法币与资产都对上，
// 外加排除自己。挡不住其中任何一条，前端就会把不能成交的人摆进列表——
// 用户点了「跟他交易」却撮不动，比一开始就不显示他更糟。
//
// 金额比较全部用 decimal 在 Go 里做，不写进 SQL：库里金额是 TEXT，
// SQL 的字符串比较会把 "9" 排在 "10" 后面，而 cast as real 会丢精度。
// 更要紧的是单位不同——remaining_qty 是币，min_lot 是法币（见 checkLot），
// 拿同一个数去比两者就会把撮不动的人放进来。所以按挂单的单价逐条换算。
func (s *Store) EligibleCounterparties(ctx context.Context,
	viewerID, side, asset, fiat, amountKind string, amount decimal.Decimal) ([]EligiblePeer, error) {
	want := "sell"
	if side == "sell" {
		want = "buy"
	}
	rows, err := s.db.QueryContext(ctx,
		`select u.id, u.display_name, coalesce(m.peer_code,''), u.hue, u.avatar_url,
		        coalesce(m.trust_score,0), coalesce(m.deals,0),
		        o.unit_price, o.remaining_qty, o.min_lot
		   from offers o
		   join users u on u.id = o.maker_id
		   left join merchant_profiles m on m.user_id = o.maker_id
		  where o.status = 'active'
		    and o.side = ?
		    and o.asset_code = ?
		    and o.fiat_code = ?
		    and o.maker_id <> ?`,
		want, asset, fiat, viewerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 一个 maker 可能挂了多条能吃的单。列表是按人给的，不是按单给的，
	// 所以每人只留价格最优的那条。
	best := map[string]EligiblePeer{}
	var order []string
	for rows.Next() {
		var p EligiblePeer
		var price, qty, minLot string
		if err := rows.Scan(&p.UserID, &p.Name, &p.PeerCode, &p.Hue, &p.AvatarURL,
			&p.TrustScore, &p.Deals, &price, &qty, &minLot); err != nil {
			return nil, err
		}
		up, err := decimal.NewFromString(price)
		if err != nil || up.IsZero() {
			continue
		}
		remaining, err := decimal.NewFromString(qty)
		if err != nil {
			continue
		}
		lot, err := decimal.NewFromString(minLot)
		if err != nil {
			continue
		}
		// amount 可能是币也可能是法币。按这条挂单的单价换算成两种口径，
		// 再分别跟余量（币）和起投额（法币）比。
		coinAmt, fiatAmt := amount, amount.Mul(up)
		if amountKind == "fiat" {
			coinAmt, fiatAmt = amount.Div(up), amount
		}
		if remaining.LessThan(coinAmt) || fiatAmt.LessThan(lot) {
			continue
		}
		p.BestPrice, p.AvailableQty = up, remaining
		if prev, seen := best[p.UserID]; seen {
			if up.LessThan(prev.BestPrice) {
				best[p.UserID] = p
			}
			continue
		}
		best[p.UserID] = p
		order = append(order, p.UserID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]EligiblePeer, 0, len(order))
	for _, id := range order {
		out = append(out, best[id])
	}
	// 价格最优的排前面——前端默认走第一个，排序就是默认选择。
	sort.SliceStable(out, func(i, j int) bool { return out[i].BestPrice.LessThan(out[j].BestPrice) })
	return out, nil
}
