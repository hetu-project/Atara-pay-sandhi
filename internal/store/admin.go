// Admin console cross-user read models.
//
// These queries differ from the product-side Orders / Offers: the product side filters by actor
// ("only mine"). The console is an operator view, spanning all users. Hence a separate set of methods
// that do not reuse the owner-filtered ones — mixing them risks wiring an unfiltered console query into the user side.
//
// Read-only: the console's write actions (review, dispute resolution, ...) have their own methods, not here.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// AdminCounts is the row of numbers at the top of the overview page.
type AdminCounts struct {
	Users          int `json:"users"`
	Merchants      int `json:"merchants"`
	OffersActive   int `json:"offers_active"`
	Orders         int `json:"orders"`
	OrdersOpen     int `json:"orders_open"`     // not yet terminal
	OrdersDisputed int `json:"orders_disputed"` // terminal=disputed, funds locked pending resolution
	Withdrawals    int `json:"withdrawals"`
	PendingReviews int `json:"pending_reviews"` // pending maker applications
	KycReview      int `json:"kyc_review"`      // KYC checks awaiting human review
}

func (s *Store) AdminCounts(ctx context.Context) (AdminCounts, error) {
	var c AdminCounts
	// One number per query, plain and simple. It's a demo-scale DB; not worth a single big SQL for one overview.
	q := func(sql string, args ...any) (int, error) {
		var n int
		err := s.db.QueryRowContext(ctx, sql, args...).Scan(&n)
		return n, err
	}
	var err error
	if c.Users, err = q(`select count(*) from users`); err != nil {
		return c, err
	}
	if c.Merchants, err = q(`select count(*) from merchant_profiles`); err != nil {
		return c, err
	}
	if c.OffersActive, err = q(`select count(*) from offers where status='active'`); err != nil {
		return c, err
	}
	if c.Orders, err = q(`select count(*) from orders`); err != nil {
		return c, err
	}
	if c.OrdersOpen, err = q(`select count(*) from orders where terminal is null`); err != nil {
		return c, err
	}
	if c.OrdersDisputed, err = q(`select count(*) from orders where terminal='disputed'`); err != nil {
		return c, err
	}
	if c.Withdrawals, err = q(`select count(*) from withdrawals`); err != nil {
		return c, err
	}
	// Pending: count only the listing-config stage -- identity belongs to the KYC module, not maker review. Same rule as PendingMakerApps.
	if c.PendingReviews, err = q(
		`select count(*) from maker_applications
		  where listing_done=1 and approved=0`); err != nil {
		return c, err
	}
	// KYC awaiting human review. The table may not exist yet (old DBs without KYC) -- treat a query error as 0.
	if c.KycReview, err = q(`select count(*) from kyc_verifications where status='review'`); err != nil {
		c.KycReview = 0
	}
	return c, nil
}

// AdminOrderRow is one row of the console order list. It carries both display names -- operators can't recognize ids.
type AdminOrderRow struct {
	ID           string `json:"id"`
	Ref          string `json:"ref"`
	Kind         string `json:"kind"`
	Owner        string `json:"owner"`
	Counterparty string `json:"counterparty"`
	Asset        string `json:"asset"`
	Amount       string `json:"amount"`
	State        string `json:"state"`
	Terminal     string `json:"terminal"`
	CreatedAt    string `json:"created_at"`
}

func (s *Store) AdminOrders(ctx context.Context, limit int) ([]AdminOrderRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`select o.id, o.ref, o.kind,
		        coalesce(uo.display_name,''), coalesce(uc.display_name,''),
		        o.asset_code, o.amount, o.state, coalesce(o.terminal,''), o.created_at
		   from orders o
		   left join users uo on uo.id = o.owner_id
		   left join users uc on uc.id = o.counterparty_id
		  order by o.created_at desc
		  limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminOrderRow{}
	for rows.Next() {
		var r AdminOrderRow
		if err := rows.Scan(&r.ID, &r.Ref, &r.Kind, &r.Owner, &r.Counterparty,
			&r.Asset, &r.Amount, &r.State, &r.Terminal, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AdminWithdrawalRow is one row of the console withdrawal list.
//
// Note the broadcast state: this version's BroadcastWithdrawal only takes a tx_hash string and
// does not verify it on chain. The console lists the tx_hash as-is so operators can check the explorer,
// but the system does not vouch for it -- the viewer must know this until real on-chain verification lands.
type AdminWithdrawalRow struct {
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
	ToAddress   string `json:"to_address"`
	State       string `json:"state"`
	TxHash      string `json:"tx_hash"`
	AdminReview string `json:"admin_review"` // '' | suspicious | held | cleared
	CreatedAt   string `json:"created_at"`
}

func (s *Store) AdminWithdrawals(ctx context.Context, limit int) ([]AdminWithdrawalRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`select w.id, coalesce(u.display_name,''), w.asset_code, w.amount,
		        coalesce(w.to_address,''), w.state, coalesce(w.tx_hash,''),
		        coalesce(w.admin_review,''), w.created_at
		   from withdrawals w
		   left join users u on u.id = w.owner_id
		  order by w.created_at desc
		  limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminWithdrawalRow{}
	for rows.Next() {
		var r AdminWithdrawalRow
		if err := rows.Scan(&r.ID, &r.Owner, &r.Asset, &r.Amount,
			&r.ToAddress, &r.State, &r.TxHash, &r.AdminReview, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// -- Console write actions --

// AdminForceDelist force-delists an offer. It only changes status, not the on-chain lock -- unlocking on chain
// requires the maker's key, which the platform doesn't have. So the coins stay in escrow and the maker
// must reclaim them; this just hides the offer from buyers. Only meaningful for active offers.
func (s *Store) AdminForceDelist(ctx context.Context, offerID string) error {
	res, err := s.db.ExecContext(ctx,
		`update offers set status='delisted', updated_at=?
		  where id=? and status='active'`, ts(Now()), offerID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows // does not exist, or is no longer active
	}
	return nil
}

// AdminReviewValues are the allowed withdrawal-review values. Empty string clears the flag.
var AdminReviewValues = map[string]bool{"": true, "suspicious": true, "held": true, "cleared": true}

// AdminSetWithdrawalReview sets/clears a withdrawal review flag. It's an operator annotation, not a fund state --
// the platform cannot actually stop an on-chain transfer (non-custodial, the coins aren't ours); the flag is just
// a marker for operations and risk.
func (s *Store) AdminSetWithdrawalReview(ctx context.Context, id, flag string) error {
	if !AdminReviewValues[flag] {
		return sql.ErrNoRows
	}
	res, err := s.db.ExecContext(ctx,
		`update withdrawals set admin_review=?, updated_at=? where id=?`, flag, ts(Now()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AdminOfferRow is one row of the console offer list.
type AdminOfferRow struct {
	ID        string `json:"id"`
	Maker     string `json:"maker"`
	Side      string `json:"side"`
	Asset     string `json:"asset"`
	Fiat      string `json:"fiat"`
	UnitPrice string `json:"unit_price"`
	Remaining string `json:"remaining_qty"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

func (s *Store) AdminOffers(ctx context.Context, limit int) ([]AdminOfferRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`select o.id, coalesce(u.display_name,''), o.side, o.asset_code, o.fiat_code,
		        o.unit_price, o.remaining_qty, o.status, o.created_at
		   from offers o
		   left join users u on u.id = o.maker_id
		  order by o.created_at desc
		  limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminOfferRow{}
	for rows.Next() {
		var r AdminOfferRow
		if err := rows.Scan(&r.ID, &r.Maker, &r.Side, &r.Asset, &r.Fiat,
			&r.UnitPrice, &r.Remaining, &r.Status, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// -- Users / merchants --

// AdminUserRow is one row of the user list. It carries "is a merchant" and "maker approved" so operators
// can tell an ordinary user from a market maker at a glance.
type AdminUserRow struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	Address       string `json:"address"`
	Kind          string `json:"kind"`
	Role          string `json:"role"`
	LoginMethod   string `json:"login_method"` // passkey | wallet | google | twitter | email
	WalletKind    string `json:"wallet_kind"`  // atara (self-custody) | ext (external wallet)
	IsMerchant    bool   `json:"is_merchant"`
	MakerApproved bool   `json:"maker_approved"`
	Banned        bool   `json:"banned"`
	CreatedAt     string `json:"created_at"`
}

func (s *Store) AdminUsers(ctx context.Context, limit int) ([]AdminUserRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`select u.id, u.display_name, u.address, u.kind, u.role,
		        coalesce(u.login_method,''), coalesce(u.wallet_kind,''),
		        (m.user_id is not null) as is_merchant,
		        coalesce(a.approved,0) as maker_approved,
		        coalesce(u.banned,0) as banned,
		        u.created_at
		   from users u
		   left join merchant_profiles m on m.user_id = u.id
		   left join maker_applications a on a.user_id = u.id
		  order by u.created_at desc
		  limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminUserRow{}
	for rows.Next() {
		var r AdminUserRow
		if err := rows.Scan(&r.ID, &r.DisplayName, &r.Address, &r.Kind, &r.Role,
			&r.LoginMethod, &r.WalletKind,
			&r.IsMerchant, &r.MakerApproved, &r.Banned, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AdminMerchant is the merchant profile (present only if any). Fields mirror merchant_profiles.
type AdminMerchant struct {
	PeerCode      string `json:"peer_code"`
	TrustScore    int    `json:"trust_score"`
	Deals         int    `json:"deals"`
	Disputes      int    `json:"disputes"`
	FillRate      string `json:"fill_rate"`
	MedianRelease int    `json:"median_release_secs"`
	Docs          string `json:"docs"`
}

// AdminUserDetail is one subject in full: profile + merchant profile + maker application + their offers/orders/withdrawals.
// A read-only aggregate for the detail page. Any sub-query failure fails the whole -- half a profile misleads more than an error.
type AdminUserDetail struct {
	User        AdminUserRow         `json:"user"`
	Email       string               `json:"email"`
	Merchant    *AdminMerchant       `json:"merchant,omitempty"`
	Application *MakerApp            `json:"application,omitempty"`
	Kyc         *AdminKycDetail      `json:"kyc,omitempty"`
	Offers      []AdminOfferRow      `json:"offers"`
	Orders      []AdminOrderRow      `json:"orders"`
	Withdrawals []AdminWithdrawalRow `json:"withdrawals"`
}

func (s *Store) AdminUserDetail(ctx context.Context, userID string) (*AdminUserDetail, error) {
	var d AdminUserDetail
	// Profile
	err := s.db.QueryRowContext(ctx,
		`select u.id, u.display_name, u.address, u.kind, u.role, u.email,
		        coalesce(u.login_method,''), coalesce(u.wallet_kind,''),
		        (m.user_id is not null), coalesce(a.approved,0), coalesce(u.banned,0), u.created_at
		   from users u
		   left join merchant_profiles m on m.user_id = u.id
		   left join maker_applications a on a.user_id = u.id
		  where u.id = ?`, userID).Scan(
		&d.User.ID, &d.User.DisplayName, &d.User.Address, &d.User.Kind, &d.User.Role,
		&d.Email, &d.User.LoginMethod, &d.User.WalletKind,
		&d.User.IsMerchant, &d.User.MakerApproved, &d.User.Banned, &d.User.CreatedAt)
	if err != nil {
		return nil, err
	}
	// Merchant profile (optional)
	var m AdminMerchant
	err = s.db.QueryRowContext(ctx,
		`select peer_code, trust_score, deals, disputes, fill_rate, median_release_secs, docs
		   from merchant_profiles where user_id = ?`, userID).Scan(
		&m.PeerCode, &m.TrustScore, &m.Deals, &m.Disputes, &m.FillRate, &m.MedianRelease, &m.Docs)
	if err == nil {
		d.Merchant = &m
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	// Maker application (optional)
	if app, err := s.MakerApp(ctx, userID); err == nil {
		d.Application = app
	}
	// Latest KYC check (optional). A failure must not break the whole detail -- the user detail must render
	// even when the KYC module isn't configured or has no record.
	if kyc, err := s.AdminKycForUser(ctx, userID); err == nil {
		d.Kyc = kyc
	}
	// Their offers / orders (as either party) / withdrawals
	if d.Offers, err = s.adminOffersBy(ctx, userID); err != nil {
		return nil, err
	}
	if d.Orders, err = s.adminOrdersBy(ctx, userID); err != nil {
		return nil, err
	}
	if d.Withdrawals, err = s.adminWithdrawalsBy(ctx, userID); err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) adminOffersBy(ctx context.Context, userID string) ([]AdminOfferRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`select o.id, coalesce(u.display_name,''), o.side, o.asset_code, o.fiat_code,
		        o.unit_price, o.remaining_qty, o.status, o.created_at
		   from offers o left join users u on u.id = o.maker_id
		  where o.maker_id = ? order by o.created_at desc limit 200`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminOfferRow{}
	for rows.Next() {
		var r AdminOfferRow
		if err := rows.Scan(&r.ID, &r.Maker, &r.Side, &r.Asset, &r.Fiat,
			&r.UnitPrice, &r.Remaining, &r.Status, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) adminOrdersBy(ctx context.Context, userID string) ([]AdminOrderRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`select o.id, o.ref, o.kind,
		        coalesce(uo.display_name,''), coalesce(uc.display_name,''),
		        o.asset_code, o.amount, o.state, coalesce(o.terminal,''), o.created_at
		   from orders o
		   left join users uo on uo.id = o.owner_id
		   left join users uc on uc.id = o.counterparty_id
		  where o.owner_id = ? or o.counterparty_id = ?
		  order by o.created_at desc limit 200`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminOrderRow{}
	for rows.Next() {
		var r AdminOrderRow
		if err := rows.Scan(&r.ID, &r.Ref, &r.Kind, &r.Owner, &r.Counterparty,
			&r.Asset, &r.Amount, &r.State, &r.Terminal, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) adminWithdrawalsBy(ctx context.Context, userID string) ([]AdminWithdrawalRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`select w.id, coalesce(u.display_name,''), w.asset_code, w.amount,
		        coalesce(w.to_address,''), w.state, coalesce(w.tx_hash,''), w.created_at
		   from withdrawals w left join users u on u.id = w.owner_id
		  where w.owner_id = ? order by w.created_at desc limit 200`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminWithdrawalRow{}
	for rows.Next() {
		var r AdminWithdrawalRow
		if err := rows.Scan(&r.ID, &r.Owner, &r.Asset, &r.Amount,
			&r.ToAddress, &r.State, &r.TxHash, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// -- Audit trail --

// LogAudit records one console action. Append-only; a failure does not roll back the business action --
// a missing audit row is lighter than rolling back a delist/review that already succeeded. Called after the action succeeds.
func (s *Store) LogAudit(ctx context.Context, actorID, action, targetType, targetID, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`insert into admin_audit(actor_id,action,target_type,target_id,detail,created_at)
		 values(?,?,?,?,?,?)`,
		actorID, action, targetType, targetID, detail, ts(Now()))
	return err
}

// AdminAuditRow is one row of the audit list, with the actor's display name.
type AdminAuditRow struct {
	ID         int64  `json:"id"`
	Actor      string `json:"actor"`
	ActorID    string `json:"actor_id"`
	Action     string `json:"action"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Detail     string `json:"detail"`
	CreatedAt  string `json:"created_at"`
}

func (s *Store) AdminAudit(ctx context.Context, limit int) ([]AdminAuditRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 300
	}
	rows, err := s.db.QueryContext(ctx,
		`select a.id, coalesce(u.display_name, a.actor_id), a.actor_id,
		        a.action, a.target_type, a.target_id, a.detail, a.created_at
		   from admin_audit a
		   left join users u on u.id = a.actor_id
		  order by a.id desc
		  limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminAuditRow{}
	for rows.Next() {
		var r AdminAuditRow
		if err := rows.Scan(&r.ID, &r.Actor, &r.ActorID, &r.Action,
			&r.TargetType, &r.TargetID, &r.Detail, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// -- User intervention --

// AdminSetUserBanned bans/unbans an account. Banned accounts are blocked in the auth middleware.
func (s *Store) AdminSetUserBanned(ctx context.Context, userID string, banned bool) error {
	v := 0
	if banned {
		v = 1
	}
	res, err := s.db.ExecContext(ctx, `update users set banned=? where id=?`, v, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AdminRevokeMaker revokes maker approval: sets approved to 0, so no new offers can be posted
// (MakerApproved queries the DB each time, so it takes effect immediately). Existing offers are untouched -- force-delist those separately.
// Only meaningful for an application currently at approved=1.
func (s *Store) AdminRevokeMaker(ctx context.Context, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`update maker_applications set approved=0, updated_at=? where user_id=? and approved=1`,
		ts(Now()), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows // no application, or it wasn't approved to begin with
	}
	return nil
}

// IsBanned is queried once per request by the auth middleware. A single-column lookup on the users primary key -- very cheap.
func (s *Store) IsBanned(ctx context.Context, userID string) bool {
	var banned bool
	err := s.db.QueryRowContext(ctx,
		`select coalesce(banned,0) from users where id=?`, userID).Scan(&banned)
	return err == nil && banned
}

// -- Trends --

// AdminTrendPoint is one day on the trend chart.
type AdminTrendPoint struct {
	Date        string `json:"date"` // YYYY-MM-DD
	Users       int    `json:"users"`
	Orders      int    `json:"orders"`
	Offers      int    `json:"offers"`
	Withdrawals int    `json:"withdrawals"`
}

// AdminTrends returns per-day new-item counts for the last `days` days. created_at is stored as RFC3339
// text, so substr of the first 10 chars is the date. Empty days are filled with 0 to keep the x-axis continuous.
func (s *Store) AdminTrends(ctx context.Context, days int) ([]AdminTrendPoint, error) {
	if days <= 0 || days > 180 {
		days = 30
	}
	// Start: midnight days-1 days back (UTC date, same basis as created_at).
	start := Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")

	daily := func(table string) (map[string]int, error) {
		rows, err := s.db.QueryContext(ctx,
			`select substr(created_at,1,10) d, count(*) n from `+table+
				` where substr(created_at,1,10) >= ? group by d`, start)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		m := map[string]int{}
		for rows.Next() {
			var d string
			var n int
			if err := rows.Scan(&d, &n); err != nil {
				return nil, err
			}
			m[d] = n
		}
		return m, rows.Err()
	}
	// The table name is a constant hardcoded in this function, not external input, so interpolating it into SQL is safe.
	users, err := daily("users")
	if err != nil {
		return nil, err
	}
	orders, err := daily("orders")
	if err != nil {
		return nil, err
	}
	offers, err := daily("offers")
	if err != nil {
		return nil, err
	}
	withdrawals, err := daily("withdrawals")
	if err != nil {
		return nil, err
	}
	out := make([]AdminTrendPoint, 0, days)
	base := Now().AddDate(0, 0, -(days - 1))
	for i := 0; i < days; i++ {
		d := base.AddDate(0, 0, i).Format("2006-01-02")
		out = append(out, AdminTrendPoint{
			Date: d, Users: users[d], Orders: orders[d], Offers: offers[d], Withdrawals: withdrawals[d],
		})
	}
	return out, nil
}

// AdminWithdrawalTx fetches a withdrawal's tx_hash (for on-chain verification). The second return value
// says whether the withdrawal exists.
func (s *Store) AdminWithdrawalTx(ctx context.Context, id string) (string, bool) {
	var tx string
	err := s.db.QueryRowContext(ctx,
		`select coalesce(tx_hash,'') from withdrawals where id=?`, id).Scan(&tx)
	if err != nil {
		return "", false
	}
	return tx, true
}

// -- KYC (identity verification) read-only models --
//
// Identity verification runs on ID Analyzer's DocuPass. accept auto-clears, reject denies,
// review means "needs a human to look" -- no one handles that today, and the console must be able to see it.
// Read-only here: identity/warnings are passed to the frontend as JSON as-is; manual approve/reject
// is a separate set of write methods (to be added once aligned with the backend), not here.

type AdminKycRow struct {
	Reference   string `json:"reference"`
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"` // pending | accept | review | reject
	ReviewScore int    `json:"review_score"`
	RejectScore int    `json:"reject_score"`
	Source      string `json:"source"`
	CreatedAt   string `json:"created_at"`
	ConcludedAt string `json:"concluded_at"`
}

// AdminKycList lists checks. When status is non-empty it filters by status (the console defaults to review first).
func (s *Store) AdminKycList(ctx context.Context, status string, limit int) ([]AdminKycRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `select k.reference, k.user_id, coalesce(u.display_name,''), k.status,
	             k.review_score, k.reject_score, coalesce(k.source,''),
	             k.created_at, coalesce(k.concluded_at,'')
	        from kyc_verifications k
	        left join users u on u.id = k.user_id`
	args := []any{}
	if status != "" {
		q += ` where k.status = ?`
		args = append(args, status)
	}
	q += ` order by k.created_at desc limit ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminKycRow{}
	for rows.Next() {
		var r AdminKycRow
		if err := rows.Scan(&r.Reference, &r.UserID, &r.DisplayName, &r.Status,
			&r.ReviewScore, &r.RejectScore, &r.Source, &r.CreatedAt, &r.ConcludedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AdminKycDetail is one check in full: row data + document fields + risk warnings.
// identity/warnings pass through as-is (already masked by the kyc layer); the frontend renders them.
type AdminKycDetail struct {
	AdminKycRow
	LastEvent string          `json:"last_event"`
	Identity  json.RawMessage `json:"identity,omitempty"`
	Warnings  json.RawMessage `json:"warnings,omitempty"`
}

// AdminKycForUser fetches a user's latest check detail. Returns nil, nil if none.
func (s *Store) AdminKycForUser(ctx context.Context, userID string) (*AdminKycDetail, error) {
	k, err := s.LatestKycCheck(ctx, userID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return kycDetailFromCheck(k), nil
}

// AdminKycByReference fetches one check detail by reference.
func (s *Store) AdminKycByReference(ctx context.Context, reference string) (*AdminKycDetail, error) {
	k, err := s.KycCheck(ctx, reference)
	if err != nil {
		return nil, err
	}
	return kycDetailFromCheck(k), nil
}

func kycDetailFromCheck(k *KycCheck) *AdminKycDetail {
	d := &AdminKycDetail{
		AdminKycRow: AdminKycRow{
			Reference: k.Reference, UserID: k.UserID, Status: k.Status,
			ReviewScore: k.ReviewScore, RejectScore: k.RejectScore, Source: k.Source,
			CreatedAt: ts(k.CreatedAt),
		},
		LastEvent: k.LastEvent,
	}
	if k.ConcludedAt != nil {
		d.ConcludedAt = ts(*k.ConcludedAt)
	}
	// identity_json/warnings_json are stored as JSON text; if valid, pass through as-is,
	// otherwise leave empty -- better to show less than to feed the frontend broken JSON.
	if json.Valid([]byte(k.IdentityJSON)) && k.IdentityJSON != "" {
		d.Identity = json.RawMessage(k.IdentityJSON)
	}
	if json.Valid([]byte(k.WarningsJSON)) && k.WarningsJSON != "" {
		d.Warnings = json.RawMessage(k.WarningsJSON)
	}
	return d
}

// -- AI assistant (call log + conversation viewer) read-only --

// LogAiCall records one AI call. Called in the app layer after each model call (success or failure).
// A failure only logs and does not affect the main flow -- a missing ops log row is lighter than failing the user's chat.
// AiCallLog is everything to record for one AI call. Many fields, so passed as a struct to avoid a long
// list of positional args at the call site.
type AiCallLog struct {
	UserID           string
	Model            string
	Ok               bool
	Err              string
	InputChars       int
	OutputChars      int
	LatencyMs        int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostMicros       int // estimated cost, in micro-USD
}

func (s *Store) LogAiCall(ctx context.Context, c AiCallLog) error {
	okv := 1
	if !c.Ok {
		okv = 0
	}
	_, err := s.db.ExecContext(ctx,
		`insert into ai_calls(user_id,model,ok,err,input_chars,output_chars,latency_ms,
		    prompt_tokens,completion_tokens,total_tokens,cost_micros,created_at)
		 values(?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.UserID, c.Model, okv, c.Err, c.InputChars, c.OutputChars, c.LatencyMs,
		c.PromptTokens, c.CompletionTokens, c.TotalTokens, c.CostMicros, ts(Now()))
	return err
}

// AdminAiStats is the aggregate overview of AI calls.
type AdminAiStats struct {
	Total       int     `json:"total"`
	Ok          int     `json:"ok"`
	Failed      int     `json:"failed"`
	Today       int     `json:"today"`
	AvgLatency  int     `json:"avg_latency_ms"`
	SuccessPct  float64 `json:"success_pct"`
	TotalTokens int     `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
}

func (s *Store) AdminAiStats(ctx context.Context) (AdminAiStats, error) {
	var st AdminAiStats
	var costMicros int
	today := Now().Format("2006-01-02")
	err := s.db.QueryRowContext(ctx,
		`select count(*),
		        coalesce(sum(ok),0),
		        coalesce(sum(case when substr(created_at,1,10)=? then 1 else 0 end),0),
		        coalesce(avg(latency_ms),0),
		        coalesce(sum(total_tokens),0),
		        coalesce(sum(cost_micros),0)
		   from ai_calls`, today).Scan(&st.Total, &st.Ok, &st.Today, &st.AvgLatency,
		&st.TotalTokens, &costMicros)
	if err != nil {
		return st, err
	}
	st.Failed = st.Total - st.Ok
	st.CostUSD = float64(costMicros) / 1e6
	if st.Total > 0 {
		st.SuccessPct = float64(st.Ok) * 100 / float64(st.Total)
	}
	return st, nil
}

type AdminAiCallRow struct {
	ID               int64   `json:"id"`
	UserID           string  `json:"user_id"`
	User             string  `json:"user"`
	Model            string  `json:"model"`
	Ok               bool    `json:"ok"`
	Err              string  `json:"err"`
	InputChars       int     `json:"input_chars"`
	OutputChars      int     `json:"output_chars"`
	LatencyMs        int     `json:"latency_ms"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	CreatedAt        string  `json:"created_at"`
}

func (s *Store) AdminAiCalls(ctx context.Context, limit int) ([]AdminAiCallRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 300
	}
	rows, err := s.db.QueryContext(ctx,
		`select a.id, a.user_id, coalesce(u.display_name, a.user_id), a.model,
		        a.ok, a.err, a.input_chars, a.output_chars, a.latency_ms,
		        a.prompt_tokens, a.completion_tokens, a.total_tokens, a.cost_micros, a.created_at
		   from ai_calls a
		   left join users u on u.id = a.user_id
		  order by a.id desc limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminAiCallRow{}
	for rows.Next() {
		var r AdminAiCallRow
		var okv, costMicros int
		if err := rows.Scan(&r.ID, &r.UserID, &r.User, &r.Model, &okv, &r.Err,
			&r.InputChars, &r.OutputChars, &r.LatencyMs,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &costMicros, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.Ok = okv == 1
		r.CostUSD = float64(costMicros) / 1e6
		out = append(out, r)
	}
	return out, rows.Err()
}

// AdminAiConvRow is one row of "who has chatted with the AI". Desk conversations are the messages with peer=DeskID.
type AdminAiConvRow struct {
	UserID   string `json:"user_id"`
	User     string `json:"user"`
	Messages int    `json:"messages"`
	LastAt   string `json:"last_at"`
}

func (s *Store) AdminAiConversations(ctx context.Context, deskID string, limit int) ([]AdminAiConvRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`select m.owner_id, coalesce(u.display_name, m.owner_id),
		        count(*) n, max(m.created_at) last_at
		   from messages m
		   left join users u on u.id = m.owner_id
		  where m.peer_id = ?
		  group by m.owner_id
		  order by last_at desc limit ?`, deskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminAiConvRow{}
	for rows.Next() {
		var r AdminAiConvRow
		if err := rows.Scan(&r.UserID, &r.User, &r.Messages, &r.LastAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AdminAiMsg is one message in an AI conversation. role: user | assistant.
type AdminAiMsg struct {
	Role      string `json:"role"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// AdminAiThread reads a user's full conversation with the AI. author='me' is the user, otherwise the AI.
func (s *Store) AdminAiThread(ctx context.Context, deskID, userID string) ([]AdminAiMsg, error) {
	rows, err := s.db.QueryContext(ctx,
		`select author, body, created_at from messages
		  where owner_id=? and peer_id=? and kind='chat'
		  order by created_at asc limit 1000`, userID, deskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminAiMsg{}
	for rows.Next() {
		var author, body, created string
		if err := rows.Scan(&author, &body, &created); err != nil {
			return nil, err
		}
		role := "assistant"
		if author == "me" {
			role = "user"
		}
		out = append(out, AdminAiMsg{Role: role, Body: body, CreatedAt: created})
	}
	return out, rows.Err()
}

// -- Console-adjustable settings (key/value) --

// GetSetting reads a setting value. Returns empty string if absent (the caller falls back to a default).
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `select value from app_settings where key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSetting writes a setting value, recording who changed it.
func (s *Store) SetSetting(ctx context.Context, key, value, updatedBy string) error {
	_, err := s.db.ExecContext(ctx,
		`insert into app_settings(key,value,updated_by,updated_at) values(?,?,?,?)
		 on conflict(key) do update set value=excluded.value,
		   updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		key, value, updatedBy, ts(Now()))
	return err
}

// DeleteSetting removes a setting (used by "reset to default").
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `delete from app_settings where key=?`, key)
	return err
}

// -- Order detail --

type AdminOrderEvent struct {
	Seq       int               `json:"seq"`
	From      string            `json:"from"`
	To        string            `json:"to"`
	Actor     string            `json:"actor"`
	Reason    string            `json:"reason"`
	Payload   map[string]string `json:"payload,omitempty"`
	CreatedAt string            `json:"created_at"`
}

type AdminOrderDetail struct {
	ID             string            `json:"id"`
	Ref            string            `json:"ref"`
	Kind           string            `json:"kind"`
	State          string            `json:"state"`
	Terminal       string            `json:"terminal"`
	Owner          string            `json:"owner"`
	OwnerID        string            `json:"owner_id"`
	Counterparty   string            `json:"counterparty"`
	CounterpartyID string            `json:"counterparty_id"`
	Asset          string            `json:"asset"`
	Amount         string            `json:"amount"`
	Note           string            `json:"note"`
	TrustScore     int               `json:"trust_score"`
	FeeAmount      string            `json:"fee_amount"`
	FeeBps         int               `json:"fee_bps"`
	FundingVia     string            `json:"funding_via"`
	EscrowTx       string            `json:"escrow_tx"`
	EscrowAddr     string            `json:"escrow_addr"`
	EscrowNetwork  string            `json:"escrow_network"`
	OTC            *AdminOrderOTC    `json:"otc,omitempty"`
	Cond           *AdminOrderCond   `json:"cond,omitempty"`
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
	Events         []AdminOrderEvent `json:"events"`
}

type AdminOrderOTC struct {
	Side       string `json:"side"`
	UnitPrice  string `json:"unit_price"`
	Fiat       string `json:"fiat"`
	FiatAmount string `json:"fiat_amount"`
	Network    string `json:"network"`
}

type AdminOrderCond struct {
	Text         string `json:"text"`
	FallbackDays int    `json:"fallback_days"`
}

// AdminOrderDetail assembles one order in full: the order + both party names + event timeline. The dispute case
// lives in the event payload (the Dispute step stored kind/details/file_ref), so including the event
// stream shows the dispute content -- no separate table needed.
func (s *Store) AdminOrderDetail(ctx context.Context, id string) (*AdminOrderDetail, error) {
	o, err := s.Order(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &AdminOrderDetail{
		ID: o.ID, Ref: o.Ref, Kind: string(o.Kind), State: string(o.State),
		Terminal: string(o.Terminal), OwnerID: o.OwnerID, CounterpartyID: o.CounterpartyID,
		Asset: o.Asset, Amount: o.Amount.String(), Note: o.Note,
		TrustScore: o.TrustScore, FeeAmount: o.FeeAmount.String(), FeeBps: o.FeeBps,
		FundingVia: o.FundingVia, EscrowTx: o.EscrowTx, EscrowAddr: o.EscrowAddr,
		EscrowNetwork: o.EscrowNetwork,
		CreatedAt:     ts(o.CreatedAt), UpdatedAt: ts(o.UpdatedAt),
		Events: []AdminOrderEvent{},
	}
	if u, err := s.User(ctx, o.OwnerID); err == nil {
		d.Owner = u.DisplayName
	}
	if o.CounterpartyID != "" {
		if u, err := s.User(ctx, o.CounterpartyID); err == nil {
			d.Counterparty = u.DisplayName
		}
	}
	if o.OTC != nil {
		d.OTC = &AdminOrderOTC{
			Side: o.OTC.Side, UnitPrice: o.OTC.UnitPrice.String(), Fiat: o.OTC.FiatCode,
			FiatAmount: o.OTC.FiatAmount.String(), Network: o.OTC.Network,
		}
	}
	if o.Cond != nil {
		d.Cond = &AdminOrderCond{Text: o.Cond.Text, FallbackDays: o.Cond.FallbackDays}
	}
	events, err := s.Events(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		d.Events = append(d.Events, AdminOrderEvent{
			Seq: e.Seq, From: e.From, To: e.To, Actor: string(e.Actor),
			Reason: e.Reason, Payload: e.Payload, CreatedAt: ts(e.CreatedAt),
		})
	}
	return d, nil
}
