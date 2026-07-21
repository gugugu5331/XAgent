package skill

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func TestBuildSnapshotPrecedence(t *testing.T) {
	definitions := []Definition{
		testDefinition("demo", SourceProject, "project"),
		testDefinition("demo", SourceBuiltin, "builtin"),
		testDefinition("demo", SourceUser, "user"),
		testDefinition("other", SourceBuiltin, "other"),
	}
	snapshot, err := BuildSnapshot(definitions, SnapshotOptions{Generation: 7, ToolNames: []string{"Read", LoadSkillToolName}})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation != 7 || snapshot.Definitions["demo"].Body != "project" {
		t.Fatalf("precedence failed: %#v", snapshot)
	}
	if got := catalogNames(snapshot.Catalog); !reflect.DeepEqual(got, []string{"demo", "other"}) {
		t.Fatalf("catalog not sorted: %#v", got)
	}
}

func TestBuildSnapshotPrecedenceFallsBackByTier(t *testing.T) {
	builtin := testDefinition("demo", SourceBuiltin, "builtin")
	user := testDefinition("demo", SourceUser, "user")
	project := testDefinition("demo", SourceProject, "project")
	toolNames := []string{LoadSkillToolName}

	for _, testCase := range []struct {
		name        string
		definitions []Definition
		wantSource  Source
		wantBody    string
	}{
		{name: "project overrides user and builtin", definitions: []Definition{user, builtin, project}, wantSource: SourceProject, wantBody: "project"},
		{name: "user is restored after project removal", definitions: []Definition{builtin, user}, wantSource: SourceUser, wantBody: "user"},
		{name: "builtin is restored after user removal", definitions: []Definition{builtin}, wantSource: SourceBuiltin, wantBody: "builtin"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot, err := BuildSnapshot(testCase.definitions, SnapshotOptions{ToolNames: toolNames})
			if err != nil {
				t.Fatal(err)
			}
			definition, ok := snapshot.Definitions["demo"]
			if !ok || definition.Source != testCase.wantSource || definition.Body != testCase.wantBody {
				t.Fatalf("unexpected effective definition: %#v", definition)
			}
		})
	}
}

func TestSnapshotValidationAndCopies(t *testing.T) {
	definition := testDefinition("clear", SourceProject, "body")
	definition.AllowedTools = []string{"Read"}
	snapshot, err := BuildSnapshot([]Definition{definition}, SnapshotOptions{
		ToolNames: []string{"Read", LoadSkillToolName}, ReservedCommands: []string{"/CLEAR"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Catalog[0].SlashEnabled || len(snapshot.Diagnostics) != 1 || snapshot.Diagnostics[0].Code != "skill_command_reserved" {
		t.Fatalf("reserved command handling failed: %#v", snapshot)
	}
	if _, err := BuildSnapshot([]Definition{definition}, SnapshotOptions{ToolNames: []string{"read", LoadSkillToolName}}); err == nil {
		t.Fatal("expected case-sensitive unknown tool error")
	}

	copySnapshot := snapshot.Clone()
	copySnapshot.Catalog[0].Description = "mutated"
	copyDefinition := copySnapshot.Definitions["clear"]
	copyDefinition.AllowedTools[0] = "mutated"
	copySnapshot.Definitions["clear"] = copyDefinition
	if snapshot.Catalog[0].Description == "mutated" || snapshot.Definitions["clear"].AllowedTools[0] == "mutated" {
		t.Fatal("snapshot clone aliases mutable data")
	}
}

func TestBuildSnapshotRejectsSameTierConflict(t *testing.T) {
	first := testDefinition("Demo", SourceUser, "one")
	first.EntryPath = "/one.md"
	second := testDefinition("demo", SourceUser, "two")
	second.EntryPath = "/two.md"
	_, err := BuildSnapshot([]Definition{first, second}, SnapshotOptions{ToolNames: []string{LoadSkillToolName}})
	if err == nil || !strings.Contains(err.Error(), "/one.md") || !strings.Contains(err.Error(), "/two.md") {
		t.Fatalf("unexpected conflict error: %v", err)
	}
}

func TestManagerInitialLoadAndInvalidOverride(t *testing.T) {
	userRoot := t.TempDir()
	projectRoot := t.TempDir()
	writeSkillFile(t, filepath.Join(userRoot, "demo.md"), "demo", ModeShared, "user body", []string{"Read"}, 0, "")
	if err := os.WriteFile(filepath.Join(projectRoot, "demo.md"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerOptions{
		Sources:   []SourceFS{{Source: SourceUser, Root: userRoot}, {Source: SourceProject, Root: projectRoot}},
		ToolNames: []string{"Read", LoadSkillToolName},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := manager.Resolve("DEMO")
	if !ok || definition.Body != "user body" {
		t.Fatalf("invalid high-priority file shadowed valid lower definition: %#v %v", definition, ok)
	}
	if manager.Snapshot().Generation != 1 || len(manager.Snapshot().Diagnostics) != 1 {
		t.Fatalf("unexpected initial snapshot: %#v", manager.Snapshot())
	}

	conflictRoot := t.TempDir()
	writeSkillFile(t, filepath.Join(conflictRoot, "one.md"), "same", ModeShared, "one", nil, 0, "")
	writeSkillFile(t, filepath.Join(conflictRoot, "two.md"), "SAME", ModeShared, "two", nil, 0, "")
	if _, err := NewManager(ManagerOptions{Sources: []SourceFS{{Source: SourceProject, Root: conflictRoot}}, ToolNames: []string{LoadSkillToolName}}); err == nil {
		t.Fatal("expected same-tier startup failure")
	}

	unknownRoot := t.TempDir()
	writeSkillFile(t, filepath.Join(unknownRoot, "unknown.md"), "unknown", ModeShared, "body", []string{"Missing"}, 0, "")
	if _, err := NewManager(ManagerOptions{Sources: []SourceFS{{Source: SourceProject, Root: unknownRoot}}, ToolNames: []string{LoadSkillToolName}}); err == nil {
		t.Fatal("expected unknown tool startup failure")
	}
}

func TestManagerRefreshIsAtomic(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "demo.md")
	writeSkillFile(t, entry, "demo", ModeShared, "version one", []string{"Read"}, 0, "")
	manager, err := NewManager(ManagerOptions{Sources: []SourceFS{{Source: SourceProject, Root: root}}, ToolNames: []string{"Read", LoadSkillToolName}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.RefreshIfChanged(context.Background())
	if err != nil || result.Changed || result.Generation != 1 {
		t.Fatalf("unchanged refresh was not a no-op: %#v %v", result, err)
	}

	writeSkillFile(t, entry, "demo", ModeShared, "version two", []string{"Read"}, 0, "")
	bumpModTime(t, entry, 1)
	result, err = manager.RefreshIfChanged(context.Background())
	if err != nil || !result.Changed || result.Generation != 2 {
		t.Fatalf("valid refresh failed: %#v %v", result, err)
	}
	if definition, _ := manager.Resolve("demo"); definition.Body != "version two" {
		t.Fatalf("new definition not published: %#v", definition)
	}

	conflict := filepath.Join(root, "duplicate.md")
	writeSkillFile(t, conflict, "demo", ModeShared, "duplicate", nil, 0, "")
	bumpModTime(t, conflict, 2)
	result, err = manager.RefreshIfChanged(context.Background())
	if err == nil || result.Changed || result.Generation != 2 || len(result.Diagnostics) == 0 {
		t.Fatalf("conflicting refresh did not roll back: %#v %v", result, err)
	}
	if definition, _ := manager.Resolve("demo"); definition.Body != "version two" {
		t.Fatal("failed candidate changed current definition")
	}
	if err := os.Remove(conflict); err != nil {
		t.Fatal(err)
	}
	writeSkillFile(t, entry, "demo", ModeShared, "unknown tool", []string{"Missing"}, 0, "")
	bumpModTime(t, entry, 3)
	result, err = manager.RefreshIfChanged(context.Background())
	if err == nil || result.Generation != 2 {
		t.Fatalf("unknown-tool refresh should fail: %#v %v", result, err)
	}
	if definition, _ := manager.Resolve("demo"); definition.Body != "version two" {
		t.Fatal("unknown-tool candidate changed current definition")
	}

	writeSkillFile(t, entry, "demo", ModeShared, "version three", []string{"Read"}, 0, "")
	bumpModTime(t, entry, 4)
	result, err = manager.RefreshIfChanged(context.Background())
	if err != nil || result.Generation != 3 || !result.Changed {
		t.Fatalf("recovery refresh failed: %#v %v", result, err)
	}
}

func TestManagerRefreshNoChangeDoesNotReopenSkillBody(t *testing.T) {
	files := fstest.MapFS{
		"demo.md": &fstest.MapFile{Data: []byte("---\nname: demo\ndescription: demo description\nmode: shared\n---\nbody"), Mode: 0o600},
	}
	counting := newEntryOpenCountingFS(files)
	manager, err := NewManager(ManagerOptions{
		Sources:   []SourceFS{{Source: SourceProject, FS: counting, Root: "."}},
		ToolNames: []string{LoadSkillToolName},
	})
	if err != nil {
		t.Fatal(err)
	}
	initialBodyOpens := counting.OpenCount("demo.md")
	if initialBodyOpens != 1 {
		t.Fatalf("initial load opened the Skill body %d times, want 1", initialBodyOpens)
	}

	for iteration := 0; iteration < 2; iteration++ {
		result, err := manager.RefreshIfChanged(context.Background())
		if err != nil || result.Changed || result.Generation != 1 {
			t.Fatalf("unchanged refresh %d was not a no-op: %#v %v", iteration, result, err)
		}
	}
	if got := counting.OpenCount("demo.md"); got != initialBodyOpens {
		t.Fatalf("unchanged refresh reopened the Skill body: before=%d after=%d", initialBodyOpens, got)
	}
}

func TestManagerRejectedRefreshPreservesActivitySnapshot(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "demo.md")
	writeSkillFile(t, entry, "demo", ModeShared, "stable body {{args}}", []string{"Read"}, 0, "stable-model")
	manager, err := NewManager(ManagerOptions{
		Sources:   []SourceFS{{Source: SourceProject, Root: root}},
		ToolNames: []string{"Read", LoadSkillToolName},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, ok := manager.Resolve("demo")
	if !ok {
		t.Fatal("initial Skill was not resolved")
	}
	activity := NewActivity()
	if _, err := activity.Activate(definition, "pinned args"); err != nil {
		t.Fatal(err)
	}
	before := activity.Snapshot()

	duplicate := filepath.Join(root, "duplicate.md")
	writeSkillFile(t, duplicate, "demo", ModeShared, "conflicting body", nil, 0, "")
	result, err := manager.RefreshIfChanged(context.Background())
	if err == nil || result.Changed || result.Generation != 1 {
		t.Fatalf("conflicting refresh was not rejected atomically: %#v %v", result, err)
	}
	if after := activity.Snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected refresh changed the active snapshot:\nbefore=%#v\nafter=%#v", before, after)
	}
	resolved, ok := manager.Resolve("demo")
	if !ok || resolved.Body != definition.Body || resolved.Fingerprint != definition.Fingerprint {
		t.Fatalf("rejected refresh changed the effective definition: %#v", resolved)
	}
}

func TestManagerRefreshAcceptsSingleFileParseFailure(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "demo.md")
	writeSkillFile(t, entry, "demo", ModeShared, "body", nil, 0, "")
	manager, err := NewManager(ManagerOptions{Sources: []SourceFS{{Source: SourceProject, Root: root}}, ToolNames: []string{LoadSkillToolName}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	bumpModTime(t, entry, 1)
	result, err := manager.RefreshIfChanged(context.Background())
	if err != nil || !result.Changed || result.Generation != 2 {
		t.Fatalf("single-file failure should produce a valid changed snapshot: %#v %v", result, err)
	}
	if _, exists := manager.Resolve("demo"); exists {
		t.Fatal("invalid definition remained in refreshed catalog")
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "skill_entry_invalid" {
		t.Fatalf("missing parse diagnostic: %#v", result.Diagnostics)
	}
}

func TestManagerRefreshDetectsSameSizeSameMtimeReplacement(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "demo.md")
	writeSkillFile(t, entry, "demo", ModeShared, "version-one", nil, 0, "")
	manager, err := NewManager(ManagerOptions{Sources: []SourceFS{{Source: SourceProject, Root: root}}, ToolNames: []string{LoadSkillToolName}})
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(entry)
	if err != nil {
		t.Fatal(err)
	}
	replacement := []byte(strings.Replace(string(original), "version-one", "version-two", 1))
	if len(replacement) != len(original) {
		t.Fatal("test replacement unexpectedly changed file size")
	}
	temporary := filepath.Join(root, "replacement.tmp")
	if err := os.WriteFile(temporary, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(temporary, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, entry); err != nil {
		t.Fatal(err)
	}

	result, err := manager.RefreshIfChanged(context.Background())
	if err != nil || !result.Changed || result.Generation != 2 {
		t.Fatalf("same-metadata replacement was not detected: %#v %v", result, err)
	}
	definition, _ := manager.Resolve("demo")
	if definition.Body != "version-two" {
		t.Fatalf("replacement body was not published: %#v", definition)
	}
}

func TestManagerRefreshDetectsOverflowCountChanges(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		writeSkillFile(t, filepath.Join(root, name+".md"), name, ModeShared, name, nil, 0, "")
	}
	limits := DefaultLimits()
	limits.MaxFiles = 2
	manager, err := NewManager(ManagerOptions{
		Sources:   []SourceFS{{Source: SourceProject, Root: root}},
		ToolNames: []string{LoadSkillToolName},
		Limits:    limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	initial := manager.Snapshot()
	if initial.Generation != 1 || len(initial.Definitions) != 2 || len(initial.Diagnostics) != 1 || !strings.Contains(initial.Diagnostics[0].Message, "3 entries") {
		t.Fatalf("unexpected initial overflow snapshot: %#v", initial)
	}
	tail := filepath.Join(root, "z.md")
	writeSkillFile(t, tail, "z", ModeShared, "z", nil, 0, "")
	result, err := manager.RefreshIfChanged(context.Background())
	if err != nil || !result.Changed || result.Generation != 2 || len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0].Message, "4 entries") {
		t.Fatalf("tail addition was not detected: %#v %v", result, err)
	}
	if len(manager.Snapshot().Definitions) != 2 {
		t.Fatal("overflow refresh changed the bounded effective set")
	}
	if err := os.Remove(tail); err != nil {
		t.Fatal(err)
	}
	result, err = manager.RefreshIfChanged(context.Background())
	if err != nil || !result.Changed || result.Generation != 3 || !strings.Contains(result.Diagnostics[0].Message, "3 entries") {
		t.Fatalf("tail deletion was not detected: %#v %v", result, err)
	}
}

func TestManagerConcurrentSnapshotResolveAndRefresh(t *testing.T) {
	root := t.TempDir()
	entry := filepath.Join(root, "demo.md")
	writeSkillFile(t, entry, "demo", ModeShared, "body", nil, 0, "")
	manager, err := NewManager(ManagerOptions{Sources: []SourceFS{{Source: SourceProject, Root: root}}, ToolNames: []string{LoadSkillToolName}})
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				snapshot := manager.Snapshot()
				definition, exists := manager.Resolve("demo")
				if snapshot.Generation == 0 || !exists || definition.Name != "demo" {
					t.Errorf("observed incomplete state: %#v %#v", snapshot, definition)
					return
				}
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		for iteration := 0; iteration < 20; iteration++ {
			_, refreshErr := manager.RefreshIfChanged(context.Background())
			if refreshErr != nil {
				t.Errorf("refresh failed: %v", refreshErr)
				return
			}
		}
	}()
	wait.Wait()
}

func TestBuiltinMetadataAndMaterialization(t *testing.T) {
	definitions, diagnosticItems, err := discover(BuiltinSource(), DefaultLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnosticItems) != 0 || !reflect.DeepEqual(definitionNames(definitions), []string{"commit", "review", "test"}) {
		t.Fatalf("unexpected builtin discovery: %#v %#v", definitions, diagnosticItems)
	}
	byName := map[string]Definition{}
	for _, definition := range definitions {
		byName[definition.Name] = definition
	}
	if byName["commit"].Mode != ModeShared || byName["commit"].History != 0 {
		t.Fatalf("invalid commit metadata: %#v", byName["commit"])
	}
	for _, name := range []string{"review", "test"} {
		if byName[name].Mode != ModeIsolated || byName[name].History != 1 {
			t.Fatalf("invalid %s metadata: %#v", name, byName[name])
		}
	}
	expectedTools := []string{"Bash", "Glob", "Grep", "Read"}
	sort.Strings(expectedTools)
	for name, definition := range byName {
		if !reflect.DeepEqual(definition.AllowedTools, expectedTools) || strings.TrimSpace(definition.Body) == "" {
			t.Fatalf("invalid builtin %s: %#v", name, definition)
		}
	}
	workflowRequirements := map[string][]string{
		"commit": {"git status", "Run the smallest relevant verification", "Stage only the intended files", "Never amend, force-push, reset, or discard"},
		"review": {"Review only; do not modify files", "highest to lowest severity", "precise file and location", "residual risk or verification gap"},
		"test":   {"identify the actual language, build system", "commands actually run", "Do not edit implementation or tests", "passed commands, failed commands"},
	}
	for name, fragments := range workflowRequirements {
		for _, fragment := range fragments {
			if !strings.Contains(byName[name].Body, fragment) {
				t.Fatalf("builtin %s SOP omitted required workflow fragment %q: %q", name, fragment, byName[name].Body)
			}
		}
	}

	root, cleanup, err := MaterializeBuiltin("review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "SKILL.md")); err != nil {
		t.Fatalf("materialized entry missing: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materialized directory was not cleaned up: %v", err)
	}
	if _, _, err := MaterializeBuiltin("missing"); err == nil {
		t.Fatal("expected unknown builtin error")
	}
}

func testDefinition(name string, source Source, body string) Definition {
	return Definition{
		Metadata: Metadata{Name: name, Description: name + " description", Mode: ModeShared},
		Body:     body, EntryPath: "/" + strings.ToLower(name) + ".md", PackageRoot: "/", Source: source,
	}
}

func catalogNames(items []CatalogItem) []string {
	names := make([]string, len(items))
	for index, item := range items {
		names[index] = item.Name
	}
	return names
}

func bumpModTime(t *testing.T, path string, seconds int) {
	t.Helper()
	stamp := time.Now().Add(time.Duration(seconds) * time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

type entryOpenCountingFS struct {
	fs.FS
	mu    sync.Mutex
	opens map[string]int
}

func newEntryOpenCountingFS(fsys fs.FS) *entryOpenCountingFS {
	return &entryOpenCountingFS{FS: fsys, opens: map[string]int{}}
}

func (f *entryOpenCountingFS) Open(name string) (fs.File, error) {
	f.mu.Lock()
	f.opens[name]++
	f.mu.Unlock()
	return f.FS.Open(name)
}

func (f *entryOpenCountingFS) OpenCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens[name]
}
