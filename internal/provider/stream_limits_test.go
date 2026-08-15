package provider

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"xagent/internal/budget"
	"xagent/internal/config"
)

func TestProviderStreamLimitsAreCumulativeAndPreallocateSafe(t *testing.T) {
	limits, err := newStreamLimits(config.StreamConfig{
		MaxResponseBytes:      10,
		MaxEventBytes:         6,
		MaxEvents:             2,
		MaxTextBytes:          7,
		MaxThinkingBytes:      8,
		MaxToolArgumentsBytes: 9,
	})
	if err != nil {
		t.Fatalf("new stream limits: %v", err)
	}

	t.Run("cumulative dimensions", func(t *testing.T) {
		assertCumulativeLimit(t, limits.consumeResponse, budget.ProviderMaxResponseBytes, budget.Bytes, 6, 4, 1)
		assertCumulativeLimit(t, limits.consumeText, budget.ProviderMaxTextBytes, budget.Bytes, 3, 4, 1)
		assertCumulativeLimit(t, limits.consumeThinking, budget.ProviderMaxThinkingBytes, budget.Bytes, 5, 3, 1)
		assertCumulativeLimit(t, limits.consumeToolArguments, budget.ProviderMaxToolArgumentsBytes, budget.Bytes, 4, 5, 1)

		if err := limits.consumeEventCount(); err != nil {
			t.Fatalf("consume first event: %v", err)
		}
		if err := limits.consumeEventCount(); err != nil {
			t.Fatalf("consume second event: %v", err)
		}
		assertLimitError(t, limits.consumeEventCount(), budget.ProviderMaxEvents, budget.Items, 2, 3)
	})

	t.Run("single event is independent", func(t *testing.T) {
		if err := limits.consumeEvent(6); err != nil {
			t.Fatalf("consume first maximum-size event: %v", err)
		}
		if err := limits.consumeEvent(6); err != nil {
			t.Fatalf("consume second maximum-size event independently: %v", err)
		}
		assertLimitError(t, limits.consumeEvent(7), budget.ProviderMaxEventBytes, budget.Bytes, 6, 7)
	})

	t.Run("failed reservation leaves usage unchanged", func(t *testing.T) {
		fresh, err := newStreamLimits(config.StreamConfig{MaxTextBytes: 5})
		if err != nil {
			t.Fatalf("new stream limits: %v", err)
		}
		assertLimitError(t, fresh.consumeText(6), budget.ProviderMaxTextBytes, budget.Bytes, 5, 6)
		if err := fresh.consumeText(5); err != nil {
			t.Fatalf("consume after failed reservation: %v", err)
		}
	})

	t.Run("preallocation is conservative", func(t *testing.T) {
		fresh, err := newStreamLimits(config.StreamConfig{
			MaxResponseBytes:      128 << 10,
			MaxEventBytes:         128 << 10,
			MaxTextBytes:          128 << 10,
			MaxThinkingBytes:      128 << 10,
			MaxToolArgumentsBytes: 128 << 10,
		})
		if err != nil {
			t.Fatalf("new stream limits: %v", err)
		}
		if err := fresh.consumeResponse(100 << 10); err != nil {
			t.Fatalf("consume response prefix: %v", err)
		}
		if got, want := fresh.responsePreallocation(math.MaxInt64), 28<<10; got != want {
			t.Fatalf("response preallocation = %d, want remaining budget %d", got, want)
		}
		for name, got := range map[string]int{
			"event":          fresh.eventPreallocation(math.MaxInt64),
			"text":           fresh.textPreallocation(math.MaxInt64),
			"thinking":       fresh.thinkingPreallocation(math.MaxInt64),
			"tool_arguments": fresh.toolArgumentsPreallocation(math.MaxInt64),
		} {
			if got != int(maxStreamInitialAllocation) {
				t.Fatalf("%s preallocation = %d, want conservative cap %d", name, got, maxStreamInitialAllocation)
			}
		}
		if got := safeStreamPreallocation(math.MaxInt64, math.MaxInt64); got < 0 || uint64(got) > uint64(^uint(0)>>1) {
			t.Fatalf("preallocation is not representable as int: %d", got)
		}
		if got := safeStreamPreallocation(-1, 100); got != 0 {
			t.Fatalf("negative requested preallocation = %d, want 0", got)
		}
	})

	t.Run("limit error retains no payload", func(t *testing.T) {
		secret := "provider-payload-canary"
		err := limits.consumeEvent(int64(len(secret) + 100))
		var limitErr *budget.LimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("error type = %T, want *budget.LimitError", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("limit error exposed payload")
		}
		typeOfLimitError := reflect.TypeOf(*limitErr)
		if typeOfLimitError.NumField() != 4 {
			t.Fatalf("LimitError contains %d fields, want only four safe numeric metadata fields", typeOfLimitError.NumField())
		}
	})
}

func assertCumulativeLimit(
	t *testing.T,
	consume func(int64) error,
	scope budget.Scope,
	dimension budget.Dimension,
	first int64,
	second int64,
	overflow int64,
) {
	t.Helper()
	if err := consume(first); err != nil {
		t.Fatalf("consume first amount for %s: %v", scope, err)
	}
	if err := consume(second); err != nil {
		t.Fatalf("consume second amount for %s: %v", scope, err)
	}
	assertLimitError(t, consume(overflow), scope, dimension, first+second, first+second+overflow)
}

func assertLimitError(t *testing.T, err error, scope budget.Scope, dimension budget.Dimension, limit, observed int64) {
	t.Helper()
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("error = %v (%T), want *budget.LimitError", err, err)
	}
	if limitErr.Scope != string(scope) || limitErr.Dimension != dimension || limitErr.Limit != limit || limitErr.Observed != observed {
		t.Fatalf("limit error = %+v, want scope=%s dimension=%s limit=%d observed=%d", limitErr, scope, dimension, limit, observed)
	}
}
