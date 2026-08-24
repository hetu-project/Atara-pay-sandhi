package store

import (
	"context"
	"database/sql"
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
	o.unit_price,o.qty,o.remaining_qty,o.min_lot,o.status,o.created_at`

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
		&price, &qty, &rem, &minLot, &o.Status, &created); err != nil {
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
			unit_price,qty,remaining_qty,min_lot,status,created_at,updated_at)
		 values(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.MakerID, o.Side, o.Asset, o.Network, strings.Join(o.Networks, ","), o.Fiat,
		decStr(o.UnitPrice), decStr(o.Qty), decStr(o.RemainingQty), decStr(o.MinLot),
		o.Status, ts(o.CreatedAt), ts(o.CreatedAt))
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
		return ErrInsufficient
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

func SetOfferStatus(tx *sql.Tx, offerID, status string) error {
	_, err := tx.Exec(`update offers set status=?, updated_at=? where id=?`, status, ts(Now()), offerID)
	return err
}
