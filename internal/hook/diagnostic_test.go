package hook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

func TestHookDiagnostic(t *testing.T) {
	codes := []string{DiagnosticConditionFailed, DiagnosticTemplateFailed, DiagnosticCommandFailed, DiagnosticHTTPFailed, DiagnosticDecisionInvalid, DiagnosticActionTimeout, DiagnosticAsyncQueueFull, DiagnosticShutdownCancelled, DiagnosticSubAgentNotImplemented, DiagnosticPromptScopeUnavailable, DiagnosticLimitExceeded, DiagnosticActionPanic}
	seen := map[string]bool{}
	rule := Rule{Source: Source{Path: "/safe/hooks.yaml", Ordinal: 2, EffectiveOrdinal: 3}, action: compiledAction{typeName: ActionCommand}}
	for _, code := range codes {
		if code == "" || seen[code] {
			t.Fatalf("bad code %q", code)
		}
		seen[code] = true
		item := hookDiagnostic(code, rule, EventToolBefore, "run", 1500*time.Millisecond, "safe category", DefaultLimits())
		if item.Code != code || item.Source != "/safe/hooks.yaml" || item.Path != "hooks[1]" || item.Attributes["effective_rule_ordinal"] != "3" || item.Attributes["duration_ms"] != "1500" {
			t.Fatalf("diagnostic = %#v", item)
		}
	}
}

func TestHookDiagnosticNoLeak(t *testing.T) {
	canary := "diagnostic-canary-secret"
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(canary)
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: runtimeRedactor.Text})
	rule := Rule{Source: Source{Path: "/tmp/" + canary + "/hooks.yaml", Ordinal: 1, EffectiveOrdinal: 1}, action: compiledAction{typeName: ActionHTTP}}
	collector.Add(hookDiagnostic(DiagnosticHTTPFailed, rule, EventToolBefore, "response", time.Second, "safe failure", DefaultLimits()))
	for _, item := range collector.List() {
		if strings.Contains(item.Text(), canary) {
			t.Fatalf("canary leaked: %s", item.Text())
		}
		for key, value := range item.Attributes {
			if strings.Contains(key+value, canary) {
				t.Fatal("attribute leak")
			}
		}
	}
}

func TestEngineSnapshot(t *testing.T) {
	rule := commandRule(EventSystemStart, 1, "original", false, false, false)
	rule.action.env = map[string]string{"A": "original"}
	snapshot := newSnapshot([]Rule{rule})
	copy := snapshot.Rules()
	copy[0].action.command = "changed"
	copy[0].action.env["A"] = "changed"
	again := snapshot.Rules()
	if again[0].action.command != "original" || again[0].action.env["A"] != "original" {
		t.Fatal("snapshot exposed mutable state")
	}
}

func TestDiagnosticSummaryLimit(t *testing.T) {
	limits := DefaultLimits()
	const canary = "diagnostic-tail-secret-canary"
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(canary)
	cases := []struct {
		name    string
		summary string
	}{
		{name: "limit", summary: strings.Repeat("a", limits.DiagnosticBytes)},
		{name: "limit plus one", summary: strings.Repeat("a", limits.DiagnosticBytes) + "b"},
		{name: "UTF-8 boundary and secret tail", summary: strings.Repeat("a", limits.DiagnosticBytes-1) + "界" + canary},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			collector := diagnostics.NewCollector(diagnostics.CollectorOptions{MaxBytes: limits.DiagnosticBytes, Redactor: runtimeRedactor.Text})
			rule := Rule{Source: Source{Path: "limits.yaml", Ordinal: 1, EffectiveOrdinal: 1}, action: compiledAction{typeName: ActionCommand}}
			diagnostic := hookDiagnostic(DiagnosticCommandFailed, rule, EventToolBefore, "command", 0, item.summary, limits)
			collector.Add(diagnostic)
			items := collector.List()
			if len(items) != 1 {
				t.Fatalf("collector items = %d", len(items))
			}
			message := items[0].Message
			if len(message) > limits.DiagnosticBytes || !utf8.ValidString(message) {
				t.Fatalf("unsafe bounded message: bytes=%d valid=%v", len(message), utf8.ValidString(message))
			}
			if strings.Contains(message, canary) || strings.Contains(items[0].Text(), canary) {
				t.Fatalf("secret tail leaked: %q", items[0].Text())
			}
			encoded, err := json.Marshal(items)
			if err != nil || !json.Valid(encoded) || strings.Contains(string(encoded), canary) {
				t.Fatalf("unsafe JSON: %q, %v", encoded, err)
			}
			if first, second := safeSummary(item.summary, limits.DiagnosticBytes), safeSummary(item.summary, limits.DiagnosticBytes); first != second {
				t.Fatal("summary truncation is unstable")
			}
		})
	}
}
