package store

import (
	"context"
	"database/sql"

	"github.com/shopspring/decimal"
)

// ChainEvent 是一条链上动作的观察记录。
//
// 这不是账本：余额不在这里，这里只记「我们看到链上发生了什么」。
// 争议时它和 order_events 一起构成证据链。
type ChainEvent struct {
	Kind    string          `json:"kind"` // deposit | release | refund | listing_lock | listing_unlock | allowance
	Asset   string          `json:"asset,omitempty"`
	Amount  decimal.Decimal `json:"amount"`
	TxHash  string          `json:"tx_hash,omitempty"`
	Memo    string          `json:"memo,omitempty"`
	OrderID string          `json:"order_id,omitempty"`
	OfferID string          `json:"offer_id,omitempty"`
}

func LogChain(tx *sql.Tx, actorID string, e ChainEvent) error {
	_, err := tx.Exec(
		`insert into chain_events(order_id,offer_id,actor_id,kind,asset,amount,tx_hash,memo,created_at)
		 values(?,?,?,?,?,?,?,?,?)`,
		emptyToNull(e.OrderID), emptyToNull(e.OfferID), emptyToNull(actorID),
		e.Kind, e.Asset, decStr(e.Amount), e.TxHash, e.Memo, ts(Now()))
	return err
}

func (s *Store) LogChainNoTx(ctx context.Context, actorID string, e ChainEvent) error {
	_, err := s.db.ExecContext(ctx,
		`insert into chain_events(order_id,offer_id,actor_id,kind,asset,amount,tx_hash,memo,created_at)
		 values(?,?,?,?,?,?,?,?,?)`,
		emptyToNull(e.OrderID), emptyToNull(e.OfferID), emptyToNull(actorID),
		e.Kind, e.Asset, decStr(e.Amount), e.TxHash, e.Memo, ts(Now()))
	return err
}

func (s *Store) ChainEvents(ctx context.Context, orderID string) ([]ChainEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`select kind,asset,amount,tx_hash,memo from chain_events where order_id=? order by id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChainEvent
	for rows.Next() {
		var e ChainEvent
		var amt string
		if err := rows.Scan(&e.Kind, &e.Asset, &amt, &e.TxHash, &e.Memo); err != nil {
			return nil, err
		}
		e.Amount = dec(amt)
		e.OrderID = orderID
		out = append(out, e)
	}
	return out, rows.Err()
}
