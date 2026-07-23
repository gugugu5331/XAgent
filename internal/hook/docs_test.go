package hook

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExampleConfig(t *testing.T) {
	repositoryRoot := hookRepositoryRoot(t)
	example, err := os.ReadFile(filepath.Join(repositoryRoot, "docs", "hook-system", "example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	home, project := t.TempDir(), t.TempDir()
	writeHookFile(t, project, ".xagent/hooks.yaml", string(example))

	snapshot, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project, Limits: DefaultLimits()})
	if err != nil {
		t.Fatalf("example must pass the production Loader: %v", err)
	}
	actions := map[ActionType]bool{}
	scopes := map[PromptScope]bool{}
	var hasOnce, hasAsync, hasTimeout, hasDecision, hasCondition bool
	for _, rule := range snapshot.Rules() {
		actions[rule.ActionType()] = true
		hasOnce = hasOnce || rule.Once
		hasAsync = hasAsync || rule.Async
		hasTimeout = hasTimeout || rule.Timeout == 3*time.Second || rule.Timeout == 5*time.Second || rule.Timeout == 10*time.Second
		hasDecision = hasDecision || rule.action.decision
		hasCondition = hasCondition || rule.condition != nil
		if rule.ActionType() == ActionPrompt {
			scopes[rule.action.scope] = true
		}
	}
	for _, action := range []ActionType{ActionCommand, ActionHTTP, ActionPrompt, ActionSubAgent} {
		if !actions[action] {
			t.Errorf("example does not cover %q action", action)
		}
	}
	for _, scope := range []PromptScope{ScopeNext, ScopeTurn, ScopeSession} {
		if !scopes[scope] {
			t.Errorf("example does not cover %q Prompt scope", scope)
		}
	}
	if !hasOnce || !hasAsync || !hasTimeout || !hasDecision || !hasCondition {
		t.Fatalf("example controls: once=%v async=%v timeout=%v decision=%v condition=%v", hasOnce, hasAsync, hasTimeout, hasDecision, hasCondition)
	}
}

func TestHookDocumentationWarnings(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join(hookRepositoryRoot(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	required := map[string]string{
		"fixed config path":             ".xagent/hooks.yaml",
		"trusted-code warning":          "受信代码",
		"pre-confirmation side effects": "权限确认前",
		"HTTP exfiltration":             "外传",
		"Prompt injection":              "Prompt injection",
		"technical fail-open":           "fail-open",
		"untrusted project review":      "不可信项目",
		"restart requirement":           "重启",
		"aggregate timeout disclosure":  "aggregate timeout",
	}
	for name, phrase := range required {
		if !strings.Contains(text, phrase) {
			t.Errorf("README is missing %s warning (%q)", name, phrase)
		}
	}
}

func hookRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve docs_test.go path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
