package subagent

import (
	"testing"
	"time"
)

func TestDefaultLimitsValidateAndRejectUnsafeRelations(t *testing.T) {
	limits := DefaultLimits()
	if err := limits.Validate(); err != nil {
		t.Fatalf("default limits: %v", err)
	}
	if limits.MaxConcurrent != 4 || limits.MaxQueued != 32 || limits.AutoBackgroundAfter != 10*time.Second {
		t.Fatalf("unexpected runtime defaults: %#v", limits)
	}

	invalid := limits
	invalid.MaxRetainedTasks = invalid.MaxConcurrent + invalid.MaxQueued - 1
	if err := invalid.Validate(); err == nil {
		t.Fatal("retained task budget below active capacity was accepted")
	}
	invalid = limits
	invalid.ReadCacheMaxValueBytes = invalid.ReadCacheMaxBytes + 1
	if err := invalid.Validate(); err == nil {
		t.Fatal("read cache value budget above total was accepted")
	}
	invalid = limits
	invalid.MaxTaskDuration = -time.Millisecond
	if err := invalid.Validate(); err == nil {
		t.Fatal("negative task duration was accepted")
	}
}
