package store

import (
	"context"
	"database/sql"
	"time"
)

func (s *Store) InsertConfirmation(ctx context.Context,
	token, userID, digest, grade string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`insert into confirmations(token,user_id,digest,grade,expires_at)
		 values(?,?,?,?,?)`,
		token, userID, digest, grade, expiresAt.UTC().Format(time.RFC3339Nano))
	return err
}

// ConsumeConfirmation 原子地作废一枚令牌，回传它绑定的摘要与分级。
// 判定与作废必须是同一条 UPDATE——读后写会让并发重放挤进那道缝。
// RowsAffected==0 即失败（不存在、已消费、过期或不属于这个用户），
// 一律返回 sql.ErrNoRows，不再细分原因：细分需要额外一次查询，
// 而查询结果本身已经不可信——那一刻令牌是否还在，取决于谁先抢到那把锁。
func (s *Store) ConsumeConfirmation(ctx context.Context,
	token, userID string, now time.Time) (digest, grade string, err error) {
	ts := now.UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx,
		`update confirmations set consumed_at=?
		  where token=? and user_id=? and consumed_at is null and expires_at>?`,
		ts, token, userID, ts)
	if err != nil {
		return "", "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", "", err
	}
	if n == 0 {
		return "", "", sql.ErrNoRows
	}
	if err := s.db.QueryRowContext(ctx,
		`select digest,grade from confirmations where token=?`, token).
		Scan(&digest, &grade); err != nil {
		return "", "", err
	}
	return digest, grade, nil
}

// PurgeConfirmations 清掉过期行。挂在 scheduler 的同一个循环里。
func (s *Store) PurgeConfirmations(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`delete from confirmations where expires_at < ?`,
		before.UTC().Format(time.RFC3339Nano))
	return err
}
