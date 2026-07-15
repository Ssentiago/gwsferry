package util

import (
	"context"
	"time"
)

type ShutdownFlag struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func NewShutdownFlag() *ShutdownFlag {
	ctx, cancel := context.WithCancel(context.Background())
	return &ShutdownFlag{ctx: ctx, cancel: cancel}
}

func (s *ShutdownFlag) IsSet() bool          { return s.ctx.Err() != nil }
func (s *ShutdownFlag) Set()                 { s.cancel() }
func (s *ShutdownFlag) Done() <-chan struct{} { return s.ctx.Done() }
func (s *ShutdownFlag) Context() context.Context { return s.ctx }

// SleepOrShutdown sleeps for d or returns early if shutdown is set.
// Returns true if shutdown fired.
func SleepOrShutdown(s *ShutdownFlag, d time.Duration) bool {
	if s.IsSet() {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.Done():
		return true
	case <-t.C:
		return false
	}
}
