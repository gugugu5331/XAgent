package budget

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCounterOverflowFailsClosed(t *testing.T) {
	maximum, err := NewLimits(Limit{Dimension: Bytes, Value: math.MaxInt64})
	if err != nil {
		t.Fatal("create maximum limits failed")
	}
	counter, err := NewCounter(maximum, maximum)
	if err != nil {
		t.Fatal("create maximum counter failed")
	}
	if err := counter.Consume(Bytes, math.MaxInt64-1); err != nil {
		t.Fatal("initial near-maximum consumption failed")
	}

	err = counter.Consume(Bytes, 2)
	var limitErr *LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("overflow error type = %T, want *LimitError", err)
	}
	if limitErr.Dimension != Bytes || limitErr.Limit != math.MaxInt64 || limitErr.Observed != math.MaxInt64 {
		t.Fatal("overflow error did not return saturated safe numeric metadata")
	}
	if got := counter.Snapshot().Used(Bytes); got != math.MaxInt64-1 {
		t.Fatalf("overflow changed used bytes to %d", got)
	}
	if got := counter.Remaining(Bytes); got != 1 {
		t.Fatalf("remaining bytes after overflow = %d, want 1", got)
	}

	boundaryLimits, err := NewLimits(Limit{Dimension: Items, Value: 1_000})
	if err != nil {
		t.Fatal("create boundary limits failed")
	}
	boundary, err := NewCounter(boundaryLimits, boundaryLimits)
	if err != nil {
		t.Fatal("create boundary counter failed")
	}
	if err := boundary.Consume(Items, 999); err != nil {
		t.Fatal("initial boundary consumption failed")
	}

	const contenders = 64
	var succeeded atomic.Int64
	var rejected atomic.Int64
	unexpected := make(chan error, contenders)
	var wait sync.WaitGroup
	for contender := 0; contender < contenders; contender++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			err := boundary.Consume(Items, 1)
			if err == nil {
				succeeded.Add(1)
				return
			}
			var concurrentLimitErr *LimitError
			if errors.As(err, &concurrentLimitErr) {
				rejected.Add(1)
				return
			}
			unexpected <- err
		}()
	}
	wait.Wait()
	close(unexpected)
	for range unexpected {
		t.Error("a boundary contender returned a non-limit error")
	}
	if got := succeeded.Load(); got != 1 {
		t.Fatalf("successful boundary contenders = %d, want 1", got)
	}
	if got := rejected.Load(); got != contenders-1 {
		t.Fatalf("rejected boundary contenders = %d, want %d", got, contenders-1)
	}
	if got := boundary.Snapshot().Used(Items); got != 1_000 {
		t.Fatalf("boundary used items = %d, want 1000", got)
	}

	err = boundary.Consume(Items, math.MaxInt64)
	if !errors.As(err, &limitErr) || limitErr.Observed != math.MaxInt64 {
		t.Fatal("extreme observed value did not fail closed with saturated metadata")
	}
	if got := boundary.Remaining(Items); got != 0 {
		t.Fatalf("extreme failed consumption reopened %d items", got)
	}
}
