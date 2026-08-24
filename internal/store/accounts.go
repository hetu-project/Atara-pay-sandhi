package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/shopspring/decimal"
)

func (s *Store) UserByHandle(ctx context.Context, handle string) (*model.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`select id,handle,email,display_name,kind,created_at from users where handle=?`, handle))
}

func (s *Store) User(ctx context.Context, id string) (*model.User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx,
		`select id,handle,email,display_name,kind,created_at from users where id=?`, id))
}

func (s *Store) scanUser(row *sql.Row) (*model.User, error) {
	var u model.User
	var created string
	if err := row.Scan(&u.ID, &u.Handle, &u.Email, &u.DisplayName, &u.Kind, &created); err != nil {
		return nil, err
	}
	u.CreatedAt = parseTS(created)
	return &u, nil
}

// Counterparties 是对手方名册：@ 面板与条件支付的对手方槽用它。
func (s *Store) Counterparties(ctx context.Context, selfID string) ([]*model.User, error) {
	rows, err := s.db.QueryContext(ctx,
		// 排除挂过单的人：他们是大厅里的对手方，不是你的联系人。
		// 不能按「有没有 merchant_profile」筛——联系人也要有 profile 才能回写履约。
		`select id,handle,email,display_name,kind,created_at from users
		  where id<>? and id not in (select distinct maker_id from offers)
		  order by display_name`, selfID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.User
	for rows.Next() {
		var u model.User
		var created string
		if err := rows.Scan(&u.ID, &u.Handle, &u.Email, &u.DisplayName, &u.Kind, &created); err != nil {
			return nil, err
		}
		u.CreatedAt = parseTS(created)
		out = append(out, &u)
	}
	return out, rows.Err()
}

func (s *Store) Merchant(ctx context.Context, userID string) (*model.Merchant, error) {
	var m model.Merchant
	var fill, docs string
	err := s.db.QueryRowContext(ctx,
		`select user_id,peer_code,trust_score,deals,disputes,fill_rate,median_release_secs,docs
		   from merchant_profiles where user_id=?`, userID).
		Scan(&m.UserID, &m.PeerCode, &m.TrustScore, &m.Deals, &m.Disputes, &fill, &m.MedianReleaseSecs, &docs)
	if err != nil {
		return nil, err
	}
	m.FillRate = dec(fill)
	m.Docs = map[string]bool{}
	_ = json.Unmarshal([]byte(docs), &m.Docs)
	return &m, nil
}

// BumpMerchant 回写履约：正向（完成）或负向（超时未履约）。
// 主动撤销不回写——它与逾期严格区分。
func (s *Store) BumpMerchant(tx *sql.Tx, userID string, completed bool) error {
	if completed {
		_, err := tx.Exec(`update merchant_profiles set deals=deals+1 where user_id=?`, userID)
		return err
	}
	_, err := tx.Exec(`update merchant_profiles set disputes=disputes+1 where user_id=?`, userID)
	return err
}

func (s *Store) Cards(ctx context.Context, ownerID string) ([]*model.Card, error) {
	rows, err := s.db.QueryContext(ctx,
		`select id,owner_id,name,kind,cycle,quota,used,per_deal_cap,allowlist,template,enabled,note
		   from authorization_cards where owner_id=? order by kind desc, name`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Card
	for rows.Next() {
		c, err := scanCard(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Card(ctx context.Context, id string) (*model.Card, error) {
	row := s.db.QueryRowContext(ctx,
		`select id,owner_id,name,kind,cycle,quota,used,per_deal_cap,allowlist,template,enabled,note
		   from authorization_cards where id=?`, id)
	return scanCard(row.Scan)
}

func scanCard(scan func(...any) error) (*model.Card, error) {
	var c model.Card
	var quota, cap_ sql.NullString
	var used string
	var enabled int
	if err := scan(&c.ID, &c.OwnerID, &c.Name, &c.Kind, &c.Cycle, &quota, &used, &cap_,
		&c.Allowlist, &c.Template, &enabled, &c.Note); err != nil {
		return nil, err
	}
	c.Used = dec(used)
	c.Enabled = enabled == 1
	if quota.Valid {
		v := dec(quota.String)
		c.Quota = &v
	}
	if cap_.Valid {
		v := dec(cap_.String)
		c.PerDealCap = &v
	}
	return &c, nil
}

// SpendCard 占用授权卡的周期额度；amount 为负即释放。
func (s *Store) SpendCard(tx *sql.Tx, cardID string, usd decimal.Decimal) error {
	if cardID == "" {
		return nil
	}
	var used string
	if err := tx.QueryRow(`select used from authorization_cards where id=?`, cardID).Scan(&used); err != nil {
		return err
	}
	next := dec(used).Add(usd)
	if next.IsNegative() {
		next = decimal.Zero
	}
	_, err := tx.Exec(`update authorization_cards set used=? where id=?`, decStr(next), cardID)
	return err
}
