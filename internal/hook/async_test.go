package hook

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAsyncCapacity(t *testing.T) {
	pool := newAsyncPool(1, 1, time.Second, time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{}, 2)
	if !pool.enqueue(time.Second, func(context.Context) actionOutcome { close(started); <-release; return actionOutcome{success: true} }, func(actionOutcome) { completed <- struct{}{} }) {
		t.Fatal("first reject")
	}
	<-started
	if !pool.enqueue(time.Second, func(context.Context) actionOutcome { return actionOutcome{success: true} }, func(actionOutcome) { completed <- struct{}{} }) {
		t.Fatal("queued reject")
	}
	if pool.enqueue(time.Second, func(context.Context) actionOutcome { return actionOutcome{success: true} }, nil) {
		t.Fatal("full queue accepted")
	}
	close(release)
	<-completed
	<-completed
	if count := pool.close(); count != 0 {
		t.Fatalf("normal drain reported %d cancelled jobs", count)
	}
	if count := pool.close(); count != 0 {
		t.Fatalf("repeated normal close reported %d cancelled jobs", count)
	}
}

func TestAsyncCloseReportsOutstandingAtDrainTimeout(t *testing.T) {
	pool := newAsyncPool(1, 3, 10*time.Millisecond, time.Second)
	started := make(chan struct{})
	completed := make(chan actionOutcome, 3)
	if !pool.enqueue(time.Second, func(ctx context.Context) actionOutcome {
		close(started)
		<-ctx.Done()
		return actionOutcome{err: ctx.Err()}
	}, func(outcome actionOutcome) { completed <- outcome }) {
		t.Fatal("running job rejected")
	}
	<-started
	var queuedRan atomic.Int64
	for index := 0; index < 2; index++ {
		if !pool.enqueue(time.Second, func(context.Context) actionOutcome {
			queuedRan.Add(1)
			return actionOutcome{success: true}
		}, func(outcome actionOutcome) { completed <- outcome }) {
			t.Fatalf("queued job %d rejected", index)
		}
	}

	const closers = 8
	startClose := make(chan struct{})
	results := make(chan int, closers)
	var group sync.WaitGroup
	for index := 0; index < closers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-startClose
			results <- pool.close()
		}()
	}
	close(startClose)
	group.Wait()
	close(results)
	for count := range results {
		if count != 3 {
			t.Fatalf("close reported %d jobs, want 3", count)
		}
	}
	for index := 0; index < 3; index++ {
		select {
		case <-completed:
		case <-time.After(time.Second):
			t.Fatalf("job %d was not settled", index)
		}
	}
	if queuedRan.Load() != 0 {
		t.Fatalf("%d cancelled queued jobs ran", queuedRan.Load())
	}
}

func TestAsyncDeadlineAndOnce(t *testing.T) {
	pool := newAsyncPool(1, 2, time.Second, time.Second)
	block := make(chan struct{})
	started := make(chan struct{})
	pool.enqueue(time.Second, func(context.Context) actionOutcome { close(started); <-block; return actionOutcome{success: true} }, nil)
	<-started
	var ran atomic.Bool
	done := make(chan actionOutcome, 1)
	pool.enqueue(time.Nanosecond, func(context.Context) actionOutcome { ran.Store(true); return actionOutcome{success: true} }, func(outcome actionOutcome) { done <- outcome })
	close(block)
	outcome := <-done
	if ran.Load() || outcome.err == nil {
		t.Fatalf("expired job ran: %#v", outcome)
	}
	pool.close()
}

func TestAsyncAdmissionCloseRace(t *testing.T) {
	pool := newAsyncPool(2, 8, time.Second, time.Second)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			pool.enqueue(time.Second, func(context.Context) actionOutcome { return actionOutcome{success: true} }, nil)
		}()
	}
	close(start)
	pool.close()
	group.Wait()
	pool.close()
}
