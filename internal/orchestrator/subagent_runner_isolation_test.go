package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/conversation"
	"xagent/internal/hook"
	"xagent/internal/prompt"
	"xagent/internal/provider"
	"xagent/internal/safefs"
	"xagent/internal/sessionctx"
	"xagent/internal/subagent"
	"xagent/internal/tool"
	"xagent/internal/workspace"
	"xagent/internal/worktree"
)

type runnerIsolationManager struct {
	calls          []string
	acquire        func(worktree.AcquireRequest) (worktree.Lease, error)
	settleRequests []worktree.SettleRequest
	settleErr      error
	releaseErr     error
}

func (manager *runnerIsolationManager) Acquire(_ context.Context, request worktree.AcquireRequest) (worktree.Lease, error) {
	manager.calls = append(manager.calls, "acquire")
	return manager.acquire(request)
}

func (manager *runnerIsolationManager) Settle(_ context.Context, _ worktree.Lease, request worktree.SettleRequest) (worktree.Settlement, error) {
	manager.calls = append(manager.calls, "settle")
	manager.settleRequests = append(manager.settleRequests, request)
	return worktree.Settlement{State: worktree.SettlementRetained}, manager.settleErr
}

func (manager *runnerIsolationManager) Release(_ context.Context, _ worktree.Lease) error {
	manager.calls = append(manager.calls, "release")
	return manager.releaseErr
}

type runnerIsolationBinder struct {
	manager   *runnerIsolationManager
	calls     []WorktreeWorkspaceBindRequest
	runtime   workspace.Runtime
	err       error
	configure func(WorktreeWorkspaceBindRequest) error
}

func (binder *runnerIsolationBinder) Bind(_ context.Context, request WorktreeWorkspaceBindRequest) (workspace.Runtime, error) {
	binder.manager.calls = append(binder.manager.calls, "bind")
	request.Role = agentrole.ResolvedRole{Generation: request.Role.Generation, Definition: request.Role.Definition.Clone()}
	binder.calls = append(binder.calls, request)
	if binder.err == nil && binder.runtime != nil && binder.configure != nil {
		if err := binder.configure(request); err != nil {
			return binder.runtime, err
		}
	}
	return binder.runtime, binder.err
}

type runnerIsolationRuntime struct {
	mu               sync.Mutex
	manager          *runnerIsolationManager
	lease            worktree.Lease
	closed           int
	closeErr         error
	hooks            hook.Runtime
	stable           sessionctx.StablePreparer
	rootHandle       *safefs.Root
	registry         *tool.Registry
	tools            *tool.Executor
	scoped           *tool.ScopedExecutor
	capabilities     *tool.CapabilitySwitch
	cache            *tool.ReadCache
	enforceAdmission bool
	activeCalls      int
	beginCalls       int
	violations       int
}

func (runtime *runnerIsolationRuntime) touch() {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.enforceAdmission && runtime.activeCalls == 0 {
		runtime.violations++
	}
}

func (runtime *runnerIsolationRuntime) Root() string {
	runtime.touch()
	return runtime.lease.Root
}
func (*runnerIsolationRuntime) ScratchRoot() string  { return "" }
func (*runnerIsolationRuntime) ArtifactRoot() string { return "" }
func (runtime *runnerIsolationRuntime) RootHandle() *safefs.Root {
	runtime.touch()
	return runtime.rootHandle
}
func (runtime *runnerIsolationRuntime) Lease() worktree.Lease {
	runtime.touch()
	return runtime.lease
}
func (*runnerIsolationRuntime) Protection() *workspace.Protection { return nil }
func (runtime *runnerIsolationRuntime) Registry() *tool.Registry {
	runtime.touch()
	return runtime.registry
}
func (runtime *runnerIsolationRuntime) ToolExecutor() *tool.Executor {
	runtime.touch()
	return runtime.tools
}
func (runtime *runnerIsolationRuntime) Executor() *tool.ScopedExecutor {
	runtime.touch()
	return runtime.scoped
}
func (runtime *runnerIsolationRuntime) Capabilities() *tool.CapabilitySwitch {
	runtime.touch()
	return runtime.capabilities
}
func (*runnerIsolationRuntime) ToolBindingReport() tool.WorkspaceBindingReport {
	return tool.WorkspaceBindingReport{}
}
func (runtime *runnerIsolationRuntime) Hooks() hook.Runtime {
	runtime.touch()
	if runtime.hooks != nil {
		return runtime.hooks
	}
	return hook.Noop()
}
func (runtime *runnerIsolationRuntime) SessionContext() sessionctx.StablePreparer {
	runtime.touch()
	return runtime.stable
}
func (*runnerIsolationRuntime) Context() context.Context { return context.Background() }
func (*runnerIsolationRuntime) StopAccepting()           {}
func (runtime *runnerIsolationRuntime) BeginCall(ctx context.Context) (context.Context, func(), error) {
	runtime.mu.Lock()
	runtime.beginCalls++
	runtime.activeCalls++
	runtime.mu.Unlock()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			runtime.mu.Lock()
			runtime.activeCalls--
			runtime.mu.Unlock()
		})
	}, nil
}
func (runtime *runnerIsolationRuntime) Close(context.Context) error {
	runtime.manager.calls = append(runtime.manager.calls, "runtime-close")
	runtime.closed++
	if runtime.closeErr == nil {
		if runtime.cache != nil {
			runtime.cache.Close()
		}
		if runtime.rootHandle != nil {
			_ = runtime.rootHandle.Close()
		}
	}
	return runtime.closeErr
}

func (runtime *runnerIsolationRuntime) admissionSnapshot() (begins, active, violations int) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.beginCalls, runtime.activeCalls, runtime.violations
}

type runnerIsolationRoleManager struct {
	role agentrole.ResolvedRole
}

func (*runnerIsolationRoleManager) Snapshot() agentrole.Snapshot { return agentrole.Snapshot{} }
func (manager *runnerIsolationRoleManager) Resolve(name string) (agentrole.ResolvedRole, bool) {
	if name != manager.role.Definition.Name {
		return agentrole.ResolvedRole{}, false
	}
	// Deliberately return shallow shared storage. The runner owns the freeze.
	return manager.role, true
}
func (*runnerIsolationRoleManager) Refresh(context.Context) (agentrole.RefreshResult, error) {
	return agentrole.RefreshResult{}, errors.New("unused")
}

type runnerIsolationStableContext struct {
	calls    int
	sections []prompt.Section
}

func (stable *runnerIsolationStableContext) PrepareStable(context.Context) sessionctx.PreparedContext {
	stable.calls++
	return sessionctx.PreparedContext{StableSections: append([]prompt.Section(nil), stable.sections...)}
}

func TestRunnerIsolationSharedPathNeverTouchesWorktreeOrWorkspace(t *testing.T) {
	manager := &runnerIsolationManager{acquire: func(worktree.AcquireRequest) (worktree.Lease, error) {
		return worktree.Lease{}, errors.New("shared path invoked Acquire")
	}}
	binder := &runnerIsolationBinder{manager: manager, err: errors.New("shared path invoked Bind")}
	factory, _ := newRunnerIsolationFactory(t, manager, binder, false)

	prepared, err := factory.Prepare(context.Background(), "shared-task", runnerIsolationInput())
	if err != nil {
		t.Fatalf("shared Prepare() changed behavior: %v", err)
	}
	if len(manager.calls) != 0 || len(binder.calls) != 0 {
		t.Fatalf("shared Prepare() touched isolation dependencies: manager=%v bind=%d", manager.calls, len(binder.calls))
	}
	prepared.(*preparedSubagentTask).runtime.close(errors.New("test cleanup"))
}

func TestRunnerIsolationAcquiresThenBindsFrozenRole(t *testing.T) {
	var captured worktree.AcquireRequest
	manager := &runnerIsolationManager{}
	runtime := &runnerIsolationRuntime{manager: manager}
	binder := &runnerIsolationBinder{manager: manager, runtime: runtime}
	factory, roles := newRunnerIsolationFactory(t, manager, binder, true)
	manager.acquire = func(request worktree.AcquireRequest) (worktree.Lease, error) {
		captured = request
		roles.role.Definition.ToolAllow[0] = "mutated-after-resolve"
		*roles.role.Definition.MaxIterations = 0
		lease := runnerIsolationLease(t, request)
		runtime.lease = lease
		return lease, nil
	}

	prepared, err := factory.Prepare(context.Background(), "isolated-task", runnerIsolationInput())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manager.calls, []string{"acquire", "bind"}) {
		t.Fatalf("isolation call order = %v", manager.calls)
	}
	if !worktree.ValidWorkspaceID(captured.WorkspaceID) || !worktree.ValidWorkspaceID(captured.OwnerID) || captured.WorkspaceID == captured.OwnerID {
		t.Fatalf("generated identities are invalid: workspace=%q owner=%q", captured.WorkspaceID, captured.OwnerID)
	}
	if captured.LogicalName != "subagent/reviewer/"+captured.WorkspaceID {
		t.Fatalf("logical name = %q", captured.LogicalName)
	}
	if len(binder.calls) != 1 {
		t.Fatalf("Bind calls = %d", len(binder.calls))
	}
	bound := binder.calls[0]
	if bound.TaskID != "isolated-task" || bound.Lease != runtime.lease || bound.Role.Definition.Isolation != agentrole.IsolationWorktree || bound.Verifier == nil {
		t.Fatalf("Bind request did not retain task ownership: %#v", bound)
	}
	if got := bound.Role.Definition.ToolAllow; !reflect.DeepEqual(got, []string{"Read"}) {
		t.Fatalf("Bind role was not frozen: %v", got)
	}
	if bound.Role.Definition.MaxIterations == nil || *bound.Role.Definition.MaxIterations != 4 {
		t.Fatalf("Bind role limit was not frozen: %#v", bound.Role.Definition.MaxIterations)
	}
	task := prepared.(*preparedSubagentTask)
	if task.workspaceRuntime != runtime || task.workspaceLease != runtime.lease || task.metadata.MaxIterations != 4 {
		t.Fatal("PreparedTask did not retain isolated ownership and frozen role")
	}
	task.runtime.close(errors.New("test cleanup"))
}

func TestRunnerIsolationBindFailureSettlesAndReleases(t *testing.T) {
	manager := &runnerIsolationManager{}
	binder := &runnerIsolationBinder{manager: manager, err: errors.New("bind failed")}
	factory, _ := newRunnerIsolationFactory(t, manager, binder, true)
	manager.acquire = func(request worktree.AcquireRequest) (worktree.Lease, error) {
		return runnerIsolationLease(t, request), nil
	}

	prepared, err := factory.Prepare(context.Background(), "bind-failure", runnerIsolationInput())
	if err == nil || prepared != nil {
		t.Fatalf("Prepare() = (%v,%v), want nil error result", prepared, err)
	}
	if !reflect.DeepEqual(manager.calls, []string{"acquire", "bind", "settle", "release"}) {
		t.Fatalf("rollback order = %v", manager.calls)
	}
	if len(manager.settleRequests) != 1 || !manager.settleRequests[0].RuntimeStopped {
		t.Fatalf("bind-failure settlement did not report absent runtime as stopped: %#v", manager.settleRequests)
	}
}

func TestRunnerIsolationBindErrorStillClosesReturnedRuntime(t *testing.T) {
	manager := &runnerIsolationManager{}
	runtime := &runnerIsolationRuntime{manager: manager}
	binder := &runnerIsolationBinder{manager: manager, runtime: runtime, err: errors.New("bind failed after allocation")}
	factory, _ := newRunnerIsolationFactory(t, manager, binder, true)
	manager.acquire = func(request worktree.AcquireRequest) (worktree.Lease, error) {
		lease := runnerIsolationLease(t, request)
		runtime.lease = lease
		return lease, nil
	}

	prepared, err := factory.Prepare(context.Background(), "partial-bind", runnerIsolationInput())
	if err == nil || prepared != nil {
		t.Fatalf("Prepare() = (%v,%v), want partial-bind rollback", prepared, err)
	}
	if !reflect.DeepEqual(manager.calls, []string{"acquire", "bind", "runtime-close", "settle", "release"}) {
		t.Fatalf("partial-bind rollback order = %v", manager.calls)
	}
	if runtime.closed != 1 || len(manager.settleRequests) != 1 || !manager.settleRequests[0].RuntimeStopped {
		t.Fatalf("partial-bind ownership leaked: closed=%d settle=%#v", runtime.closed, manager.settleRequests)
	}
}

func TestRunnerIsolationPostBindFailureClosesThenSettlesAndReleases(t *testing.T) {
	manager := &runnerIsolationManager{}
	runtime := &runnerIsolationRuntime{manager: manager}
	binder := &runnerIsolationBinder{manager: manager, runtime: runtime}
	factory, _ := newRunnerIsolationFactory(t, manager, binder, true)
	policy, err := NewSkillHistoryPolicy(1, 1000, 1)
	if err != nil {
		t.Fatal(err)
	}
	factory.options.ContextPolicy = policy
	manager.acquire = func(request worktree.AcquireRequest) (worktree.Lease, error) {
		lease := runnerIsolationLease(t, request)
		runtime.lease = lease
		return lease, nil
	}

	prepared, err := factory.Prepare(context.Background(), "budget-failure", runnerIsolationInput())
	if err == nil || prepared != nil {
		t.Fatalf("Prepare() = (%v,%v), want post-bind failure", prepared, err)
	}
	if !reflect.DeepEqual(manager.calls, []string{"acquire", "bind", "runtime-close", "settle", "release"}) {
		t.Fatalf("post-bind rollback order = %v", manager.calls)
	}
	if runtime.closed != 1 {
		t.Fatalf("workspace runtime close count = %d", runtime.closed)
	}
}

func TestRunnerIsolationRollbackDoesNotClaimFailedRuntimeCloseStopped(t *testing.T) {
	manager := &runnerIsolationManager{}
	runtime := &runnerIsolationRuntime{manager: manager, closeErr: errors.New("runtime still live")}
	binder := &runnerIsolationBinder{manager: manager, runtime: runtime}
	factory, _ := newRunnerIsolationFactory(t, manager, binder, true)
	policy, err := NewSkillHistoryPolicy(1, 1000, 1)
	if err != nil {
		t.Fatal(err)
	}
	factory.options.ContextPolicy = policy
	manager.acquire = func(request worktree.AcquireRequest) (worktree.Lease, error) {
		lease := runnerIsolationLease(t, request)
		runtime.lease = lease
		return lease, nil
	}

	if prepared, prepareErr := factory.Prepare(context.Background(), "close-failure", runnerIsolationInput()); prepareErr == nil || prepared != nil {
		t.Fatalf("Prepare() = (%v,%v), want rollback failure", prepared, prepareErr)
	}
	if len(manager.settleRequests) != 1 || manager.settleRequests[0].RuntimeStopped {
		t.Fatalf("failed Runtime.Close was promoted to stopped: %#v", manager.settleRequests)
	}
	if !reflect.DeepEqual(manager.calls, []string{"acquire", "bind", "runtime-close", "settle"}) {
		t.Fatalf("failed-close rollback order = %v", manager.calls)
	}
}

func TestDefinedWorktreeRebuildsPromptFromBoundRuntime(t *testing.T) {
	manager := &runnerIsolationManager{}
	workspaceContext := &runnerIsolationStableContext{sections: []prompt.Section{
		{Name: "global", Priority: 100, Content: "WORKTREE-GLOBAL", Stable: true, Scope: prompt.ScopeGlobal},
		{Name: "user", Priority: 90, Content: "WORKTREE-USER", Stable: true, Scope: prompt.ScopeUser},
		{Name: "project", Priority: 80, Content: "WORKTREE-PROJECT", Stable: true, Scope: prompt.ScopeProject},
		{Name: "context-runtime", Priority: 70, Content: "CONTEXT-RUNTIME-MUST-NOT-PASS", Stable: true, Scope: prompt.ScopeRuntime},
	}}
	runtime := &runnerIsolationRuntime{manager: manager, stable: workspaceContext, hooks: newSubagentLifecycleRecorder()}
	binder := &runnerIsolationBinder{manager: manager, runtime: runtime}
	factory, _ := newRunnerIsolationFactory(t, manager, binder, true)
	mainContext := &runnerIsolationStableContext{sections: []prompt.Section{{
		Name: "main-project", Priority: 100, Content: "MAIN-PROJECT-MUST-NOT-PASS", Stable: true, Scope: prompt.ScopeProject,
	}}}
	factory.options.SessionContext = mainContext
	manager.acquire = func(request worktree.AcquireRequest) (worktree.Lease, error) {
		lease := runnerIsolationLease(t, request)
		runtime.lease = lease
		return lease, nil
	}

	prepared, err := factory.Prepare(context.Background(), "defined-worktree", runnerIsolationInput())
	if err != nil {
		t.Fatal(err)
	}
	task := prepared.(*preparedSubagentTask)
	if mainContext.calls != 0 || workspaceContext.calls != 1 {
		t.Fatalf("stable context calls: main=%d workspace=%d", mainContext.calls, workspaceContext.calls)
	}
	if task.projectRoot != runtime.lease.Root || !reflect.DeepEqual(task.runtime.Profile.ReadRoots, []string{runtime.lease.Root}) {
		t.Fatalf("task retained main root: project=%q read=%v worktree=%q", task.projectRoot, task.runtime.Profile.ReadRoots, runtime.lease.Root)
	}
	stableText := runnerIsolationSystemText(task.runtime.Prompt.StableSystem)
	for _, retained := range []string{"WORKTREE-GLOBAL", "WORKTREE-USER", "WORKTREE-PROJECT"} {
		if !strings.Contains(stableText, retained) {
			t.Fatalf("worktree stable context omitted %q: %s", retained, stableText)
		}
	}
	for _, forbidden := range []string{"MAIN-PROJECT-MUST-NOT-PASS", "CONTEXT-RUNTIME-MUST-NOT-PASS"} {
		if strings.Contains(stableText, forbidden) {
			t.Fatalf("worktree stable context retained %q: %s", forbidden, stableText)
		}
	}
	dynamic := runnerIsolationSystemText(task.runtime.Prompt.DynamicSystem)
	for _, retained := range []string{
		"ROLE-BODY", runtime.lease.WorkspaceID, runtime.lease.BaseOID, runtime.lease.Branch,
		"显式 cwd", "主目录", "其他 Worktree", "共享依赖", runtime.lease.Root,
	} {
		if !strings.Contains(dynamic, retained) {
			t.Fatalf("worktree runtime block omitted %q: %s", retained, dynamic)
		}
	}
	for _, block := range task.runtime.Prompt.DynamicSystem {
		if block.Scope != prompt.ScopeRuntime {
			t.Fatalf("dynamic block %q scope = %q", block.Name, block.Scope)
		}
	}
	request := task.runtime.Prompt.BuildChild(nil, []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: factory.options.RuntimeRedactor.Redact("review the isolated task")}}, task.runtime.Prompt.Tools)
	if err := request.Validate(); err != nil {
		t.Fatalf("worktree Provider request invalid: %v", err)
	}
	task.runtime.close(errors.New("test cleanup"))
	_ = runtime.Close(context.Background())
}

func TestDefinedWorktreeRunUsesBoundAdmissionToolsAndHooks(t *testing.T) {
	providerFixture := &subagentScriptProvider{scripts: [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("workspace-call", "Echo", `{"value":"inside"}`)}, {Type: provider.StreamEventDone}},
		{{Type: provider.StreamEventTextDelta, Delta: testSafeText("workspace done")}, {Type: provider.StreamEventDone}},
	}}
	mainHooks := newSubagentLifecycleRecorder()
	base, echo := newTaskLoopToolFactory(t, providerFixture, mainHooks, 2)
	role := base.options.Roles.(*taskRuntimeRoleManager).roles["reviewer"]
	role.Definition = role.Definition.Clone()
	role.Definition.Isolation = agentrole.IsolationWorktree
	role.Definition.ToolAllow = []string{"Echo"}
	base.options.Roles.(*taskRuntimeRoleManager).roles["reviewer"] = role

	manager := &runnerIsolationManager{}
	workspaceHooks := newSubagentLifecycleRecorder()
	runtime := &runnerIsolationRuntime{
		manager: manager, hooks: workspaceHooks, enforceAdmission: true,
		stable: &runnerIsolationStableContext{sections: []prompt.Section{{
			Name: "project", Priority: 10, Content: "BOUND-PROJECT", Stable: true, Scope: prompt.ScopeProject,
		}}},
	}
	binder := &runnerIsolationBinder{manager: manager, runtime: runtime}
	repository := t.TempDir()
	common := filepath.Join(repository, ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	identity, err := worktree.NewRepositoryIdentity(repository, common)
	if err != nil {
		t.Fatal(err)
	}
	options := base.options
	options.WorktreeManager = manager
	options.WorkspaceBinder = binder
	options.WorktreeAcquireTemplate = worktree.AcquireRequest{
		RepositoryRoot: identity.Root, RepositoryIdentity: identity, LogicalName: "subagent",
	}
	binder.configure = func(request WorktreeWorkspaceBindRequest) error {
		return configureRunnerIsolationRuntime(runtime, request, options)
	}
	factory, err := NewSubagentRunnerFactory(options)
	if err != nil {
		t.Fatal(err)
	}
	manager.acquire = func(request worktree.AcquireRequest) (worktree.Lease, error) {
		lease := runnerIsolationLease(t, request)
		runtime.lease = lease
		return lease, nil
	}
	var executionRoot string
	var executionAdmitted bool
	echo.execute = func(ctx context.Context, input tool.Input) tool.Result {
		scope, ok := tool.ReadScopeFromContext(ctx)
		if ok {
			executionRoot = scope.ProjectRoot
		}
		_, active, _ := runtime.admissionSnapshot()
		executionAdmitted = active > 0
		result, _ := echo.factory.Build(tool.ResultFactoryInput{
			CallID: input.CallID, Name: input.Name, State: tool.Completed, Status: tool.StatusSuccess,
			Summary: "echo completed", Preview: "echo:inside",
		})
		return result
	}

	prepared, err := factory.Prepare(context.Background(), "defined-worktree-run", runnerIsolationInput())
	if err != nil {
		t.Fatal(err)
	}
	task := prepared.(*preparedSubagentTask)
	if task.runtime.Registry != runtime.registry || task.runtime.ToolExecutor != runtime.tools ||
		task.runtime.Executor != runtime.scoped || task.runtime.ActiveTools != runtime.capabilities || task.runtime.Hooks != workspaceHooks {
		t.Fatal("PreparedTask retained a main workspace execution dependency")
	}
	result := task.Run(context.Background(), func(subagent.AgentEvent) error { return nil })
	completion := task.Settle(context.Background(), result)
	if completion.Status != subagent.StatusCompleted || echo.Calls() != 1 {
		t.Fatalf("worktree run = status %q echo calls %d", completion.Status, echo.Calls())
	}
	if executionRoot != runtime.lease.Root || !executionAdmitted {
		t.Fatalf("workspace tool execution root/admission = %q/%v, want %q/true", executionRoot, executionAdmitted, runtime.lease.Root)
	}
	if calls := mainHooks.snapshot(); len(calls) != 0 {
		t.Fatalf("main hooks were reused: %v", calls)
	}
	if calls := workspaceHooks.snapshot(); len(calls) == 0 {
		t.Fatal("workspace hooks were not used")
	}
	begin, active, violations := runtime.admissionSnapshot()
	if begin < 2 || active != 0 || violations != 0 {
		t.Fatalf("workspace admission = begins %d active %d violations %d", begin, active, violations)
	}
	_ = runtime.Close(context.Background())
}

func TestForkWorkspaceFiltersParentScopeAndRebuildsBoundProject(t *testing.T) {
	manager := &runnerIsolationManager{}
	workspaceContext := &runnerIsolationStableContext{sections: []prompt.Section{
		{Name: "borrowed-global", Priority: 100, Content: "WORKSPACE-GLOBAL-MUST-NOT-DUPLICATE", Stable: true, Scope: prompt.ScopeGlobal},
		{Name: "project", Priority: 90, Content: "WORKTREE-FORK-PROJECT", Stable: true, Scope: prompt.ScopeProject},
		{Name: "runtime", Priority: 80, Content: "WORKSPACE-CONTEXT-RUNTIME-MUST-NOT-PASS", Stable: true, Scope: prompt.ScopeRuntime},
	}}
	runtime := &runnerIsolationRuntime{manager: manager, stable: workspaceContext, hooks: newSubagentLifecycleRecorder(), enforceAdmission: true}
	binder := &runnerIsolationBinder{manager: manager, runtime: runtime}
	factory, _ := newRunnerIsolationFactory(t, manager, binder, true)

	now := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	parentConversation := conversation.NewConversation("parent-worktree-fork", now)
	parentConversation.Messages = append(parentConversation.Messages, conversation.Message{
		Role: conversation.RoleUser, Content: factory.options.RuntimeRedactor.Redact("PARENT-AUDIT"), CreatedAt: now,
	})
	parentConversation.UpdatedAt = now
	conversationSnapshot, err := conversation.TakeSnapshot(parentConversation)
	if err != nil {
		t.Fatal(err)
	}
	mainCanary := filepath.Join(t.TempDir(), "SECRET-MAIN-ABSOLUTE-PATH")
	parentPrompt, err := provider.CapturePromptPrefix(provider.ChatRequest{
		Model: "model-parent",
		StableSystem: []provider.SystemBlock{
			{Name: "global", Content: factory.options.RuntimeRedactor.Redact("PARENT-GLOBAL"), Cacheable: true, Scope: prompt.ScopeGlobal},
			{Name: "user", Content: factory.options.RuntimeRedactor.Redact("PARENT-USER"), Cacheable: true, Scope: prompt.ScopeUser},
			{Name: "main-project", Content: factory.options.RuntimeRedactor.Redact("MAIN-PROJECT " + mainCanary), Cacheable: true, Scope: prompt.ScopeProject},
		},
		DynamicSystem: []provider.SystemBlock{{
			Name: "main-runtime", Content: factory.options.RuntimeRedactor.Redact("MAIN-RUNTIME " + mainCanary), Scope: prompt.ScopeRuntime,
		}},
		Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: factory.options.RuntimeRedactor.Redact("PARENT-PROVIDER-MESSAGE")}},
		Tools:    []provider.ToolDefinition{{Name: "ParentOnly", Description: "parent tool", Schema: tool.Schema{Type: "object"}}},
		Cache: provider.CachePolicy{
			EnablePromptCache: true, CacheTools: true, SystemBreakpointName: mainCanary,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	parentFingerprint := parentPrompt.Fingerprint
	factory.options.ParentRuntime = func(context.Context, subagent.ParentRef) (ParentRuntimeSnapshot, error) {
		return ParentRuntimeSnapshot{
			Model: "model-parent", Registry: factory.options.Registry,
			Conversation: conversationSnapshot, Prompt: parentPrompt,
		}, nil
	}
	manager.acquire = func(request worktree.AcquireRequest) (worktree.Lease, error) {
		lease := runnerIsolationLease(t, request)
		runtime.lease = lease
		return lease, nil
	}

	prepared, err := factory.Prepare(context.Background(), "fork-worktree", subagent.SubmitInput{
		Task: "FORK-WORKTREE-TASK", Type: subagent.TypeFork, Role: "reviewer",
		Placement: subagent.PlacementBackground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent-worktree-fork", RequestGeneration: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := prepared.(*preparedSubagentTask)
	request, err := task.firstRequest()
	if err != nil {
		t.Fatal(err)
	}
	stable, dynamic := requestSystemText(request)
	for _, retained := range []string{"PARENT-GLOBAL", "PARENT-USER", "WORKTREE-FORK-PROJECT"} {
		if !strings.Contains(stable, retained) {
			t.Fatalf("fork workspace stable omitted %q: %s", retained, stable)
		}
	}
	for _, forbidden := range []string{mainCanary, "MAIN-PROJECT", "MAIN-RUNTIME", "WORKSPACE-GLOBAL-MUST-NOT-DUPLICATE", "WORKSPACE-CONTEXT-RUNTIME-MUST-NOT-PASS"} {
		if strings.Contains(stable+dynamic, forbidden) {
			t.Fatalf("fork workspace leaked %q: stable=%s dynamic=%s", forbidden, stable, dynamic)
		}
	}
	for _, retained := range []string{"ROLE-BODY", runtime.lease.WorkspaceID, runtime.lease.BaseOID, runtime.lease.Branch, runtime.lease.Root} {
		if !strings.Contains(dynamic, retained) {
			t.Fatalf("fork workspace runtime omitted %q: %s", retained, dynamic)
		}
	}
	if len(request.Messages) != 2 || request.Messages[0].Content.Text() != "PARENT-PROVIDER-MESSAGE" || request.Messages[1].Content.Text() != "FORK-WORKTREE-TASK" {
		t.Fatalf("fork workspace messages = %#v", request.Messages)
	}
	if len(request.Tools) != 0 || request.Cache.CacheTools || request.Cache.SystemBreakpointName != "" || task.runtime.Prompt.ToolFingerprint == parentPrompt.ToolFingerprint {
		t.Fatalf("fork workspace reused parent tools/cache: tools=%#v cache=%#v fingerprint=%q", request.Tools, request.Cache, task.runtime.Prompt.ToolFingerprint)
	}
	if task.projectRoot != runtime.lease.Root || task.runtime.Registry != runtime.registry || task.runtime.ToolExecutor != runtime.tools || task.runtime.Hooks != runtime.hooks {
		t.Fatal("fork workspace retained main runtime dependencies")
	}
	if parentPrompt.Fingerprint != parentFingerprint || parentPrompt.StableSystem[2].Content.Text() != "MAIN-PROJECT "+mainCanary || parentPrompt.Tools[0].Name != "ParentOnly" {
		t.Fatal("fork workspace preparation mutated parent snapshot")
	}
	begin, active, violations := runtime.admissionSnapshot()
	if begin != 1 || active != 0 || violations != 0 || workspaceContext.calls != 1 {
		t.Fatalf("fork workspace preparation admission/context = %d/%d/%d calls=%d", begin, active, violations, workspaceContext.calls)
	}
	task.runtime.close(errors.New("test cleanup"))
	_ = runtime.Close(context.Background())
}

func runnerIsolationSystemText(blocks []provider.SystemBlock) string {
	values := make([]string, 0, len(blocks))
	for _, block := range blocks {
		values = append(values, block.Content.Text())
	}
	return strings.Join(values, "\n")
}

func newRunnerIsolationFactory(t *testing.T, manager WorktreeLifecycleManager, binder WorktreeWorkspaceBinder, isolated bool) (*SubagentRunnerFactory, *runnerIsolationRoleManager) {
	t.Helper()
	base, _ := newTaskRuntimeFactoryFixture(t, 4)
	limit := 4
	mode := agentrole.IsolationNone
	if isolated {
		mode = agentrole.IsolationWorktree
	}
	roles := &runnerIsolationRoleManager{role: agentrole.ResolvedRole{Generation: 9, Definition: agentrole.Definition{
		Metadata: agentrole.Metadata{
			Name: "reviewer", ToolAllow: []string{"Read"}, Model: agentrole.ModelInherit,
			MaxIterations: &limit, PermissionMode: agentrole.PermissionStrict, Isolation: mode,
		},
		Instructions: base.options.RuntimeRedactor.Redact("ROLE-BODY"),
	}}}
	repository := t.TempDir()
	common := filepath.Join(repository, ".git")
	if err := makeTestDirectory(common); err != nil {
		t.Fatal(err)
	}
	identity, err := worktree.NewRepositoryIdentity(repository, common)
	if err != nil {
		t.Fatal(err)
	}
	options := base.options
	options.Roles = roles
	options.WorktreeManager = manager
	options.WorkspaceBinder = binder
	options.WorktreeAcquireTemplate = worktree.AcquireRequest{
		RepositoryRoot: identity.Root, RepositoryIdentity: identity, LogicalName: "subagent",
	}
	if binder != nil {
		if concrete, ok := binder.(*runnerIsolationBinder); ok && concrete.configure == nil {
			concrete.configure = func(request WorktreeWorkspaceBindRequest) error {
				runtime, ok := concrete.runtime.(*runnerIsolationRuntime)
				if !ok || runtime == nil {
					return nil
				}
				return configureRunnerIsolationRuntime(runtime, request, options)
			}
		}
	}
	result, err := NewSubagentRunnerFactory(options)
	if err != nil {
		t.Fatal(err)
	}
	return result, roles
}

func configureRunnerIsolationRuntime(runtime *runnerIsolationRuntime, request WorktreeWorkspaceBindRequest, options SubagentRunnerOptions) error {
	if err := os.MkdirAll(request.Lease.Root, 0o755); err != nil {
		return err
	}
	opened, err := safefs.Bootstrap(request.Lease.Root, safefs.Policy{})
	if err != nil {
		return err
	}
	foreground, background, err := tool.BuildCapabilityViews(
		options.Registry, &request.Role.Definition, options.BackgroundPolicy, options.GlobalDenied, request.PlanMode,
	)
	if err != nil {
		_ = opened.Root.Close()
		return err
	}
	capabilities, err := tool.NewCapabilitySwitch(foreground, background)
	if err != nil {
		_ = opened.Root.Close()
		return err
	}
	base, err := tool.NewExecutorWithResultFactory(options.Registry, request.Lease.Root, time.Second, 4096, options.ResultFactory)
	if err != nil {
		_ = opened.Root.Close()
		return err
	}
	cache, err := tool.NewReadCache(tool.ReadCacheLimits{
		MaxEntries: options.Limits.ReadCacheMaxEntries, MaxBytes: options.Limits.ReadCacheMaxBytes,
		MaxValueBytes: options.Limits.ReadCacheMaxValueBytes, MaxDependenciesPerEntry: options.Limits.ReadCacheMaxDependenciesPerEntry,
	}, options.ResultFactory)
	if err != nil {
		_ = opened.Root.Close()
		return err
	}
	scoped, err := tool.NewScopedExecutor(base, request.Verifier, request.TaskID, capabilities, cache)
	if err != nil {
		cache.Close()
		_ = opened.Root.Close()
		return err
	}
	runtime.rootHandle = opened.Root
	runtime.registry = options.Registry
	runtime.tools = base
	runtime.scoped = scoped
	runtime.capabilities = capabilities
	runtime.cache = cache
	if runtime.stable == nil {
		runtime.stable = &runnerIsolationStableContext{}
	}
	if runtime.hooks == nil {
		runtime.hooks = hook.Noop()
	}
	return nil
}

func makeTestDirectory(path string) error {
	return os.MkdirAll(path, 0o755)
}

func runnerIsolationInput() subagent.SubmitInput {
	return subagent.SubmitInput{
		Task: "review the isolated task", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent-conversation"},
	}
}

func runnerIsolationLease(t *testing.T, request worktree.AcquireRequest) worktree.Lease {
	t.Helper()
	return worktree.Lease{
		WorkspaceID: request.WorkspaceID, OwnerID: request.OwnerID,
		Root:    filepath.Join(request.RepositoryRoot, ".xagent", "worktrees", "tasks", request.WorkspaceID[:2], request.WorkspaceID),
		Branch:  "xagent/worktree/" + request.WorkspaceID,
		BaseOID: "0123456789abcdef0123456789abcdef01234567", AcquiredAt: time.Now(),
	}
}
