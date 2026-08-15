package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/diagnostics"
)

func TestChatStreamCloseIsIdempotent(t *testing.T) {
	events := make(chan StreamEvent)
	close(events)
	wantErr := errors.New("final cleanup result")
	var cleanupCalls atomic.Int64
	stream, err := newChatStream(events, ChatStreamOptions{CleanupTimeout: time.Second}, func(context.Context) error {
		cleanupCalls.Add(1)
		return wantErr
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stream.Events() != events {
		t.Fatal("Events did not return the producer-owned event channel")
	}

	const callers = 16
	results := make(chan error, callers)
	var callersDone sync.WaitGroup
	for range callers {
		callersDone.Add(1)
		go func() {
			defer callersDone.Done()
			results <- stream.Close(context.Background())
		}()
	}
	callersDone.Wait()
	close(results)
	for got := range results {
		if !errors.Is(got, wantErr) {
			t.Fatalf("Close result = %v, want cached final result", got)
		}
	}
	if cleanupCalls.Load() != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls.Load())
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := stream.Close(cancelled); !errors.Is(got, wantErr) {
		t.Fatalf("Close after completion = %v, want cached result over caller cancellation", got)
	}
}

func TestCloseContinuesAfterWaiterTimeout(t *testing.T) {
	events := make(chan StreamEvent)
	close(events)
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	stream, err := newChatStream(events, ChatStreamOptions{CleanupTimeout: time.Second}, func(context.Context) error {
		close(cleanupStarted)
		<-releaseCleanup
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelWait()
	if err := stream.Close(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close = %v, want caller deadline", err)
	}
	select {
	case <-cleanupStarted:
	default:
		t.Fatal("caller timeout stopped the background cleanup")
	}
	close(releaseCleanup)
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("second Close = %v, want final cleanup result", err)
	}
}

func TestChatStreamCleanupTimeoutForcesCloseAndDiagnosesOnce(t *testing.T) {
	events := make(chan StreamEvent)
	close(events)
	sink := &providerRecordingSink{}
	releaseCleanup := make(chan struct{})
	var cleanupCalls atomic.Int64
	var forceCalls atomic.Int64
	stream, err := newChatStream(events, ChatStreamOptions{
		CleanupTimeout: 10 * time.Millisecond,
		Diagnostics:    sink,
	}, func(context.Context) error {
		cleanupCalls.Add(1)
		<-releaseCleanup
		return nil
	}, func() {
		if forceCalls.Add(1) == 1 {
			close(releaseCleanup)
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := stream.Close(context.Background()); !errors.Is(err, errChatStreamCleanupTimeout) {
		t.Fatalf("first Close = %v, want hard cleanup timeout", err)
	}
	if err := stream.Close(context.Background()); !errors.Is(err, errChatStreamCleanupTimeout) {
		t.Fatalf("second Close = %v, want cached hard cleanup timeout", err)
	}
	if cleanupCalls.Load() != 1 || forceCalls.Load() != 1 {
		t.Fatalf("cleanup/force calls = %d/%d, want 1/1", cleanupCalls.Load(), forceCalls.Load())
	}
	if sink.count() != 1 || sink.code() != "provider_stream_cleanup_timeout" {
		t.Fatalf("diagnostics = %d/%q, want one cleanup timeout", sink.count(), sink.code())
	}
}

func TestChatStreamCloseUnblocksFullEventChannel(t *testing.T) {
	events := make(chan StreamEvent, 1)
	events <- StreamEvent{Type: StreamEventTextDelta, Delta: safeText("first")}
	producerDone := make(chan struct{})
	readCancelled := make(chan struct{})
	var cancelOnce sync.Once
	stream, err := newChatStream(events, ChatStreamOptions{CleanupTimeout: time.Second}, func(ctx context.Context) error {
		cancelOnce.Do(func() { close(readCancelled) })
		select {
		case <-producerDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	sendStarted := make(chan struct{})
	sendResult := make(chan bool, 1)
	go func() {
		close(sendStarted)
		sendResult <- stream.emit(context.Background(), StreamEvent{Type: StreamEventTextDelta, Delta: safeText("second")})
		close(events)
		close(producerDone)
	}()
	<-sendStarted
	select {
	case sent := <-sendResult:
		t.Fatalf("full event channel did not block producer; send result = %v", sent)
	case <-time.After(20 * time.Millisecond):
	}

	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("Close with full event channel = %v", err)
	}
	if sent := <-sendResult; sent {
		t.Fatal("producer published an event after stream close began")
	}
	select {
	case <-readCancelled:
	default:
		t.Fatal("Close did not cancel the underlying read")
	}
	first, ok := <-stream.Events()
	if !ok || first.Delta.Text() != "first" {
		t.Fatalf("buffered event = %#v/%v, want only the first event", first, ok)
	}
	if _, ok := <-stream.Events(); ok {
		t.Fatal("producer did not close its event channel after unblocking")
	}
}

type providerRecordingSink struct {
	mu    sync.Mutex
	items []diagnostics.SanitizeInput
}

func (s *providerRecordingSink) Add(input diagnostics.SanitizeInput) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, input)
}

func (s *providerRecordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

func (s *providerRecordingSink) code() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) == 0 {
		return ""
	}
	return s.items[0].Code
}
