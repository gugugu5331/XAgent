package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
	"xagent/internal/memory"
	"xagent/internal/permission"
	"xagent/internal/proctree"
	"xagent/internal/redact"
	"xagent/internal/safefs"
	"xagent/internal/tool"
	"xagent/internal/worktree"
)

func TestFactoryBuildsPrivateRuntimeAndProtection(t *testing.T) {
	t.Parallel()
	resultFactory, err := tool.NewResultFactory(redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatal(err)
	}
	source := tool.NewSafeCandidateRegistry()
	probe := &workspaceToolProbe{name: "Contextual", root: t.TempDir(), tracker: &workspaceToolTracker{}}
	registerWorkspaceToolProbe(t, source, probe, tool.WorkspacePolicy{Mode: tool.WorkspaceContextual}, probe)
	if err := source.Seal(); err != nil {
		t.Fatal(err)
	}
	factory := newWorkspaceFactoryForTest(t, source, resultFactory)
	request, scope := newFactoryBindFixture(t)
	runtime, err := factory.Bind(context.Background(), request.withVerifier(scope.Verifier))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	defer runtime.Close(context.Background())

	if runtime.Root() != request.Root || runtime.ScratchRoot() != request.ScratchRoot || runtime.ArtifactRoot() != request.ArtifactRoot {
		t.Fatalf("runtime roots = (%q,%q,%q), request=%#v", runtime.Root(), runtime.ScratchRoot(), runtime.ArtifactRoot(), request)
	}
	if runtime.Lease() != request.Lease {
		t.Fatal("runtime did not retain the exact worktree lease")
	}
	if runtime.Protection() == nil || runtime.Protection().ProcessPlans() == nil {
		t.Fatal("runtime protection or process protection factory is missing")
	}
	if err := runtime.Protection().Verify(); err != nil {
		t.Fatal(err)
	}
	if got := runtime.Protection().Readonly(); len(got) != len(request.ReadonlyRoots) {
		t.Fatalf("readonly roots = %d, want %d", len(got), len(request.ReadonlyRoots))
	}
}

func TestFactoryCloseStopsAllRuntimesAndRejectsNewBindings(t *testing.T) {
	t.Parallel()
	resultFactory, err := tool.NewResultFactory(redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatal(err)
	}
	source := tool.NewSafeCandidateRegistry()
	probe := &workspaceToolProbe{name: "Contextual", root: t.TempDir(), tracker: &workspaceToolTracker{}}
	registerWorkspaceToolProbe(t, source, probe, tool.WorkspacePolicy{Mode: tool.WorkspaceContextual}, probe)
	if err := source.Seal(); err != nil {
		t.Fatal(err)
	}
	factory := newWorkspaceFactoryForTest(t, source, resultFactory)
	request, scope := newFactoryBindFixture(t)
	runtime, err := factory.Bind(context.Background(), request.withVerifier(scope.Verifier))
	if err != nil {
		t.Fatal(err)
	}
	if err := factory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.RootHandle().Identity() != (safefs.Identity{}) {
		t.Fatal("Factory.Close left a runtime protection root open")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := factory.Close(canceled); err != nil {
		t.Fatalf("completed Factory.Close returned caller cancellation: %v", err)
	}
	secondRequest, secondScope := newFactoryBindFixture(t)
	if second, bindErr := factory.Bind(context.Background(), secondRequest.withVerifier(secondScope.Verifier)); bindErr == nil {
		_ = second.Close(context.Background())
		t.Fatal("closed Factory accepted a new runtime")
	}
}

func TestFactoryRejectsNonWorktreeIsolation(t *testing.T) {
	t.Parallel()
	resultFactory, err := tool.NewResultFactory(redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatal(err)
	}
	source := tool.NewSafeCandidateRegistry()
	probe := &workspaceToolProbe{name: "Contextual", root: t.TempDir(), tracker: &workspaceToolTracker{}}
	registerWorkspaceToolProbe(t, source, probe, tool.WorkspacePolicy{Mode: tool.WorkspaceContextual}, probe)
	if err := source.Seal(); err != nil {
		t.Fatal(err)
	}
	factory := newWorkspaceFactoryForTest(t, source, resultFactory)
	defer factory.Close(context.Background())
	request, scope := newFactoryBindFixture(t)
	request.Isolation = agentrole.IsolationNone
	if runtime, bindErr := factory.Bind(context.Background(), request.withVerifier(scope.Verifier)); bindErr == nil {
		_ = runtime.Close(context.Background())
		t.Fatal("worktree Factory accepted a non-isolated task")
	}
}

func TestFactoryRuntimeCloseUnregistersTaskIDWithoutAffectingReplacement(t *testing.T) {
	t.Parallel()
	resultFactory, err := tool.NewResultFactory(redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatal(err)
	}
	source := tool.NewSafeCandidateRegistry()
	probe := &workspaceToolProbe{name: "Contextual", root: t.TempDir(), tracker: &workspaceToolTracker{}}
	registerWorkspaceToolProbe(t, source, probe, tool.WorkspacePolicy{Mode: tool.WorkspaceContextual}, probe)
	if err := source.Seal(); err != nil {
		t.Fatal(err)
	}
	factory := newWorkspaceFactoryForTest(t, source, resultFactory)
	firstRequest, firstScope := newFactoryBindFixture(t)
	first, err := factory.Bind(context.Background(), firstRequest.withVerifier(firstScope.Verifier))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	secondRequest, _ := newFactoryBindFixture(t)
	secondRequest.TaskID = firstRequest.TaskID
	secondRequest.Lease.WorkspaceID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	secondRequest.Lease.Branch = "xagent/worktree/" + secondRequest.Lease.WorkspaceID
	secondScope, err := (&permission.Authorizer{}).NewTaskScope(permission.TaskScopeOptions{ScopeID: secondRequest.TaskID, Mode: permission.ModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	second, err := factory.Bind(context.Background(), secondRequest.withVerifier(secondScope.Verifier))
	if err != nil {
		t.Fatalf("Bind() did not release the completed TaskID: %v", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	callContext, release, err := second.BeginCall(context.Background())
	if err != nil || callContext.Err() != nil {
		t.Fatalf("old runtime close affected replacement: ctx=%v err=%v", callContext.Err(), err)
	}
	release()
	if second.RootHandle().Identity() == (safefs.Identity{}) {
		t.Fatal("replacement runtime was closed by the old registration")
	}
	if err := factory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if second.RootHandle().Identity() != (safefs.Identity{}) {
		t.Fatal("Factory.Close did not close the replacement runtime")
	}
}

func TestFactoryCloseConcurrentWithRuntimeCloseDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	resultFactory, err := tool.NewResultFactory(redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatal(err)
	}
	source := tool.NewSafeCandidateRegistry()
	probe := &workspaceToolProbe{name: "Contextual", root: t.TempDir(), tracker: &workspaceToolTracker{}}
	registerWorkspaceToolProbe(t, source, probe, tool.WorkspacePolicy{Mode: tool.WorkspaceContextual}, probe)
	if err := source.Seal(); err != nil {
		t.Fatal(err)
	}
	factory := newWorkspaceFactoryForTest(t, source, resultFactory)
	request, scope := newFactoryBindFixture(t)
	runtime, err := factory.Bind(context.Background(), request.withVerifier(scope.Verifier))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 2)
	go func() {
		_ = runtime.Close(context.Background())
		done <- struct{}{}
	}()
	go func() {
		_ = factory.Close(context.Background())
		done <- struct{}{}
	}()
	for range 2 {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent Runtime/Factory Close deadlocked")
		}
	}
}

func TestFactoryCloseCallerDeadlineDoesNotSettleBeforeActiveRuntime(t *testing.T) {
	t.Parallel()
	resultFactory, err := tool.NewResultFactory(redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatal(err)
	}
	source := tool.NewSafeCandidateRegistry()
	probe := &workspaceToolProbe{name: "Contextual", root: t.TempDir(), tracker: &workspaceToolTracker{}}
	registerWorkspaceToolProbe(t, source, probe, tool.WorkspacePolicy{Mode: tool.WorkspaceContextual}, probe)
	if err := source.Seal(); err != nil {
		t.Fatal(err)
	}
	factory := newWorkspaceFactoryForTest(t, source, resultFactory)
	request, scope := newFactoryBindFixture(t)
	runtime, err := factory.Bind(context.Background(), request.withVerifier(scope.Verifier))
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := runtime.BeginCall(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if closeErr := factory.Close(short); !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("short Factory.Close = %v, want deadline", closeErr)
	}
	if runtime.RootHandle().Identity() == (safefs.Identity{}) {
		t.Fatal("Factory settled Protection while a Runtime call was active")
	}
	result := make(chan error, 1)
	go func() { result <- factory.Close(context.Background()) }()
	select {
	case closeErr := <-result:
		t.Fatalf("second Factory.Close returned before active release: %v", closeErr)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case closeErr := <-result:
		if closeErr != nil {
			t.Fatalf("settled Factory.Close = %v", closeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Factory owner cleanup did not settle")
	}
	if runtime.RootHandle().Identity() != (safefs.Identity{}) {
		t.Fatal("Factory owner cleanup left Protection open")
	}
}

func TestFactoryRejectsMismatchedLeaseBeforeProjectBinding(t *testing.T) {
	source, resultFactory := newFactoryToolSource(t)
	factory := newWorkspaceFactoryForTest(t, source, resultFactory).(*workspaceFactory)
	defer factory.Close(context.Background())
	called := 0
	factory.contextBinder = &recordingContextBinder{
		err: errors.New("project binding must not run"),
		bindFn: func() {
			called++
		},
	}
	fixture, scope := newFactoryBindFixture(t)
	request := fixture.withVerifier(scope.Verifier)
	request.Lease.Root = filepath.Join(filepath.Dir(request.Root), "different-worktree")
	if runtime, err := factory.Bind(context.Background(), request); err == nil || runtime != nil {
		t.Fatalf("Bind() = (%T,%v), want invalid lease failure", runtime, err)
	}
	if called != 0 {
		t.Fatalf("project binding ran %d time(s) before lease/root validation", called)
	}
	factory.mu.Lock()
	_, registered := factory.runtimes[request.TaskID]
	active := factory.activeBind
	factory.mu.Unlock()
	if registered || active != 0 {
		t.Fatalf("invalid Bind left registration: registered=%t active=%d", registered, active)
	}
}

func newWorkspaceFactoryForTest(t *testing.T, source *tool.Registry, resultFactory *tool.ResultFactory) Factory {
	t.Helper()
	contextFactory, err := NewContextFactory(ContextFactoryOptions{
		Instructions: config.InstructionsConfig{}, Memory: memory.ManagerOptions{}, ConfigDigest: "workspace-factory-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	hookFactory := newWorkspaceHookFactoryForTest(t)
	factory, err := NewFactory(FactoryOptions{
		SourceRegistry: source,
		ResultFactory:  resultFactory,
		ReadCacheLimits: tool.ReadCacheLimits{
			MaxEntries: 4, MaxBytes: 64 << 10, MaxValueBytes: 16 << 10, MaxDependenciesPerEntry: 32,
		},
		ExecutorTimeout: time.Second,
		MaxOutputBytes:  4096,
		ContextFactory:  contextFactory,
		HookFactory:     hookFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

type factoryBindFixture struct {
	BindRequest
}

func (fixture factoryBindFixture) withVerifier(verifier permission.TicketVerifier) BindRequest {
	request := fixture.BindRequest
	request.Verifier = verifier
	return request
}

func newFactoryBindFixture(t *testing.T) (factoryBindFixture, permission.TaskScope) {
	t.Helper()
	base := t.TempDir()
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	working := makeDirectory(t, base, "worktree")
	if err := os.WriteFile(filepath.Join(working, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := makeDirectory(t, base, "scratch")
	artifact := makeDirectory(t, base, "artifact")
	main := makeDirectory(t, base, "main")
	workspaceID := "0123456789abcdef0123456789abcdef"
	taskID := "task-" + filepath.Base(base)
	scope, err := (&permission.Authorizer{}).NewTaskScope(permission.TaskScopeOptions{ScopeID: taskID, Mode: permission.ModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	return factoryBindFixture{BindRequest: BindRequest{
		TaskID:       taskID,
		Isolation:    agentrole.IsolationWorktree,
		Root:         working,
		ScratchRoot:  scratch,
		ArtifactRoot: artifact,
		ReadonlyRoots: []string{
			main,
		},
		Lease: worktree.Lease{
			WorkspaceID: workspaceID,
			OwnerID:     "fedcba9876543210fedcba9876543210",
			Root:        working,
			Branch:      "xagent/worktree/" + workspaceID,
			BaseOID:     "0123456789abcdef0123456789abcdef01234567",
			AcquiredAt:  time.Now(),
		},
	}}, scope
}

func newWorkspaceHookFactoryForTest(t *testing.T) *hook.WorkspaceFactory {
	t.Helper()
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor: redact.NewRuntimeRedactor(), MaxItems: 8, MaxItemBytes: 512, MaxTotalBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := hook.NewWorkspaceFactory(hook.WorkspaceFactoryOptions{
		Snapshot: hook.Snapshot{},
		Engine:   hook.EngineOptions{Diagnostics: sink},
		Runner:   workspaceHookRunnerForTest{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

type workspaceHookRunnerForTest struct{}

func (workspaceHookRunnerForTest) Start(context.Context, proctree.Request) (proctree.Process, error) {
	return nil, errors.New("workspace hook test runner must not execute")
}
