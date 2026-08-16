package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
	"xagent/internal/memory"
	"xagent/internal/proctree"
	"xagent/internal/redact"
	"xagent/internal/safefs"
	"xagent/internal/sessionctx"
	"xagent/internal/tool"
)

func TestContextBindingBuildsStrictTaskStablePreparerAndFreezesOptions(t *testing.T) {
	root := canonicalWorkspaceCompositionDir(t)
	if err := os.WriteFile(filepath.Join(root, "MEWCODE.md"), []byte("project-context-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := true
	factory, err := NewContextFactory(ContextFactoryOptions{
		Instructions: config.InstructionsConfig{Enabled: &enabled},
		Memory:       memory.ManagerOptions{UserDir: canonicalWorkspaceCompositionDir(t), Redactor: redact.NewRuntimeRedactor()},
		ConfigDigest: "config-a",
		Redactor:     redact.NewRuntimeRedactor(),
	})
	if err != nil {
		t.Fatalf("NewContextFactory: %v", err)
	}
	enabled = false
	opened, err := safefs.Bootstrap(root, safefs.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Root.Close()
	bound, err := factory.Bind(context.Background(), ContextBindRequest{
		Root: root, WorkspaceID: "0123456789abcdef0123456789abcdef", WorkingDirectory: opened.Root,
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	prepared := bound.PrepareStable(context.Background())
	found := false
	for _, section := range prepared.StableSections {
		found = found || strings.Contains(section.Content, "project-context-marker")
	}
	if !found {
		t.Fatalf("strict project instructions missing: %#v", prepared.StableSections)
	}
	concrete := bound.(*boundProjectContext)
	if concrete.root != root || concrete.workspaceID == "" || concrete.memory.ProjectCacheNamespace() == "" {
		t.Fatalf("context binding lost root/workspace identity: %#v", concrete)
	}
	if err := bound.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := concrete.memory.LoadIndex(memory.ScopeProject); !errors.Is(err, memory.ErrWorkspaceManagerClosed) {
		t.Fatalf("project memory remained usable after Close: %v", err)
	}
}

func TestContextBindingMemoryDisabledDoesNotConstructOrInjectProjectMemory(t *testing.T) {
	root := canonicalWorkspaceCompositionDir(t)
	memoryRoot := filepath.Join(root, ".xagent", "memory")
	if err := os.MkdirAll(memoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	const marker = "disabled-worktree-project-memory-marker"
	index := "# Memory Index\n\n- [" + marker + "](disabled.md) — must not be injected\n"
	if err := os.WriteFile(filepath.Join(memoryRoot, memory.IndexFileName), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}

	enabled := false
	factory, err := NewContextFactory(ContextFactoryOptions{
		Instructions: config.InstructionsConfig{}, MemoryEnabled: &enabled,
		Memory: memory.ManagerOptions{Redactor: redact.NewRuntimeRedactor()}, ConfigDigest: "memory-disabled",
		Redactor: redact.NewRuntimeRedactor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The factory must freeze the resolved flag rather than retaining the
	// caller's mutable configuration pointer.
	enabled = true
	opened, err := safefs.Bootstrap(root, safefs.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Root.Close()
	bound, err := factory.Bind(context.Background(), ContextBindRequest{
		Root: root, WorkspaceID: "0123456789abcdef0123456789abcdef", WorkingDirectory: opened.Root,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	prepared := bound.PrepareStable(context.Background())
	for _, section := range prepared.StableSections {
		if strings.Contains(section.Content, marker) {
			t.Fatalf("memory.enabled=false injected project memory: %#v", prepared.StableSections)
		}
	}
	if concrete := bound.(*boundProjectContext); concrete.memory != nil {
		t.Fatal("memory.enabled=false constructed a task project memory manager")
	}
}

func TestContextBindingSharesBorrowedUserMemoryButIsolatesProjectNamespaces(t *testing.T) {
	shared := &sharedUserMemory{index: memory.Index{Scope: memory.ScopeUser, Entries: []memory.IndexEntry{{
		ID: "shared", Title: "shared-user-memory", Scope: memory.ScopeUser, Body: "visible to both tasks",
	}}}}
	factory, err := NewContextFactory(ContextFactoryOptions{
		Instructions: config.InstructionsConfig{}, ConfigDigest: "shared-user-config", UserMemory: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	bind := func(workspaceID string) BoundContext {
		root := canonicalWorkspaceCompositionDir(t)
		opened, openErr := safefs.Bootstrap(root, safefs.Policy{})
		if openErr != nil {
			t.Fatal(openErr)
		}
		t.Cleanup(func() { _ = opened.Root.Close() })
		bound, bindErr := factory.Bind(context.Background(), ContextBindRequest{
			Root: root, WorkspaceID: workspaceID, WorkingDirectory: opened.Root,
		})
		if bindErr != nil {
			t.Fatal(bindErr)
		}
		return bound
	}
	first := bind("0123456789abcdef0123456789abcdef")
	second := bind("fedcba9876543210fedcba9876543210")
	firstConcrete := first.(*boundProjectContext)
	secondConcrete := second.(*boundProjectContext)
	if firstConcrete.memory.ProjectCacheNamespace() == secondConcrete.memory.ProjectCacheNamespace() {
		t.Fatal("different task roots shared a project memory namespace")
	}
	for _, bound := range []BoundContext{first, second} {
		prepared := bound.PrepareStable(context.Background())
		found := false
		for _, section := range prepared.StableSections {
			found = found || strings.Contains(section.Content, "shared-user-memory")
		}
		if !found {
			t.Fatalf("borrowed user memory missing: %#v", prepared.StableSections)
		}
		if closeErr := bound.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if shared.closed {
		t.Fatal("task context closed borrowed user memory")
	}
	if _, err := shared.LoadIndex(memory.ScopeUser); err != nil {
		t.Fatalf("borrowed user memory unavailable after task Close: %v", err)
	}
}

func TestContextBindingAndHookBindingRequireRuntimeAdmission(t *testing.T) {
	factory, request := newCompositionFactory(t)
	concrete := factory.(*workspaceFactory)
	contextRuntime := &recordingBoundContext{}
	hookRuntime := &recordingBoundHook{Runtime: hook.Noop()}
	concrete.contextBinder = &recordingContextBinder{bound: contextRuntime}
	hookBinder := &recordingHookBinder{bound: hookRuntime}
	concrete.hookBinder = hookBinder
	runtime, err := factory.Bind(context.Background(), request)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if hookBinder.plans == nil || hookBinder.plans != runtime.Protection().ProcessPlans() {
		t.Fatal("Hook binding did not receive the exact task Protection plan factory")
	}
	runtime.Hooks().SystemStart(context.Background())
	_ = runtime.SessionContext().PrepareStable(context.Background())
	if hookRuntime.starts != 1 || contextRuntime.prepares != 1 {
		t.Fatalf("admitted calls missing: hooks=%d context=%d", hookRuntime.starts, contextRuntime.prepares)
	}
	runtime.StopAccepting()
	decision := runtime.Hooks().BeforeTool(context.Background(), hook.ExecutionRef{}, hook.NewToolInput("call", "Read", nil))
	prepared := runtime.SessionContext().PrepareStable(context.Background())
	if !decision.IsDeny() || len(prepared.StableSections) != 0 || hookRuntime.beforeTools != 0 || contextRuntime.prepares != 1 {
		t.Fatalf("post-close admission escaped: decision=%#v prepared=%#v hookCalls=%d contextCalls=%d", decision, prepared, hookRuntime.beforeTools, contextRuntime.prepares)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackClosesContextBeforeCacheAndProtectionWhenHookBindingFails(t *testing.T) {
	factory, request := newCompositionFactory(t)
	concrete := factory.(*workspaceFactory)
	var mu sync.Mutex
	events := []string{}
	record := func(event string) { mu.Lock(); events = append(events, event); mu.Unlock() }
	contextRuntime := &recordingBoundContext{closeFn: func() { record("context-close") }}
	concrete.contextBinder = &recordingContextBinder{bound: contextRuntime, bindFn: func() { record("context-bind") }}
	hooks := &recordingHookBinder{err: errors.New("hook failed"), bindFn: func(root *safefs.Root) {
		record("hook-bind")
		concreteRoot := root
		if concreteRoot.Identity() == (safefs.Identity{}) {
			t.Error("Protection closed before Hook Bind returned")
		}
	}}
	concrete.hookBinder = hooks
	if runtime, err := factory.Bind(context.Background(), request); runtime != nil || err == nil {
		t.Fatalf("Bind hook failure = runtime=%T err=%v", runtime, err)
	}
	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	want := []string{"context-bind", "hook-bind", "context-close"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rollback order = %v, want %v", got, want)
	}
	if hooks.working == nil || hooks.working.Identity() != (safefs.Identity{}) {
		t.Fatal("Protection root remained open after rollback")
	}
}

func TestRollbackClosesPartiallyReturnedBindingExactlyOnce(t *testing.T) {
	t.Run("context and error", func(t *testing.T) {
		factory, request := newCompositionFactory(t)
		concrete := factory.(*workspaceFactory)
		closed := 0
		partial := &recordingBoundContext{closeFn: func() { closed++ }}
		concrete.contextBinder = &recordingContextBinder{bound: partial, err: errors.New("context partial failure")}
		if runtime, err := factory.Bind(context.Background(), request); runtime != nil || err == nil {
			t.Fatalf("Bind = (%T,%v), want partial context failure", runtime, err)
		}
		if closed != 1 {
			t.Fatalf("partially returned context close count = %d, want 1", closed)
		}
	})

	t.Run("hook and error", func(t *testing.T) {
		factory, request := newCompositionFactory(t)
		concrete := factory.(*workspaceFactory)
		var events []string
		contextRuntime := &recordingBoundContext{closeFn: func() { events = append(events, "context") }}
		hookRuntime := &recordingBoundHook{Runtime: hook.Noop(), closeFn: func() { events = append(events, "hook") }}
		concrete.contextBinder = &recordingContextBinder{bound: contextRuntime}
		concrete.hookBinder = &recordingHookBinder{bound: hookRuntime, err: errors.New("hook partial failure")}
		if runtime, err := factory.Bind(context.Background(), request); runtime != nil || err == nil {
			t.Fatalf("Bind = (%T,%v), want partial hook failure", runtime, err)
		}
		if got := strings.Join(events, ","); got != "hook,context" {
			t.Fatalf("partial binding cleanup order/count = %q, want hook,context", got)
		}
	})
}

func TestPartialBindingFailureJoinsCleanupErrors(t *testing.T) {
	factory, request := newCompositionFactory(t)
	concrete := factory.(*workspaceFactory)
	bindErr := errors.New("hook partial failure")
	hookCloseErr := errors.New("hook close failure")
	contextCloseErr := errors.New("context close failure")
	concrete.contextBinder = &recordingContextBinder{bound: &recordingBoundContext{closeErr: contextCloseErr}}
	concrete.hookBinder = &recordingHookBinder{
		bound: &recordingBoundHook{Runtime: hook.Noop(), closeErr: hookCloseErr},
		err:   bindErr,
	}
	runtime, err := factory.Bind(context.Background(), request)
	if runtime != nil {
		t.Fatalf("Bind runtime = %T, want nil", runtime)
	}
	for _, want := range []error{bindErr, hookCloseErr, contextCloseErr} {
		if !errors.Is(err, want) {
			t.Fatalf("Bind error %v does not include %v", err, want)
		}
	}
}

func TestHookBindingClosesBeforeContextThenProtectionAndBorrowedFactoriesSurvive(t *testing.T) {
	factory, request := newCompositionFactory(t)
	concrete := factory.(*workspaceFactory)
	var events []string
	contextRuntime := &recordingBoundContext{closeFn: func() { events = append(events, "context") }}
	hookRuntime := &recordingBoundHook{Runtime: hook.Noop(), closeFn: func() {
		events = append(events, "hook")
		if concrete.hookBinder.(*recordingHookBinder).working.Identity() == (safefs.Identity{}) {
			t.Error("Protection closed before Hook")
		}
	}}
	contextBinder := &recordingContextBinder{bound: contextRuntime}
	hookBinder := &recordingHookBinder{bound: hookRuntime}
	concrete.contextBinder = contextBinder
	concrete.hookBinder = hookBinder
	runtime, err := factory.Bind(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "hook,context" {
		t.Fatalf("close order = %v", events)
	}
	if hookBinder.closed || contextBinder.closed {
		t.Fatal("Workspace Runtime closed borrowed factories")
	}
	if hookBinder.working.Identity() != (safefs.Identity{}) {
		t.Fatal("Protection remained open after task resources")
	}
}

func canonicalWorkspaceCompositionDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(root)
}

type recordingBoundContext struct {
	prepares int
	closeFn  func()
	closeErr error
}

type sharedUserMemory struct {
	index  memory.Index
	closed bool
}

func (m *sharedUserMemory) LoadIndex(scope memory.Scope) (memory.Index, error) {
	if m.closed {
		return memory.Index{}, errors.New("shared user memory closed")
	}
	if scope != memory.ScopeUser {
		return memory.Index{}, errors.New("unexpected shared memory scope")
	}
	return m.index, nil
}
func (*sharedUserMemory) Diagnostics() []diagnostics.Diagnostic { return nil }
func (m *sharedUserMemory) Close() error {
	m.closed = true
	return nil
}

func (c *recordingBoundContext) PrepareStable(context.Context) sessionctx.PreparedContext {
	c.prepares++
	return sessionctx.PreparedContext{}
}
func (c *recordingBoundContext) Close() error {
	if c.closeFn != nil {
		c.closeFn()
	}
	return c.closeErr
}

type recordingContextBinder struct {
	bound  BoundContext
	err    error
	bindFn func()
	closed bool
}

func (b *recordingContextBinder) Bind(context.Context, ContextBindRequest) (BoundContext, error) {
	if b.bindFn != nil {
		b.bindFn()
	}
	return b.bound, b.err
}
func (b *recordingContextBinder) Close() { b.closed = true }

type recordingBoundHook struct {
	hook.Runtime
	starts      int
	beforeTools int
	closeFn     func()
	closeErr    error
}

func (h *recordingBoundHook) SystemStart(context.Context) { h.starts++ }
func (h *recordingBoundHook) BeforeTool(context.Context, hook.ExecutionRef, hook.ToolInput) hook.ToolDecision {
	h.beforeTools++
	return hook.Continue()
}
func (h *recordingBoundHook) Close(context.Context) error {
	if h.closeFn != nil {
		h.closeFn()
	}
	return h.closeErr
}

type recordingHookBinder struct {
	bound   BoundHook
	err     error
	bindFn  func(*safefs.Root)
	working *safefs.Root
	plans   proctree.ProtectionPlanFactory
	closed  bool
}

func (b *recordingHookBinder) Bind(_ context.Context, request HookBindRequest) (BoundHook, error) {
	b.working = request.WorkingDirectory
	b.plans = request.Plans
	if b.bindFn != nil {
		b.bindFn(b.working)
	}
	return b.bound, b.err
}
func (b *recordingHookBinder) Close() { b.closed = true }

func newCompositionFactory(t *testing.T) (Factory, BindRequest) {
	t.Helper()
	source, resultFactory := newFactoryToolSource(t)
	contextFactory, err := NewContextFactory(ContextFactoryOptions{
		Instructions: config.InstructionsConfig{}, Memory: memory.ManagerOptions{UserDir: canonicalWorkspaceCompositionDir(t)}, ConfigDigest: "config-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	hookFactory := newWorkspaceHookFactoryForTest(t)
	factory, err := NewFactory(FactoryOptions{
		SourceRegistry: source, ResultFactory: resultFactory,
		ReadCacheLimits: toolReadCacheLimitsForComposition(), ExecutorTimeout: time.Second, MaxOutputBytes: 4096,
		ContextFactory: contextFactory, HookFactory: hookFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture, scope := newFactoryBindFixture(t)
	return factory, fixture.withVerifier(scope.Verifier)
}

func newFactoryToolSource(t *testing.T) (*tool.Registry, *tool.ResultFactory) {
	t.Helper()
	resultFactory, err := tool.NewResultFactory(redact.NewRuntimeRedactor())
	if err != nil {
		t.Fatal(err)
	}
	source := tool.NewSafeCandidateRegistry()
	if err := source.Seal(); err != nil {
		t.Fatal(err)
	}
	return source, resultFactory
}

func toolReadCacheLimitsForComposition() tool.ReadCacheLimits {
	return tool.ReadCacheLimits{MaxEntries: 4, MaxBytes: 64 << 10, MaxValueBytes: 16 << 10, MaxDependenciesPerEntry: 32}
}
