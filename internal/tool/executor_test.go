package tool

import (
	"reflect"
	"strings"
	"testing"
)

func TestExecutionStateAndPolicyAreIndependent(t *testing.T) {
	if _, coupled := reflect.TypeOf(ExecutionPolicy{}).FieldByName("Risk"); coupled {
		t.Fatal("ExecutionPolicy coupled scheduling to risk classification")
	}
	if (ExecutionPolicy{}).AllowsConcurrentExecution() {
		t.Fatal("a safe risk classification would not make a zero scheduling policy concurrent")
	}
	if !(ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}).AllowsConcurrentExecution() {
		t.Fatal("a dangerous risk classification would not override an explicitly concurrent-safe policy")
	}

	for _, test := range []struct {
		name   string
		policy ExecutionPolicy
		want   bool
	}{
		{name: "neither", policy: ExecutionPolicy{}},
		{name: "read only", policy: ExecutionPolicy{ReadOnly: true}},
		{name: "concurrent safe", policy: ExecutionPolicy{ConcurrentSafe: true}},
		{name: "both", policy: ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.policy.AllowsConcurrentExecution(); got != test.want {
				t.Fatalf("AllowsConcurrentExecution() = %v, want %v", got, test.want)
			}
		})
	}

	states := []ExecutionState{Prepared, Rejected, CancelledBeforeStart, Running, Completed, CancelledAfterStart}
	seen := make(map[ExecutionState]bool, len(states))
	for _, state := range states {
		if !state.Valid() || seen[state] {
			t.Fatalf("execution state is invalid or duplicated: %q", state)
		}
		seen[state] = true
	}
	if len(seen) != 6 {
		t.Fatalf("execution state count = %d, want 6", len(seen))
	}
	for _, state := range []ExecutionState{Prepared, Running, CancelledBeforeStart} {
		if state.CanProduceResult() {
			t.Fatalf("state %q may not produce a result", state)
		}
	}
	for _, state := range []ExecutionState{Rejected, Completed, CancelledAfterStart} {
		if !state.CanProduceResult() {
			t.Fatalf("state %q must be able to produce a result", state)
		}
	}

	resultType := reflect.TypeOf(Result{})
	for index := 0; index < resultType.NumField(); index++ {
		name := strings.ToLower(resultType.Field(index).Name)
		if strings.Contains(name, "stdout") || strings.Contains(name, "stderr") || strings.HasPrefix(name, "raw") {
			t.Fatalf("Result exposes raw output field %q", resultType.Field(index).Name)
		}
	}
}
