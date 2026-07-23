package hook

import (
	"encoding/json"
	"strings"
	"testing"

	"xagent/internal/redact"
)

func TestParseDecisionProtocol(t *testing.T) {
	valid := map[string]ToolDecisionKind{" {\"decision\":\"allow\"}\n": DecisionContinue, "{\"decision\":\"deny\",\"reason\":\"stop\"}": DecisionDeny}
	for input, want := range valid {
		decision, err := parseDecision([]byte(input), DefaultLimits(), nil)
		if err != nil || decision.Kind != want {
			t.Fatalf("%q = %#v,%v", input, decision, err)
		}
	}
	invalid := []string{"", "null", "[]", "{\"decision\":\"allow\",\"reason\":\"x\"}", "{\"decision\":\"deny\"}", "{\"decision\":\"deny\",\"reason\":\" \"}", "{\"decision\":\"other\"}", "{\"decision\":\"allow\",\"decision\":\"allow\"}", "{\"decision\":\"allow\",\"extra\":\"x\"}", "{\"decision\":\"allow\"} trailing", "{\"decision\":1}"}
	for _, input := range invalid {
		if _, err := parseDecision([]byte(input), DefaultLimits(), nil); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	if _, err := parseDecision([]byte{0xff}, DefaultLimits(), nil); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestDecisionReasonSafety(t *testing.T) {
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret("canary-secret")
	decision, err := parseDecision([]byte("{\"decision\":\"deny\",\"reason\":\"\\u001b[31mcanary-secret\\u001b[0m\"}"), DefaultLimits(), runtimeRedactor)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Reason != "[redacted]" {
		t.Fatalf("reason = %q", decision.Reason)
	}
	cleaned, err := parseDecision([]byte("{\"decision\":\"deny\",\"reason\":\"\\u001b[31mkeep\\u001b[0m\\u0000\\u0001\"}"), DefaultLimits(), nil)
	if err != nil || cleaned.Reason != "keep" {
		t.Fatalf("control-character cleanup = %#v, %v", cleaned, err)
	}
	if _, err := parseDecision([]byte("{\"decision\":\"deny\",\"reason\":\"\\u001b[31m\\u001b[0m\\u0000\\u0001\"}"), DefaultLimits(), nil); err == nil {
		t.Fatal("reason empty after safety cleanup was accepted")
	}
	limit := DefaultLimits()
	reason := strings.Repeat("a", limit.DenyReasonBytes+1)
	input := "{\"decision\":\"deny\",\"reason\":\"" + reason + "\"}"
	if _, err := parseDecision([]byte(input), limit, nil); err == nil {
		t.Fatal("oversize reason accepted")
	}
	expandingRedactor := redact.NewRuntimeRedactor()
	expandingRedactor.RegisterSecret("x")
	limit.DenyReasonBytes = 8
	if _, err := parseDecision([]byte(`{"decision":"deny","reason":"x x"}`), limit, expandingRedactor); err == nil {
		t.Fatal("reason that exceeded the limit after redaction was accepted")
	}
}

func TestDecisionReasonRedactsSecretsContainingRemovedControls(t *testing.T) {
	cases := []struct {
		name   string
		secret string
	}{
		{name: "ANSI", secret: "ansi-\x1b[31m-secret"},
		{name: "carriage return", secret: "carriage\rreturn-secret"},
		{name: "C0 control", secret: "control-\x01-secret"},
		{name: "delete", secret: "delete-\x7f-secret"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			runtimeRedactor := redact.NewRuntimeRedactor()
			runtimeRedactor.RegisterSecret(item.secret)
			input, err := json.Marshal(map[string]string{"decision": "deny", "reason": item.secret})
			if err != nil {
				t.Fatal(err)
			}
			decision, err := parseDecision(input, DefaultLimits(), runtimeRedactor)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Reason != "[redacted]" {
				t.Fatalf("control-bearing secret leaked after cleanup: %q", decision.Reason)
			}
		})
	}
}

func TestDecisionReasonPreservesShortSecretBoundaries(t *testing.T) {
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret("x")
	decision, err := parseDecision([]byte(`{"decision":"deny","reason":"prefix"}`), DefaultLimits(), runtimeRedactor)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Reason != "prefix" {
		t.Fatalf("embedded short secret changed ordinary text: %q", decision.Reason)
	}
}
