package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xagent/internal/config"
	"xagent/internal/redact"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

func TestNewMemoryManagerHonorsEnabled(t *testing.T) {
	disabled := false
	disabledManager := newMemoryManager(t.TempDir(), config.MemoryConfig{Enabled: &disabled}, nil)
	if disabledManager != nil {
		t.Fatal("memory.enabled=false constructed a production memory manager")
	}
	if manager := newSessionManager(nil, disabledManager, nil); manager.Memory != nil {
		t.Fatal("disabled memory became a non-nil typed interface in the session manager")
	}

	enabled := true
	enabledManager := newMemoryManager(t.TempDir(), config.MemoryConfig{Enabled: &enabled}, nil)
	if enabledManager == nil {
		t.Fatal("memory.enabled=true did not construct a production memory manager")
	}
	if manager := newSessionManager(nil, enabledManager, nil); manager.Memory == nil {
		t.Fatal("enabled memory was not attached to the session manager")
	}
	if manager := newMemoryManager(t.TempDir(), config.MemoryConfig{}, nil); manager == nil {
		t.Fatal("unset memory.enabled must preserve the enabled-by-default behavior")
	}
}

func TestStartupSkillAssemblyLoadsThreeTiers(t *testing.T) {
	projectRoot := t.TempDir()
	userRoot := filepath.Join(t.TempDir(), "skills")
	projectSkills := filepath.Join(projectRoot, ".xagent", "skills")
	if err := os.MkdirAll(projectSkills, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStartupSkill(t, filepath.Join(userRoot, "commit.md"), "commit", "user commit", "shared", "Read")
	writeStartupSkill(t, filepath.Join(projectSkills, "commit.md"), "commit", "project commit", "shared", "Read")
	writeStartupSkill(t, filepath.Join(projectSkills, "clear.md"), "clear", "reserved skill", "shared", "Read")

	registry, err := tool.NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	manager, err := newSkillManager(projectRoot, userRoot, registry, redact.Text)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	if len(snapshot.Catalog) < 4 {
		t.Fatalf("expected builtins plus project skill, got %#v", snapshot.Catalog)
	}
	commit, ok := manager.Resolve("commit")
	if !ok || commit.Source != skill.SourceProject || commit.Description != "project commit" {
		t.Fatalf("project override did not win: %#v ok=%v", commit, ok)
	}
	var clearFound bool
	for _, item := range snapshot.Catalog {
		if item.Name == "clear" {
			clearFound = true
			if item.SlashEnabled {
				t.Fatal("reserved command conflict unexpectedly received a slash command")
			}
		}
	}
	if !clearFound {
		t.Fatal("reserved-name skill should remain loadable in catalog")
	}
}

func TestStartupSkillAssemblyRejectsUnknownTool(t *testing.T) {
	projectRoot := t.TempDir()
	projectSkills := filepath.Join(projectRoot, ".xagent", "skills")
	if err := os.MkdirAll(projectSkills, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStartupSkill(t, filepath.Join(projectSkills, "bad.md"), "bad", "bad tool", "shared", "MissingTool")
	registry, err := tool.NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	if _, err := newSkillManager(projectRoot, filepath.Join(t.TempDir(), "missing"), registry, redact.Text); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown tool startup error, got %v", err)
	}
}

func TestStartupSkillAssemblyAcceptsRegisteredExtensionTool(t *testing.T) {
	projectRoot := t.TempDir()
	projectSkills := filepath.Join(projectRoot, ".xagent", "skills")
	if err := os.MkdirAll(projectSkills, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStartupSkill(t, filepath.Join(projectSkills, "extended.md"), "extended", "extension tool", "shared", "mcp__demo__search")
	registry, err := tool.NewRegistry(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(startupTestTool{name: "mcp__demo__search"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	if _, err := newSkillManager(projectRoot, filepath.Join(t.TempDir(), "missing"), registry, redact.Text); err != nil {
		t.Fatalf("registered extension tool was rejected: %v", err)
	}
}

func writeStartupSkill(t *testing.T, path string, name string, description string, mode string, allowedTool string) {
	t.Helper()
	content := "---\nname: " + name + "\ndescription: " + description + "\nallowed_tools: [" + allowedTool + "]\nmode: " + mode + "\n---\nSOP {{args}}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

type startupTestTool struct{ name string }

func (t startupTestTool) Name() string      { return t.name }
func (startupTestTool) Description() string { return "test extension tool" }
func (startupTestTool) Schema() tool.Schema { return tool.ObjectSchema(nil, nil) }
func (startupTestTool) Risk() tool.Risk     { return tool.RiskSafe }
func (startupTestTool) Execute(_ context.Context, input tool.Input) tool.Result {
	return tool.Success(input, "ok", "ok", nil)
}
