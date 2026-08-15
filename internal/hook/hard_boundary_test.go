package hook

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

func TestHookCannotWidenPermission(t *testing.T) {
	decisionType := reflect.TypeOf(ToolDecision{})
	if decisionType.NumField() != 2 {
		t.Fatalf("ToolDecision field count = %d, want closed kind/reason pair", decisionType.NumField())
	}
	for _, forbidden := range []string{"argument", "ticket", "grant", "permission", "approve", "allow"} {
		for index := 0; index < decisionType.NumField(); index++ {
			field := decisionType.Field(index)
			if field.IsExported() {
				t.Fatalf("ToolDecision exposes writable field %q", field.Name)
			}
			if strings.Contains(strings.ToLower(field.Name), forbidden) || strings.Contains(strings.ToLower(field.Type.String()), forbidden) {
				t.Fatalf("ToolDecision carries forbidden capability %q", field.Name)
			}
		}
	}

	inputType := reflect.TypeOf(ToolInput{})
	for index := 0; index < inputType.NumField(); index++ {
		field := inputType.Field(index)
		if field.IsExported() && field.Type.Kind() == reflect.Map {
			t.Fatalf("ToolInput exposes writable argument map %q", field.Name)
		}
	}
	original := map[string]any{
		"path":   "approved.txt",
		"nested": map[string]any{"mode": "read"},
		"items":  []any{"one"},
	}
	input := NewToolInput("call", "Read", original)
	view := input.Arguments()
	view["path"] = "outside.txt"
	view["nested"].(map[string]any)["mode"] = "write"
	view["items"].([]any)[0] = "changed"
	bound := input.Arguments()
	if bound["path"] != "approved.txt" || bound["nested"].(map[string]any)["mode"] != "read" || bound["items"].([]any)[0] != "one" {
		t.Fatalf("Hook mutated bound arguments through read view: %#v", bound)
	}

	for _, payload := range []string{
		`{"decision":"allow","arguments":{"path":"outside.txt"}}`,
		`{"decision":"allow","ticket":"forged"}`,
		`{"decision":"approve"}`,
	} {
		if _, err := parseDecision([]byte(payload), DefaultLimits(), redact.NewRuntimeRedactor()); err == nil {
			t.Fatalf("decision protocol accepted permission-widening payload shape")
		}
	}
	if Continue().IsDeny() || !Deny("blocked").IsDeny() || Deny("blocked").Reason() != "blocked" {
		t.Fatal("closed continue/deny decision constructors changed semantics")
	}
}

func TestHookDiagnosticsAcceptOnlySafeValues(t *testing.T) {
	safeTextType := reflect.TypeOf(redact.SafeText{})
	outputType := reflect.TypeOf(ToolOutput{})
	contentField, ok := outputType.FieldByName("Content")
	if !ok || contentField.Type != safeTextType {
		t.Fatalf("ToolOutput.Content type = %v, want redact.SafeText", contentField.Type)
	}
	errorField, ok := outputType.FieldByName("Error")
	if !ok || errorField.Type != reflect.TypeOf((*SafeError)(nil)) {
		t.Fatalf("ToolOutput.Error type = %v, want *hook.SafeError", errorField.Type)
	}
	messageField, ok := reflect.TypeOf(SafeError{}).FieldByName("Message")
	if !ok || messageField.Type != safeTextType {
		t.Fatalf("SafeError.Message type = %v, want redact.SafeText", messageField.Type)
	}

	diagnosticFunction := reflect.TypeOf(hookDiagnostic)
	if diagnosticFunction.In(5) != safeTextType {
		t.Fatalf("hookDiagnostic summary type = %v, want redact.SafeText", diagnosticFunction.In(5))
	}
	addDiagnostic, ok := reflect.TypeOf((*Engine)(nil)).MethodByName("addDiagnostic")
	if ok {
		t.Fatalf("Engine unexpectedly exports diagnostic entry %s", addDiagnostic.Name)
	}
	diagnosticRuleType := reflect.TypeOf(diagnosticRule{})
	sourceField, ok := diagnosticRuleType.FieldByName("source")
	if !ok || sourceField.Type != safeTextType {
		t.Fatalf("diagnostic source type = %v, want redact.SafeText", sourceField.Type)
	}

	const canary = "hook-boundary-canary-4ea89d"
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(canary)
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{})
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{},
		errors:  map[string]error{"raw-" + canary: errors.New("raw action output " + canary)},
		block:   map[string]chan struct{}{},
	}
	rule := commandRule(EventToolBefore, 1, "raw-"+canary, false, false, false)
	rule.Source.Path = "/private/" + canary + "/hooks.yaml"
	engine, err := NewEngine(newSnapshot([]Rule{rule}), EngineOptions{
		ProjectRoot: t.TempDir(), CommandRunner: runner, LegacyDiagnostics: collector, Redactor: runtimeRedactor,
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := engine.BeforeTool(context.Background(), ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t"}, NewToolInput("c", "Read", map[string]any{"config": canary}))
	if decision.IsDeny() {
		t.Fatalf("action failure widened permission decision: %#v", decision)
	}
	items := collector.List()
	if len(items) != 1 || items[0].Message != "hook action failed" {
		t.Fatalf("unexpected safe Hook diagnostic: %#v", items)
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), canary) || strings.Contains(string(encoded), "raw action output") {
		t.Fatal("Hook diagnostic JSON retained raw action or configuration data")
	}

	safeOutput := ToolOutput{
		Status:  ToolErrorStatus,
		Content: runtimeRedactor.Redact("preview " + canary),
		Error: &SafeError{
			Code: "hook_failed", Message: runtimeRedactor.Redact("error " + canary), Recoverable: true,
		},
	}
	if strings.Contains(safeOutput.Content.Text(), canary) || strings.Contains(safeOutput.Error.Message.Text(), canary) {
		t.Fatal("safe Hook output retained canary")
	}

	engine.AfterTool(context.Background(), ExecutionRef{}, NewToolInput("c", "Read", nil), safeOutput, time.Millisecond)
}
