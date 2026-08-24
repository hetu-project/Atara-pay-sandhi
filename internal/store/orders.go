package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/advaita/atara-pay/internal/domain/condition"
	"github.com/advaita/atara-pay/internal/domain/order"
)

const orderCols = `id,ref,kind,owner_id,counterparty_id,asset_code,amount,note,card_id,
	state,terminal,state_deadline,created_at,updated_at`

func (s *Store) InsertOrder(tx *sql.Tx, o *order.Order) error {
	if _, err := tx.Exec(
		`insert into orders(`+orderCols+`) values(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.Ref, o.Kind, o.OwnerID, emptyToNull(o.CounterpartyID), o.Asset, decStr(o.Amount),
		o.Note, emptyToNull(o.CardID), o.State, emptyToNull(string(o.Terminal)),
		nullTS(o.StateDeadline), ts(o.CreatedAt), ts(o.UpdatedAt)); err != nil {
		return err
	}
	if o.Cond != nil {
		if _, err := tx.Exec(
			`insert into order_conditional(order_id,main_branch,waiting_on,condition_text,fallback_days,dispute_window_secs)
			 values(?,?,?,?,?,?)`,
			o.ID, o.Cond.Main, o.Cond.WaitingOn, o.Cond.Text, o.Cond.FallbackDays, o.Cond.DisputeWindowSecs); err != nil {
			return err
		}
		for i, a := range o.Conds {
			p, _ := json.Marshal(a.Params)
			if _, err := tx.Exec(
				`insert into order_conditions(order_id,seq,atom_type,params) values(?,?,?,?)`,
				o.ID, i+1, a.Type, string(p)); err != nil {
				return err
			}
		}
	}
	if o.OTC != nil {
		if _, err := tx.Exec(
			`insert into order_otc(order_id,offer_id,side,unit_price,fiat_code,fiat_amount,network)
			 values(?,?,?,?,?,?,?)`,
			o.ID, o.OTC.OfferID, o.OTC.Side, decStr(o.OTC.UnitPrice), o.OTC.FiatCode,
			decStr(o.OTC.FiatAmount), o.OTC.Network); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Order(ctx context.Context, id string) (*order.Order, error) {
	return loadOrder(s.db.QueryRowContext(ctx, `select `+orderCols+` from orders where id=?`, id).Scan,
		func(q string, a ...any) (*sql.Rows, error) { return s.db.QueryContext(ctx, q, a...) },
		func(q string, a ...any) *sql.Row { return s.db.QueryRowContext(ctx, q, a...) })
}

func OrderTx(tx *sql.Tx, id string) (*order.Order, error) {
	return loadOrder(tx.QueryRow(`select `+orderCols+` from orders where id=?`, id).Scan,
		tx.Query, tx.QueryRow)
}

func loadOrder(scan func(...any) error,
	query func(string, ...any) (*sql.Rows, error),
	queryRow func(string, ...any) *sql.Row) (*order.Order, error) {

	o, err := scanOrder(scan)
	if err != nil {
		return nil, err
	}
	if o.Kind == order.ConditionalTransfer {
		var c order.Conditional
		err := queryRow(`select main_branch,waiting_on,condition_text,fallback_days,dispute_window_secs
			from order_conditional where order_id=?`, o.ID).
			Scan(&c.Main, &c.WaitingOn, &c.Text, &c.FallbackDays, &c.DisputeWindowSecs)
		if err == nil {
			o.Cond = &c
		}
		rows, err := query(`select atom_type,params from order_conditions where order_id=? order by seq`, o.ID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var a condition.Atom
				var p string
				if err := rows.Scan(&a.Type, &p); err != nil {
					return nil, err
				}
				a.Params = map[string]string{}
				_ = json.Unmarshal([]byte(p), &a.Params)
				o.Conds = append(o.Conds, a)
			}
		}
	}
	if o.Kind == order.OTCTake {
		var t order.OTC
		var price, amt string
		err := queryRow(`select offer_id,side,unit_price,fiat_code,fiat_amount,network
			from order_otc where order_id=?`, o.ID).
			Scan(&t.OfferID, &t.Side, &price, &t.FiatCode, &amt, &t.Network)
		if err == nil {
			t.UnitPrice, t.FiatAmount = dec(price), dec(amt)
			o.OTC = &t
		}
	}
	return o, nil
}

func scanOrder(scan func(...any) error) (*order.Order, error) {
	var o order.Order
	var cp, card, term, deadline sql.NullString
	var amount, created, updated string
	if err := scan(&o.ID, &o.Ref, &o.Kind, &o.OwnerID, &cp, &o.Asset, &amount, &o.Note, &card,
		&o.State, &term, &deadline, &created, &updated); err != nil {
		return nil, err
	}
	o.CounterpartyID, o.CardID = nullStr(cp), nullStr(card)
	o.Terminal = order.Terminal(nullStr(term))
	o.Amount = dec(amount)
	o.CreatedAt, o.UpdatedAt = parseTS(created), parseTS(updated)
	if deadline.Valid {
		t := parseTS(deadline.String)
		o.StateDeadline = &t
	}
	return &o, nil
}

type OrderFilter struct {
	Owner    string
	Kind     string
	State    string
	Terminal string
	Open     bool // 只要非终态
}

func (s *Store) Orders(ctx context.Context, f OrderFilter) ([]*order.Order, error) {
	q := `select ` + orderCols + ` from orders where 1=1`
	var args []any
	if f.Owner != "" {
		q += ` and (owner_id=? or counterparty_id=?)`
		args = append(args, f.Owner, f.Owner)
	}
	if f.Kind != "" {
		q += ` and kind=?`
		args = append(args, f.Kind)
	}
	if f.State != "" {
		q += ` and state=?`
		args = append(args, f.State)
	}
	if f.Terminal != "" {
		q += ` and terminal=?`
		args = append(args, f.Terminal)
	}
	if f.Open {
		q += ` and terminal is null`
	}
	q += ` order by created_at desc limit 200`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		o, err := scanOrder(rows.Scan)
		if err != nil {
			return nil, err
		}
		ids = append(ids, o.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*order.Order, 0, len(ids))
	for _, id := range ids {
		o, err := s.Order(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// Due 返回到期需要系统推进的工单。SQLite 单写者，调度器本身也是单实例，
// 所以不需要 SELECT ... FOR UPDATE SKIP LOCKED。
func (s *Store) Due(ctx context.Context, now time.Time) ([]*order.Order, error) {
	rows, err := s.db.QueryContext(ctx,
		`select id from orders where terminal is null and state_deadline is not null and state_deadline<=?
		 order by state_deadline limit 100`, ts(now))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]*order.Order, 0, len(ids))
	for _, id := range ids {
		o, err := s.Order(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func SaveState(tx *sql.Tx, o *order.Order) error {
	_, err := tx.Exec(
		`update orders set state=?, terminal=?, state_deadline=?, updated_at=? where id=?`,
		o.State, emptyToNull(string(o.Terminal)), nullTS(o.StateDeadline), ts(Now()), o.ID)
	return err
}

func AppendEvent(tx *sql.Tx, orderID, from, to string, actor order.Actor, reason string, payload map[string]string) error {
	var seq int
	_ = tx.QueryRow(`select coalesce(max(seq),0) from order_events where order_id=?`, orderID).Scan(&seq)
	p, _ := json.Marshal(payload)
	_, err := tx.Exec(
		`insert into order_events(order_id,seq,from_state,to_state,actor,reason,payload,created_at)
		 values(?,?,?,?,?,?,?,?)`,
		orderID, seq+1, emptyToNull(from), to, actor, reason, string(p), ts(Now()))
	return err
}

func (s *Store) Events(ctx context.Context, orderID string) ([]order.Log, error) {
	rows, err := s.db.QueryContext(ctx,
		`select seq,from_state,to_state,actor,reason,payload,created_at
		   from order_events where order_id=? order by seq`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []order.Log
	for rows.Next() {
		var e order.Log
		var from sql.NullString
		var payload, created string
		if err := rows.Scan(&e.Seq, &from, &e.To, &e.Actor, &e.Reason, &payload, &created); err != nil {
			return nil, err
		}
		e.From = nullStr(from)
		e.CreatedAt = parseTS(created)
		e.Payload = map[string]string{}
		_ = json.Unmarshal([]byte(payload), &e.Payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

func InsertReceipt(tx *sql.Tx, orderID, uploaderID, fileRef string) (string, error) {
	id := NewID()
	_, err := tx.Exec(
		`insert into fiat_receipts(id,order_id,uploader_id,file_ref,created_at) values(?,?,?,?,?)`,
		id, orderID, uploaderID, fileRef, ts(Now()))
	return id, err
}

func (s *Store) Receipt(ctx context.Context, orderID string) (string, bool) {
	var ref string
	err := s.db.QueryRowContext(ctx,
		`select file_ref from fiat_receipts where order_id=? order by created_at desc limit 1`, orderID).Scan(&ref)
	return ref, err == nil
}
