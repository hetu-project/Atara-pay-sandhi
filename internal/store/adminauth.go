// Admin console identity: account + password + session token. Independent of the
// consumer `users` table (that side is wallet/social self-signup) — the admin
// console is for internal staff, provisioned logins, not Privy self-signup.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"time"

	"github.com/advaita/atara-pay/internal/domain/model"
	"golang.org/x/crypto/bcrypt"
)

type AdminAccount struct {
	ID           string
	Email        string
	PasswordHash string
	Name         string
	Role         string
	Disabled     bool
}

// AdminByEmail looks up an account by email (for login). Returns sql.ErrNoRows if none.
func (s *Store) AdminByEmail(ctx context.Context, email string) (*AdminAccount, error) {
	var a AdminAccount
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`select id,email,password_hash,name,role,disabled from admin_accounts where email=?`, email).
		Scan(&a.ID, &a.Email, &a.PasswordHash, &a.Name, &a.Role, &disabled)
	if err != nil {
		return nil, err
	}
	a.Disabled = disabled == 1
	return &a, nil
}

// CreateAdminAccount creates an admin account. The password is hashed here;
// plaintext never hits the database.
func (s *Store) CreateAdminAccount(ctx context.Context, email, password, name, role string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if role != "reviewer" && role != "admin" {
		role = "admin"
	}
	_, err = s.db.ExecContext(ctx,
		`insert into admin_accounts(id,email,password_hash,name,role,disabled,created_at)
		 values(?,?,?,?,?,0,?)`,
		NewID(), email, string(hash), name, role, ts(Now()))
	return err
}

// EnsureSeedAdmin creates the configured bootstrap admin if that login doesn't
// exist yet — this breaks the chicken-and-egg. It keys on the username: if the
// account already exists it does nothing (so it never overwrites a password you
// have changed). Changing ATARA_ADMIN_USER seeds the new login on next start.
func (s *Store) EnsureSeedAdmin(ctx context.Context, username, password, name string) (created bool, err error) {
	if _, err := s.AdminByEmail(ctx, username); err == nil {
		return false, nil // already exists
	} else if err != sql.ErrNoRows {
		return false, err
	}
	return true, s.CreateAdminAccount(ctx, username, password, name, "admin")
}

// VerifyAdminPassword checks email + password. On success returns the account.
// On failure returns sql.ErrNoRows (it does not distinguish "no such email" from
// "wrong password" — don't hand a brute-forcer extra information).
func (s *Store) VerifyAdminPassword(ctx context.Context, email, password string) (*AdminAccount, error) {
	a, err := s.AdminByEmail(ctx, email)
	if err != nil {
		// Run bcrypt even when the email doesn't exist, to flatten the timing
		// difference — don't let response latency leak whether an account exists.
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidin"), []byte(password))
		return nil, sql.ErrNoRows
	}
	if a.Disabled {
		return nil, sql.ErrNoRows
	}
	if err := bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(password)); err != nil {
		return nil, sql.ErrNoRows
	}
	return a, nil
}

const adminSessionTTL = 12 * time.Hour

// CreateAdminSession issues a session token.
func (s *Store) CreateAdminSession(ctx context.Context, adminID string) (token string, expiresAt time.Time, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	token = hex.EncodeToString(b)
	expiresAt = Now().Add(adminSessionTTL)
	_, err = s.db.ExecContext(ctx,
		`insert into admin_sessions(token,admin_id,expires_at,created_at) values(?,?,?,?)`,
		token, adminID, ts(expiresAt), ts(Now()))
	return token, expiresAt, err
}

// DeleteAdminSession revokes a token (logout).
func (s *Store) DeleteAdminSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `delete from admin_sessions where token=?`, token)
	return err
}

// AdminActorByToken resolves a valid session token into an actor (with role),
// for the auth middleware. Returns sql.ErrNoRows if expired or missing, and
// cleans up the expired session in passing.
//
// It returns a *model.User shape so every admin handler's actorID / RequireRole /
// audit works unchanged — no need for a separate actor type for admins. The
// "admin:" id prefix keeps admin ids from colliding with consumer user ids.
func (s *Store) AdminActorByToken(ctx context.Context, token string) (*model.User, error) {
	if token == "" {
		return nil, sql.ErrNoRows
	}
	var adminID string
	var expires string
	err := s.db.QueryRowContext(ctx,
		`select admin_id, expires_at from admin_sessions where token=?`, token).Scan(&adminID, &expires)
	if err != nil {
		return nil, err
	}
	if parseTS(expires).Before(Now()) {
		_, _ = s.db.ExecContext(ctx, `delete from admin_sessions where token=?`, token)
		return nil, sql.ErrNoRows
	}
	var a AdminAccount
	var disabled int
	if err := s.db.QueryRowContext(ctx,
		`select id,email,name,role,disabled from admin_accounts where id=?`, adminID).
		Scan(&a.ID, &a.Email, &a.Name, &a.Role, &disabled); err != nil {
		return nil, err
	}
	if disabled == 1 {
		return nil, sql.ErrNoRows
	}
	name := a.Name
	if name == "" {
		name = a.Email
	}
	return &model.User{ID: "admin:" + a.ID, DisplayName: name, Email: a.Email, Role: a.Role}, nil
}

// AdminAccountRow is one row of the admin-account list (no password hash).
type AdminAccountRow struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Disabled  bool   `json:"disabled"`
	CreatedAt string `json:"created_at"`
}

// AdminAccounts lists all admin accounts, newest first. Never returns the hash.
func (s *Store) AdminAccounts(ctx context.Context) ([]AdminAccountRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`select id,email,name,role,disabled,created_at from admin_accounts order by created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminAccountRow{}
	for rows.Next() {
		var r AdminAccountRow
		var disabled int
		if err := rows.Scan(&r.ID, &r.Username, &r.Name, &r.Role, &disabled, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Disabled = disabled == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetAdminPassword hashes and stores a new password for an account.
func (s *Store) SetAdminPassword(ctx context.Context, id, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`update admin_accounts set password_hash=? where id=?`, string(hash), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	// Changing a password revokes that admin's live sessions — a compromised or
	// shared old password shouldn't keep a session alive.
	_, _ = s.db.ExecContext(ctx, `delete from admin_sessions where admin_id=?`, id)
	return nil
}

// SetAdminDisabled enables/disables an account. A disabled account can't log in,
// and AdminActorByToken rejects its live sessions.
func (s *Store) SetAdminDisabled(ctx context.Context, id string, disabled bool) error {
	v := 0
	if disabled {
		v = 1
	}
	res, err := s.db.ExecContext(ctx, `update admin_accounts set disabled=? where id=?`, v, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if disabled {
		_, _ = s.db.ExecContext(ctx, `delete from admin_sessions where admin_id=?`, id)
	}
	return nil
}
