package tool

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/redact"
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

func TestResultFactoryProducesThreeSafeViews(t *testing.T) {
	const (
		canary      = "result-runtime-canary-8f21"
		privatePath = "/private/artifacts/raw-output.txt"
		artifactID  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(canary)
	runtimeRedactor.RegisterSecret(privatePath)
	factory, err := NewResultFactory(runtimeRedactor)
	if err != nil {
		t.Fatal(err)
	}
	ref := &artifact.Ref{
		ID:        artifactID,
		Bytes:     8192,
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		Available: true,
		Complete:  false,
	}
	result, err := factory.Build(ResultFactoryInput{
		CallID:           "call-17",
		Name:             "Bash",
		State:            Completed,
		Status:           StatusError,
		Summary:          "command failed: " + canary,
		Preview:          "bounded preview " + canary + " staged at " + privatePath,
		Artifact:         ref,
		Truncated:        true,
		TruncationReason: "capture limit reached after " + canary,
		Error:            &Error{Code: ErrCommandFailed, Message: "failure: " + canary, Recoverable: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The result owns an immutable copy of artifact metadata.
	ref.ID = strings.Repeat("f", len(artifactID))
	model := result.ModelContent().Text()
	user := result.UserView()
	persisted := result.PersistedContent().Text()
	meta := result.OutputMeta()
	userText := strings.Join([]string{
		user.Summary.Text(),
		user.Preview.Text(),
		user.TruncationReason.Text(),
	}, "\n")
	for name, view := range map[string]string{
		"model":     model,
		"user":      userText,
		"persisted": persisted,
	} {
		if strings.Contains(view, canary) || strings.Contains(view, privatePath) {
			t.Fatalf("%s view leaked raw output: %q", name, view)
		}
		if !strings.Contains(view, "[redacted]") {
			t.Fatalf("%s view did not retain a visible redaction marker: %q", name, view)
		}
	}
	if user.Artifact == nil || user.Artifact.ID != artifactID || user.Artifact.Bytes != 8192 {
		t.Fatalf("user view did not expose the opaque artifact ref: %#v", user.Artifact)
	}
	if meta.Artifact == nil || meta.Artifact.ID != artifactID || !meta.Truncated {
		t.Fatalf("output metadata did not preserve the opaque artifact ref: %#v", meta)
	}
	if !strings.Contains(model, artifactID) || !strings.Contains(persisted, artifactID) {
		t.Fatal("safe model and persisted views must expose the opaque artifact ref")
	}
	if strings.Contains(persisted, "bounded preview") {
		t.Fatal("persisted content retained the model/user preview")
	}
	if strings.Contains(result.Content, canary) || strings.Contains(result.Error.Message, canary) {
		t.Fatal("legacy compatibility fields retained unredacted data")
	}

	if _, err := factory.Build(ResultFactoryInput{State: CancelledBeforeStart}); err == nil {
		t.Fatal("a call cancelled before start must not produce a Result")
	}
}
