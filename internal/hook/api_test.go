package hook

import (
	"context"
	"testing"
	"time"
)

func TestDefaultLimits(t *testing.T) {
	l := DefaultLimits()
	checks := map[string]struct{ got, want int }{
		"yaml": {l.YAMLBytes, 256 << 10}, "rules": {l.RulesPerFile, 256}, "predicates": {l.PredicatesPerRule, 32},
		"command": {l.CommandBytes, 16 << 10}, "env count": {l.CommandEnvCount, 64}, "env key": {l.EnvKeyBytes, 128}, "env value": {l.EnvValueBytes, 8 << 10},
		"event": {l.EventJSONBytes, 1 << 20}, "stdout": {l.CommandStdoutBytes, 32 << 10}, "stderr": {l.CommandStderrBytes, 32 << 10},
		"url": {l.HTTPURLBytes, 2 << 10}, "headers": {l.HTTPHeaderCount, 32}, "header name": {l.HTTPHeaderNameBytes, 128}, "header value": {l.HTTPHeaderValueBytes, 8 << 10},
		"request": {l.HTTPRequestBytes, 1 << 20}, "response": {l.HTTPResponseBytes, 64 << 10}, "redirects": {l.HTTPRedirects, 3},
		"template": {l.PromptTemplateBytes, 64 << 10}, "fragment": {l.PromptFragmentBytes, 128 << 10}, "owner": {l.PromptOwnerBytes, 256 << 10},
		"agent": {l.SubAgentNameBytes, 64}, "input": {l.SubAgentInputBytes, 64 << 10}, "reason": {l.DenyReasonBytes, 2 << 10}, "diagnostic": {l.DiagnosticBytes, 2 << 10},
	}
	for name, check := range checks {
		if check.got != check.want {
			t.Errorf("%s = %d, want %d", name, check.got, check.want)
		}
	}
	copy := DefaultLimits()
	copy.YAMLBytes = 1
	if DefaultLimits().YAMLBytes != 256<<10 {
		t.Fatal("production defaults were mutated")
	}
}

func TestAPIEnums(t *testing.T) {
	if len(allEvents) != 12 {
		t.Fatalf("events = %d", len(allEvents))
	}
	seen := map[Event]bool{}
	for _, event := range allEvents {
		if seen[event] || !validEvent(event) {
			t.Fatalf("invalid event %q", event)
		}
		seen[event] = true
	}
	if !Deny("no").IsDeny() || Continue().IsDeny() {
		t.Fatal("decision helpers")
	}
}

func TestNoopRuntime(t *testing.T) {
	runtime := Noop()
	runtime.SystemStart(context.Background())
	runtime.SessionStart(context.Background(), "s", SessionNew)
	ref := runtime.BeginTurn(context.Background(), "s", ExecutionMain, ModeDefault)
	if runtime.BeforeTool(context.Background(), ref, ToolInput{}).IsDeny() {
		t.Fatal("noop denied")
	}
	lease, err := runtime.AcquirePrompts(context.Background(), ref)
	if err != nil || len(lease.Blocks()) != 0 {
		t.Fatal("noop prompts")
	}
	lease.Commit()
	lease.Release()
	runtime.AfterTool(context.Background(), ref, ToolInput{}, ToolOutput{}, time.Second)
	runtime.EndTurn(context.Background(), ref, TurnCompleted, "")
	runtime.SessionEnd(context.Background(), "s", SessionEndExit)
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
