package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/shopspring/decimal"
)

var ErrInsufficient = errors.New("insufficient balance")

func (s *Store) Wallets(ctx context.Context, userID string) ([]*model.Wallet, error) {
	rows, err := s.db.QueryContext(ctx,
		`select id,user_id,asset_code,available,escrowed from wallets where user_id=? order by asset_code`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Wallet
	for rows.Next() {
		var w model.Wallet
		var av, es string
		if err := rows.Scan(&w.ID, &w.UserID, &w.Asset, &av, &es); err != nil {
			return nil, err
		}
		w.Available, w.Escrowed = dec(av), dec(es)
		out = append(out, &w)
	}
	return out, rows.Err()
}

func (s *Store) Wallet(ctx context.Context, userID, asset string) (*model.Wallet, error) {
	var w model.Wallet
	var av, es string
	err := s.db.QueryRowContext(ctx,
		`select id,user_id,asset_code,available,escrowed from wallets where user_id=? and asset_code=?`,
		userID, asset).Scan(&w.ID, &w.UserID, &w.Asset, &av, &es)
	if errors.Is(err, sql.ErrNoRows) {
		return &model.Wallet{UserID: userID, Asset: asset, Available: decimal.Zero, Escrowed: decimal.Zero}, nil
	}
	if err != nil {
		return nil, err
	}
	w.Available, w.Escrowed = dec(av), dec(es)
	return &w, nil
}

// walletTx 取或建钱包行。法币永远不会走到这里——wallets 只有 crypto 行。
func walletTx(tx *sql.Tx, userID, asset string) (*model.Wallet, error) {
	var w model.Wallet
	var av, es string
	err := tx.QueryRow(
		`select id,user_id,asset_code,available,escrowed from wallets where user_id=? and asset_code=?`,
		userID, asset).Scan(&w.ID, &w.UserID, &w.Asset, &av, &es)
	if errors.Is(err, sql.ErrNoRows) {
		w = model.Wallet{ID: NewID(), UserID: userID, Asset: asset, Available: decimal.Zero, Escrowed: decimal.Zero}
		if _, err := tx.Exec(
			`insert into wallets(id,user_id,asset_code,available,escrowed) values(?,?,?,'0','0')`,
			w.ID, userID, asset); err != nil {
			return nil, err
		}
		return &w, nil
	}
	if err != nil {
		return nil, err
	}
	w.Available, w.Escrowed = dec(av), dec(es)
	return &w, nil
}

// Move 是资金变动的唯一入口：同事务内写一条 ledger_entries + 更新 wallets。
// available/escrowed 是物化的和，负数即拒——正常路径应在应用层先拦下。
func Move(tx *sql.Tx, userID, asset, kind string, dAvail, dEsc decimal.Decimal, orderID, offerID, memo string) error {
	w, err := walletTx(tx, userID, asset)
	if err != nil {
		return err
	}
	nextAvail := w.Available.Add(dAvail)
	nextEsc := w.Escrowed.Add(dEsc)
	if nextAvail.IsNegative() {
		return fmt.Errorf("%w: %s available %s, need %s", ErrInsufficient, asset, w.Available, dAvail.Neg())
	}
	if nextEsc.IsNegative() {
		return fmt.Errorf("escrow would go negative for %s %s", userID, asset)
	}
	if _, err := tx.Exec(`update wallets set available=?, escrowed=? where id=?`,
		decStr(nextAvail), decStr(nextEsc), w.ID); err != nil {
		return err
	}
	_, err = tx.Exec(
		`insert into ledger_entries(wallet_id,order_id,offer_id,kind,delta_available,delta_escrowed,memo,created_at)
		 values(?,?,?,?,?,?,?,?)`,
		w.ID, emptyToNull(orderID), emptyToNull(offerID), kind, decStr(dAvail), decStr(dEsc), memo, ts(Now()))
	return err
}
