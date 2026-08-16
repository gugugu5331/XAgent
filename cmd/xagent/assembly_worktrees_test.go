package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
	"xagent/internal/memory"
	"xagent/internal/orchestrator"
	"xagent/internal/permission"
	"xagent/internal/proctree"
	"xagent/internal/redact"
	"xagent/internal/tool"
	"xagent/internal/workspace"
	"xagent/internal/worktree"
)

func TestAssemblyWorktreeGraphBuildsNarrowProductionOwners(t *testing.T) {
	options := assemblyWorktreeGraphFixture(t)
	graph, err := newAssemblyWorktreeGraph(options)
	if err != nil {
		t.Fatalf("newAssemblyWorktreeGraph: %v", err)
	}
	t.Cleanup(func() { closeAssemblyWorktreeGraphTest(t, graph) })
	if graph.manager == nil || graph.workspace == nil || graph.janitor == nil ||
		graph.owners.worktrees == nil || graph.owners.workspace == nil || graph.owners.janitor == nil {
		t.Fatalf("incomplete graph: %#v", graph)
	}
	for _, fieldName := range []string{"manager", "workspace", "janitor"} {
		field, ok := reflect.TypeOf(*graph).FieldByName(fieldName)
		if !ok || field.Type.Kind() != reflect.Interface {
			t.Fatalf("graph field %s exposes a concrete owner: %v", fieldName, field.Type)
		}
	}
	layout, err := worktree.ResolveManagedLayout(options.ProjectRoot, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{layout.Control, layout.Records, layout.Locks, filepath.Join(layout.Control, "manifests")} {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("worktree graph path unavailable/unsafe %q: info=%v err=%v", path, info, statErr)
		}
	}
}

func TestAssemblyWorktreeGraphBinderCreatesPrivateTaskRootsAndMainReadonly(t *testing.T) {
	options := assemblyWorktreeGraphFixture(t)
	graph, err := newAssemblyWorktreeGraph(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeAssemblyWorktreeGraphTest(t, graph) })
	first := bindAssemblyWorktreeRuntime(t, graph.workspace, options.ProjectRoot, "0123456789abcdef0123456789abcdef", "task-one")
	defer first.Close(context.Background())
	second := bindAssemblyWorktreeRuntime(t, graph.workspace, options.ProjectRoot, "fedcba9876543210fedcba9876543210", "task-two")
	defer second.Close(context.Background())
	if first.ScratchRoot() == second.ScratchRoot() || first.ArtifactRoot() == second.ArtifactRoot() ||
		first.ScratchRoot() == first.ArtifactRoot() {
		t.Fatalf("task writable roots collided: first=(%q,%q) second=(%q,%q)", first.ScratchRoot(), first.ArtifactRoot(), second.ScratchRoot(), second.ArtifactRoot())
	}
	if first.ScratchRoot() != filepath.Join(options.ScratchBase, first.Lease().WorkspaceID) ||
		first.ArtifactRoot() != filepath.Join(options.ArtifactBase, first.Lease().WorkspaceID) {
		t.Fatalf("task roots not derived from configured authority: scratch=%q artifact=%q", first.ScratchRoot(), first.ArtifactRoot())
	}
	for _, path := range []string{first.ScratchRoot(), first.ArtifactRoot(), second.ScratchRoot(), second.ArtifactRoot()} {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			t.Fatalf("task root is not private: path=%q info=%v err=%v", path, info, statErr)
		}
	}
	readonly := first.Protection().Readonly()
	if len(readonly) == 0 || readonly[0].Path != options.ProjectRoot {
		t.Fatalf("canonical main root is not readonly: %#v", readonly)
	}
}

func TestAssemblyWorktreeGraphBinderConservativelyRetainsFailedTaskRoots(t *testing.T) {
	mainRoot := canonicalAssemblyWorktreeTestDir(t, "main")
	scratchBase := canonicalAssemblyWorktreeTestDir(t, "scratch")
	artifactBase := canonicalAssemblyWorktreeTestDir(t, "artifact")
	workspaceID := "0123456789abcdef0123456789abcdef"
	lease := assemblyWorktreeLease(t, mainRoot, workspaceID)
	failing := &assemblyWorkspaceFactoryTestDouble{bind: func(request workspace.BindRequest) (workspace.Runtime, error) {
		if request.ScratchRoot != filepath.Join(scratchBase, workspaceID) || request.ArtifactRoot != filepath.Join(artifactBase, workspaceID) {
			t.Fatalf("unexpected derived roots: %#v", request)
		}
		return nil, errors.New("injected bind failure")
	}}
	adapter, err := newAssemblyWorktreeWorkspaceAdapter(assemblyWorktreeWorkspaceAdapterOptions{
		Factory: failing, ProjectRoot: mainRoot, ScratchBase: scratchBase, ArtifactBase: artifactBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close(context.Background())
	request := assemblyWorktreeBindRequest(t, lease, "task-rollback")
	if runtime, bindErr := adapter.Bind(context.Background(), request); runtime != nil || bindErr == nil {
		t.Fatalf("Bind failure = runtime=%T err=%v", runtime, bindErr)
	}
	for _, path := range []string{filepath.Join(scratchBase, workspaceID), filepath.Join(artifactBase, workspaceID)} {
		if info, statErr := os.Lstat(path); statErr != nil || !info.IsDir() {
			t.Fatalf("conservative rollback did not retain task metadata root: %q info=%v err=%v", path, info, statErr)
		}
	}

	secondWorkspaceID := "fedcba9876543210fedcba9876543210"
	unknown := &assemblyWorkspaceFactoryTestDouble{bind: func(request workspace.BindRequest) (workspace.Runtime, error) {
		if err := os.Remove(request.ScratchRoot); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(request.ScratchRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(request.ScratchRoot, "unknown"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		return nil, errors.New("injected replacement")
	}}
	adapter.factory = unknown
	secondRequest := assemblyWorktreeBindRequest(t, assemblyWorktreeLease(t, mainRoot, secondWorkspaceID), "task-replacement")
	if runtime, bindErr := adapter.Bind(context.Background(), secondRequest); runtime != nil || bindErr == nil {
		t.Fatalf("replacement Bind = runtime=%T err=%v", runtime, bindErr)
	}
	if contents, readErr := os.ReadFile(filepath.Join(scratchBase, secondWorkspaceID, "unknown")); readErr != nil || string(contents) != "keep" {
		t.Fatalf("rollback deleted unknown replacement: contents=%q err=%v", contents, readErr)
	}
}

func TestAssemblyWorktreeBinderReportsOwnedRuntimeCleanupFailure(t *testing.T) {
	mainRoot := canonicalAssemblyWorktreeTestDir(t, "main")
	scratchBase := canonicalAssemblyWorktreeTestDir(t, "scratch")
	artifactBase := canonicalAssemblyWorktreeTestDir(t, "artifact")
	closeFailure := errors.New("sensitive cleanup detail")
	bound := &assemblyCloseFailureRuntime{closeErr: closeFailure}
	failing := &assemblyWorkspaceFactoryTestDouble{bind: func(workspace.BindRequest) (workspace.Runtime, error) {
		return bound, errors.New("sensitive bind detail")
	}}
	adapter, err := newAssemblyWorktreeWorkspaceAdapter(assemblyWorktreeWorkspaceAdapterOptions{
		Factory: failing, ProjectRoot: mainRoot, ScratchBase: scratchBase, ArtifactBase: artifactBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })

	runtime, bindErr := adapter.Bind(context.Background(), assemblyWorktreeBindRequest(
		t, assemblyWorktreeLease(t, mainRoot, "0123456789abcdef0123456789abcdef"), "task-cleanup-failure",
	))
	if runtime != nil || bindErr == nil {
		t.Fatalf("Bind = runtime=%T err=%v", runtime, bindErr)
	}
	if got := bindErr.Error(); !strings.Contains(got, "binding failed") || !strings.Contains(got, "runtime cleanup failed") {
		t.Fatalf("combined bounded failure missing phases: %q", got)
	} else if strings.Contains(got, "sensitive") {
		t.Fatalf("combined failure leaked dependency detail: %q", got)
	}
	if got := bound.closes.Load(); got != 1 {
		t.Fatalf("owned runtime Close calls = %d, want 1", got)
	}
}

func TestAssemblyWorktreeBinderReportsMismatchRuntimeCleanupFailure(t *testing.T) {
	mainRoot := canonicalAssemblyWorktreeTestDir(t, "main")
	bound := &assemblyCloseFailureRuntime{closeErr: errors.New("sensitive cleanup detail")}
	adapter, err := newAssemblyWorktreeWorkspaceAdapter(assemblyWorktreeWorkspaceAdapterOptions{
		Factory: &assemblyWorkspaceFactoryTestDouble{bind: func(workspace.BindRequest) (workspace.Runtime, error) {
			return bound, nil
		}},
		ProjectRoot:  mainRoot,
		ScratchBase:  canonicalAssemblyWorktreeTestDir(t, "scratch"),
		ArtifactBase: canonicalAssemblyWorktreeTestDir(t, "artifact"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })
	runtime, bindErr := adapter.Bind(context.Background(), assemblyWorktreeBindRequest(
		t, assemblyWorktreeLease(t, mainRoot, "0123456789abcdef0123456789abcdef"), "task-mismatch-cleanup",
	))
	if runtime != nil || bindErr == nil {
		t.Fatalf("Bind = runtime=%T err=%v", runtime, bindErr)
	}
	if got := bindErr.Error(); !strings.Contains(got, "task roots changed") || !strings.Contains(got, "runtime cleanup failed") {
		t.Fatalf("combined bounded failure missing phases: %q", got)
	} else if strings.Contains(got, "sensitive") {
		t.Fatalf("combined failure leaked dependency detail: %q", got)
	}
	if got := bound.closes.Load(); got != 1 {
		t.Fatalf("owned runtime Close calls = %d, want 1", got)
	}
}

func TestAssemblyWorktreeBinderReportsClosingRuntimeCleanupFailure(t *testing.T) {
	options := assemblyWorktreeGraphFixture(t)
	graph, err := newAssemblyWorktreeGraph(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeAssemblyWorktreeGraphTest(t, graph) })
	productionAdapter, ok := graph.workspace.(*assemblyWorktreeWorkspaceAdapter)
	if !ok {
		t.Fatalf("workspace binder type = %T", graph.workspace)
	}
	var adapter *assemblyWorktreeWorkspaceAdapter
	var bound *assemblyCloseFailureRuntime
	factory := &assemblyWorkspaceFactoryTestDouble{bind: func(request workspace.BindRequest) (workspace.Runtime, error) {
		actual, bindErr := productionAdapter.factory.Bind(context.Background(), request)
		if bindErr != nil {
			return actual, bindErr
		}
		bound = &assemblyCloseFailureRuntime{Runtime: actual, closeErr: errors.New("sensitive cleanup detail")}
		adapter.mu.Lock()
		adapter.closed = true
		adapter.mu.Unlock()
		return bound, nil
	}}
	adapter, err = newAssemblyWorktreeWorkspaceAdapter(assemblyWorktreeWorkspaceAdapterOptions{
		Factory: factory, ProjectRoot: options.ProjectRoot,
		ScratchBase: options.ScratchBase, ArtifactBase: options.ArtifactBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })
	runtime, bindErr := adapter.Bind(context.Background(), assemblyWorktreeBindRequest(
		t, assemblyWorktreeLease(t, options.ProjectRoot, "0123456789abcdef0123456789abcdef"), "task-closing-cleanup",
	))
	if runtime != nil || bindErr == nil {
		t.Fatalf("Bind = runtime=%T err=%v", runtime, bindErr)
	}
	if got := bindErr.Error(); !strings.Contains(got, "adapter is closing") || !strings.Contains(got, "runtime cleanup failed") {
		t.Fatalf("combined bounded failure missing phases: %q", got)
	} else if strings.Contains(got, "sensitive") {
		t.Fatalf("combined failure leaked dependency detail: %q", got)
	}
	if bound == nil || bound.closes.Load() != 1 {
		t.Fatalf("owned runtime Close calls = %v, want 1", bound)
	}
}

func TestAssemblyWorktreeGraphRollbackNeverRemovesEmptyInodeSwap(t *testing.T) {
	mainRoot := canonicalAssemblyWorktreeTestDir(t, "main")
	scratchBase := canonicalAssemblyWorktreeTestDir(t, "scratch")
	artifactBase := canonicalAssemblyWorktreeTestDir(t, "artifact")
	workspaceID := "0123456789abcdef0123456789abcdef"
	scratchPath := filepath.Join(scratchBase, workspaceID)
	movedPath := filepath.Join(scratchBase, workspaceID+"-owned")
	swapped := false
	failing := &assemblyWorkspaceFactoryTestDouble{bind: func(workspace.BindRequest) (workspace.Runtime, error) {
		return nil, errors.New("injected bind failure")
	}}
	adapter, err := newAssemblyWorktreeWorkspaceAdapter(assemblyWorktreeWorkspaceAdapterOptions{
		Factory: failing, ProjectRoot: mainRoot, ScratchBase: scratchBase, ArtifactBase: artifactBase,
		BeforeTaskRootRemove: func(path string) {
			if swapped || path != scratchPath {
				return
			}
			swapped = true
			if renameErr := os.Rename(path, movedPath); renameErr != nil {
				t.Fatal(renameErr)
			}
			if mkdirErr := os.Mkdir(path, 0o700); mkdirErr != nil {
				t.Fatal(mkdirErr)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close(context.Background())
	request := assemblyWorktreeBindRequest(t, assemblyWorktreeLease(t, mainRoot, workspaceID), "task-empty-swap")
	if runtime, bindErr := adapter.Bind(context.Background(), request); runtime != nil || bindErr == nil {
		t.Fatalf("Bind failure = runtime=%T err=%v", runtime, bindErr)
	}
	if !swapped {
		t.Fatal("rollback did not reach the final would-be remove barrier")
	}
	for _, path := range []string{scratchPath, movedPath} {
		if info, statErr := os.Lstat(path); statErr != nil || !info.IsDir() {
			t.Fatalf("rollback removed an empty inode across rename swap: path=%q info=%v err=%v", path, info, statErr)
		}
	}
}

func TestAssemblyWorktreeGraphRejectsSharedOrSymlinkWritableAuthority(t *testing.T) {
	options := assemblyWorktreeGraphFixture(t)
	options.ArtifactBase = options.ScratchBase
	if graph, err := newAssemblyWorktreeGraph(options); graph != nil || err == nil {
		t.Fatalf("shared writable base accepted: graph=%T err=%v", graph, err)
	}
	options = assemblyWorktreeGraphFixture(t)
	link := filepath.Join(filepath.Dir(options.ScratchBase), "scratch-link")
	if err := os.Symlink(options.ScratchBase, link); err != nil {
		t.Fatal(err)
	}
	options.ScratchBase = link
	if graph, err := newAssemblyWorktreeGraph(options); graph != nil || err == nil {
		t.Fatalf("symlink writable base accepted: graph=%T err=%v", graph, err)
	}
}

func TestWorktreeIgnoreRuleIsPrecise(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位 Worktree Assembly 测试文件")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	contents, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("读取仓库根 .gitignore 失败: %v", err)
	}

	lines := strings.Split(strings.ReplaceAll(string(contents), "\r\n", "\n"), "\n")
	found := false
	for _, line := range lines {
		switch strings.TrimSpace(line) {
		case "/.xagent/worktrees/":
			found = true
		case "/.xagent/", ".xagent/", "/.xagent/**", ".xagent/**":
			t.Fatalf("Worktree ignore 规则扩大到了整个 .xagent: %q", line)
		}
	}
	if !found {
		t.Fatal("缺少精确的 /.xagent/worktrees/ ignore 规则")
	}
}

func TestWorktreeShutdownClosesOwnersInDependencyOrderAndContinuesAfterFailure(t *testing.T) {
	registry := newOwnershipRegistry()
	var mu sync.Mutex
	var closed []string
	secret := "private worktree shutdown detail"
	owners := []string{"worktree", "workspace", "orchestrator", "inbox", "subagent", "janitor"}
	for _, name := range owners {
		name := name
		if err := registry.register(assemblyStageOrchestration, func(context.Context) error {
			mu.Lock()
			closed = append(closed, name)
			mu.Unlock()
			if name == "inbox" {
				return errors.New(secret)
			}
			return nil
		}); err != nil {
			t.Fatalf("register %s owner: %v", name, err)
		}
	}
	if err := registry.seal(); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{owners: registry, cleanupTimeout: time.Second}
	err := runtime.Close(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("shutdown error is not safe: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), closed...)
	mu.Unlock()
	want := []string{"janitor", "subagent", "inbox", "orchestrator", "workspace", "worktree"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
}

func TestWorktreeShutdownStartsJanitorOnlyAfterOwnerRegistration(t *testing.T) {
	closeJanitor := func(context.Context) error { return nil }
	registrationFailure := errors.New("injected owner registration failure")
	started := false
	err := registerStartedOwner(
		func() error { return registrationFailure },
		func() { started = true },
	)
	if !errors.Is(err, registrationFailure) || started {
		t.Fatalf("failed registration = err=%v started=%v", err, started)
	}

	var registered ownerClose
	err = registerStartedOwner(
		func() error { registered = closeJanitor; return nil },
		func() { started = true },
	)
	if err != nil || registered == nil || !started {
		t.Fatalf("successful registration = err=%v registered=%v started=%v", err, registered != nil, started)
	}
}

func TestWorktreeShutdownConcurrentCallsCloseEveryOwnerOnce(t *testing.T) {
	registry := newOwnershipRegistry()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls [6]atomic.Int32
	for index := range calls {
		index := index
		if err := registry.register(assemblyStageOrchestration, func(context.Context) error {
			calls[index].Add(1)
			if index == len(calls)-1 {
				close(started)
				<-release
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	runtime := &Runtime{owners: registry, cleanupTimeout: time.Second}
	const closers = 16
	results := make(chan error, closers)
	for range closers {
		go func() { results <- runtime.Close(context.Background()) }()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("janitor shutdown did not start")
	}
	close(release)
	for range closers {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("concurrent shutdown: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent shutdown did not converge")
		}
	}
	for index := range calls {
		if got := calls[index].Load(); got != 1 {
			t.Fatalf("owner %d close calls = %d, want 1", index, got)
		}
	}
}

func TestWorktreeShutdownCleanupTimeoutIsStableAndStillVisitsEarlierOwners(t *testing.T) {
	registry := newOwnershipRegistry()
	earlierClosed := make(chan struct{})
	if err := registry.register(assemblyStageOrchestration, func(context.Context) error {
		close(earlierClosed)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	blockingStarted := make(chan struct{})
	releaseBlocking := make(chan struct{})
	defer close(releaseBlocking)
	if err := registry.register(assemblyStageOrchestration, func(context.Context) error {
		close(blockingStarted)
		<-releaseBlocking
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{owners: registry, cleanupTimeout: 10 * time.Millisecond}
	first := runtime.Close(context.Background())
	if !errors.Is(first, errAssemblyRollbackTimeout) {
		t.Fatalf("shutdown result = %v, want conservative timeout", first)
	}
	select {
	case <-blockingStarted:
	default:
		t.Fatal("blocking owner was not visited")
	}
	select {
	case <-earlierClosed:
	case <-time.After(time.Second):
		t.Fatal("timeout skipped an earlier owner")
	}
	second := runtime.Close(context.Background())
	if !errors.Is(second, errAssemblyRollbackTimeout) || second.Error() != first.Error() {
		t.Fatalf("repeated shutdown changed timeout result: first=%v second=%v", first, second)
	}
}

func TestOwnershipRollbackWorktreeOwnersClosesEveryRegisteredPrefixInReverse(t *testing.T) {
	ownerNames := []string{"worktree", "workspace", "orchestrator", "inbox", "subagent", "janitor"}
	for prefix := 1; prefix <= len(ownerNames); prefix++ {
		prefix := prefix
		t.Run(ownerNames[prefix-1], func(t *testing.T) {
			registry := newOwnershipRegistry()
			var closed []string
			for _, name := range ownerNames[:prefix] {
				name := name
				if err := registry.register(assemblyStageOrchestration, func(context.Context) error {
					closed = append(closed, name)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := rollbackOwnershipRegistry(context.Background(), registry, time.Second); err != nil {
				t.Fatal(err)
			}
			want := make([]string, prefix)
			for index := range prefix {
				want[index] = ownerNames[prefix-1-index]
			}
			if !reflect.DeepEqual(closed, want) {
				t.Fatalf("rollback order = %v, want %v", closed, want)
			}
		})
	}
}

func assemblyWorktreeGraphFixture(t *testing.T) assemblyWorktreeGraphOptions {
	t.Helper()
	redactor := redact.NewRuntimeRedactor()
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor: redactor, MaxItems: 16, MaxItemBytes: 1024, MaxTotalBytes: 16 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewSafeCandidateRegistry()
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	resultFactory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatal(err)
	}
	return assemblyWorktreeGraphOptions{
		ProjectRoot:  canonicalAssemblyWorktreeTestDir(t, "project"),
		ScratchBase:  canonicalAssemblyWorktreeTestDir(t, "scratch"),
		ArtifactBase: canonicalAssemblyWorktreeTestDir(t, "artifact"),
		Config: worktree.Config{
			Lifecycle: worktree.LifecycleConfig{
				RetentionTTL: time.Hour, JanitorInterval: time.Hour, GitTimeout: time.Second,
				LockTimeout: time.Second, InitTimeout: time.Second, RecoveryTimeout: time.Second,
				SettleTimeout: time.Second, JanitorTimeout: time.Second,
			},
			Limits: worktree.Limits{
				MaxActive: 2, MaxRetained: 4, MaxNameBytes: 192, MaxSegmentBytes: 64, MaxDepth: 8,
				MaxInitFiles: 32, MaxInitBytes: 1 << 20, MaxInitDepth: 8,
				MaxJanitorCandidates: 16, MaxJanitorConcurrency: 2,
			},
		},
		GitExecutable: "git", GitRunner: assemblyGitRunnerTestDouble{}, GitMaxOutputBytes: 4096,
		Clock:          time.Now,
		SourceRegistry: registry, ResultFactory: resultFactory,
		ReadCacheLimits: tool.ReadCacheLimits{MaxEntries: 8, MaxBytes: 64 << 10, MaxValueBytes: 16 << 10, MaxDependenciesPerEntry: 32},
		ExecutorTimeout: time.Second, MaxOutputBytes: 4096,
		Instructions: config.InstructionsConfig{}, Memory: memory.ManagerOptions{}, ConfigDigest: "assembly-worktree-test",
		RuntimeRedactor: redactor,
		HookSnapshot:    hook.Snapshot{}, HookEngine: hook.EngineOptions{Diagnostics: sink, Redactor: redactor},
		ProcessRunner: assemblyProcessRunnerTestDouble{},
	}
}

func canonicalAssemblyWorktreeTestDir(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(real)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(abs)
}

func bindAssemblyWorktreeRuntime(t *testing.T, binder orchestrator.WorktreeWorkspaceBinder, mainRoot, workspaceID, taskID string) workspace.Runtime {
	t.Helper()
	lease := assemblyWorktreeLease(t, mainRoot, workspaceID)
	runtime, err := binder.Bind(context.Background(), assemblyWorktreeBindRequest(t, lease, taskID))
	if err != nil {
		t.Fatalf("Bind(%s): %v", workspaceID, err)
	}
	return runtime
}

func assemblyWorktreeLease(t *testing.T, mainRoot, workspaceID string) worktree.Lease {
	t.Helper()
	layout, err := worktree.ResolveManagedLayout(mainRoot, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.WorkspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(layout.WorkspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.WorkspaceRoot, ".git"), []byte("gitdir: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return worktree.Lease{
		WorkspaceID: workspaceID, OwnerID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Root: layout.WorkspaceRoot,
		Branch: layout.Branch, BaseOID: "0123456789abcdef0123456789abcdef01234567", AcquiredAt: time.Now(),
	}
}

func assemblyWorktreeBindRequest(t *testing.T, lease worktree.Lease, taskID string) orchestrator.WorktreeWorkspaceBindRequest {
	t.Helper()
	scope, err := (&permission.Authorizer{}).NewTaskScope(permission.TaskScopeOptions{ScopeID: taskID, Mode: permission.ModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	return orchestrator.WorktreeWorkspaceBindRequest{
		TaskID: taskID, Lease: lease,
		Role:     agentrole.ResolvedRole{Definition: agentrole.Definition{Metadata: agentrole.Metadata{Isolation: agentrole.IsolationWorktree}}},
		Verifier: scope.Verifier,
	}
}

func closeAssemblyWorktreeGraphTest(t *testing.T, graph *assemblyWorktreeGraph) {
	t.Helper()
	if graph == nil {
		return
	}
	for _, closeOwner := range []ownerClose{graph.owners.janitor, graph.owners.workspace, graph.owners.worktrees} {
		if closeOwner != nil {
			if err := closeOwner(context.Background()); err != nil {
				t.Errorf("close graph owner: %v", err)
			}
		}
	}
}

type assemblyWorkspaceFactoryTestDouble struct {
	bind func(workspace.BindRequest) (workspace.Runtime, error)
}

func (f *assemblyWorkspaceFactoryTestDouble) Bind(_ context.Context, request workspace.BindRequest) (workspace.Runtime, error) {
	return f.bind(request)
}
func (*assemblyWorkspaceFactoryTestDouble) Close(context.Context) error { return nil }

type assemblyCloseFailureRuntime struct {
	workspace.Runtime
	closeErr error
	closes   atomic.Int32
}

func (r *assemblyCloseFailureRuntime) Protection() *workspace.Protection {
	if r.Runtime == nil {
		return nil
	}
	return r.Runtime.Protection()
}
func (r *assemblyCloseFailureRuntime) Close(ctx context.Context) error {
	r.closes.Add(1)
	var baseErr error
	if r.Runtime != nil {
		baseErr = r.Runtime.Close(ctx)
	}
	return errors.Join(baseErr, r.closeErr)
}

type assemblyGitRunnerTestDouble struct{}

func (assemblyGitRunnerTestDouble) Run(context.Context, worktree.GitCommand) (worktree.GitOutput, error) {
	return worktree.GitOutput{}, errors.New("assembly graph test git runner must not execute")
}

type assemblyProcessRunnerTestDouble struct{}

func (assemblyProcessRunnerTestDouble) Start(context.Context, proctree.Request) (proctree.Process, error) {
	return nil, errors.New("assembly graph test process runner must not execute")
}
