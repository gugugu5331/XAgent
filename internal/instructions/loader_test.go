package instructions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/prompt"
)

func TestLoaderUsesInstructionFallbackFiles(t *testing.T) {
	projectRoot := t.TempDir()
	cfg := testConfig()
	writeInstructionFile(t, filepath.Join(projectRoot, "CLAUDE.md"), "claude instruction")
	loader := Loader{ProjectRoot: projectRoot, Config: cfg}
	sections, items := loader.Load(context.Background())
	if len(items) != 0 {
		t.Fatalf("diagnostics = %#v, want empty", items)
	}
	if len(sections) != 1 || sections[0].Content != "claude instruction" {
		t.Fatalf("sections = %#v, want CLAUDE.md fallback", sections)
	}

	projectRoot = t.TempDir()
	writeInstructionFile(t, filepath.Join(projectRoot, "AGENTS.md"), "agents instruction")
	loader = Loader{ProjectRoot: projectRoot, Config: cfg}
	sections, items = loader.Load(context.Background())
	if len(items) != 0 {
		t.Fatalf("diagnostics = %#v, want empty", items)
	}
	if len(sections) != 1 || sections[0].Content != "agents instruction" {
		t.Fatalf("sections = %#v, want AGENTS.md fallback", sections)
	}
}

func TestLoaderExplicitProjectFileOverridesFallbackFiles(t *testing.T) {
	projectRoot := t.TempDir()
	cfg := testConfig()
	cfg.ProjectFile = "CUSTOM.md"
	writeInstructionFile(t, filepath.Join(projectRoot, "CUSTOM.md"), "custom instruction")
	writeInstructionFile(t, filepath.Join(projectRoot, "CLAUDE.md"), "claude instruction")
	loader := Loader{ProjectRoot: projectRoot, Config: cfg}
	sections, items := loader.Load(context.Background())
	if len(items) != 0 {
		t.Fatalf("diagnostics = %#v, want empty", items)
	}
	if len(sections) != 1 || sections[0].Content != "custom instruction" {
		t.Fatalf("sections = %#v, want only explicit project file", sections)
	}
}

func TestLoaderOrdersInstructionScopes(t *testing.T) {
	projectRoot := t.TempDir()
	userRoot := t.TempDir()
	cfg := testConfig()

	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectFile), "project root instruction")
	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectDir, cfg.ProjectFile), "project dir instruction")
	writeInstructionFile(t, filepath.Join(userRoot, cfg.ProjectFile), "user instruction")

	loader := Loader{ProjectRoot: projectRoot, UserDir: userRoot, Config: cfg}
	sections, items := loader.Load(context.Background())
	if len(items) != 0 {
		t.Fatalf("diagnostics = %#v, want empty", items)
	}
	if len(sections) != 3 {
		t.Fatalf("len(sections) = %d, want 3", len(sections))
	}

	want := []string{"project root instruction", "project dir instruction", "user instruction"}
	for i, content := range want {
		if sections[i].Content != content {
			t.Fatalf("sections[%d].Content = %q, want %q", i, sections[i].Content, content)
		}
		if !sections[i].Stable {
			t.Fatalf("sections[%d].Stable = false, want true", i)
		}
	}
	if !(sections[0].Priority < sections[1].Priority && sections[1].Priority < sections[2].Priority) {
		t.Fatalf("priorities = %d, %d, %d, want ascending by scope", sections[0].Priority, sections[1].Priority, sections[2].Priority)
	}
}

func TestLoaderSkipsMissingAndEmptyInstructionFiles(t *testing.T) {
	projectRoot := t.TempDir()
	userRoot := t.TempDir()
	cfg := testConfig()

	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectDir, cfg.ProjectFile), "   \n\t")
	writeInstructionFile(t, filepath.Join(userRoot, cfg.ProjectFile), "user instruction")

	loader := Loader{ProjectRoot: projectRoot, UserDir: userRoot, Config: cfg}
	sections, items := loader.Load(context.Background())
	if len(items) != 0 {
		t.Fatalf("diagnostics = %#v, want empty", items)
	}
	if len(sections) != 1 || sections[0].Content != "user instruction" {
		t.Fatalf("sections = %#v, want only user instruction", sections)
	}
}

func TestLoaderReportsOversizedInstructionFile(t *testing.T) {
	projectRoot := t.TempDir()
	cfg := testConfig()
	cfg.MaxFileBytes = 4
	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectFile), "too large")

	loader := Loader{ProjectRoot: projectRoot, Config: cfg}
	sections, items := loader.Load(context.Background())
	if len(sections) != 0 {
		t.Fatalf("len(sections) = %d, want 0", len(sections))
	}
	assertInstructionDiagnostic(t, items, "instructions_file_too_large")
}

func TestLoaderOptionalSectionsStayBelowFixedStableSections(t *testing.T) {
	projectRoot := t.TempDir()
	cfg := testConfig()
	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectFile), "project root instruction")

	loader := Loader{ProjectRoot: projectRoot, Config: cfg}
	sections, items := loader.Load(context.Background())
	if len(items) != 0 {
		t.Fatalf("diagnostics = %#v, want empty", items)
	}
	stable := prompt.StableSections(sections)
	if len(stable) < 2 {
		t.Fatalf("len(stable) = %d, want fixed sections plus instructions", len(stable))
	}
	last := stable[len(stable)-1]
	if last.Content != "project root instruction" {
		t.Fatalf("last stable content = %q, want project instruction after fixed sections", last.Content)
	}
}

func TestLoadSourcesReturnsScopeMetadata(t *testing.T) {
	projectRoot := t.TempDir()
	userRoot := t.TempDir()
	cfg := testConfig()

	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectFile), "project root")
	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectDir, cfg.ProjectFile), "project dir")
	writeInstructionFile(t, filepath.Join(userRoot, cfg.ProjectFile), "user")

	loader := Loader{ProjectRoot: projectRoot, UserDir: userRoot, Config: cfg}
	sources, items := loader.LoadSources(context.Background())
	if len(items) != 0 {
		t.Fatalf("diagnostics = %#v, want empty", items)
	}
	want := []Scope{ScopeProjectRoot, ScopeProjectDir, ScopeUserDir}
	for i, scope := range want {
		if sources[i].Scope != scope {
			t.Fatalf("sources[%d].Scope = %q, want %q", i, sources[i].Scope, scope)
		}
	}
}

func TestIncludeDepthAndCycleDiagnostics(t *testing.T) {
	projectRoot := t.TempDir()
	cfg := testConfig()
	cfg.MaxIncludeDepth = 2

	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectFile), "root\n@include a.md")
	writeInstructionFile(t, filepath.Join(projectRoot, "a.md"), "a\n@include b.md")
	writeInstructionFile(t, filepath.Join(projectRoot, "b.md"), "b\n@include c.md")
	writeInstructionFile(t, filepath.Join(projectRoot, "c.md"), "c")

	loader := Loader{ProjectRoot: projectRoot, Config: cfg}
	sections, items := loader.Load(context.Background())
	if len(sections) != 1 {
		t.Fatalf("sections = %#v, want one root section", sections)
	}
	if !strings.Contains(sections[0].Content, "root\na\nb") || strings.Contains(sections[0].Content, "c") {
		t.Fatalf("unexpected expanded content: %q", sections[0].Content)
	}
	assertInstructionDiagnostic(t, items, "instructions_include_too_deep")

	projectRoot = t.TempDir()
	cycleCfg := testConfig()
	cycleCfg.MaxIncludeDepth = 5
	writeInstructionFile(t, filepath.Join(projectRoot, cycleCfg.ProjectFile), "root\n@include a.md")
	writeInstructionFile(t, filepath.Join(projectRoot, "a.md"), "a\n@include b.md")
	writeInstructionFile(t, filepath.Join(projectRoot, "b.md"), "b\n@include a.md")
	loader = Loader{ProjectRoot: projectRoot, Config: cycleCfg}
	_, items = loader.Load(context.Background())
	assertInstructionDiagnostic(t, items, "instructions_include_cycle")
}

func TestIncludeRejectsSymlinkEscape(t *testing.T) {
	projectRoot := t.TempDir()
	outside := t.TempDir()
	cfg := testConfig()

	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectFile), "root\n@include link.md")
	writeInstructionFile(t, filepath.Join(outside, "secret.md"), "secret")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(projectRoot, "link.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	loader := Loader{ProjectRoot: projectRoot, Config: cfg}
	sections, items := loader.Load(context.Background())
	if len(sections) != 1 || strings.Contains(sections[0].Content, "secret") {
		t.Fatalf("symlink escaped content leaked into sections: %#v", sections)
	}
	assertInstructionDiagnostic(t, items, "instructions_path_escape")
}

func TestIncludeRejectsParentEscape(t *testing.T) {
	projectRoot := t.TempDir()
	outside := t.TempDir()
	cfg := testConfig()

	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectFile), "root\n@include ../outside/secret.md")
	writeInstructionFile(t, filepath.Join(filepath.Dir(projectRoot), "outside", "secret.md"), "wrong")
	writeInstructionFile(t, filepath.Join(outside, "secret.md"), "secret")

	loader := Loader{ProjectRoot: projectRoot, Config: cfg}
	sections, items := loader.Load(context.Background())
	if len(sections) != 1 || strings.Contains(sections[0].Content, "secret") || strings.Contains(sections[0].Content, "wrong") {
		t.Fatalf("parent escaped content leaked into sections: %#v", sections)
	}
	assertInstructionDiagnostic(t, items, "instructions_path_escape")
}

func TestCachedLoaderInvalidatesWhenRootInstructionChanges(t *testing.T) {
	projectRoot := t.TempDir()
	cfg := testConfig()
	path := filepath.Join(projectRoot, cfg.ProjectFile)
	writeInstructionFile(t, path, "first")

	loader := &CachedLoader{Loader: Loader{ProjectRoot: projectRoot, Config: cfg}}
	sections, items := loader.Load(context.Background())
	if len(items) != 0 || len(sections) != 1 || sections[0].Content != "first" {
		t.Fatalf("initial load = %#v diagnostics=%#v", sections, items)
	}
	sections[0].Content = "polluted"
	writeInstructionFile(t, path, "second")
	sections, items = loader.Load(context.Background())
	if len(items) != 0 || len(sections) != 1 || sections[0].Content != "second" {
		t.Fatalf("reloaded sections = %#v diagnostics=%#v", sections, items)
	}
}

func TestCachedLoaderInvalidatesWhenIncludeChanges(t *testing.T) {
	projectRoot := t.TempDir()
	cfg := testConfig()
	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectFile), "root\n@include inc.md")
	writeInstructionFile(t, filepath.Join(projectRoot, "inc.md"), "first include")

	loader := &CachedLoader{Loader: Loader{ProjectRoot: projectRoot, Config: cfg}}
	sections, items := loader.Load(context.Background())
	if len(items) != 0 || len(sections) != 1 || !strings.Contains(sections[0].Content, "first include") {
		t.Fatalf("initial load = %#v diagnostics=%#v", sections, items)
	}
	writeInstructionFile(t, filepath.Join(projectRoot, "inc.md"), "second include")
	sections, items = loader.Load(context.Background())
	if len(items) != 0 || len(sections) != 1 || !strings.Contains(sections[0].Content, "second include") {
		t.Fatalf("reloaded sections = %#v diagnostics=%#v", sections, items)
	}
}

func TestCachedLoaderInvalidatesWhenMissingInstructionAppears(t *testing.T) {
	projectRoot := t.TempDir()
	cfg := testConfig()
	loader := &CachedLoader{Loader: Loader{ProjectRoot: projectRoot, Config: cfg}}
	sections, items := loader.Load(context.Background())
	if len(items) != 0 || len(sections) != 0 {
		t.Fatalf("initial load = %#v diagnostics=%#v", sections, items)
	}
	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectFile), "created later")
	sections, items = loader.Load(context.Background())
	if len(items) != 0 || len(sections) != 1 || sections[0].Content != "created later" {
		t.Fatalf("reloaded sections = %#v diagnostics=%#v", sections, items)
	}
}

func testConfig() config.InstructionsConfig {
	return config.InstructionsConfig{ProjectFile: "MEWCODE.md", ProjectDir: ".mewcode", UserDir: ".mewcode", MaxIncludeDepth: 5, MaxFileBytes: 64 * 1024}
}

func writeInstructionFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func assertInstructionDiagnostic(t *testing.T, items []diagnostics.Diagnostic, code string) {
	t.Helper()
	for _, item := range items {
		if item.Code == code {
			return
		}
	}
	t.Fatalf("diagnostics missing %q: %#v", code, items)
}
