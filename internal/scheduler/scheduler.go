// Package scheduler 驱动状态机里的系统步。
//
// 人的步（确认、上传回执、撤单、异议）走显式接口；
// 系统步（对方注资、平台核验、日期到期、异议窗口静默）由这里推。
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/advaita/atara-pay/internal/app"
)

type Scheduler struct {
	Svc  *app.Service
	Tick time.Duration
}

// deskTick 是准入审核那条循环的节奏。
//
// 它跟上面那个 Tick 是两件事，所以不共用：工单要去链上确认状态，慢一点没
// 关系；准入放行只是把库里一行的一个布尔位翻过来，不碰链、不发 RPC。
// 搭在一起的后果是——链上扫描一改成每分钟，交完材料的人就要盯着「审核中」
// 干等一分钟，而那是任何人打开这个演示第一眼看到的东西。
const deskTick = time.Second

// New 按配置定扫描节奏。接了真链是每分钟一次，mock 是每秒——
// 见 config.SchedTick 那段注释。
func New(svc *app.Service) *Scheduler {
	return &Scheduler{Svc: svc, Tick: svc.Cfg.SchedTick}
}

func (s *Scheduler) Run(ctx context.Context) {
	if s.Tick <= 0 {
		s.Tick = time.Second
	}
	log.Printf("scheduler: 每 %s 扫一次到期工单（链），每 %s 扫一次准入审核（只读库）",
		s.Tick, deskTick)
	t := time.NewTicker(s.Tick)
	defer t.Stop()
	d := time.NewTicker(deskTick)
	defer d.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		case <-d.C:
			s.sweepDesk(ctx)
		}
	}
}

// sweepDesk 放行到点的准入申请。单独一条循环——它不碰链，见 deskTick。
func (s *Scheduler) sweepDesk(ctx context.Context) {
	if err := s.Svc.SweepMakerReviews(ctx, time.Now()); err != nil {
		log.Printf("scheduler: maker reviews: %v", err)
	}
}

// sweep 找出所有到期的工单，逐个推到下一站。
// 一笔失败不能挡住其它笔——记下来接着走。
func (s *Scheduler) sweep(ctx context.Context) {
	due, err := s.Svc.St.Due(ctx, time.Now())
	if err != nil {
		log.Printf("scheduler: due: %v", err)
		return
	}
	for _, o := range due {
		if err := s.Svc.Tick(ctx, o); err != nil {
			log.Printf("scheduler: order %s at %s: %v", o.Ref, o.State, err)
		}
	}
	// 过期令牌不清会一直堆着。挂在同一个循环里，不另起 goroutine。
	if err := s.Svc.St.PurgeConfirmations(ctx, time.Now()); err != nil {
		log.Printf("scheduler: purge confirmations: %v", err)
	}
}
