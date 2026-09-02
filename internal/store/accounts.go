package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/shopspring/decimal"
)

const userCols = `id,address,display_name,email,kind,wallet_kind,login_method,hue,avatar_url,role,created_at`

func (s *Store) User(ctx context.Context, id string) (*model.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `select `+userCols+` from users where id=?`, id).Scan)
}

// UserByAddress 是登录的入口：地址就是账户。
func (s *Store) UserByAddress(ctx context.Context, addr string) (*model.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `select `+userCols+` from users where address=?`, addr).Scan)
}

// UserByHandle 兼容 X-Atara-User：既认地址，也认展示名（demo 里方便切身份）。
func (s *Store) UserByHandle(ctx context.Context, h string) (*model.User, error) {
	if u, err := s.UserByAddress(ctx, h); err == nil {
		return u, nil
	}
	return scanUser(s.db.QueryRowContext(ctx,
		`select `+userCols+` from users where lower(display_name)=lower(?) limit 1`, h).Scan)
}

func scanUser(scan func(...any) error) (*model.User, error) {
	var u model.User
	var created string
	if err := scan(&u.ID, &u.Address, &u.DisplayName, &u.Email, &u.Kind,
		&u.WalletKind, &u.LoginMethod, &u.Hue, &u.AvatarURL, &u.Role, &created); err != nil {
		return nil, err
	}
	u.CreatedAt = parseTS(created)
	return &u, nil
}

func (s *Store) InsertUser(tx *sql.Tx, u *model.User) error {
	_, err := tx.Exec(`insert into users(`+userCols+`) values(?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Address, u.DisplayName, u.Email, u.Kind, u.WalletKind, u.LoginMethod,
		u.Hue, u.AvatarURL, u.Role, ts(u.CreatedAt))
	return err
}

func (s *Store) SetWalletKind(ctx context.Context, userID, kind string) error {
	_, err := s.db.ExecContext(ctx, `update users set wallet_kind=? where id=?`, kind, userID)
	return err
}

// ── 联系人 ──

func (s *Store) Contacts(ctx context.Context, ownerID string) ([]*model.Contact, error) {
	rows, err := s.db.QueryContext(ctx,
		`select u.id,u.address,u.display_name,u.kind,c.label,c.nickname
		   from contacts c join users u on u.id=c.contact_id
		  where c.owner_id=? order by u.display_name`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Contact
	for rows.Next() {
		var c model.Contact
		if err := rows.Scan(&c.ContactID, &c.Address, &c.Name, &c.Kind, &c.Label, &c.Nickname); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// ResolveContact 收一个字段：名字或地址。
// 地址是精确匹配，名字是精确匹配——都不做模糊搜索，那是开放撞库面。
func (s *Store) ResolveContact(ctx context.Context, q string) (*model.User, error) {
	q = strings.TrimSpace(q)
	if u, err := s.UserByAddress(ctx, q); err == nil {
		return u, nil
	}
	return scanUser(s.db.QueryRowContext(ctx,
		`select `+userCols+` from users where lower(display_name)=lower(?) limit 1`, q).Scan)
}

func (s *Store) AddContact(ctx context.Context, ownerID, contactID, label, nickname string) error {
	_, err := s.db.ExecContext(ctx,
		`insert into contacts(owner_id,contact_id,label,nickname,created_at) values(?,?,?,?,?)
		 on conflict(owner_id,contact_id) do update set label=excluded.label, nickname=excluded.nickname`,
		ownerID, contactID, label, nickname, ts(Now()))
	return err
}

// ── 商户画像 ──

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

// ── 额度 ──

const allowCols = `id,owner_id,spender,kind,asset,per_payment,window_cap,used,cycle,
	expires_at,recipients,template,wallet_kind,chain_tx,status,note`

func (s *Store) Allowances(ctx context.Context, ownerID string) ([]*model.Allowance, error) {
	rows, err := s.db.QueryContext(ctx,
		`select `+allowCols+` from allowances where owner_id=? order by kind desc, spender`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Allowance
	for rows.Next() {
		a, err := scanAllowance(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Allowance(ctx context.Context, id string) (*model.Allowance, error) {
	return scanAllowance(s.db.QueryRowContext(ctx, `select `+allowCols+` from allowances where id=?`, id).Scan)
}

func scanAllowance(scan func(...any) error) (*model.Allowance, error) {
	var a model.Allowance
	var per, cap_, used string
	var exp sql.NullString
	if err := scan(&a.ID, &a.OwnerID, &a.Spender, &a.Kind, &a.Asset, &per, &cap_, &used, &a.Cycle,
		&exp, &a.Recipients, &a.Template, &a.WalletKind, &a.ChainTx, &a.Status, &a.Note); err != nil {
		return nil, err
	}
	a.PerPayment, a.WindowCap, a.Used = dec(per), dec(cap_), dec(used)
	if exp.Valid && exp.String != "" {
		t := parseTS(exp.String)
		a.ExpiresAt = &t
	}
	return &a, nil
}

func (s *Store) SaveAllowance(ctx context.Context, a *model.Allowance) error {
	var exp any
	if a.ExpiresAt != nil {
		exp = ts(*a.ExpiresAt)
	}
	_, err := s.db.ExecContext(ctx,
		`insert into allowances(`+allowCols+`) values(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 on conflict(id) do update set spender=excluded.spender, per_payment=excluded.per_payment,
		   window_cap=excluded.window_cap, cycle=excluded.cycle, expires_at=excluded.expires_at,
		   recipients=excluded.recipients, wallet_kind=excluded.wallet_kind,
		   chain_tx=excluded.chain_tx, status=excluded.status`,
		a.ID, a.OwnerID, a.Spender, a.Kind, a.Asset, decStr(a.PerPayment), decStr(a.WindowCap),
		decStr(a.Used), a.Cycle, exp, a.Recipients, a.Template, a.WalletKind, a.ChainTx, a.Status, a.Note)
	return err
}

// SpendAllowance 占用窗口额度；amount 为负即释放。
func (s *Store) SpendAllowance(tx *sql.Tx, id string, usd decimal.Decimal) error {
	if id == "" {
		return nil
	}
	var used string
	if err := tx.QueryRow(`select used from allowances where id=?`, id).Scan(&used); err != nil {
		return err
	}
	next := dec(used).Add(usd)
	if next.IsNegative() {
		next = decimal.Zero
	}
	_, err := tx.Exec(`update allowances set used=? where id=?`, decStr(next), id)
	return err
}

var _ = time.Now
