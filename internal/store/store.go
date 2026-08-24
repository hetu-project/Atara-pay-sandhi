// Package store 是持久化层。
//
// 用 SQLite（modernc，纯 Go 无 CGO）——本机没有 Docker 也没有 Postgres，
// 目标是 `go run` 直接起。分层与 SQL 结构与 Postgres 版一致，换的只是方言。
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct{ db *sql.DB }

func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// SQLite 单写者：连接池开到 1，省掉一整类 database is locked 的偶发失败。
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

// Tx 是唯一的事务入口。事务边界都在 app 层，handler 不碰它。
func (s *Store) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ── 小工具 ──

func NewID() string { return uuid.NewString() }

func Now() time.Time { return time.Now().UTC() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func dec(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return v
}

func decStr(d decimal.Decimal) string { return d.String() }

func nullStr(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}

func emptyToNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}
