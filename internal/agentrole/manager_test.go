package agentrole

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

func TestManagerAppliesOverrideOrderAndIgnoresInvalidHigherCandidate(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	plugin := newTestRoleProvider("plugin-a")
	plugin.load = func(context.Context, ProviderLoadOptions) ([]Candidate, error) {
		return []Candidate{validTestCandidate(redactor, "shared", "plugin", "plugin.md")}, nil
	}
	plugins, err := NewProviderRegistry(4, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := plugins.Register(plugin); err != nil {
		t.Fatal(err)
	}
	if err := plugins.Seal(); err != nil {
		t.Fatal(err)
	}
	builtin := roleMapFS("builtin/shared.md", "shared", "builtin")
	user := roleMapFS("user/shared.md", "shared", "user")
	project := fstest.MapFS{
		"project/shared.md": {Data: []byte("---\nname: shared\ndescription: invalid project\nunknown: true\n---\nproject\n")},
	}
	manager, err := NewManager(context.Background(), ManagerOptions{
		Sources: []FileSource{
			{Source: SourceProject, ID: "project", FS: project, Root: "project"},
			{Source: SourceUser, ID: "user", FS: user, Root: "user"},
			{Source: SourceBuiltin, ID: "builtin", FS: builtin, Root: "builtin"},
		},
		Plugins:  plugins,
		Tools:    testToolMetadata(),
		Models:   testModelCatalog(t),
		Limits:   DefaultLimits(),
		Redactor: redactor,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	resolved, ok := manager.Resolve(" SHARED ")
	if !ok {
		t.Fatal("shared role not resolved")
	}
	if resolved.Generation != 1 || resolved.Definition.Source != SourceUser || resolved.Definition.Instructions.Text() != "user" {
		t.Fatalf("resolved role = %#v", resolved)
	}
	snapshot := manager.Snapshot()
	if snapshot.Generation != 1 || len(snapshot.Catalog) != 1 || len(snapshot.Diagnostics) == 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestManagerAppliesAllFourSourceTiersInStrictOverrideOrder(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	plugin := newTestRoleProvider("plugin-a")
	plugin.load = func(context.Context, ProviderLoadOptions) ([]Candidate, error) {
		return []Candidate{
			validTestCandidate(redactor, "all-tiers", "plugin", "plugin/all.md"),
			validTestCandidate(redactor, "through-user", "plugin", "plugin/user.md"),
			validTestCandidate(redactor, "through-builtin", "plugin", "plugin/builtin.md"),
			validTestCandidate(redactor, "plugin-only", "plugin", "plugin/only.md"),
		}, nil
	}
	plugins, err := NewProviderRegistry(4, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := plugins.Register(plugin); err != nil {
		t.Fatal(err)
	}
	if err := plugins.Seal(); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(context.Background(), ManagerOptions{
		Sources: []FileSource{
			{Source: SourceProject, ID: "project", FS: fstest.MapFS{
				"project/all.md": {Data: roleMarkdown("all-tiers", "project")},
			}, Root: "project"},
			{Source: SourceUser, ID: "user", FS: fstest.MapFS{
				"user/all.md":  {Data: roleMarkdown("all-tiers", "user")},
				"user/user.md": {Data: roleMarkdown("through-user", "user")},
			}, Root: "user"},
			{Source: SourceBuiltin, ID: "builtin", FS: fstest.MapFS{
				"builtin/all.md":     {Data: roleMarkdown("all-tiers", "builtin")},
				"builtin/user.md":    {Data: roleMarkdown("through-user", "builtin")},
				"builtin/builtin.md": {Data: roleMarkdown("through-builtin", "builtin")},
			}, Root: "builtin"},
		},
		Plugins:  plugins,
		Tools:    testToolMetadata(),
		Models:   testModelCatalog(t),
		Limits:   DefaultLimits(),
		Redactor: redactor,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	for name, want := range map[string]struct {
		source Source
		body   string
	}{
		"all-tiers":       {source: SourceProject, body: "project"},
		"through-user":    {source: SourceUser, body: "user"},
		"through-builtin": {source: SourceBuiltin, body: "builtin"},
		"plugin-only":     {source: SourcePlugin, body: "plugin"},
	} {
		resolved, ok := manager.Resolve(name)
		if !ok || resolved.Definition.Source != want.source || resolved.Definition.Instructions.Text() != want.body {
			t.Errorf("Resolve(%q) = %#v, want source=%q body=%q", name, resolved, want.source, want.body)
		}
	}
}

func TestManagerRejectsSameTierDuplicateAcrossSourceBoundaries(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	for _, source := range []Source{SourceBuiltin, SourceUser, SourceProject} {
		t.Run(string(source)+" file sources", func(t *testing.T) {
			_, err := NewManager(context.Background(), ManagerOptions{
				Sources: []FileSource{
					{Source: source, ID: "source-a", FS: roleMapFS("agents-a/one.md", "duplicate", "one"), Root: "agents-a"},
					{Source: source, ID: "source-b", FS: roleMapFS("agents-b/two.md", "duplicate", "two"), Root: "agents-b"},
				},
				Tools: testToolMetadata(), Models: testModelCatalog(t), Limits: DefaultLimits(), Redactor: redactor,
			})
			requireRoleSourceConflict(t, err)
		})
	}

	t.Run("plugin providers", func(t *testing.T) {
		plugins, err := NewProviderRegistry(2, 64)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"plugin-a", "plugin-b"} {
			provider := newTestRoleProvider(id)
			provider.load = func(context.Context, ProviderLoadOptions) ([]Candidate, error) {
				return []Candidate{validTestCandidate(redactor, "duplicate", id, id+".md")}, nil
			}
			if err := plugins.Register(provider); err != nil {
				t.Fatal(err)
			}
		}
		if err := plugins.Seal(); err != nil {
			t.Fatal(err)
		}
		_, err = NewManager(context.Background(), ManagerOptions{
			Plugins: plugins, Tools: testToolMetadata(), Models: testModelCatalog(t), Limits: DefaultLimits(), Redactor: redactor,
		})
		requireRoleSourceConflict(t, err)
	})
}

func requireRoleSourceConflict(t *testing.T, err error) {
	t.Helper()
	var safeErr *diagnostics.SafeError
	if !errors.As(err, &safeErr) || safeErr.Code != "role_source_conflict" {
		t.Fatalf("same-tier duplicate error = %T %#v, want role_source_conflict", err, err)
	}
}

func TestManagerRefreshPublishesAtomicallyAndKeepsOldSnapshotOnConflict(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	files := roleMapFS("agents/one.md", "worker", "version one")
	manager, err := NewManager(context.Background(), ManagerOptions{
		Sources:  []FileSource{{Source: SourceProject, ID: "project", FS: files, Root: "agents"}},
		Tools:    testToolMetadata(),
		Models:   testModelCatalog(t),
		Limits:   DefaultLimits(),
		Redactor: redactor,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	unchanged, err := manager.Refresh(context.Background())
	if err != nil || !unchanged.Published || unchanged.Changed || unchanged.Generation != 1 {
		t.Fatalf("unchanged refresh = %#v, %v", unchanged, err)
	}
	files["agents/one.md"] = &fstest.MapFile{Data: roleMarkdown("worker", "version two")}
	changed, err := manager.Refresh(context.Background())
	if err != nil || !changed.Published || !changed.Changed || changed.Generation != 2 {
		t.Fatalf("changed refresh = %#v, %v", changed, err)
	}
	files["agents/two.md"] = &fstest.MapFile{Data: roleMarkdown("worker", "conflict")}
	rejected, err := manager.Refresh(context.Background())
	if err != nil || rejected.Published || rejected.Changed || rejected.Error == nil || rejected.Generation != 2 {
		t.Fatalf("rejected refresh = %#v, %v", rejected, err)
	}
	resolved, ok := manager.Resolve("worker")
	if !ok || resolved.Generation != 2 || resolved.Definition.Instructions.Text() != "version two" {
		t.Fatalf("old snapshot was replaced: %#v", resolved)
	}
}

func TestSnapshotAndResolvedRoleAreDeepCopies(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	files := fstest.MapFS{
		"agents/worker.md": {Data: []byte("---\nname: worker\ndescription: Worker\nallowed_tools: [Read]\ndenied_tools: []\nmax_iterations: 2\n---\nDo work.\n")},
	}
	manager, err := NewManager(context.Background(), ManagerOptions{
		Sources:  []FileSource{{Source: SourceProject, ID: "project", FS: files, Root: "agents"}},
		Tools:    testToolMetadata(),
		Models:   testModelCatalog(t),
		Limits:   DefaultLimits(),
		Redactor: redactor,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	definition := snapshot.Definitions["worker"]
	definition.ToolAllow[0] = "Write"
	definition.ToolDeny = append(definition.ToolDeny, "Read")
	*definition.MaxIterations = 99
	snapshot.Definitions["worker"] = definition
	snapshot.Catalog[0].Name = "mutated"
	snapshot.Tools[0].Name = "mutated"
	snapshot.Models[0].Concrete = "mutated"

	resolved, ok := manager.Resolve("worker")
	if !ok || resolved.Definition.ToolAllow[0] != "Read" || *resolved.Definition.MaxIterations != 2 {
		t.Fatalf("manager storage mutated through snapshot: %#v", resolved)
	}
	resolved.Definition.ToolAllow[0] = "again"
	if next, _ := manager.Resolve("worker"); next.Definition.ToolAllow[0] != "Read" {
		t.Fatal("Resolve returned mutable manager storage")
	}
}

func TestResolvedRoleIsolationIsFrozenUntilRefresh(t *testing.T) {
	files := fstest.MapFS{
		"agents/worker.md": {Data: []byte("---\nname: worker\ndescription: Worker\nisolation: worktree\n---\nDo work.\n")},
	}
	manager, err := NewManager(context.Background(), ManagerOptions{
		Sources: []FileSource{{Source: SourceProject, ID: "project", FS: files, Root: "agents"}},
		Tools:   testToolMetadata(), Models: testModelCatalog(t), Limits: DefaultLimits(), Redactor: redact.NewRuntimeRedactor(),
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	files["agents/worker.md"].Data = roleMarkdown("worker", "Do shared work.")
	resolved, ok := manager.Resolve("worker")
	if !ok || resolved.Definition.Isolation != IsolationWorktree {
		t.Fatalf("frozen role changed with source file: %#v", resolved)
	}
}

func TestManagerRejectsProviderProtocolFailureWithoutPublishing(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	provider := newTestRoleProvider("bad-provider")
	provider.load = func(context.Context, ProviderLoadOptions) ([]Candidate, error) {
		return []Candidate{validTestCandidate(redactor, "partial", "partial", "partial.md")}, errors.New("secret plugin failure")
	}
	plugins, _ := NewProviderRegistry(2, 64)
	_ = plugins.Register(provider)
	_ = plugins.Seal()
	_, err := NewManager(context.Background(), ManagerOptions{
		Plugins: plugins, Tools: testToolMetadata(), Models: testModelCatalog(t), Limits: DefaultLimits(), Redactor: redactor,
	})
	var safeErr *diagnostics.SafeError
	if !errors.As(err, &safeErr) || safeErr.Code != string(ErrProviderFailed) || safeErr.Message.Text() == "secret plugin failure" {
		t.Fatalf("NewManager() error = %T %#v", err, err)
	}
}

func TestManagerContainsProviderPanicAndOversizedCandidate(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	for _, test := range []struct {
		name     string
		load     func(context.Context, ProviderLoadOptions) ([]Candidate, error)
		wantCode ErrorCode
	}{
		{
			name: "panic",
			load: func(context.Context, ProviderLoadOptions) ([]Candidate, error) {
				panic("provider secret")
			},
			wantCode: ErrProviderFailed,
		},
		{
			name: "oversized tool list",
			load: func(context.Context, ProviderLoadOptions) ([]Candidate, error) {
				candidate := validTestCandidate(redactor, "oversized", "body", "oversized.md")
				candidate.ToolAllow = make([]string, DefaultLimits().MaxToolNames+1)
				return []Candidate{candidate}, nil
			},
			wantCode: ErrProviderLimit,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := newTestRoleProvider("provider")
			provider.load = test.load
			plugins, _ := NewProviderRegistry(1, 64)
			_ = plugins.Register(provider)
			_ = plugins.Seal()
			_, err := NewManager(context.Background(), ManagerOptions{
				Plugins: plugins, Tools: testToolMetadata(), Models: testModelCatalog(t), Limits: DefaultLimits(), Redactor: redactor,
			})
			var safeErr *diagnostics.SafeError
			if !errors.As(err, &safeErr) || safeErr.Code != string(test.wantCode) || strings.Contains(safeErr.Message.Text(), "secret") {
				t.Fatalf("NewManager() error = %T %#v", err, err)
			}
		})
	}
}

func TestManagerRejectsSameSourceDuplicateOnInitialLoad(t *testing.T) {
	files := fstest.MapFS{
		"agents/one.md": {Data: roleMarkdown("duplicate", "one")},
		"agents/two.md": {Data: roleMarkdown("duplicate", "two")},
	}
	_, err := NewManager(context.Background(), ManagerOptions{
		Sources: []FileSource{{Source: SourceProject, ID: "project", FS: files, Root: "agents"}}, Tools: testToolMetadata(),
		Models: testModelCatalog(t), Limits: DefaultLimits(), Redactor: redact.NewRuntimeRedactor(),
	})
	if err == nil {
		t.Fatal("same-source duplicate role was accepted")
	}
}

func TestManagerTruncatesDiagnosticsWithStableSentinel(t *testing.T) {
	files := fstest.MapFS{
		"agents/one.md": {Data: []byte("---\nname: one\ndescription: One\nunknown: true\n---\none\n")},
		"agents/two.md": {Data: []byte("---\nname: two\ndescription: Two\nunknown: true\n---\ntwo\n")},
	}
	limits := DefaultLimits()
	limits.MaxDiagnostics = 1
	manager, err := NewManager(context.Background(), ManagerOptions{
		Sources: []FileSource{{Source: SourceProject, ID: "project", FS: files, Root: "agents"}}, Tools: testToolMetadata(),
		Models: testModelCatalog(t), Limits: limits, Redactor: redact.NewRuntimeRedactor(),
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	snapshot := manager.Snapshot()
	if len(snapshot.Diagnostics) != 1 || snapshot.Diagnostics[0].Code != "role_diagnostics_truncated" || snapshot.DiagnosticsDropped != 2 {
		t.Fatalf("diagnostic truncation = %#v, dropped=%d", snapshot.Diagnostics, snapshot.DiagnosticsDropped)
	}
}

func validTestCandidate(redactor *redact.RuntimeRedactor, name, instructions, origin string) Candidate {
	return Candidate{
		Metadata: Metadata{
			Name: name, Description: redactor.Redact(name + " description"), Model: ModelInherit, PermissionMode: PermissionInherit,
		},
		Instructions: redactor.Redact(instructions),
		Origin:       redactor.Redact(origin),
		Valid:        true,
	}
}

func roleMapFS(path, name, body string) fstest.MapFS {
	return fstest.MapFS{path: {Data: roleMarkdown(name, body)}}
}

func roleMarkdown(name, body string) []byte {
	return []byte("---\nname: " + name + "\ndescription: " + name + " description\n---\n" + body + "\n")
}

func testToolMetadata() []ToolMetadata {
	return []ToolMetadata{
		{Name: "Read", ReadOnly: true, SideEffectFree: true, ConcurrentSafe: true},
		{Name: "Write"},
	}
}

func testModelCatalog(t *testing.T) ModelCatalog {
	t.Helper()
	catalog, err := NewModelCatalog(ModelCatalogOptions{
		ProviderID: "test", DefaultModel: "default-model", MaxModelBytes: 256, Validate: func(string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
