package transport

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/diagnostics"
)

func TestTransportStateTransitions(t *testing.T) {
	t.Run("running failure and close", func(t *testing.T) {
		sink := &recordingSink{}
		var cleanupCalls atomic.Int64
		var forceCalls atomic.Int64
		machine, err := NewStateMachine(Options{CleanupTimeout: time.Second, Diagnostics: sink}, func(context.Context) error {
			cleanupCalls.Add(1)
			return nil
		}, func() {
			forceCalls.Add(1)
		})
		if err != nil {
			t.Fatalf("new state machine: %v", err)
		}
		if got := machine.State(); got != TransportStateNew {
			t.Fatalf("initial state = %s, want new", got)
		}
		if err := machine.RequireRunning(); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("new admission error = %v", err)
		}
		if err := machine.Start(context.Background(), func(context.Context) error {
			if got := machine.State(); got != TransportStateStarting {
				t.Fatalf("state inside Start = %s, want starting", got)
			}
			return nil
		}); err != nil {
			t.Fatalf("start state machine: %v", err)
		}
		if got := machine.State(); got != TransportStateRunning {
			t.Fatalf("started state = %s, want running", got)
		}
		if err := machine.RequireRunning(); err != nil {
			t.Fatalf("running admission: %v", err)
		}
		if err := machine.Start(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("second Start = %v, want invalid transition", err)
		}
		if err := machine.MarkFailed(); err != nil {
			t.Fatalf("mark failed: %v", err)
		}
		if got := machine.State(); got != TransportStateFailed {
			t.Fatalf("failed state = %s, want failed", got)
		}
		if err := machine.RequireRunning(); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("failed admission error = %v", err)
		}
		if err := machine.Close(context.Background()); err != nil {
			t.Fatalf("close failed state: %v", err)
		}
		if err := machine.Close(context.Background()); err != nil {
			t.Fatalf("repeat close: %v", err)
		}
		if got := machine.State(); got != TransportStateClosed {
			t.Fatalf("closed state = %s, want closed", got)
		}
		if cleanupCalls.Load() != 1 || forceCalls.Load() != 0 || sink.count() != 0 {
			t.Fatalf("cleanup/force/diagnostics = %d/%d/%d", cleanupCalls.Load(), forceCalls.Load(), sink.count())
		}
	})

	t.Run("start failure", func(t *testing.T) {
		wantErr := errors.New("safe start failure")
		machine := newTestStateMachine(t, func(context.Context) error { return nil })
		if err := machine.Start(context.Background(), func(context.Context) error { return wantErr }); !errors.Is(err, wantErr) {
			t.Fatalf("Start error = %v, want start failure", err)
		}
		if got := machine.State(); got != TransportStateFailed {
			t.Fatalf("start failure state = %s, want failed", got)
		}
		if err := machine.Close(context.Background()); err != nil {
			t.Fatalf("close after start failure: %v", err)
		}
	})

	t.Run("hard cleanup timeout", func(t *testing.T) {
		sink := &recordingSink{}
		release := make(chan struct{})
		var forceCalls atomic.Int64
		machine, err := NewStateMachine(Options{CleanupTimeout: 10 * time.Millisecond, Diagnostics: sink}, func(context.Context) error {
			<-release
			return nil
		}, func() {
			if forceCalls.Add(1) == 1 {
				close(release)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := machine.Start(context.Background(), func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
		first := machine.Close(context.Background())
		second := machine.Close(context.Background())
		if !errors.Is(first, ErrCleanupTimeout) || !errors.Is(second, ErrCleanupTimeout) {
			t.Fatalf("cleanup timeout results = %v / %v", first, second)
		}
		if forceCalls.Load() != 1 || sink.countCode("mcp_transport_cleanup_timeout") != 1 {
			t.Fatalf("force/timeout diagnostics = %d/%d", forceCalls.Load(), sink.countCode("mcp_transport_cleanup_timeout"))
		}
	})
}

func TestTransportCloseContinuesAfterWaiterTimeout(t *testing.T) {
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	wantFinal := errors.New("stable final cleanup result")
	var cleanupCalls atomic.Int64
	machine := newTestStateMachine(t, func(context.Context) error {
		cleanupCalls.Add(1)
		close(cleanupStarted)
		<-releaseCleanup
		return wantFinal
	})
	if err := machine.Start(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelWait()
	if err := machine.Close(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close = %v, want caller deadline", err)
	}
	select {
	case <-cleanupStarted:
	default:
		t.Fatal("caller timeout stopped independent cleanup")
	}
	if got := machine.State(); got != TransportStateClosing {
		t.Fatalf("state after caller timeout = %s, want closing", got)
	}
	if err := machine.RequireRunning(); !errors.Is(err, ErrClosing) {
		t.Fatalf("closing admission error = %v", err)
	}

	close(releaseCleanup)
	const callers = 16
	results := make(chan error, callers)
	var waiters sync.WaitGroup
	for range callers {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			results <- machine.Close(context.Background())
		}()
	}
	waiters.Wait()
	close(results)
	for err := range results {
		if !errors.Is(err, wantFinal) {
			t.Fatalf("final Close = %v, want stable cleanup result", err)
		}
	}
	if cleanupCalls.Load() != 1 || machine.State() != TransportStateClosed {
		t.Fatalf("cleanup calls/state = %d/%s", cleanupCalls.Load(), machine.State())
	}
}

func newTestStateMachine(t *testing.T, cleanup CleanupFunc) *StateMachine {
	t.Helper()
	machine, err := NewStateMachine(Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}}, cleanup, func() {})
	if err != nil {
		t.Fatalf("new test state machine: %v", err)
	}
	return machine
}

type recordingSink struct {
	mu    sync.Mutex
	items []diagnostics.SanitizeInput
}

func (sink *recordingSink) Add(input diagnostics.SanitizeInput) {
	sink.mu.Lock()
	sink.items = append(sink.items, input)
	sink.mu.Unlock()
}

func (sink *recordingSink) count() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return len(sink.items)
}

func (sink *recordingSink) countCode(code string) int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	count := 0
	for _, item := range sink.items {
		if item.Code == code {
			count++
		}
	}
	return count
}
