package app

import (
	"context"
	"fmt"
	"log"
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

	built := desk.BuildWith(s.deskPersona(ctx), snap, history)
	inChars := 0
	for _, m := range built {
		inChars += len(m.Content)
	}
	start := time.Now()
	full, usage, err := s.Desk.StreamUsage(ctx, built, onDelta)
	// 记一条调用日志：模型、耗时、成败、进出字数、token 用量与估算成本。
	// 运维视角用，异步写、失败只记 log——日志缺一条比让用户那次回答挂掉要轻。
	go func(entry store.AiCallLog) {
		if e := s.St.LogAiCall(context.Background(), entry); e != nil {
			log.Printf("ai call log: %v", e)
		}
	}(store.AiCallLog{
		UserID: ownerID, Model: s.Desk.Model, Ok: err == nil, Err: errStr(err),
		InputChars: inChars, OutputChars: len(full),
		LatencyMs:        int(time.Since(start).Milliseconds()),
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		CostMicros:       aiCostMicros(s.Desk.Model, usage.PromptTokens, usage.CompletionTokens),
	})
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

// aiPrice 是每百万 token 的美元单价（输入 / 输出）。这是**估算**用的近似价，
// 上线前应改成实际合约价。未知模型落到 default。
var aiPrice = map[string][2]float64{
	"default":        {0.27, 1.10},
	"deepseek-chat":  {0.27, 1.10},
	"deepseek-flash": {0.07, 0.28},
}

// aiCostMicros 按 token 用量估算成本，单位微美元（cost_usd = 返回值/1e6）。
// 没拿到 usage（上游不支持 include_usage）时 tokens 为 0，成本自然是 0。
func aiCostMicros(model string, promptTokens, completionTokens int) int {
	p, ok := aiPrice[model]
	if !ok {
		p = aiPrice["default"]
	}
	// cost_micros = tokens * (美元/百万token)，两者相乘正好落在微美元量级。
	cost := float64(promptTokens)*p[0] + float64(completionTokens)*p[1]
	return int(cost + 0.5)
}

// deskPersonaKey 是 AI 人设在 app_settings 里的键。
const deskPersonaKey = "desk_persona"

// deskPersona 取当前生效的 AI 人设：后台改过就用改的，没改过用默认。
// 读设置失败不该让对话挂掉——回落默认，模型照常能答。
func (s *Service) deskPersona(ctx context.Context) string {
	v, err := s.St.GetSetting(ctx, deskPersonaKey)
	if err != nil || strings.TrimSpace(v) == "" {
		return desk.DefaultPersona
	}
	return v
}

// DeskPrompt 是后台「提示词」页要展示的：当前生效人设、默认人设、锁死的护栏、
// 以及是不是被改过。护栏只读——后台看得到但改不了。
type DeskPrompt struct {
	Persona    string `json:"persona"`    // 当前生效（可编辑）
	Default    string `json:"default"`    // 出厂默认，用于「恢复默认」
	Guardrails string `json:"guardrails"` // 锁死的安全护栏，只读
	IsCustom   bool   `json:"is_custom"`  // 是否被后台改过
}

func (s *Service) GetDeskPrompt(ctx context.Context) (*DeskPrompt, error) {
	v, err := s.St.GetSetting(ctx, deskPersonaKey)
	if err != nil {
		return nil, err
	}
	custom := strings.TrimSpace(v) != ""
	persona := v
	if !custom {
		persona = desk.DefaultPersona
	}
	return &DeskPrompt{
		Persona: persona, Default: desk.DefaultPersona,
		Guardrails: desk.Guardrails(), IsCustom: custom,
	}, nil
}

// SetDeskPrompt 保存后台改过的人设。空内容当成「恢复默认」处理（删掉覆盖）。
func (s *Service) SetDeskPrompt(ctx context.Context, persona, editorID string) error {
	if strings.TrimSpace(persona) == "" {
		return s.St.DeleteSetting(ctx, deskPersonaKey)
	}
	return s.St.SetSetting(ctx, deskPersonaKey, persona, editorID)
}

// ResetDeskPrompt 恢复默认人设。
func (s *Service) ResetDeskPrompt(ctx context.Context) error {
	return s.St.DeleteSetting(ctx, deskPersonaKey)
}

// errStr 把 error 转成日志里存的字符串，nil 就是空。
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
		snap.ListingDone, snap.Approved = app.ListingDone, app.Approved
		/* 被拒的理由要带上：人问「为什么没过」的时候，这是唯一能回答的依据。
		   没有它模型只会说「请联系审核员」。 */
		snap.ReviewNote = app.RejectReason
	}

	/* 身份核验读 KYC 那条流程本身，不读 maker_applications 的两个布尔。
	   那两个布尔只在 accept 时才被写（MarkKycOk），所以 pending / review /
	   reject 三种状态在它们眼里都是「没交」——人刚做完全套证件和活体、正卡在
	   人工复核，问一句「我的核验怎么样了」会被告知「你还没提交」。

	   refresh=false：只读库，不去打上游。这是每问一句话都会走的路径，
	   而前端本来就在轮询那个接口，结论迟早会落库。 */
	if k, err := s.KycStatusFor(ctx, ownerID, false); err == nil && k != nil {
		snap.IDState, snap.KycOK = k.State, k.KycOk
		for _, w := range k.Warnings {
			/* 只带描述，不带 code 和置信度——那两个是给我们排查用的，
			   念给用户听只会让他以为自己该去查一个错误码。 */
			if w.Description != "" {
				snap.IDWarnings = append(snap.IDWarnings, w.Description)
			}
		}
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
