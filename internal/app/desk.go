package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/advaita/atara-pay/internal/desk"
	"github.com/advaita/atara-pay/internal/domain/model"
	"github.com/advaita/atara-pay/internal/domain/order"
	"github.com/advaita/atara-pay/internal/httpx"
	"github.com/advaita/atara-pay/internal/money"
	"github.com/advaita/atara-pay/internal/store"
	"github.com/shopspring/decimal"
)

// 带进模型的历史条数。往前翻太多没有用：这条线程里还混着准入流程播报的
// 系统消息，二十条已经覆盖了正常的一问一答上下文，再多只是在烧 token。
const deskHistory = 20

// 带进快照的工单条数。跑久了的账户有几百单，全带上一是烧钱，二是把要紧的
// 那几条埋掉。二十条覆盖了「我最近这些单怎么样了」这个问法。
const deskOrders = 20

// 没配模型时的固定回话。不报错——对用户来说「这条线暂时没人」是一个状态，
// 不是一次失败。
const deskOffline = "The desk is not connected to its assistant on this server yet. " +
	"Your message is saved; everything else in the console works as usual."

// DeskConfigured 说这台机器上对话台能不能真的回话。
func (s *Service) DeskConfigured() bool { return s.Desk != nil }

// DeskReply 收下用户这句话，把模型的回答流式送出去，最后整段存库。
//
// onDelta 每收到一段文字调一次（HTTP 层据此推 SSE）。它返回 error 表示
// 客户端已经走了——那时停止向模型要字，但**已经拿到的部分照样存**：
// 用户刷新页面看到半句，好过看到自己那句话石沉大海。
//
// 返回值是存下来的那条回复。
func (s *Service) DeskReply(ctx context.Context, ownerID, body string,
	onDelta func(string) error) (*model.Message, error) {

	if strings.TrimSpace(body) == "" {
		return nil, httpx.Fail(http.StatusBadRequest, "EMPTY_MESSAGE", "body", "nothing to send")
	}

	/* 用户那句先落库再去问模型。反过来的话，模型一挂，人说的话就丢了——
	   而那句话是他自己打的，丢了比没回答更难接受。
	   只写自己这一侧：desk 的收件箱没人看，PostBoth 会白占一半的表。 */
	mine := &model.Message{Author: "me", Kind: "chat", Body: body}
	if err := s.St.Post(ctx, ownerID, store.DeskID, mine); err != nil {
		return nil, err
	}

	if s.Desk == nil {
		return s.deskSay(ctx, ownerID, deskOffline)
	}

	snap, err := s.deskSnapshot(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	history, err := s.deskHistoryMsgs(ctx, ownerID)
	if err != nil {
		return nil, err
	}

	full, err := s.Desk.Stream(ctx, desk.Build(snap, history), onDelta)
	full = strings.TrimSpace(full)
	if full == "" {
		if err != nil {
			return nil, err
		}
		/* 模型正常结束却一个字没说。存一句空消息会在界面上留一个空气泡，
		   比说实话更糟。 */
		return nil, httpx.Fail(http.StatusBadGateway, "DESK_EMPTY", "",
			"the assistant returned nothing — try asking again")
	}
	/* 走到这儿 full 非空：即使 err 非 nil（中途断了 / 客户端走了），
	   也把已经说出口的存下来，让库里和屏幕上看到的一致。 */
	return s.deskSay(ctx, ownerID, full)
}

// deskSay 把一句话记成 desk 说的。
func (s *Service) deskSay(ctx context.Context, ownerID, body string) (*model.Message, error) {
	m := &model.Message{Author: "them", Kind: "chat", Body: body}
	if err := s.St.Post(ctx, ownerID, store.DeskID, m); err != nil {
		return nil, err
	}
	return m, nil
}

// deskHistoryMsgs 取这条线程最近的对话，翻成模型认的 role。
func (s *Service) deskHistoryMsgs(ctx context.Context, ownerID string) ([]desk.Msg, error) {
	msgs, err := s.St.Thread(ctx, ownerID, store.DeskID)
	if err != nil {
		return nil, err
	}
	if len(msgs) > deskHistory {
		msgs = msgs[len(msgs)-deskHistory:]
	}
	out := make([]desk.Msg, 0, len(msgs))
	for _, m := range msgs {
		if strings.TrimSpace(m.Body) == "" {
			continue
		}
		role := "assistant"
		if m.Author == "me" {
			role = "user"
		}
		out = append(out, desk.Msg{Role: role, Content: m.Body})
	}
	return out, nil
}

// deskSnapshot 把这个账户此刻的状态收集成一份快照。
//
// 范围就是这个身份在界面上本来就看得到的东西——余额、在飞的工单、额度、
// 准入进度。对话台不扩大暴露面，它只是换一种方式念同样的数。
func (s *Service) deskSnapshot(ctx context.Context, ownerID string) (desk.Snapshot, error) {
	u, err := s.St.User(ctx, ownerID)
	if err != nil {
		return desk.Snapshot{}, err
	}
	snap := desk.Snapshot{Name: u.DisplayName, Address: u.Address, WalletKind: u.WalletKind}

	// ── 准入 ──
	if app, err := s.St.MakerApp(ctx, ownerID); err == nil && app != nil {
		snap.KycDone, snap.KycOK = app.KYCDone, app.KYCOk
		snap.ListingDone, snap.Approved = app.ListingDone, app.Approved
		/* 被拒的理由要带上：人问「为什么没过」的时候，这是唯一能回答的依据。
		   没有它模型只会说「请联系审核员」。 */
		snap.ReviewNote = app.RejectReason
	}

	// ── 余额 ── 与 /wallet 同源：链上余额 + 托管里锁着的
	total, esc := decimal.Zero, decimal.Zero
	net := s.Ch.Info(ctx).Network
	if net == "" {
		net = "—"
	}
	for _, a := range money.Cryptos() {
		bal, err := s.Ch.Balance(ctx, u.Address, a.Code)
		if err != nil {
			continue // 一个币读不到不该让整次对话失败
		}
		locked := s.EscrowedFor(ctx, u.ID, a.Code)
		if bal.IsZero() && locked.IsZero() {
			continue
		}
		total = total.Add(money.New(bal.Add(locked), a.Code).USD())
		esc = esc.Add(money.New(locked, a.Code).USD())
		snap.Balances = append(snap.Balances, desk.Balance{
			Asset: a.Code, Network: net, OnChain: bal.String(), InEscrow: locked.String(),
			USD: money.New(bal.Add(locked), a.Code).USD().Round(2).String(),
		})
	}
	snap.TotalUSD, snap.EscrowUSD = total.Round(2).String(), esc.Round(2).String()

	// ── 工单 ── 阶段按「站在我这边看」算，和 Tasks 那一版同一套口径
	orders, err := s.St.Orders(ctx, store.OrderFilter{Owner: ownerID})
	if err != nil {
		return snap, err
	}
	/* 只带最近这些条。跑久了的账户有几百单，整个塞进提示词一是烧钱，
	   二是把真正要紧的那几条埋在里面——模型读到最后已经不记得开头了。
	   Orders 是按新到旧排的，所以截前面这一段。 */
	if len(orders) > deskOrders {
		orders = orders[:deskOrders]
	}
	for _, o := range orders {
		line := desk.OrderLine{
			Ref: o.Ref, Amount: o.Amount.String(), Asset: o.Asset,
			State: string(o.State), Updated: rel(o.UpdatedAt),
			Counterparty: s.deskPeerName(ctx, o),
		}
		if o.IsTerminal() {
			line.State = "settled: " + string(o.Terminal)
		} else if p, actor, has := o.PhaseFor(ownerID); has {
			line.Phase = deskPhase[p]
			line.Actor = "waiting on them"
			if actor == order.ViewerYou {
				line.Actor = "waiting on you"
			}
		}
		snap.Orders = append(snap.Orders, line)
	}

	// ── 额度 ──
	if as, err := s.St.Allowances(ctx, ownerID); err == nil {
		for _, a := range as {
			snap.Allowances = append(snap.Allowances, fmt.Sprintf(
				"%s (%s) — up to %s %s per payment, %s %s per %s, %s already used, status %s",
				a.Spender, a.Kind, a.PerPayment, a.Asset, a.WindowCap, a.Asset, a.Cycle,
				a.Used, a.Status))
		}
	}

	// ── 自己挂的单 ──
	if offers, err := s.St.Offers(ctx, store.OfferFilter{Maker: ownerID}); err == nil {
		for _, o := range offers {
			snap.Offers = append(snap.Offers, fmt.Sprintf(
				"%s %s for %s at %s — %s of %s left, status %s",
				o.Side, o.Asset, o.Fiat, o.UnitPrice, o.RemainingQty, o.Qty, o.Status))
		}
	}
	return snap, nil
}

// deskPeerName 查对手方的名字。查不到就退回「对手方」而不是一串 id——
// 模型会把 id 原样念出来，而那个字符串对人没有意义。
func (s *Service) deskPeerName(ctx context.Context, o *order.Order) string {
	if o.CounterpartyID == "" {
		return "no counterparty yet"
	}
	if p, err := s.St.User(ctx, o.CounterpartyID); err == nil && p != nil {
		return p.DisplayName
	}
	return "the counterparty"
}

// deskPhase 和 Tasks 接口用的是同一套措辞。两边各写一份的话，
// 同一个阶段在待办列表里叫一个名字、AI 嘴里叫另一个名字。
var deskPhase = map[order.Phase]string{
	order.PhasePay:    "Send the transfer",
	order.PhaseVerify: "Verify their receipt",
	order.PhaseWait:   "Waiting on their transfer",
	order.PhaseLock:   "Locking into escrow",
	order.PhaseRel:    "Releasing to them",
}

// rel 把时间写成「多久以前」。给绝对时间戳的话，模型得自己算差值才能回答
// 「这单卡多久了」——而它算不准，还会一本正经地说错。
func rel(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}
