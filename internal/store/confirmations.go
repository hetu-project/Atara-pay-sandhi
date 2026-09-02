package store

import (
	"context"
	"time"
)

func (s *Store) InsertConfirmation(ctx context.Context,
	token, userID, digest, grade string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`insert into confirmations(token,user_id,digest,grade,expires_at)
		 values(?,?,?,?,?)`,
		token, userID, digest, grade, ts(expiresAt))
	return err
}

// ConsumeConfirmation 原子地作废一枚令牌，回传它绑定的摘要与分级。
//
// 判定、作废、取值必须是同一条语句——分成「UPDATE 判定作废」+「SELECT 取值」
// 两条语句时，即便 UPDATE 已经把这一行标成功了，两条语句之间仍有一道缝：
// scheduler 的 PurgeConfirmations 在这道缝里跑到、且此刻真实时间已经越过
// 这一行的 expires_at，会把这一行整个删掉，随后的 SELECT 就会扑空，一个
// 刚刚判定成功的合法确认反而被当成失败——不是重放漏洞，但确实丢了一次
// 本该成立的确认。用 UPDATE ... RETURNING 把三件事收进一条语句，中间不
// 留缝，也就不必再靠 PurgeConfirmations 的时序去猜「这一行会不会被删」。
//
// 没有命中（不存在、已消费、过期或不属于这个用户）时，Scan 直接把
// sql.ErrNoRows 传上来，原样返回即可——不再细分是哪种没命中：细分需要
// 另一条 SELECT，而那条 SELECT 面对的又是同一类「语句之间会变」的问题。
func (s *Store) ConsumeConfirmation(ctx context.Context,
	token, userID string, now time.Time) (digest, grade string, err error) {
	t := ts(now)
	err = s.db.QueryRowContext(ctx,
		`update confirmations set consumed_at=?
		  where token=? and user_id=? and consumed_at is null and expires_at>?
		  returning digest, grade`,
		t, token, userID, t).Scan(&digest, &grade)
	if err != nil {
		return "", "", err
	}
	return digest, grade, nil
}

// PurgeConfirmations 清掉过期行。挂在 scheduler 的同一个循环里。
func (s *Store) PurgeConfirmations(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`delete from confirmations where expires_at < ?`, ts(before))
	return err
}
