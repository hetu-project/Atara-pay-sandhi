// 管理后台的跨用户读模型。
//
// 这些查询和产品侧的 Orders / Offers 不同：产品侧按 actor 过滤（「只看我的」），
// 后台是运营视角，跨所有用户看全量。所以单独一组方法，不复用带 owner 过滤的那套——
// 混用的话迟早有人把后台的无过滤查询接到用户端，把别人的单也列出来。
//
// 只读：后台的写动作（审核、将来的裁决）各有专门方法，不走这里。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// AdminCounts 是概览页顶部那排数字。
type AdminCounts struct {
	Users          int `json:"users"`
	Merchants      int `json:"merchants"`
	OffersActive   int `json:"offers_active"`
	Orders         int `json:"orders"`
	OrdersOpen     int `json:"orders_open"`     // 还没到终态的
	OrdersDisputed int `json:"orders_disputed"` // terminal=disputed，资金锁定待裁决
	Withdrawals    int `json:"withdrawals"`
	PendingReviews int `json:"pending_reviews"` // 待审的准入申请（两段合计）
	KycReview      int `json:"kyc_review"`      // 待人工复核的身份核验
}

func (s *Store) AdminCounts(ctx context.Context) (AdminCounts, error) {
	var c AdminCounts
	// 一行一个数，简单直接。量级是演示库，不值得为一次概览拼一条大 SQL。
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
	// 待审：kyc 段交了没过，或 listing 段交了没过——和 PendingMakerApps 同口径。
	if c.PendingReviews, err = q(
		`select count(*) from maker_applications
		  where (kyc_done=1 and kyc_ok=0) or (listing_done=1 and approved=0)`); err != nil {
		return c, err
	}
	// KYC 待人工复核。这张表可能还没建（老库没接身份核验），查不到当 0。
	if c.KycReview, err = q(`select count(*) from kyc_verifications where status='review'`); err != nil {
		c.KycReview = 0
	}
	return c, nil
}

// AdminOrderRow 是后台订单列表的一行。带上双方展示名——运营看 id 认不出人。
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

// AdminWithdrawalRow 是后台提现列表的一行。
//
// 注意 broadcast 态：这一版 BroadcastWithdrawal 只收一个 tx_hash 字符串、
// 不去链上核实。后台把 tx_hash 原样列出来，运营能自己去区块浏览器对，
// 但系统没有替它背书——这一点在真接链核之前必须让看的人知道。
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

// ── 后台写动作 ──

// AdminForceDelist 强制下架一条挂单。只改状态，不动链上锁仓——链上解锁认
// maker 私钥，平台签不了。所以币仍锁在托管里，需 maker 自己去取回；这里
// 只是让它不再对买家可见。只对 active 的挂单有意义。
func (s *Store) AdminForceDelist(ctx context.Context, offerID string) error {
	res, err := s.db.ExecContext(ctx,
		`update offers set status='delisted', updated_at=?
		  where id=? and status='active'`, ts(Now()), offerID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows // 不存在，或已经不是 active
	}
	return nil
}

// AdminReviewValues 是提现复核允许的取值。空串表示撤销标记。
var AdminReviewValues = map[string]bool{"": true, "suspicious": true, "held": true, "cleared": true}

// AdminSetWithdrawalReview 打/撤提现的复核标记。这是运营批注，不改资金状态——
// 真正拦截一笔链上转账平台做不到（非托管，币不在我们手里），标记只是给
// 运营和风控留个记号。
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

// AdminOfferRow 是后台挂单列表的一行。
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

// ── 用户 / 商户 ──

// AdminUserRow 是用户列表的一行。带上「是不是商户」「做市有没有过审」，
// 让运营一眼看出这是普通用户还是做市方。
type AdminUserRow struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	Address       string `json:"address"`
	Kind          string `json:"kind"`
	Role          string `json:"role"`
	LoginMethod   string `json:"login_method"` // passkey | wallet | google | twitter | email
	WalletKind    string `json:"wallet_kind"`  // atara（自建）| ext（外部钱包）
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

// AdminMerchant 是商户画像（有才带）。字段对齐 merchant_profiles。
type AdminMerchant struct {
	PeerCode      string `json:"peer_code"`
	TrustScore    int    `json:"trust_score"`
	Deals         int    `json:"deals"`
	Disputes      int    `json:"disputes"`
	FillRate      string `json:"fill_rate"`
	MedianRelease int    `json:"median_release_secs"`
	Docs          string `json:"docs"`
}

// AdminUserDetail 是单个主体的全貌：资料 + 商户画像 + 准入申请 + 其挂单/订单/提现。
// 纯读聚合，给详情页用。任一子查询失败即整体失败——半份画像比报错更容易误导。
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
	// 资料
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
	// 商户画像（可空）
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
	// 准入申请（可空）
	if app, err := s.MakerApp(ctx, userID); err == nil {
		d.Application = app
	}
	// KYC 最近一次核验（可空）。失败不阻断整个详情——身份核验模块没配或没记录
	// 时，用户详情照样要能看。
	if kyc, err := s.AdminKycForUser(ctx, userID); err == nil {
		d.Kyc = kyc
	}
	// 其挂单 / 订单（作为任一方）/ 提现
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

// ── 审计留痕 ──

// LogAudit 记一条后台操作。append-only，失败不回滚业务动作——审计缺一条
// 比让已经成功的下架/审核回滚要轻。调用方在业务动作成功后调它。
func (s *Store) LogAudit(ctx context.Context, actorID, action, targetType, targetID, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`insert into admin_audit(actor_id,action,target_type,target_id,detail,created_at)
		 values(?,?,?,?,?,?)`,
		actorID, action, targetType, targetID, detail, ts(Now()))
	return err
}

// AdminAuditRow 是审计列表的一行，带上执行人展示名。
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

// ── 用户干预 ──

// AdminSetUserBanned 封禁/解封账户。被封的账户在 auth 中间层被挡下。
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

// AdminRevokeMaker 撤销做市资格：把 approved 置 0，之后就挂不了新单
// （MakerApproved 每次查库，立刻生效）。已挂出的单不动——要下架另走强制下架。
// 只对当前 approved=1 的申请有意义。
func (s *Store) AdminRevokeMaker(ctx context.Context, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`update maker_applications set approved=0, updated_at=? where user_id=? and approved=1`,
		ts(Now()), userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows // 没有申请，或本就不是已过审
	}
	return nil
}

// IsBanned 供 auth 中间层每请求查一次。单列查询，走 users 主键，很轻。
func (s *Store) IsBanned(ctx context.Context, userID string) bool {
	var banned bool
	err := s.db.QueryRowContext(ctx,
		`select coalesce(banned,0) from users where id=?`, userID).Scan(&banned)
	return err == nil && banned
}

// ── 趋势 ──

// AdminTrendPoint 是趋势图上的一天。
type AdminTrendPoint struct {
	Date        string `json:"date"` // YYYY-MM-DD
	Users       int    `json:"users"`
	Orders      int    `json:"orders"`
	Offers      int    `json:"offers"`
	Withdrawals int    `json:"withdrawals"`
}

// AdminTrends 返回最近 days 天、按天分桶的新增计数。created_at 存的是 RFC3339
// 文本，substr 取前 10 位就是日期。缺数据的天补 0，保证 x 轴连续。
func (s *Store) AdminTrends(ctx context.Context, days int) ([]AdminTrendPoint, error) {
	if days <= 0 || days > 180 {
		days = 30
	}
	// 起点：今天往前 days-1 天的零点（按 UTC 日期算，和 created_at 同口径）。
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
	// table 名是本函数内写死的常量，不来自外部输入，拼进 SQL 安全。
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

// AdminWithdrawalTx 取一笔提现的 tx_hash（供后台链上核验用）。第二个返回值
// 表示这笔提现是否存在。
func (s *Store) AdminWithdrawalTx(ctx context.Context, id string) (string, bool) {
	var tx string
	err := s.db.QueryRowContext(ctx,
		`select coalesce(tx_hash,'') from withdrawals where id=?`, id).Scan(&tx)
	if err != nil {
		return "", false
	}
	return tx, true
}

// ── KYC（身份核验）只读读模型 ──
//
// 身份核验走 ID Analyzer 的 DocuPass。accept 自动放行、reject 拒绝，
// review 是「要人看一眼」——那一档目前没人接，后台要能看见它。
// 这里只读：identity/warnings 原样以 JSON 交给前端渲染，人工放行/驳回
// 是另一组写方法（待与后端口径对齐后再加），不在这里。

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

// AdminKycList 列出核验记录。status 非空时按状态筛（后台默认先看 review）。
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

// AdminKycDetail 是单次核验的全貌：行数据 + 证件字段 + 风险警告。
// identity/warnings 原样透传（已由 kyc 层打好码），前端自己渲染。
type AdminKycDetail struct {
	AdminKycRow
	LastEvent string          `json:"last_event"`
	Identity  json.RawMessage `json:"identity,omitempty"`
	Warnings  json.RawMessage `json:"warnings,omitempty"`
}

// AdminKycForUser 取一个用户最近一次核验的详情。没有则返回 nil, nil。
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

// AdminKycByReference 按 reference 取单次核验详情。
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
	// identity_json/warnings_json 存的就是 JSON 文本，合法就原样透传，
	// 空或非法就留空——宁可少显示，不给前端塞坏 JSON。
	if json.Valid([]byte(k.IdentityJSON)) && k.IdentityJSON != "" {
		d.Identity = json.RawMessage(k.IdentityJSON)
	}
	if json.Valid([]byte(k.WarningsJSON)) && k.WarningsJSON != "" {
		d.Warnings = json.RawMessage(k.WarningsJSON)
	}
	return d
}

// ── AI 助手（调用日志 + 对话查看）只读 ──

// LogAiCall 记一次 AI 调用。在 app 层每次调完模型后调用（成功失败都记）。
// 失败只记 log、不影响主流程——运维日志缺一条比让用户那次对话失败要轻。
// AiCallLog 是一次 AI 调用要记的全部指标。字段多，用结构体传，免得调用处
// 排一长串位置参数。
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
	CostMicros       int // 估算成本，微美元
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

// AdminAiStats 是 AI 调用的聚合概览。
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

// AdminAiConvRow 是「谁跟 AI 聊过」的一行。desk 对话就是 messages 里 peer=DeskID 的那些。
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

// AdminAiMsg 是 AI 会话里的一条。role: user | assistant。
type AdminAiMsg struct {
	Role      string `json:"role"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// AdminAiThread 读某个用户跟 AI 的整段对话。author='me' 是用户，其它是 AI。
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

// ── 后台可调设置（键值）──

// GetSetting 读一个设置值。查不到返回空串（调用方回落默认）。
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `select value from app_settings where key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSetting 写一个设置值，记下是谁改的。
func (s *Store) SetSetting(ctx context.Context, key, value, updatedBy string) error {
	_, err := s.db.ExecContext(ctx,
		`insert into app_settings(key,value,updated_by,updated_at) values(?,?,?,?)
		 on conflict(key) do update set value=excluded.value,
		   updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		key, value, updatedBy, ts(Now()))
	return err
}

// DeleteSetting 删一个设置（用于「恢复默认」）。
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `delete from app_settings where key=?`, key)
	return err
}

// ── 订单详情 ──

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

// AdminOrderDetail 拼一笔订单的全貌：本体 + 双方名字 + 事件时间线。争议案卷
// 就在事件的 payload 里（Dispute 那一步存了 kind/details/file_ref），所以带上
// 事件流就看得到争议内容，不用另开一张表。
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
