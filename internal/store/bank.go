package store

import (
	"context"
	"time"
)

// BankAccount 是用户自己的法币收款账户。
// AccountNo 恒为掩码形式——全量号码从不落库，见 app.checkAccount。
type BankAccount struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"-"`
	Holder    string    `json:"holder"`
	Bank      string    `json:"bank"`
	AccountNo string    `json:"account_no"`
	Currency  string    `json:"currency"`
	Region    string    `json:"region"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const bankCols = `id,owner_id,holder,bank,account_no,currency,region,created_at,updated_at`

func scanBank(scan func(...any) error) (*BankAccount, error) {
	var a BankAccount
	var created, updated string
	if err := scan(&a.ID, &a.OwnerID, &a.Holder, &a.Bank, &a.AccountNo,
		&a.Currency, &a.Region, &created, &updated); err != nil {
		return nil, err
	}
	a.CreatedAt, a.UpdatedAt = parseTS(created), parseTS(updated)
	return &a, nil
}

func (s *Store) BankAccounts(ctx context.Context, ownerID string) ([]BankAccount, error) {
	rows, err := s.db.QueryContext(ctx,
		`select `+bankCols+` from bank_accounts where owner_id=? order by created_at`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BankAccount{}
	for rows.Next() {
		a, err := scanBank(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// BankAccount 按 owner 限定取一条——不带 owner 的按 id 取是越权读的入口。
func (s *Store) BankAccount(ctx context.Context, ownerID, id string) (*BankAccount, error) {
	return scanBank(s.db.QueryRowContext(ctx,
		`select `+bankCols+` from bank_accounts where owner_id=? and id=?`, ownerID, id).Scan)
}

func (s *Store) UpsertBankAccount(ctx context.Context, a BankAccount) error {
	_, err := s.db.ExecContext(ctx,
		`insert into bank_accounts(`+bankCols+`) values(?,?,?,?,?,?,?,?,?)
		 on conflict(id) do update set
		   holder=excluded.holder, bank=excluded.bank, account_no=excluded.account_no,
		   currency=excluded.currency, region=excluded.region, updated_at=excluded.updated_at`,
		a.ID, a.OwnerID, a.Holder, a.Bank, a.AccountNo, a.Currency, a.Region,
		ts(a.CreatedAt), ts(a.UpdatedAt))
	return err
}

func (s *Store) DeleteBankAccount(ctx context.Context, ownerID, id string) error {
	_, err := s.db.ExecContext(ctx,
		`delete from bank_accounts where owner_id=? and id=?`, ownerID, id)
	return err
}
