package orchestrator

import (
	"context"
	"sync"
)

// runTracker tracks every Agent run whose lifecycle has not finished yet.
// The idle channel is replaced for each busy generation so waiters never
// depend on polling or scheduler timing.
type runTracker struct {
	mu     sync.Mutex
	active int
	idle   chan struct{}
}

func newRunTracker() *runTracker {
	idle := make(chan struct{})
	close(idle)
	return &runTracker{idle: idle}
}

func (t *runTracker) begin() func() {
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	if t.active == 0 {
		t.idle = make(chan struct{})
	}
	t.active++
	ended := false
	t.mu.Unlock()

	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if ended {
			return
		}
		ended = true
		if t.active == 0 {
			return
		}
		t.active--
		if t.active == 0 {
			close(t.idle)
		}
	}
}

func (t *runTracker) wait(ctx context.Context) error {
	if t == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	t.mu.Lock()
	idle := t.idle
	t.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *runTracker) count() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active
}
