package diagnostics

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"xagent/internal/redact"
)

func TestBoundedSinkAggregatesAndDrops(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	canary := "diagnostic-value-6c42e1"
	redactor.RegisterSecret(canary)

	sink, err := NewBoundedSink(BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      2,
		MaxItemBytes:  128,
		MaxTotalBytes: 512,
	})
	if err != nil {
		t.Fatal("create bounded sink failed")
	}

	base := SanitizeInput{
		Code:     "provider_failed",
		Source:   "provider",
		Hint:     "retry",
		Severity: SeverityWarning,
		Err:      errors.New("first " + canary),
	}
	sink.Add(base)
	base.Err = errors.New("different raw payload " + canary)
	sink.Add(base)
	sink.Add(SanitizeInput{Code: "store_failed", Source: "conversation", Hint: "retry", Err: errors.New("safe")})
	sink.Add(SanitizeInput{Code: "third_unique", Source: "tool", Hint: "inspect", Err: errors.New("safe")})

	const concurrentDuplicates = 100
	var wait sync.WaitGroup
	for worker := 0; worker < concurrentDuplicates; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			sink.Add(base)
		}()
	}
	wait.Wait()

	snapshot := sink.Snapshot()
	items := snapshot.Items()
	if len(items) != 2 {
		t.Fatalf("snapshot item count = %d, want 2", len(items))
	}
	if items[0].Diagnostic.Code != "provider_failed" || items[0].Count != concurrentDuplicates+2 {
		t.Fatal("duplicate diagnostics were not aggregated by stable identity")
	}
	if snapshot.Dropped() != 1 {
		t.Fatalf("snapshot dropped = %d, want 1", snapshot.Dropped())
	}
	if snapshot.Bytes() <= 0 || snapshot.Bytes() > 512 {
		t.Fatalf("snapshot bytes = %d, want a positive bounded total", snapshot.Bytes())
	}
	if items[0].Diagnostic.Message.Text() == "" {
		t.Fatal("aggregated diagnostic lost its safe message")
	}
	if strings.Contains(items[0].Diagnostic.Message.Text(), canary) {
		t.Fatal("aggregated diagnostic leaked the runtime value")
	}

	items[0].Count = 1
	items[0].Diagnostic.Code = "mutated"
	immutable := sink.Snapshot().Items()
	if immutable[0].Count != concurrentDuplicates+2 || immutable[0].Diagnostic.Code != "provider_failed" {
		t.Fatal("Snapshot.Items returned mutable sink storage")
	}

	byteLimited, err := NewBoundedSink(BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      10,
		MaxItemBytes:  30,
		MaxTotalBytes: 30,
	})
	if err != nil {
		t.Fatal("create byte-limited sink failed")
	}
	byteLimited.Add(SanitizeInput{Code: "a", Err: errors.New("x")})
	byteLimited.Add(SanitizeInput{Code: "b", Err: errors.New("x")})
	if got := len(byteLimited.Snapshot().Items()); got != 1 {
		t.Fatalf("byte-limited sink retained %d items, want 1", got)
	}
	if got := byteLimited.Snapshot().Dropped(); got != 1 {
		t.Fatalf("byte-limited sink dropped = %d, want 1", got)
	}
}
