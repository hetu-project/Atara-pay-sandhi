package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// MakerReview 是一次准入预审的留痕。一次审一行，不覆盖。
//
// 为什么要留痕：模型哪天变笨了，要能回答「从哪天开始的」。覆盖式地存在
// maker_applications 上答不了这个问题——也答不了「同一份材料换个模型
// 判得一样吗」。同一条理由写在 kyc_checks 上面。
type MakerReview struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	Stage      string    `json:"stage"`   // kyc | listing
	Source     string    `json:"source"`  // rule | ai | human
	Verdict    string    `json:"verdict"` // pass | revise | escalate
	IssuesJSON string    `json:"issues"`
	ModelID    string    `json:"model_id,omitempty"`
	InputHash  string    `json:"input_hash,omitempty"`
	LatencyMs  int       `json:"latency_ms,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// InsertMakerReview 记一次预审。
//
// 失败不该拖垮提交本身——留痕是为了事后复盘，而申请人这一刻在等一个回复。
// 调用方记日志接着走，不把这个错回给用户。
func (s *Store) InsertMakerReview(ctx context.Context, r MakerReview) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = Now()
	}
	if r.IssuesJSON == "" {
		r.IssuesJSON = "[]"
	}
	_, err := s.db.ExecContext(ctx,
		`insert into maker_reviews
		   (id,user_id,stage,source,verdict,issues_json,model_id,input_hash,latency_ms,created_at)
		 values(?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.UserID, r.Stage, r.Source, r.Verdict, r.IssuesJSON,
		r.ModelID, r.InputHash, r.LatencyMs, ts(r.CreatedAt))
	return err
}

// MakerReviews 是某个人的预审历史，最近在前。
func (s *Store) MakerReviews(ctx context.Context, userID string, limit int) ([]MakerReview, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`select id,user_id,stage,source,verdict,issues_json,model_id,input_hash,latency_ms,created_at
		   from maker_reviews where user_id=? order by created_at desc limit ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MakerReview{}
	for rows.Next() {
		var r MakerReview
		var created string
		if err := rows.Scan(&r.ID, &r.UserID, &r.Stage, &r.Source, &r.Verdict,
			&r.IssuesJSON, &r.ModelID, &r.InputHash, &r.LatencyMs, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTS(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// HashInput 是送审那份表单的指纹。
//
// 存哈希不存原文：原文已经在 maker_applications.form_json 里了，这里要的
// 只是「这次审的和上次审的是不是同一份材料」。
func HashInput(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}
