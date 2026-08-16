package instructions

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"xagent/internal/budget"
	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/prompt"
	"xagent/internal/safefs"
)

func TestInstructionCacheKeyBindsFileContentAndIncludeOrder(t *testing.T) {
	projectRoot := t.TempDir()
	writeInstructionFile(t, filepath.Join(projectRoot, "root.md"), "root")
	writeInstructionFile(t, filepath.Join(projectRoot, "child.md"), "child")
	opened, err := safefs.Bootstrap(projectRoot, safefs.Policy{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	t.Cleanup(func() {
		if err := opened.Root.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	rootBinding, err := opened.Root.Bind("root.md")
	if err != nil {
		t.Fatalf("Bind root: %v", err)
	}
	childBinding, err := opened.Root.Bind("child.md")
	if err != nil {
		t.Fatalf("Bind child: %v", err)
	}
	rootIdentity, err := FileIdentityFromBinding(rootBinding)
	if err != nil {
		t.Fatalf("FileIdentityFromBinding root: %v", err)
	}
	childIdentity, err := FileIdentityFromBinding(childBinding)
	if err != nil {
		t.Fatalf("FileIdentityFromBinding child: %v", err)
	}
	rootVersion, err := NewFileVersion(filepath.Join(projectRoot, "root.md"), rootIdentity, []byte("root"))
	if err != nil {
		t.Fatalf("NewFileVersion root: %v", err)
	}
	childVersion, err := NewFileVersion(filepath.Join(projectRoot, "child.md"), childIdentity, []byte("child"))
	if err != nil {
		t.Fatalf("NewFileVersion child: %v", err)
	}
	source := GraphSource{Name: "项目根指令", Scope: ScopeProjectRoot, Priority: PriorityProjectRoot, RootPath: filepath.Join(projectRoot, "root.md"), Root: rootIdentity}
	edge := IncludeEdge{From: rootIdentity, To: childIdentity, Position: 0}
	key, err := NewCacheKey(source, []FileVersion{rootVersion, childVersion}, []IncludeEdge{edge})
	if err != nil {
		t.Fatalf("NewCacheKey: %v", err)
	}

	changedChild, err := NewFileVersion(filepath.Join(projectRoot, "child.md"), childIdentity, []byte("changed child"))
	if err != nil {
		t.Fatalf("NewFileVersion changed child: %v", err)
	}
	contentKey, err := NewCacheKey(source, []FileVersion{rootVersion, changedChild}, []IncludeEdge{edge})
	if err != nil {
		t.Fatalf("NewCacheKey changed content: %v", err)
	}
	orderKey, err := NewCacheKey(source, []FileVersion{rootVersion, childVersion}, []IncludeEdge{{From: rootIdentity, To: childIdentity, Position: 1}})
	if err != nil {
		t.Fatalf("NewCacheKey changed include order: %v", err)
	}
	if key == contentKey || key == orderKey || contentKey == orderKey {
		t.Fatal("cache key did not bind file content and include order independently")
	}
}

func TestInstructionCacheStillConsumesExpansionBudget(t *testing.T) {
	const content = "cached expansion"
	root := t.TempDir()
	writeInstructionFile(t, filepath.Join(root, "MEWCODE.md"), "same content")
	key, source, files := instructionWorkspaceGraph(t, root, ScopeProjectRoot)
	cache := &ExpansionCache{}
	if err := cache.Store(key, CachedExpansion{Source: source, Content: content, Files: files}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	limits, err := budget.NewLimits(budget.Limit{Dimension: budget.ExpandedBytes, Value: int64(len(content))})
	if err != nil {
		t.Fatalf("NewLimits: %v", err)
	}
	counter, err := budget.NewCounter(limits, limits)
	if err != nil {
		t.Fatalf("NewCounter: %v", err)
	}

	first, found, err := cache.Lookup(key, counter)
	if err != nil || !found || first.Content != content {
		t.Fatalf("first Lookup = (%#v, %t, %v), want cached content", first, found, err)
	}
	if got := counter.Snapshot().Used(budget.ExpandedBytes); got != int64(len(content)) {
		t.Fatalf("expanded bytes after first hit = %d, want %d", got, len(content))
	}

	second, found, err := cache.Lookup(key, counter)
	if err == nil || !found {
		t.Fatalf("second Lookup = (%#v, %t, %v), want budget failure on cache hit", second, found, err)
	}
	if second.Content != "" || second.Files != nil || second.Edges != nil {
		t.Fatalf("second Lookup exposed content after budget failure: %#v", second)
	}
	if got := counter.Snapshot().Used(budget.ExpandedBytes); got != int64(len(content)) {
		t.Fatalf("failed cache hit changed expanded bytes to %d, want %d", got, len(content))
	}
}

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

func TestLoaderUsesSharedCumulativeInstructionBudget(t *testing.T) {
	projectRoot := t.TempDir()
	userRoot := t.TempDir()
	cfg := testConfig()
	rootContent := "root\n@include child.md"
	childContent := "child"
	cfg.MaxFiles = 2
	cfg.MaxTotalBytes = int64(len(rootContent) + len(childContent))
	cfg.MaxExpandedBytes = int64(len("root\nchild"))
	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectFile), rootContent)
	writeInstructionFile(t, filepath.Join(projectRoot, "child.md"), childContent)

	loader := Loader{ProjectRoot: projectRoot, UserDir: userRoot, Config: cfg}
	sections, items := loader.Load(context.Background())
	if len(sections) != 1 || sections[0].Content != "root\nchild" {
		t.Fatalf("boundary load = %#v diagnostics=%#v", sections, items)
	}
	assertNoInstructionDiagnostic(t, items, "instructions_files_limit")
	assertNoInstructionDiagnostic(t, items, "instructions_total_bytes_limit")
	assertNoInstructionDiagnostic(t, items, "instructions_expanded_bytes_limit")

	cfg.MaxFiles = 1
	loader.Config = cfg
	sections, items = loader.Load(context.Background())
	if len(sections) != 1 || sections[0].Content != "root" {
		t.Fatalf("file-limited load exposed rejected include: %#v diagnostics=%#v", sections, items)
	}
	assertInstructionDiagnostic(t, items, "instructions_files_limit")
}

func TestLoaderCancellationStopsSafefsDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sections, items := (Loader{ProjectRoot: t.TempDir(), UserDir: t.TempDir(), Config: testConfig()}).Load(ctx)
	if len(sections) != 0 {
		t.Fatalf("canceled load returned sections: %#v", sections)
	}
	assertInstructionDiagnostic(t, items, "instructions_context_cancelled")
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

func TestLoaderMapsInstructionScopesToPromptScopes(t *testing.T) {
	projectRoot := t.TempDir()
	userRoot := t.TempDir()
	cfg := testConfig()
	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectFile), "project root")
	writeInstructionFile(t, filepath.Join(projectRoot, cfg.ProjectDir, cfg.ProjectFile), "project dir")
	writeInstructionFile(t, filepath.Join(userRoot, cfg.ProjectFile), "user")

	sections, items := (Loader{ProjectRoot: projectRoot, UserDir: userRoot, Config: cfg}).Load(context.Background())
	if len(items) != 0 || len(sections) != 3 {
		t.Fatalf("load result sections=%#v diagnostics=%#v", sections, items)
	}
	want := []prompt.Scope{prompt.ScopeProject, prompt.ScopeProject, prompt.ScopeUser}
	for index, scope := range want {
		if sections[index].Scope != scope {
			t.Fatalf("sections[%d].Scope = %q, want %q", index, sections[index].Scope, scope)
		}
	}
	if got := promptScope(Scope("future")); got.Valid() {
		t.Fatalf("unknown instruction scope mapped to valid prompt scope %q", got)
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

func runIncludeLinkRace(t *testing.T) {
	t.Helper()
	projectRoot := t.TempDir()
	outside := t.TempDir()
	userRoot := t.TempDir()
	const canary = "outside-instruction-canary-73194628"
	writeInstructionFile(t, filepath.Join(projectRoot, "MEWCODE.md"), "root\n@include slot/secret.md")
	writeInstructionFile(t, filepath.Join(outside, "secret.md"), canary)
	slot := filepath.Join(projectRoot, "slot")
	held := filepath.Join(projectRoot, "slot-held")
	writeInstructionFile(t, filepath.Join(slot, "secret.md"), "inside")

	stop := make(chan struct{})
	done := make(chan struct{})
	started := make(chan struct{})
	errorsSeen := make(chan error, 1)
	var startedOnce sync.Once
	var swaps atomic.Int64
	reportError := func(err error) {
		select {
		case errorsSeen <- err:
		default:
		}
		startedOnce.Do(func() { close(started) })
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.Rename(slot, held); err != nil {
				reportError(err)
				return
			}
			if err := os.Symlink(outside, slot); err != nil {
				_ = os.Rename(held, slot)
				reportError(err)
				return
			}
			swaps.Add(1)
			startedOnce.Do(func() { close(started) })
			runtime.Gosched()
			if err := os.Remove(slot); err != nil {
				reportError(err)
				return
			}
			if err := os.Rename(held, slot); err != nil {
				reportError(err)
				return
			}
			runtime.Gosched()
		}
	}()
	<-started

	loader := Loader{ProjectRoot: projectRoot, UserDir: userRoot, Config: testConfig()}
	leaked := false
	for attempt := 0; attempt < 64; attempt++ {
		sections, items := loader.Load(context.Background())
		for _, section := range sections {
			leaked = leaked || strings.Contains(section.Content, canary)
		}
		for _, item := range items {
			leaked = leaked || strings.Contains(item.Message, canary) || strings.Contains(item.Path, canary)
		}
		runtime.Gosched()
	}
	close(stop)
	<-done
	select {
	case err := <-errorsSeen:
		t.Fatalf("link race fixture failed: %v", err)
	default:
	}
	if swaps.Load() == 0 {
		t.Fatal("link race fixture made no replacement")
	}
	if leaked {
		t.Fatal("safefs instruction load exposed outside-root content during link replacement")
	}
}
