// Package store 是持久化层。
//
// 用 SQLite（modernc，纯 Go 无 CGO）——本机没有 Docker 也没有 Postgres，
// 目标是 `go run` 直接起。分层与 SQL 结构与 Postgres 版一致，换的只是方言。
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
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
	if err := addColumns(ctx, db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// addColumns 给已经存在的表补新列。
//
// schema.sql 用的是 create table if not exists——对新库够用，但老库不会因此
// 长出新字段。SQLite 又没有 add column if not exists，所以先问一遍 table_info。
// 不做这一步的话，部署到已有数据的机器上会在第一次查询时才炸，
// 而那时错误信息只是「no such column」，看不出是漏了迁移。
func addColumns(ctx context.Context, db *sql.DB) error {
	want := []struct{ table, col, decl string }{
		{"withdrawals", "to_address", "text not null default ''"},
		{"withdrawals", "to_chain", "text not null default ''"},
		{"maker_applications", "auto_review_at", "text"},
		{"orders", "trust_score", "integer not null default 0"},
		{"allowances", "network", "text not null default ''"},
		{"orders", "fee_amount", "text not null default '0'"},
		{"orders", "fee_bps", "integer not null default 0"},
		{"orders", "assessment", "text not null default ''"},
		{"contacts", "status", "text not null default 'accepted'"},
	}
	for _, w := range want {
		rows, err := db.QueryContext(ctx, "select name from pragma_table_info(?)", w.table)
		if err != nil {
			return err
		}
		has := false
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			if n == w.col {
				has = true
			}
		}
		rows.Close()
		if has {
			continue
		}
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf("alter table %s add column %s %s", w.table, w.col, w.decl)); err != nil {
			return err
		}
	}
	return nil
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

// ErrOversold：并发吃同一挂单时，可成交量的守门人。
var ErrOversold = errors.New("not enough volume left on this listing")
