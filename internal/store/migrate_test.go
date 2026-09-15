package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

/*
Every column added after a table first shipped must be registered in addColumns.

schema.sql uses `create table if not exists`, so an existing database never
grows new fields on its own. Forgetting the registration does not fail at
startup — it fails on the first query that selects the new column, and the
error a reader sees is whatever the caller turns "no such column" into.
When this happened for real, it surfaced as "application not found", which
sends whoever is debugging it looking for the wrong thing entirely.

This test opens a database built from an OLD schema (the table without the
new columns), then opens it again through the normal path. If the column is
missing from addColumns, the second open leaves the table short and the
assertion below fails — here, not in production.
*/
func TestAddColumnsBringsAnOldDatabaseUpToDate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	ctx := context.Background()

	// An "old" database: maker_applications as it looked before the review
	// work added its four columns.
	if err := writeOldDB(path); err != nil {
		t.Fatalf("seed old db: %v", err)
	}

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.db.Close()

	for _, col := range []string{"ai_retry_at", "ai_attempts", "appeal_note", "appealed_at"} {
		if !hasColumn(t, st, "maker_applications", col) {
			t.Fatalf("maker_applications.%s missing after open — "+
				"add it to addColumns in store.go, or every existing database breaks "+
				"on the first query that selects it", col)
		}
	}

	// And the store can actually read the table afterwards. Having the column
	// is not the same as the scan lining up with it.
	if _, err := st.MakerApp(ctx, "nobody"); err == nil {
		t.Fatalf("expected ErrNoRows for a user with no application")
	}
}

func writeOldDB(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`
		create table users(id text primary key, address text, display_name text,
		  email text, kind text, wallet_kind text, login_method text, hue integer,
		  created_at text);
		create table maker_applications (
		  user_id       text primary key references users(id),
		  phase         text not null default 'kyc',
		  kyc_done      integer not null default 0,
		  kyc_ok        integer not null default 0,
		  listing_done  integer not null default 0,
		  approved      integer not null default 0,
		  form_json     text not null default '{}',
		  reject_reason text not null default '',
		  submitted_at  text,
		  reviewed_at   text,
		  reviewer_id   text references users(id),
		  auto_review_at text,
		  updated_at    text not null
		);`)
	return err
}

func hasColumn(t *testing.T, st *Store, table, col string) bool {
	t.Helper()
	rows, err := st.db.Query("select name from pragma_table_info(?)", table)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n == col {
			return true
		}
	}
	return false
}
