package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/config"
	"xagent/internal/orchestrator"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tool"
	"xagent/internal/worktree"
)

func TestAssemblySubagentSourcesUseOnlyExplicitRuntimeRoots(t *testing.T) {
	t.Parallel()

	paths := RuntimePaths{ProjectRoot: t.TempDir(), UserConfigRoot: t.TempDir()}
	sources, err := assemblySubagentRoleSources(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 3 {
		t.Fatalf("role sources = %#v", sources)
	}
	want := []struct {
		source agentrole.Source
		id     string
		root   string
	}{
		{agentrole.SourceBuiltin, "builtin", "builtins"},
		{agentrole.SourceUser, "user", "agents"},
		{agentrole.SourceProject, "project", filepath.ToSlash(filepath.Join(".xagent", "agents"))},
	}
	for index := range want {
		if sources[index].Source != want[index].source || sources[index].ID != want[index].id ||
			sources[index].Root != want[index].root || sources[index].FS == nil {
			t.Fatalf("source %d = %#v, want %#v", index, sources[index], want[index])
		}
	}
}

func TestAssemblySubagentToolMetadataComesFromSealedRegistry(t *testing.T) {
	t.Parallel()

	registry, err := tool.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assemblySubagentToolMetadata(registry); err == nil {
		t.Fatal("unsealed registry exported role metadata")
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	metadata, err := assemblySubagentToolMetadata(registry)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]agentrole.ToolMetadata, len(metadata))
	for _, item := range metadata {
		byName[item.Name] = item
	}
	if !reflect.DeepEqual(byName["Read"], agentrole.ToolMetadata{
		Name: "Read", ReadOnly: true, SideEffectFree: true, ConcurrentSafe: true,
	}) {
		t.Fatalf("Read metadata = %#v", byName["Read"])
	}
	if !reflect.DeepEqual(byName["Write"], agentrole.ToolMetadata{Name: "Write"}) {
		t.Fatalf("Write metadata = %#v", byName["Write"])
	}
}

func TestAssemblySubagentBuilderFailsClosedBeforeSealedRegistry(t *testing.T) {
	t.Parallel()

	graph, err := newAssemblySubagents(context.Background(), assemblySubagentRequest{
		Registry: tool.NewSafeCandidateRegistry(),
	})
	if err == nil || graph != nil {
		t.Fatalf("unsealed/incomplete subagent graph = %#v, err=%v", graph, err)
	}
}

func TestAssemblySubagentInjectsExactWorktreeBoundary(t *testing.T) {
	options := assemblyWorktreeGraphFixture(t)
	if err := os.Mkdir(filepath.Join(options.ProjectRoot, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := worktree.NewRepositoryIdentity(options.ProjectRoot, filepath.Join(options.ProjectRoot, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	graph, err := newAssemblyWorktreeGraph(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeAssemblyWorktreeGraphTest(t, graph) })
	template := worktree.AcquireRequest{
		RepositoryRoot: options.ProjectRoot, RepositoryIdentity: identity, LogicalName: "subagent",
	}
	runnerOptions := orchestrator.SubagentRunnerOptions{}
	if err := applyAssemblySubagentWorktrees(&runnerOptions, graph, template); err != nil {
		t.Fatal(err)
	}
	if runnerOptions.WorktreeManager != graph.manager || runnerOptions.WorkspaceBinder != graph.workspace ||
		runnerOptions.WorktreeAcquireTemplate != template {
		t.Fatalf("runner received a different worktree boundary: %#v", runnerOptions)
	}

	shared := orchestrator.SubagentRunnerOptions{}
	if err := applyAssemblySubagentWorktrees(&shared, nil, worktree.AcquireRequest{}); err != nil {
		t.Fatalf("shared-only runner rejected absent optional worktree graph: %v", err)
	}
	if shared.WorktreeManager != nil || shared.WorkspaceBinder != nil || shared.WorktreeAcquireTemplate != (worktree.AcquireRequest{}) {
		t.Fatalf("shared-only runner was granted worktree authority: %#v", shared)
	}
	partial := &assemblyWorktreeGraph{manager: graph.manager}
	if err := applyAssemblySubagentWorktrees(&orchestrator.SubagentRunnerOptions{}, partial, template); err == nil {
		t.Fatal("partial worktree authority was accepted")
	}
}

func TestAssemblySubagentBuildsCanonicalPrivateWorktreeGraph(t *testing.T) {
	fixture := assemblyWorktreeGraphFixture(t)
	if err := os.Mkdir(filepath.Join(fixture.ProjectRoot, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	userConfig := canonicalAssemblyWorktreeTestDir(t, "user-config")
	userData := canonicalAssemblyWorktreeTestDir(t, "user-data")
	userCache := canonicalAssemblyWorktreeTestDir(t, "user-cache")
	limits := subagent.DefaultLimits()
	built, err := newAssemblySubagentWorktrees(assemblySubagentWorktreeRequest{
		Paths: RuntimePaths{
			ProjectRoot: fixture.ProjectRoot, UserConfigRoot: userConfig,
			UserDataRoot: userData, UserCacheRoot: userCache,
		},
		Config: config.AppConfig{
			Tool:     config.ToolConfig{TimeoutMS: 1000, MaxOutputBytes: 4096},
			Memory:   config.MemoryConfig{UserDir: "memory"},
			Subagent: config.SubagentConfig{Limits: limits, Worktree: fixture.Config},
		},
		Registry: fixture.SourceRegistry, ResultFactory: fixture.ResultFactory, Provider: startupProvider{},
		HookSnapshot: fixture.HookSnapshot, ProcessRunner: fixture.ProcessRunner,
		RuntimeRedactor: redact.NewRuntimeRedactor(), LifecycleDiagnostics: fixture.HookEngine.Diagnostics,
		CleanupTimeout: time.Second,
		GitRunner: &worktreeCapabilityGitRunner{output: worktree.GitOutput{
			Stdout: []byte(strings.Repeat("a", 40) + "\n"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if built == nil || built.graph == nil {
		t.Fatal("git project did not construct its worktree graph")
	}
	t.Cleanup(func() { closeAssemblyWorktreeGraphTest(t, built.graph) })
	identity, err := worktree.NewRepositoryIdentity(fixture.ProjectRoot, filepath.Join(fixture.ProjectRoot, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if built.template.RepositoryRoot != fixture.ProjectRoot || built.template.RepositoryIdentity != identity ||
		built.template.LogicalName != "subagent" || built.template.WorkspaceID != "" || built.template.OwnerID != "" {
		t.Fatalf("acquire template is not the canonical repository snapshot: %#v", built.template)
	}
	adapter, ok := built.graph.workspace.(*assemblyWorktreeWorkspaceAdapter)
	if !ok {
		t.Fatalf("workspace binder type = %T", built.graph.workspace)
	}
	for _, path := range []string{adapter.scratch.path, adapter.artifact.path} {
		if !strings.HasPrefix(path, userCache+string(filepath.Separator)) {
			t.Fatalf("task writable authority escaped configured cache root: %q", path)
		}
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			t.Fatalf("task writable authority is unsafe: path=%q info=%v err=%v", path, info, statErr)
		}
	}
}

func TestAssemblySubagentMemoryDisabledDoesNotInjectWorktreeProjectMemory(t *testing.T) {
	fixture := assemblyWorktreeGraphFixture(t)
	if err := os.Mkdir(filepath.Join(fixture.ProjectRoot, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	disabled := false
	built, err := newAssemblySubagentWorktrees(assemblySubagentWorktreeRequest{
		Paths: RuntimePaths{
			ProjectRoot:    fixture.ProjectRoot,
			UserConfigRoot: canonicalAssemblyWorktreeTestDir(t, "user-config"),
			UserDataRoot:   canonicalAssemblyWorktreeTestDir(t, "user-data"),
			UserCacheRoot:  canonicalAssemblyWorktreeTestDir(t, "user-cache"),
		},
		Config: config.AppConfig{
			Tool:     config.ToolConfig{TimeoutMS: 1000, MaxOutputBytes: 4096},
			Memory:   config.MemoryConfig{Enabled: &disabled},
			Subagent: config.SubagentConfig{Limits: subagent.DefaultLimits(), Worktree: fixture.Config},
		},
		Registry: fixture.SourceRegistry, ResultFactory: fixture.ResultFactory, Provider: startupProvider{},
		HookSnapshot: fixture.HookSnapshot, ProcessRunner: fixture.ProcessRunner,
		RuntimeRedactor: redact.NewRuntimeRedactor(), LifecycleDiagnostics: fixture.HookEngine.Diagnostics,
		CleanupTimeout: time.Second,
		GitRunner: &worktreeCapabilityGitRunner{output: worktree.GitOutput{
			Stdout: []byte(strings.Repeat("a", 40) + "\n"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if built == nil || built.graph == nil {
		t.Fatal("git project did not construct its worktree graph")
	}
	t.Cleanup(func() { closeAssemblyWorktreeGraphTest(t, built.graph) })

	const workspaceID = "0123456789abcdef0123456789abcdef"
	lease := assemblyWorktreeLease(t, fixture.ProjectRoot, workspaceID)
	memoryRoot := filepath.Join(lease.Root, ".xagent", "memory")
	if err := os.MkdirAll(memoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	const marker = "disabled-worktree-memory-marker"
	index := "# Memory Index\n\n- [" + marker + "](disabled.md) — must not be injected\n"
	if err := os.WriteFile(filepath.Join(memoryRoot, "MEMORY.md"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := built.graph.workspace.Bind(context.Background(), assemblyWorktreeBindRequest(t, lease, "memory-disabled-task"))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(context.Background())
	prepared := runtime.SessionContext().PrepareStable(context.Background())
	for _, section := range prepared.StableSections {
		if strings.Contains(section.Content, marker) {
			t.Fatalf("memory.enabled=false injected worktree project memory: %#v", prepared.StableSections)
		}
	}
}

func TestAssemblySubagentNonGitProjectRemainsSharedOnly(t *testing.T) {
	fixture := assemblyWorktreeGraphFixture(t)
	userCache := canonicalAssemblyWorktreeTestDir(t, "user-cache")
	built, err := newAssemblySubagentWorktrees(assemblySubagentWorktreeRequest{
		Paths: RuntimePaths{
			ProjectRoot:    fixture.ProjectRoot,
			UserConfigRoot: canonicalAssemblyWorktreeTestDir(t, "user-config"),
			UserDataRoot:   canonicalAssemblyWorktreeTestDir(t, "user-data"),
			UserCacheRoot:  userCache,
		},
		Registry: fixture.SourceRegistry, ResultFactory: fixture.ResultFactory, Provider: startupProvider{},
		ProcessRunner: fixture.ProcessRunner, RuntimeRedactor: redact.NewRuntimeRedactor(),
		LifecycleDiagnostics: fixture.HookEngine.Diagnostics, CleanupTimeout: time.Second,
	})
	if err != nil || built != nil {
		t.Fatalf("non-git shared-only graph = %T, err=%v", built, err)
	}
	if _, statErr := os.Lstat(filepath.Join(userCache, "worktrees")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("shared-only construction created worktree writable roots: %v", statErr)
	}
}

func TestWorktreeOwnershipRegisteredBeforeJanitorStart(t *testing.T) {
	source, err := os.ReadFile("assembly.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	markers := []string{
		"register(subagentWorktrees.graph.owners.worktrees)",
		"register(subagentWorktrees.graph.owners.workspace)",
		"orchestration.orchestrator.WaitIdle(waitCtx)",
		"register(subagents.closeResultInbox)",
		"register(subagents.shutdown)",
		"register(subagentWorktrees.graph.owners.janitor)",
		"subagentWorktrees.graph.janitor.Start(context.Background())",
	}
	previous := -1
	for _, marker := range markers {
		position := strings.Index(text, marker)
		if position < 0 {
			t.Fatalf("assembly owner graph is missing %q", marker)
		}
		if position <= previous {
			t.Fatalf("assembly owner/start order is invalid at %q", marker)
		}
		previous = position
	}
}

func TestConfigExampleIncludesResolvableSubagentConfiguration(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "config.example.yaml")
	partial, err := config.DecodePartial(path)
	if err != nil {
		t.Fatal(err)
	}
	if !partial.Subagent.MaxConcurrent.Set || !partial.Subagent.RoleLimits.MaxFiles.Set ||
		!partial.Subagent.BackgroundTools.Set || !partial.LLM.ModelAliases.Haiku.Set {
		t.Fatalf("config example omitted subagent presence fields: %#v", partial.Subagent)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
