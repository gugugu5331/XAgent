package http

import (
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestBodyRegistryCloseRace(t *testing.T) {
	t.Run("consumer close removes body", func(t *testing.T) {
		registry := newBodyRegistry()
		body := &countedBody{}
		tracked, err := registry.register(body)
		if err != nil {
			t.Fatal(err)
		}
		if registry.activeCount() != 1 {
			t.Fatal("registered body was not active")
		}
		if err := tracked.Close(); err != nil {
			t.Fatal(err)
		}
		if registry.activeCount() != 0 || body.closes.Load() != 1 {
			t.Fatalf("consumer close left active/duplicate body: active=%d closes=%d", registry.activeCount(), body.closes.Load())
		}
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
		if body.closes.Load() != 1 {
			t.Fatal("registry re-closed a consumed body")
		}
	})

	t.Run("register and close never lose a body", func(t *testing.T) {
		const (
			rounds         = 64
			bodiesPerRound = 32
		)
		for round := range rounds {
			registry := newBodyRegistry()
			bodies := make([]*countedBody, bodiesPerRound)
			start := make(chan struct{})
			var waiters sync.WaitGroup
			for index := range bodies {
				body := &countedBody{}
				bodies[index] = body
				waiters.Add(1)
				go func(index int) {
					defer waiters.Done()
					<-start
					tracked, err := registry.register(body)
					if err == nil && index%3 == 0 {
						_ = tracked.Close()
					}
				}(index)
			}
			for range 4 {
				waiters.Add(1)
				go func() {
					defer waiters.Done()
					<-start
					_ = registry.Close()
				}()
			}
			close(start)
			waiters.Wait()
			if err := registry.Close(); err != nil {
				t.Fatalf("round %d repeated Close: %v", round, err)
			}
			if registry.activeCount() != 0 {
				t.Fatalf("round %d retained %d active bodies", round, registry.activeCount())
			}
			for index, body := range bodies {
				if got := body.closes.Load(); got != 1 {
					t.Fatalf("round %d body %d close calls = %d, want 1", round, index, got)
				}
			}
			late := &countedBody{}
			if _, err := registry.register(late); !errors.Is(err, ErrBodyRegistryClosed) {
				t.Fatalf("round %d late registration = %v", round, err)
			}
			if late.closes.Load() != 1 {
				t.Fatalf("round %d late body was not closed", round)
			}
		}
	})

	t.Run("close continues after one body error", func(t *testing.T) {
		registry := newBodyRegistry()
		failed := &countedBody{err: errors.New("raw close failure")}
		success := &countedBody{}
		if _, err := registry.register(failed); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.register(success); err != nil {
			t.Fatal(err)
		}
		if err := registry.Close(); !errors.Is(err, ErrBodyCloseFailed) || strings.Contains(err.Error(), "raw close failure") {
			t.Fatalf("registry close error = %v", err)
		}
		if failed.closes.Load() != 1 || success.closes.Load() != 1 {
			t.Fatalf("body close calls after failure = %d/%d", failed.closes.Load(), success.closes.Load())
		}
	})

	t.Run("non-comparable body implementation is supported", func(t *testing.T) {
		registry := newBodyRegistry()
		closes := &atomic.Int64{}
		body := nonComparableBody{data: []byte("body"), closes: closes}
		if _, err := registry.register(body); err != nil {
			t.Fatal(err)
		}
		if err := registry.Close(); err != nil || closes.Load() != 1 {
			t.Fatalf("non-comparable body close = %v/%d", err, closes.Load())
		}
	})
}

type countedBody struct {
	closes atomic.Int64
	err    error
}

func (*countedBody) Read([]byte) (int, error) { return 0, io.EOF }

func (body *countedBody) Close() error {
	body.closes.Add(1)
	return body.err
}

type nonComparableBody struct {
	data   []byte
	closes *atomic.Int64
}

func (nonComparableBody) Read([]byte) (int, error) { return 0, io.EOF }

func (body nonComparableBody) Close() error {
	body.closes.Add(1)
	return nil
}
