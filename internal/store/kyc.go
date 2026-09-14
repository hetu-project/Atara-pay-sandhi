// 身份核验的读写。一次 DocuPass 会话一行——见 schema.sql 里那段说明。
package store

import (
	"context"
	"database/sql"
	"time"
)

type KycCheck struct {
	Reference     string     `json:"reference"`
	UserID        string     `json:"user_id"`
	Status        string     `json:"status"` // pending | accept | review | reject
	TransactionID string     `json:"transaction_id,omitempty"`
	DocupassID    string     `json:"docupass_id,omitempty"`
	ProfileID     string     `json:"profile_id,omitempty"`
	ReviewScore   int        `json:"review_score"`
	RejectScore   int        `json:"reject_score"`
	LastEvent     string     `json:"last_event,omitempty"`
	IdentityJSON  string     `json:"-"`
	WarningsJSON  string     `json:"-"`
	RawJSON       string     `json:"-"`
	Source        string     `json:"source,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ConcludedAt   *time.Time `json:"concluded_at,omitempty"`
}

// Concluded 说这次会话有没有走到头。pending 以外的三档都是结论。
func (k *KycCheck) Concluded() bool { return k != nil && k.Status != "" && k.Status != "pending" }

const kycCols = `reference,user_id,status,transaction_id,docupass_id,profile_id,
	review_score,reject_score,last_event,identity_json,warnings_json,raw_json,
	source,created_at,updated_at,concluded_at`

func scanKyc(scan func(...any) error) (*KycCheck, error) {
	var k KycCheck
	var created, updated string
	var concluded sql.NullString
	if err := scan(&k.Reference, &k.UserID, &k.Status, &k.TransactionID, &k.DocupassID,
		&k.ProfileID, &k.ReviewScore, &k.RejectScore, &k.LastEvent, &k.IdentityJSON,
		&k.WarningsJSON, &k.RawJSON, &k.Source, &created, &updated, &concluded); err != nil {
		return nil, err
	}
	k.CreatedAt, k.UpdatedAt = parseTS(created), parseTS(updated)
	if concluded.Valid {
		t := parseTS(concluded.String)
		k.ConcludedAt = &t
	}
	return &k, nil
}

// StartKycCheck 记下一次刚建好的会话。
func (s *Store) StartKycCheck(ctx context.Context, reference, userID string) error {
	now := ts(Now())
	_, err := s.db.ExecContext(ctx,
		`insert into kyc_verifications(reference,user_id,status,created_at,updated_at)
		 values(?,?,'pending',?,?)`, reference, userID, now, now)
	return err
}

func (s *Store) KycCheck(ctx context.Context, reference string) (*KycCheck, error) {
	return scanKyc(s.db.QueryRowContext(ctx,
		`select `+kycCols+` from kyc_verifications where reference=?`, reference).Scan)
}

// LatestKycCheck 取一个用户最近的一次核验。
//
// 界面上要回答的是「我现在到哪一步了」，那就是最后开的那一次。历史留在表里，
// 要查「他哪年通过的」可以按 user_id 翻。
func (s *Store) LatestKycCheck(ctx context.Context, userID string) (*KycCheck, error) {
	return scanKyc(s.db.QueryRowContext(ctx,
		`select `+kycCols+` from kyc_verifications
		 where user_id=? order by created_at desc limit 1`, userID).Scan)
}

// LatestPassedKycCheck 取这个用户最近一次**通过**的核验。
//
// 和上面那个分开是有意的：一个人通过之后又开了一次新会话（比如换了证件），
// 那次还在 pending 时，他的身份并没有失效。拿最近一次去判断放不放行，
// 会让一个已经验过的人在点开新会话的那一刻变回未验证。
func (s *Store) LatestPassedKycCheck(ctx context.Context, userID string) (*KycCheck, error) {
	return scanKyc(s.db.QueryRowContext(ctx,
		`select `+kycCols+` from kyc_verifications
		 where user_id=? and status='accept' order by created_at desc limit 1`, userID).Scan)
}

// KycResult 是一次结论的落库内容。
type KycResult struct {
	Status        string
	TransactionID string
	DocupassID    string
	ProfileID     string
	ReviewScore   int
	RejectScore   int
	Event         string
	IdentityJSON  string
	WarningsJSON  string
	RawJSON       string
	Source        string
}

// SaveKycResult 落一次结论。
//
// concluded_at 只在第一次落结论时写，之后不动：它记的是「这个人什么时候
// 走完的」。回调会重投（失败重试 4 次，门户里还能手动重发 48 小时），
// 每次都刷新的话这个时间会一路漂到最后一次重投，那就不是通过的时间了。
func (s *Store) SaveKycResult(ctx context.Context, reference string, r KycResult) error {
	now := ts(Now())
	concluded := ""
	if r.Status != "" && r.Status != "pending" {
		concluded = now
	}
	res, err := s.db.ExecContext(ctx,
		`update kyc_verifications set
		   status=?, transaction_id=?, docupass_id=?, profile_id=?,
		   review_score=?, reject_score=?, last_event=?,
		   identity_json=?, warnings_json=?, raw_json=?, source=?,
		   updated_at=?,
		   concluded_at=case when concluded_at is null and ?<>'' then ? else concluded_at end
		 where reference=?`,
		r.Status, r.TransactionID, r.DocupassID, r.ProfileID,
		r.ReviewScore, r.RejectScore, r.Event,
		r.IdentityJSON, r.WarningsJSON, r.RawJSON, r.Source,
		now, concluded, concluded, reference)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkKycOk 把准入申请里的身份那一段置成已通过。
//
// 为什么直接写这张表：kyc_ok 是全系统唯一的「这个人能不能下单」开关，
// 原来由人工审核台或演示用的钟来置位。身份核验接上之后，通过 DocuPass
// 就是置位它的正当理由——但挂单资格（approved）不动，那是另一段审核。
//
// 没有申请记录时要补一行：很多人是先被下单拦下来才去验身份的，
// 他从没走过做市申请，没有这一行 kyc_ok 就无处可写。
func (s *Store) MarkKycOk(ctx context.Context, userID string) error {
	now := ts(Now())
	if _, err := s.db.ExecContext(ctx,
		`insert into maker_applications(user_id,phase,kyc_done,kyc_ok,form_json,updated_at)
		 values(?, 'kyc', 1, 1, '{}', ?)
		 on conflict(user_id) do update set kyc_done=1, kyc_ok=1, reject_reason='', updated_at=excluded.updated_at`,
		userID, now); err != nil {
		return err
	}
	return nil
}
