package hook

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestConfigContainerLimits(t *testing.T) {
	limits := DefaultLimits()

	t.Run("YAML bytes", func(t *testing.T) {
		home, project := t.TempDir(), t.TempDir()
		path := writeHookFile(t, home, ".config/xagent/hooks.yaml", exactSizeHookYAML(limits.YAMLBytes))
		if info, err := os.Stat(path); err != nil || info.Size() != int64(limits.YAMLBytes) {
			t.Fatalf("limit fixture size = %v, %v", info, err)
		}
		if snapshot, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project, Limits: limits}); err != nil || snapshot.Len() != 0 {
			t.Fatalf("YAML byte limit: snapshot=%d err=%v", snapshot.Len(), err)
		}
		if err := os.WriteFile(path, []byte(exactSizeHookYAML(limits.YAMLBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project, Limits: limits}); err == nil || !strings.Contains(err.Error(), "configuration exceeds size limit") {
			t.Fatalf("YAML limit+1 error = %v", err)
		}
	})

	t.Run("rules per file", func(t *testing.T) {
		home, project := t.TempDir(), t.TempDir()
		path := writeHookFile(t, home, ".config/xagent/hooks.yaml", rulesHookYAML(limits.RulesPerFile))
		if snapshot, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project, Limits: limits}); err != nil || snapshot.Len() != limits.RulesPerFile {
			t.Fatalf("rule limit: snapshot=%d err=%v", snapshot.Len(), err)
		}
		if err := os.WriteFile(path, []byte(rulesHookYAML(limits.RulesPerFile+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project, Limits: limits}); err == nil || !strings.Contains(err.Error(), "rule count exceeds limit") {
			t.Fatalf("rule limit+1 error = %v", err)
		}
	})

	t.Run("predicates per rule", func(t *testing.T) {
		home, project := t.TempDir(), t.TempDir()
		path := writeHookFile(t, home, ".config/xagent/hooks.yaml", predicatesHookYAML(limits.PredicatesPerRule))
		if snapshot, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project, Limits: limits}); err != nil || snapshot.Len() != 1 {
			t.Fatalf("predicate limit: snapshot=%d err=%v", snapshot.Len(), err)
		}
		if err := os.WriteFile(path, []byte(predicatesHookYAML(limits.PredicatesPerRule+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project, Limits: limits}); err == nil || !strings.Contains(err.Error(), "predicate count exceeds limit") {
			t.Fatalf("predicate limit+1 error = %v", err)
		}
	})
}

func TestHookSnapshotIgnoresRuntimeFileChanges(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	path := writeHookFile(t, home, ".config/xagent/hooks.yaml", singleCommandHookYAML("old-command"))
	snapshot, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(singleCommandHookYAML("new-command")), 0o600); err != nil {
		t.Fatal(err)
	}

	oldRunner := &fakeCommandRunner{}
	oldEngine, err := NewEngine(snapshot, EngineOptions{ProjectRoot: project, CommandRunner: oldRunner})
	if err != nil {
		t.Fatal(err)
	}
	oldEngine.SystemStart(context.Background())
	if calls := oldRunner.Calls(); len(calls) != 1 || calls[0] != "old-command" {
		t.Fatalf("live snapshot changed after file rewrite: %#v", calls)
	}

	reloaded, err := Load(LoadOptions{HomeDir: home, ProjectRoot: project})
	if err != nil {
		t.Fatal(err)
	}
	newRunner := &fakeCommandRunner{}
	newEngine, err := NewEngine(reloaded, EngineOptions{ProjectRoot: project, CommandRunner: newRunner})
	if err != nil {
		t.Fatal(err)
	}
	newEngine.SystemStart(context.Background())
	if calls := newRunner.Calls(); len(calls) != 1 || calls[0] != "new-command" {
		t.Fatalf("restart did not load rewritten config: %#v", calls)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if rules := snapshot.Rules(); len(rules) != 1 || rules[0].action.command != "old-command" {
		t.Fatalf("file deletion mutated published snapshot: %#v", rules)
	}
}

func exactSizeHookYAML(size int) string {
	base := "version: 1\nhooks: []\n"
	if size <= len(base) {
		return base[:size]
	}
	remaining := size - len(base) - 1
	return base + "#" + strings.Repeat("界", remaining/3) + strings.Repeat("x", remaining%3)
}

func rulesHookYAML(count int) string {
	var output strings.Builder
	output.WriteString("version: 1\nhooks:\n")
	for index := 0; index < count; index++ {
		fmt.Fprintf(&output, "- event: system_start\n  action: {type: command, command: 'true-%d'}\n", index)
	}
	return output.String()
}

func predicatesHookYAML(count int) string {
	var output strings.Builder
	output.WriteString("version: 1\nhooks:\n- event: tool_before\n  if:\n    all:\n")
	for range count {
		output.WriteString("    - {field: tool.name, match: exact, value: Read}\n")
	}
	output.WriteString("  action: {type: command, command: 'true'}\n")
	return output.String()
}

func singleCommandHookYAML(command string) string {
	return "version: 1\nhooks:\n- event: system_start\n  once: true\n  action: {type: command, command: '" + command + "'}\n"
}
