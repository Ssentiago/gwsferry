package timer

import (
	"fmt"
	"sync"
	"time"
)

type Timer struct {
	mu      sync.Mutex
	start   time.Time
	running bool
}

func New() *Timer {
	return &Timer{}
}

func (t *Timer) Start() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.start = time.Now()
	t.running = true
}

func (t *Timer) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.running = false
}

func (t *Timer) Elapsed() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.running {
		return 0
	}
	return time.Since(t.start)
}

func (t *Timer) Render() string {
	elapsed := t.Elapsed()
	h := int(elapsed.Hours())
	m := int(elapsed.Minutes()) % 60
	s := int(elapsed.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}
