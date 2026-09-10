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

// New 按配置定扫描节奏。接了真链是每分钟一次，mock 是每秒——
// 见 config.SchedTick 那段注释。
func New(svc *app.Service) *Scheduler {
	return &Scheduler{Svc: svc, Tick: svc.Cfg.SchedTick}
}

func (s *Scheduler) Run(ctx context.Context) {
	if s.Tick <= 0 {
		s.Tick = time.Second
	}
	log.Printf("scheduler: 每 %s 扫一次到期工单", s.Tick)
	t := time.NewTicker(s.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
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
	// 准入申请到点自动放行（演示口径）。同一个循环，不另起 goroutine。
	if err := s.Svc.SweepMakerReviews(ctx, time.Now()); err != nil {
		log.Printf("scheduler: maker reviews: %v", err)
	}
	// 过期令牌不清会一直堆着。挂在同一个循环里，不另起 goroutine。
	if err := s.Svc.St.PurgeConfirmations(ctx, time.Now()); err != nil {
		log.Printf("scheduler: purge confirmations: %v", err)
	}
}
