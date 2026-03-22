package scheduler

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

type CheckFunc func(ctx context.Context)

type Scheduler struct {
	interval  time.Duration
	checkFn   CheckFunc
	triggerCh chan struct{}
	running   atomic.Bool
	mu        sync.Mutex
	lastRun   time.Time
	nextRun   time.Time
}

func New(interval time.Duration, checkFn CheckFunc) *Scheduler {
	return &Scheduler{
		interval:  interval,
		checkFn:   checkFn,
		triggerCh: make(chan struct{}, 1),
	}
}

func (s *Scheduler) Start(ctx context.Context) {
	// Run first check immediately
	s.runCheck(ctx)

	timer := time.NewTimer(s.interval)
	s.mu.Lock()
	s.nextRun = time.Now().Add(s.interval)
	s.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.runCheck(ctx)
			timer.Reset(s.interval)
			s.mu.Lock()
			s.nextRun = time.Now().Add(s.interval)
			s.mu.Unlock()
		case <-s.triggerCh:
			// Manual trigger: run now and reset timer
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			s.runCheck(ctx)
			timer.Reset(s.interval)
			s.mu.Lock()
			s.nextRun = time.Now().Add(s.interval)
			s.mu.Unlock()
		}
	}
}

func (s *Scheduler) runCheck(ctx context.Context) {
	if s.running.Load() {
		return
	}
	s.running.Store(true)
	defer func() {
		s.running.Store(false)
		s.mu.Lock()
		s.lastRun = time.Now()
		s.mu.Unlock()
	}()
	log.Println("Scheduled check starting...")
	s.checkFn(ctx)
	log.Println("Scheduled check completed")
}

func (s *Scheduler) Trigger() bool {
	if s.running.Load() {
		return false
	}
	select {
	case s.triggerCh <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Scheduler) IsRunning() bool {
	return s.running.Load()
}

func (s *Scheduler) LastRun() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRun
}

func (s *Scheduler) NextRun() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextRun
}
