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

func New(svc *app.Service) *Scheduler { return &Scheduler{Svc: svc, Tick: time.Second} }

func (s *Scheduler) Run(ctx context.Context) {
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
}
