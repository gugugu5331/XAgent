package tool

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/redact"
)

func TestResultFactoryProducesFourSafeViewsOnce(t *testing.T) {
	const (
		canary     = "factory-four-view-canary-74d2"
		artifactID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(canary)
	factory, err := NewResultFactory(redactor)
	if err != nil {
		t.Fatal(err)
	}
	ref := &artifact.Ref{
		ID:        artifactID,
		Bytes:     17,
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		Available: true,
		Complete:  false,
	}
	result, err := factory.Build(ResultFactoryInput{
		CallID:           "call-four-views",
		Name:             "Read",
		State:            Completed,
		Status:           StatusError,
		Summary:          "summary " + canary,
		Preview:          "preview " + canary,
		Artifact:         ref,
		CapturedBytes:    ref.Bytes,
		Truncated:        true,
		TruncationReason: string(CaptureTruncatedHardLimit),
		Error:            &Error{Code: ErrCommandFailed, Message: "error " + canary, Recoverable: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	model := result.ModelContent()
	user := result.UserView()
	persisted := result.PersistedContent()
	meta := result.OutputMeta()
	if model.Text() == "" || user.Summary.Text() == "" || persisted.Text() == "" {
		t.Fatal("factory returned an empty safe view")
	}
	for name, value := range map[string]string{
		"model":     model.Text(),
		"user":      user.Summary.Text() + user.Preview.Text() + user.Error.Message.Text(),
		"persisted": persisted.Text(),
	} {
		if strings.Contains(value, canary) || !strings.Contains(value, "[redacted]") {
			t.Fatalf("%s view did not cross the shared redaction boundary: %q", name, value)
		}
	}
	if !json.Valid([]byte(model.Text())) || !json.Valid([]byte(persisted.Text())) {
		t.Fatal("model or persisted view is not valid JSON")
	}
	if strings.Contains(persisted.Text(), "preview") {
		t.Fatal("persisted view retained the model/user preview")
	}
	if user.Artifact == nil || meta.Artifact == nil || meta.CapturedBytes != ref.Bytes ||
		!reflect.DeepEqual(user.Artifact, meta.Artifact) {
		t.Fatalf("artifact metadata diverged across safe views: user=%#v meta=%#v", user.Artifact, meta)
	}

	// Every accessor returns an immutable value copy. Mutating one projection
	// cannot alter the Result or another projection.
	user.Artifact.ID = strings.Repeat("f", 64)
	user.Error.Message = redactor.Redact("mutated")
	meta.Artifact.Bytes++
	againUser := result.UserView()
	againMeta := result.OutputMeta()
	if againUser.Artifact.ID != artifactID || againMeta.Artifact.Bytes != ref.Bytes ||
		againUser.Error.Message.Text() == "mutated" {
		t.Fatal("safe accessor returned mutable Result-owned state")
	}
	if strings.Contains(result.Content, canary) || strings.Contains(result.Error.Message, canary) {
		t.Fatal("temporary compatibility fields retained unredacted content")
	}
}

func TestResultFactoryCapturedArtifactMatrix(t *testing.T) {
	factory, err := NewResultFactory(redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatal(err)
	}
	baseRef := artifact.Ref{
		ID:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Bytes:     9,
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		Available: true,
		Complete:  true,
	}
	base := ResultFactoryInput{
		CallID:           "matrix",
		Name:             "Bash",
		State:            Completed,
		Status:           StatusSuccess,
		Summary:          "complete",
		Preview:          "12345678",
		Artifact:         &baseRef,
		CapturedBytes:    baseRef.Bytes,
		Truncated:        true,
		TruncationReason: string(CaptureTruncatedInline),
	}

	valid := []struct {
		name  string
		input ResultFactoryInput
	}{
		{name: "inline without ref", input: ResultFactoryInput{CallID: "inline", Name: "Read", State: Completed, Status: StatusSuccess, Summary: "ok", Preview: "12345678", CapturedBytes: 8}},
		{name: "zero byte synthetic failure", input: ResultFactoryInput{CallID: "failure", Name: "Read", State: CancelledAfterStart, Status: StatusError, Summary: "failed", Error: &Error{Code: ErrCommandFailed, Message: "capture failed", Recoverable: true}}},
		{name: "complete artifact", input: base},
	}
	for _, reason := range []CaptureTruncationReason{CaptureTruncatedHardLimit, CaptureTruncatedArtifact, CaptureTruncatedWriteFailure, CaptureTruncatedCanceled} {
		input := base
		ref := baseRef
		ref.Complete = false
		input.Artifact = &ref
		input.TruncationReason = string(reason)
		valid = append(valid, struct {
			name  string
			input ResultFactoryInput
		}{name: "incomplete " + string(reason), input: input})
	}
	for _, test := range valid {
		result, buildErr := factory.Build(test.input)
		if buildErr != nil {
			t.Fatalf("valid case %q failed: %v", test.name, buildErr)
		}
		meta := result.OutputMeta()
		if meta.CapturedBytes != test.input.CapturedBytes {
			t.Fatalf("case %q captured bytes = %d, want %d", test.name, meta.CapturedBytes, test.input.CapturedBytes)
		}
		if test.input.Artifact != nil {
			user := result.UserView()
			if user.Artifact == nil || meta.Artifact == nil || !reflect.DeepEqual(user.Artifact, meta.Artifact) ||
				meta.Artifact.Bytes != meta.CapturedBytes {
				t.Fatalf("case %q artifact views diverged: user=%#v meta=%#v", test.name, user.Artifact, meta)
			}
		}
	}

	invalid := []struct {
		name   string
		mutate func(*ResultFactoryInput)
	}{
		{name: "negative captured bytes", mutate: func(input *ResultFactoryInput) { input.CapturedBytes = -1 }},
		{name: "artifact bytes mismatch", mutate: func(input *ResultFactoryInput) { input.CapturedBytes++ }},
		{name: "empty id", mutate: func(input *ResultFactoryInput) { input.Artifact.ID = "" }},
		{name: "uppercase id", mutate: func(input *ResultFactoryInput) { input.Artifact.ID = strings.Repeat("A", 64) }},
		{name: "path id", mutate: func(input *ResultFactoryInput) { input.Artifact.ID = strings.Repeat("a", 62) + "/x" }},
		{name: "zero created at", mutate: func(input *ResultFactoryInput) { input.Artifact.CreatedAt = time.Time{} }},
		{name: "unavailable", mutate: func(input *ResultFactoryInput) { input.Artifact.Available = false }},
		{name: "artifact not truncated", mutate: func(input *ResultFactoryInput) { input.Truncated = false; input.TruncationReason = "" }},
		{name: "complete wrong reason", mutate: func(input *ResultFactoryInput) { input.TruncationReason = string(CaptureTruncatedHardLimit) }},
		{name: "incomplete inline reason", mutate: func(input *ResultFactoryInput) { input.Artifact.Complete = false }},
		{name: "no ref truncated", mutate: func(input *ResultFactoryInput) { input.Artifact = nil; input.CapturedBytes = 8 }},
		{name: "no ref reason", mutate: func(input *ResultFactoryInput) {
			input.Artifact = nil
			input.CapturedBytes = 8
			input.Truncated = false
		}},
		{name: "cancelled before start", mutate: func(input *ResultFactoryInput) { input.State = CancelledBeforeStart }},
		{name: "invalid status", mutate: func(input *ResultFactoryInput) { input.Status = "unknown" }},
	}
	for _, test := range invalid {
		input := base
		ref := baseRef
		input.Artifact = &ref
		test.mutate(&input)
		if test.name == "no ref reason" {
			input.TruncationReason = string(CaptureTruncatedInline)
		}
		if _, buildErr := factory.Build(input); buildErr == nil {
			t.Fatalf("invalid case %q was accepted: %#v", test.name, input)
		}
	}
}
