// 收款方与提现。非托管下链上转账由用户自己签，平台既不代持也不代发——
// 这里存的是地址簿、提现意图与合规材料（用途、凭证），加一个待回填的 tx_hash。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

type Payee struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"-"`
	Label     string    `json:"label"`
	Chain     string    `json:"chain"`
	Address   string    `json:"address"`
	CreatedAt time.Time `json:"created_at"`
}

type Withdrawal struct {
	ID          string          `json:"id"`
	OwnerID     string          `json:"-"`
	PayeeID     string          `json:"payee_id"`
	Asset       string          `json:"asset"`
	Amount      decimal.Decimal `json:"amount"`
	Purpose     string          `json:"purpose"`
	DocUploadID string          `json:"doc_upload_id,omitempty"`
	TxHash      string          `json:"tx_hash,omitempty"`
	State       string          `json:"state"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`

	// 列表要显示打给谁，前端不该为此再查一遍地址簿。
	PayeeLabel   string `json:"payee_label"`
	PayeeChain   string `json:"payee_chain"`
	PayeeAddress string `json:"payee_address"`
}

func (s *Store) Payees(ctx context.Context, ownerID string) ([]Payee, error) {
	rows, err := s.db.QueryContext(ctx,
		`select id,owner_id,label,chain,address,created_at from payees
		  where owner_id=? order by created_at desc`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Payee{}
	for rows.Next() {
		var p Payee
		var created string
		if err := rows.Scan(&p.ID, &p.OwnerID, &p.Label, &p.Chain, &p.Address, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = parseTS(created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Payee 按 (owner, id) 取，不只按 id——归属校验和读取是同一件事，
// 分成两步就会出现「先读到别人的，再想起来校验」这类漏洞。
func (s *Store) Payee(ctx context.Context, ownerID, id string) (*Payee, bool) {
	var p Payee
	var created string
	err := s.db.QueryRowContext(ctx,
		`select id,owner_id,label,chain,address,created_at from payees
		  where owner_id=? and id=?`, ownerID, id).
		Scan(&p.ID, &p.OwnerID, &p.Label, &p.Chain, &p.Address, &created)
	if err != nil {
		return nil, false
	}
	p.CreatedAt = parseTS(created)
	return &p, true
}

// AddPayee 靠表上的 unique (owner_id, chain, address) 报重复，不在 Go 里查后写。
func (s *Store) AddPayee(ctx context.Context, p Payee) error {
	_, err := s.db.ExecContext(ctx,
		`insert into payees(id,owner_id,label,chain,address,created_at) values(?,?,?,?,?,?)`,
		p.ID, p.OwnerID, p.Label, p.Chain, p.Address, ts(p.CreatedAt))
	return err
}

func (s *Store) DeletePayee(ctx context.Context, ownerID, id string) error {
	res, err := s.db.ExecContext(ctx, `delete from payees where owner_id=? and id=?`, ownerID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no such payee")
	}
	return nil
}

func (s *Store) InsertWithdrawal(ctx context.Context, w Withdrawal) error {
	_, err := s.db.ExecContext(ctx,
		`insert into withdrawals(id,owner_id,payee_id,asset_code,amount,purpose,
		                         doc_upload_id,tx_hash,state,created_at,updated_at)
		 values(?,?,?,?,?,?,?,?,?,?,?)`,
		w.ID, w.OwnerID, w.PayeeID, w.Asset, w.Amount.String(), w.Purpose,
		w.DocUploadID, w.TxHash, w.State, ts(w.CreatedAt), ts(w.UpdatedAt))
	return err
}

func (s *Store) Withdrawals(ctx context.Context, ownerID string) ([]Withdrawal, error) {
	rows, err := s.db.QueryContext(ctx,
		`select w.id,w.owner_id,w.payee_id,w.asset_code,w.amount,w.purpose,
		        w.doc_upload_id,w.tx_hash,w.state,w.created_at,w.updated_at,
		        p.label,p.chain,p.address
		   from withdrawals w
		   join payees p on p.id = w.payee_id
		  where w.owner_id=? order by w.created_at desc`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Withdrawal{}
	for rows.Next() {
		var w Withdrawal
		var amount, created, updated string
		if err := rows.Scan(&w.ID, &w.OwnerID, &w.PayeeID, &w.Asset, &amount, &w.Purpose,
			&w.DocUploadID, &w.TxHash, &w.State, &created, &updated,
			&w.PayeeLabel, &w.PayeeChain, &w.PayeeAddress); err != nil {
			return nil, err
		}
		// 金额全程走字符串与 decimal，不经 float——18 位精度下 float 会改尾数。
		w.Amount, _ = decimal.NewFromString(amount)
		w.CreatedAt, w.UpdatedAt = parseTS(created), parseTS(updated)
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetWithdrawalTx 回填用户自己签出来的那笔链上转账。
func (s *Store) SetWithdrawalTx(ctx context.Context, ownerID, id, txHash, state string) error {
	_, err := s.db.ExecContext(ctx,
		`update withdrawals set tx_hash=?, state=?, updated_at=? where owner_id=? and id=?`,
		txHash, state, ts(Now()), ownerID, id)
	return err
}

var _ = sql.ErrNoRows
