package util

import "context"

type ShutdownFlag struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func NewShutdownFlag() *ShutdownFlag {
	ctx, cancel := context.WithCancel(context.Background())
	return &ShutdownFlag{ctx: ctx, cancel: cancel}
}

func (s *ShutdownFlag) IsSet() bool           { return s.ctx.Err() != nil }
func (s *ShutdownFlag) Set()                  { s.cancel() }
func (s *ShutdownFlag) Done() <-chan struct{}  { return s.ctx.Done() }
