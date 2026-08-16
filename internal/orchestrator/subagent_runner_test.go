package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/permission"
	"xagent/internal/prompt"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/sessionctx"
	"xagent/internal/subagent"
	"xagent/internal/tool"
)

type subagentScriptProvider struct {
	mu          sync.Mutex
	requests    []provider.ChatRequest
	scripts     [][]provider.StreamEvent
	beforeFirst func(context.Context)
}

func (*subagentScriptProvider) Name() string { return "subagent-script" }

func (script *subagentScriptProvider) StreamChat(ctx context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	script.mu.Lock()
	index := len(script.requests)
	script.requests = append(script.requests, request)
	beforeFirst := script.beforeFirst
	var events []provider.StreamEvent
	if index < len(script.scripts) {
		events = append(events, script.scripts[index]...)
	}
	script.mu.Unlock()
	if index == 0 && beforeFirst != nil {
		beforeFirst(ctx)
	}
	return newOrchestratorTestChatStream(events...), nil
}

func (script *subagentScriptProvider) Requests() []provider.ChatRequest {
	script.mu.Lock()
	defer script.mu.Unlock()
	return append([]provider.ChatRequest(nil), script.requests...)
}

type subagentLifecycleRecorder struct {
	hook.Runtime
	mu      sync.Mutex
	calls   []string
	prompts hook.PromptLease
}

type subagentPromptLease struct {
	mu       sync.Mutex
	blocks   []hook.PromptBlock
	commits  int
	releases int
}

func (lease *subagentPromptLease) Blocks() []hook.PromptBlock {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return append([]hook.PromptBlock(nil), lease.blocks...)
}

func (lease *subagentPromptLease) Commit() {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	lease.commits++
}

func (lease *subagentPromptLease) Release() {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	lease.releases++
}

func (lease *subagentPromptLease) counts() (int, int) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.commits, lease.releases
}

type subagentSafeEchoTool struct {
	factory *tool.ResultFactory
	mu      sync.Mutex
	calls   int
	execute func(context.Context, tool.Input) tool.Result
}

func (*subagentSafeEchoTool) Name() string        { return "Echo" }
func (*subagentSafeEchoTool) Description() string { return "echo a bounded value" }
func (*subagentSafeEchoTool) Schema() tool.Schema {
	return tool.ObjectSchema([]string{"value"}, map[string]tool.SchemaProperty{"value": tool.StringProperty("value")})
}
func (*subagentSafeEchoTool) Risk() tool.Risk { return tool.RiskSafe }
func (echo *subagentSafeEchoTool) UsesSafeResultBoundary() bool {
	return echo != nil && echo.factory != nil
}
func (echo *subagentSafeEchoTool) Execute(ctx context.Context, input tool.Input) tool.Result {
	echo.mu.Lock()
	echo.calls++
	execute := echo.execute
	echo.mu.Unlock()
	if execute != nil {
		return execute(ctx, input)
	}
	value, _ := input.Arguments["value"].(string)
	result, _ := echo.factory.Build(tool.ResultFactoryInput{
		CallID: input.CallID, Name: input.Name, State: tool.Completed, Status: tool.StatusSuccess,
		Summary: "echo completed", Preview: "echo:" + value,
	})
	return result
}

func (echo *subagentSafeEchoTool) Calls() int {
	echo.mu.Lock()
	defer echo.mu.Unlock()
	return echo.calls
}

func newSubagentLifecycleRecorder() *subagentLifecycleRecorder {
	return &subagentLifecycleRecorder{Runtime: hook.Noop()}
}

func (recorder *subagentLifecycleRecorder) append(value string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.calls = append(recorder.calls, value)
}

func (recorder *subagentLifecycleRecorder) SessionStart(_ context.Context, sessionID string, state hook.SessionState) {
	recorder.append("session_start:" + sessionID + ":" + string(state))
}

func (recorder *subagentLifecycleRecorder) BeginTurn(_ context.Context, sessionID string, kind hook.ExecutionKind, mode hook.HookMode) hook.ExecutionRef {
	recorder.append("turn_start:" + sessionID + ":" + string(kind) + ":" + string(mode))
	return hook.ExecutionRef{SessionID: sessionID, ExecutionID: "subagent-execution", TurnID: "subagent-turn", Kind: kind, Mode: mode}
}

func (recorder *subagentLifecycleRecorder) EndTurn(_ context.Context, ref hook.ExecutionRef, status hook.TurnStatus, detail string) {
	recorder.append("turn_end:" + ref.SessionID + ":" + string(status) + ":" + detail)
}

func (recorder *subagentLifecycleRecorder) SessionEnd(_ context.Context, sessionID string, reason hook.SessionEndReason) {
	recorder.append("session_end:" + sessionID + ":" + string(reason))
}

func (recorder *subagentLifecycleRecorder) AcquirePrompts(context.Context, hook.ExecutionRef) (hook.PromptLease, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.prompts, nil
}

func (recorder *subagentLifecycleRecorder) snapshot() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.calls...)
}

type taskRuntimeRoleManager struct {
	roles map[string]agentrole.ResolvedRole
}

func (manager *taskRuntimeRoleManager) Snapshot() agentrole.Snapshot { return agentrole.Snapshot{} }

func (manager *taskRuntimeRoleManager) Resolve(name string) (agentrole.ResolvedRole, bool) {
	role, ok := manager.roles[name]
	if !ok {
		return agentrole.ResolvedRole{}, false
	}
	return agentrole.ResolvedRole{Generation: role.Generation, Definition: role.Definition.Clone()}, true
}

func (*taskRuntimeRoleManager) Refresh(context.Context) (agentrole.RefreshResult, error) {
	return agentrole.RefreshResult{}, errors.New("unused")
}

type taskRuntimeStableContext struct {
	sections []prompt.Section
}

func (stable taskRuntimeStableContext) PrepareStable(context.Context) sessionctx.PreparedContext {
	return sessionctx.PreparedContext{StableSections: append([]prompt.Section(nil), stable.sections...)}
}

func newTaskRuntimeFactoryFixture(t *testing.T, maxIterations int) (*SubagentRunnerFactory, *taskRuntimeRoleManager) {
	t.Helper()
	redactor := redact.NewRuntimeRedactor()
	factory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewSafeCandidateRegistry()
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	executor, err := tool.NewExecutorWithResultFactory(registry, t.TempDir(), time.Second, 1024, factory)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agentrole.NewModelCatalog(agentrole.ModelCatalogOptions{
		ProviderID: "fixture", DefaultModel: "model-default", MaxModelBytes: 128,
		Validate: func(model string) error {
			if !strings.HasPrefix(model, "model-") {
				return errors.New("unsupported")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	roleLimit := maxIterations
	roles := &taskRuntimeRoleManager{roles: map[string]agentrole.ResolvedRole{
		"reviewer": {
			Generation: 7,
			Definition: agentrole.Definition{
				Metadata: agentrole.Metadata{
					Name: "reviewer", Model: agentrole.ModelInherit, MaxIterations: &roleLimit,
					PermissionMode: agentrole.PermissionStrict,
				},
				Instructions: redactor.Redact("ROLE-BODY-ONLY"),
				Provenance:   agentrole.Provenance{Source: agentrole.SourceProject, SourceID: "roles/reviewer.md", Origin: redactor.Redact("fixture")},
			},
		},
		"strict-reviewer": {
			Generation: 11,
			Definition: agentrole.Definition{
				Metadata:     agentrole.Metadata{Name: "strict-reviewer", Model: agentrole.ModelInherit, MaxIterations: &roleLimit, PermissionMode: agentrole.PermissionStrict},
				Instructions: redactor.Redact("STRICT-TOOL-LOOP-ROLE"),
			},
		},
	}}
	limits := subagent.DefaultLimits()
	contextPolicy, err := NewSkillHistoryPolicy(1<<30, 10_000_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewSubagentRunnerFactory(SubagentRunnerOptions{
		Registry: registry, Executor: executor, ResultFactory: factory,
		Roles: roles, Models: catalog,
		Authorizer: &permission.Authorizer{Session: permission.NewSession(), Redact: redactor.Text},
		Limits:     limits, RunOptions: RunOptions{MaxIterations: 6, MaxUnknownToolCalls: 2},
		SessionContext: taskRuntimeStableContext{sections: []prompt.Section{{
			Name: "project-rules", Priority: 10, Content: "PROJECT-INSTRUCTIONS-ONLY", Stable: true, Scope: prompt.ScopeProject,
		}}},
		RequestBudgeter: contextmgr.NewRequestBudgeter(), ContextPolicy: contextPolicy,
		Thinking: config.ThinkingConfig{}, RuntimeRedactor: redactor,
		ParentRuntime: func(context.Context, subagent.ParentRef) (ParentRuntimeSnapshot, error) {
			return ParentRuntimeSnapshot{Model: "model-parent", PermissionMode: permission.ModeDefault}, nil
		},
		Clock: func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner, roles
}

func newTaskLoopToolFactory(t *testing.T, providerImpl provider.Provider, hooks hook.Runtime, maxIterations int) (*SubagentRunnerFactory, *subagentSafeEchoTool) {
	t.Helper()
	redactor := redact.NewRuntimeRedactor()
	resultFactory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewSafeCandidateRegistry()
	echo := &subagentSafeEchoTool{factory: resultFactory}
	if err := registry.RegisterWithOptions(echo, tool.RegistrationOptions{
		Policy: tool.ExecutionPolicy{ReadOnly: true, SideEffectFree: true, ConcurrentSafe: true},
	}); err != nil {
		t.Fatal(err)
	}
	agentTool, err := tool.NewAgentToolWithResultFactory(resultFactory)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(agentTool); err != nil {
		t.Fatal(err)
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	executor, err := tool.NewExecutorWithResultFactory(registry, root, time.Second, 4096, resultFactory)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := agentrole.NewModelCatalog(agentrole.ModelCatalogOptions{
		ProviderID: "fixture", DefaultModel: "model-default", MaxModelBytes: 128,
		Validate: func(model string) error {
			if !strings.HasPrefix(model, "model-") {
				return errors.New("unsupported")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	roleLimit := maxIterations
	roles := &taskRuntimeRoleManager{roles: map[string]agentrole.ResolvedRole{
		"reviewer": {
			Generation: 11,
			Definition: agentrole.Definition{
				Metadata:     agentrole.Metadata{Name: "reviewer", Model: agentrole.ModelInherit, MaxIterations: &roleLimit, PermissionMode: agentrole.PermissionPermissive},
				Instructions: redactor.Redact("TOOL-LOOP-ROLE"),
			},
		},
		"strict-reviewer": {
			Generation: 11,
			Definition: agentrole.Definition{
				Metadata:     agentrole.Metadata{Name: "strict-reviewer", Model: agentrole.ModelInherit, MaxIterations: &roleLimit, PermissionMode: agentrole.PermissionStrict},
				Instructions: redactor.Redact("STRICT-TOOL-LOOP-ROLE"),
			},
		},
	}}
	contextManager, err := contextmgr.New(nil, contextmgr.ManagerOptions{
		Context: config.ContextConfig{
			Enabled:                  func() *bool { value := false; return &value }(),
			ToolResultThresholdChars: 8, ToolResultsThresholdChars: 16,
			ModelWindowTokens: 100_000, AutoMarginTokens: 1_000, ManualMarginTokens: 100,
			RecentKeepTokens: 10, RecentKeepMessages: 5, SummaryFailureLimit: 3, PreviewChars: 64,
		},
		InlineOutputBytes: 4096, RuntimeRedactor: redactor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hooks == nil {
		hooks = hook.Noop()
	}
	contextPolicy, err := NewSkillHistoryPolicy(1<<30, 10_000_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewSubagentRunnerFactory(SubagentRunnerOptions{
		Provider: providerImpl, Registry: registry, Executor: executor, ResultFactory: resultFactory,
		Roles: roles, Models: catalog,
		Authorizer: &permission.Authorizer{Session: permission.NewSession(), Redact: redactor.Text},
		Limits:     subagent.DefaultLimits(), RunOptions: RunOptions{MaxIterations: maxIterations, MaxUnknownToolCalls: 2},
		ContextManager: contextManager, RequestBudgeter: contextmgr.NewRequestBudgeter(), ContextPolicy: contextPolicy,
		Hooks: hooks, RuntimeRedactor: redactor,
		ParentRuntime: func(context.Context, subagent.ParentRef) (ParentRuntimeSnapshot, error) {
			return ParentRuntimeSnapshot{Model: "model-parent", PermissionMode: permission.ModePermissive, Registry: registry}, nil
		},
		Clock: func() time.Time { return time.Date(2026, 8, 15, 12, 30, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return runner, echo
}

func TestTaskRuntimeIsolationAndFrozenDefinedProfile(t *testing.T) {
	factory, roles := newTaskRuntimeFactoryFixture(t, 4)
	input := subagent.SubmitInput{
		Task: "inspect the child only", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent-conversation"},
	}
	firstPrepared, err := factory.Prepare(context.Background(), "task-a", input)
	if err != nil {
		t.Fatal(err)
	}
	secondPrepared, err := factory.Prepare(context.Background(), "task-b", input)
	if err != nil {
		t.Fatal(err)
	}
	first := firstPrepared.(*preparedSubagentTask)
	second := secondPrepared.(*preparedSubagentTask)

	if first.runtime == second.runtime || first.runtime.Conversation == second.runtime.Conversation ||
		first.runtime.Authorizer == second.runtime.Authorizer || first.runtime.Confirm == second.runtime.Confirm ||
		first.runtime.ReadCache == second.runtime.ReadCache || first.runtime.Executor == second.runtime.Executor {
		t.Fatal("task runtime dependencies were shared")
	}
	if first.runtime.Conversation.ID != "subagent:task-a" || len(first.runtime.Conversation.Messages) != 1 ||
		first.runtime.Conversation.Messages[0].Role != conversation.RoleUser ||
		first.runtime.Conversation.Messages[0].Content.Text() != input.Task {
		t.Fatalf("defined conversation = %#v", first.runtime.Conversation)
	}
	if first.runtime.Profile.Depth != 1 || first.runtime.Profile.Persist || first.runtime.Profile.UpdateMemory ||
		first.runtime.Profile.Model != "model-parent" || first.runtime.Profile.PermissionMode != permission.ModeStrict ||
		first.runtime.Profile.MaxIterations != 4 {
		t.Fatalf("defined runtime profile = %#v", first.runtime.Profile)
	}
	if first.runtime.HookSessionID != "subagent:task-a" || first.Metadata().RoleGeneration != 7 ||
		first.Metadata().ForegroundToolFingerprint == "" || first.Metadata().BackgroundToolFingerprint == "" {
		t.Fatalf("defined metadata = %#v runtime=%#v", first.Metadata(), first.runtime)
	}

	// A later role refresh/mutation must not affect already prepared tasks.
	mutated := roles.roles["reviewer"]
	mutated.Generation = 99
	mutated.Definition.Instructions = redact.NewRuntimeRedactor().Redact("MUTATED-ROLE")
	roles.roles["reviewer"] = mutated
	if first.runtime.Role.Generation != 7 || first.runtime.Role.Definition.Instructions.Text() != "ROLE-BODY-ONLY" {
		t.Fatalf("prepared role changed after refresh: %#v", first.runtime.Role)
	}
}

func TestDefinedPromptContainsNoParentHistoryOrSkillActivity(t *testing.T) {
	factory, _ := newTaskRuntimeFactoryFixture(t, 3)
	prepared, err := factory.Prepare(context.Background(), "task-defined", subagent.SubmitInput{
		Task: "TASK-MESSAGE-ONLY", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent-secret-message"},
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
	joined := stable + "\n" + dynamic
	for _, wanted := range []string{"PROJECT-INSTRUCTIONS-ONLY", "ROLE-BODY-ONLY"} {
		if !strings.Contains(joined, wanted) {
			t.Fatalf("request system omitted %q: %s", wanted, joined)
		}
	}
	for _, forbidden := range []string{"parent-secret-message", "<active-skill", "MUTATED-ROLE"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("request system leaked %q: %s", forbidden, joined)
		}
	}
	for _, block := range request.StableSystem {
		want := prompt.ScopeGlobal
		if block.Content.Text() == "PROJECT-INSTRUCTIONS-ONLY" {
			want = prompt.ScopeProject
		}
		if block.Scope != want {
			t.Fatalf("stable block %q scope = %q, want %q", block.Name, block.Scope, want)
		}
	}
	for _, block := range request.DynamicSystem {
		if block.Scope != prompt.ScopeRuntime {
			t.Fatalf("dynamic block %q scope = %q, want runtime", block.Name, block.Scope)
		}
	}
	if len(request.Messages) != 1 || request.Messages[0].Role != provider.ModelMessageRoleUser || request.Messages[0].Content.Text() != "TASK-MESSAGE-ONLY" {
		t.Fatalf("defined messages = %#v", request.Messages)
	}
	if request.Model != "model-parent" || len(request.Tools) != 0 || request.ToolDefs != nil {
		t.Fatalf("defined frozen request = %#v", request)
	}
}

func TestDefinedZeroIterationReturnsLimitWithoutProvider(t *testing.T) {
	factory, _ := newTaskRuntimeFactoryFixture(t, 0)
	prepared, err := factory.Prepare(context.Background(), "task-zero", subagent.SubmitInput{
		Task: "do not call provider", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := prepared.Run(context.Background(), func(subagent.AgentEvent) error { return nil })
	if result.Status != subagent.StatusLimitReached || result.StopReason != subagent.StopMaxIterations ||
		result.Error == nil || result.Error.Code != string(subagent.ErrLimitReached) {
		t.Fatalf("zero-iteration run result = %#v", result)
	}
	completion := prepared.Settle(context.Background(), result)
	if completion.ID != "task-zero" || completion.EndedAt.IsZero() || completion.Workspace != (subagent.WorkspaceSummary{}) {
		t.Fatalf("zero-iteration settlement = %#v", completion)
	}
	again := prepared.Settle(context.Background(), subagent.RunResult{})
	if !reflect.DeepEqual(again, completion) {
		t.Fatalf("shared settlement is not idempotent: first=%#v again=%#v", completion, again)
	}
}

func TestTaskRuntimeParentCancelBridgeDetachLinearizes(t *testing.T) {
	t.Run("detach wins", func(t *testing.T) {
		parent, cancelParent := context.WithCancelCause(context.Background())
		child, cancelChild := context.WithCancelCause(context.Background())
		bridge := newParentCancelBridge(parent, cancelChild)
		if !bridge.Detach() {
			t.Fatal("live parent bridge did not detach")
		}
		cancelParent(errors.New("parent cancelled after detach"))
		select {
		case <-child.Done():
			t.Fatalf("detached parent cancelled child: %v", context.Cause(child))
		case <-time.After(20 * time.Millisecond):
		}
	})

	t.Run("parent cancel wins", func(t *testing.T) {
		parent, cancelParent := context.WithCancelCause(context.Background())
		child, cancelChild := context.WithCancelCause(context.Background())
		bridge := newParentCancelBridge(parent, cancelChild)
		cause := errors.New("parent cancellation won")
		cancelParent(cause)
		select {
		case <-child.Done():
		case <-time.After(time.Second):
			t.Fatal("parent cancellation did not reach child")
		}
		if bridge.Detach() {
			t.Fatal("bridge reported detach after cancellation callback started")
		}
		if !errors.Is(context.Cause(child), cause) {
			t.Fatalf("child cancellation cause = %v", context.Cause(child))
		}
	})
}

func TestTaskRuntimeMoveToBackgroundIsMonotonicBeforeRun(t *testing.T) {
	factory, _ := newTaskRuntimeFactoryFixture(t, 3)
	prepared, err := factory.Prepare(context.Background(), "task-detach", subagent.SubmitInput{
		Task: "detach safely", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := prepared.(*preparedSubagentTask)
	if task.runtime.ActiveTools.Current().Fingerprint != task.runtime.Profile.ForegroundTools.Fingerprint {
		t.Fatal("prepared foreground task did not start in foreground view")
	}
	if !task.MoveToBackground() {
		t.Fatal("first background transition failed")
	}
	if task.MoveToBackground() {
		t.Fatal("background transition was reversible/repeated")
	}
	if task.runtime.ActiveTools.Current().Fingerprint != task.runtime.Profile.BackgroundTools.Fingerprint {
		t.Fatal("background transition did not install frozen background view")
	}
}

func TestTaskRuntimePlacementLinearizesAgainstCapturedParentCancellation(t *testing.T) {
	prepare := func(t *testing.T) (*preparedSubagentTask, context.CancelCauseFunc) {
		t.Helper()
		factory, _ := newTaskRuntimeFactoryFixture(t, 3)
		parentCtx, cancelParent := context.WithCancelCause(context.Background())
		factory.options.ParentRuntime = func(context.Context, subagent.ParentRef) (ParentRuntimeSnapshot, error) {
			return ParentRuntimeSnapshot{
				Model: "model-parent", PermissionMode: permission.ModeDefault,
				Registry: factory.options.Registry, ParentContext: parentCtx,
			}, nil
		}
		prepared, err := factory.Prepare(context.Background(), "task-parent-race", subagent.SubmitInput{
			Task: "linearize placement", Type: subagent.TypeDefined, Role: "reviewer",
			Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
			Parent: subagent.ParentRef{ConversationID: "parent"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return prepared.(*preparedSubagentTask), cancelParent
	}

	t.Run("detach wins", func(t *testing.T) {
		task, cancelParent := prepare(t)
		if !task.MoveToBackground() {
			t.Fatal("live parent task did not detach")
		}
		cancelParent(errors.New("late parent cancel"))
		select {
		case <-task.runtime.Context.Done():
			t.Fatalf("late parent cancellation crossed detached bridge: %v", context.Cause(task.runtime.Context))
		case <-time.After(20 * time.Millisecond):
		}
	})

	t.Run("parent wins", func(t *testing.T) {
		task, cancelParent := prepare(t)
		cancelParent(errors.New("parent won"))
		select {
		case <-task.runtime.Context.Done():
		case <-time.After(time.Second):
			t.Fatal("captured parent cancellation did not reach runtime")
		}
		if task.MoveToBackground() {
			t.Fatal("placement reported success after parent cancellation")
		}
		if task.runtime.ActiveTools.Current().Fingerprint != task.runtime.Profile.ForegroundTools.Fingerprint {
			t.Fatal("failed detach changed the active capability view")
		}
	})
}

func TestTaskRuntimeBackgroundStillObservesRunLifecycleCancellation(t *testing.T) {
	factory, _ := newTaskRuntimeFactoryFixture(t, 3)
	prepared, err := factory.Prepare(context.Background(), "task-background-cancel", subagent.SubmitInput{
		Task: "observe lifecycle", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementBackground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancelRun := context.WithCancelCause(context.Background())
	cancelRun(errors.New("manager shutdown"))
	completion := prepared.Run(runCtx, func(subagent.AgentEvent) error { return nil })
	if completion.Status != subagent.StatusCancelled || completion.StopReason != subagent.StopCancelled {
		t.Fatalf("background lifecycle cancellation = %#v", completion)
	}
}

func TestRunnerFactoryPrepareReturnsStructuredRoleModelAndPermissionErrors(t *testing.T) {
	tests := []struct {
		name   string
		role   string
		mutate func(*agentrole.ResolvedRole)
		code   subagent.ErrorCode
	}{
		{name: "unknown role", role: "missing", code: subagent.ErrUnknownRole},
		{name: "unmapped model", role: "reviewer", mutate: func(role *agentrole.ResolvedRole) {
			role.Definition.Model = agentrole.ModelHaiku
		}, code: subagent.ErrModelAliasUnavailable},
		{name: "permission escalation", role: "reviewer", mutate: func(role *agentrole.ResolvedRole) {
			role.Definition.PermissionMode = agentrole.PermissionPermissive
		}, code: subagent.ErrPermissionEscalation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factory, roles := newTaskRuntimeFactoryFixture(t, 3)
			if test.mutate != nil {
				role := roles.roles["reviewer"]
				test.mutate(&role)
				roles.roles["reviewer"] = role
			}
			_, err := factory.Prepare(context.Background(), "task-error", subagent.SubmitInput{
				Task: "validate preparation", Type: subagent.TypeDefined, Role: test.role,
				Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
				Parent: subagent.ParentRef{ConversationID: "parent"},
			})
			var safe *diagnostics.SafeError
			if !errors.As(err, &safe) || safe.Code != string(test.code) || !safe.Recoverable {
				t.Fatalf("Prepare() error = %#v, want recoverable %s", err, test.code)
			}
		})
	}
}

func TestTaskRuntimeConfirmationBrokersAreIsolated(t *testing.T) {
	factory, _ := newTaskRuntimeFactoryFixture(t, 3)
	prepare := func(id subagent.ID) *preparedSubagentTask {
		prepared, err := factory.Prepare(context.Background(), id, subagent.SubmitInput{
			Task: "wait independently", Type: subagent.TypeDefined, Role: "reviewer",
			Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
			Parent: subagent.ParentRef{ConversationID: "parent"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return prepared.(*preparedSubagentTask)
	}
	first, second := prepare("task-confirm-a"), prepare("task-confirm-b")
	request := events.ToolConfirmationRequest{
		ConfirmationID: "confirmation-same", CallID: "call-same",
		Scopes: []events.ConfirmationScopeDisplay{{Scope: "once", Available: true}},
	}
	type confirmationResult struct {
		decision events.ToolConfirmationDecision
		err      error
	}
	firstResult := make(chan confirmationResult, 1)
	secondResult := make(chan confirmationResult, 1)
	go func() {
		decision, err := first.runtime.Confirm.Request(context.Background(), request)
		firstResult <- confirmationResult{decision: decision, err: err}
	}()
	go func() {
		decision, err := second.runtime.Confirm.Request(context.Background(), request)
		secondResult <- confirmationResult{decision: decision, err: err}
	}()
	deadline := time.Now().Add(time.Second)
	for (first.runtime.Confirm.Pending() == nil || second.runtime.Confirm.Pending() == nil) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	allow := events.ToolConfirmationDecision{
		ConfirmationID: request.ConfirmationID, CallID: request.CallID,
		Action: events.PermissionAllowOnce, Allowed: true,
	}
	if err := first.ResolveConfirmation(allow); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-firstResult:
		if result.err != nil || !result.decision.Allowed {
			t.Fatalf("first confirmation result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("first confirmation did not resolve")
	}
	select {
	case result := <-secondResult:
		t.Fatalf("resolving first task also resolved second: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}
	deny := events.ToolConfirmationDecision{
		ConfirmationID: request.ConfirmationID, CallID: request.CallID,
		Action: events.PermissionDeny, Allowed: false,
	}
	if err := second.ResolveConfirmation(deny); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-secondResult:
		if result.err != nil || result.decision.Allowed {
			t.Fatalf("second confirmation result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("second confirmation did not resolve")
	}
}

func TestParallelTaskRuntimeMutableStateIsFullyIsolated(t *testing.T) {
	factory, _ := newTaskRuntimeFactoryFixture(t, 3)
	prepare := func(id subagent.ID) *preparedSubagentTask {
		t.Helper()
		prepared, err := factory.Prepare(context.Background(), id, subagent.SubmitInput{
			Task: "isolate mutable runtime", Type: subagent.TypeDefined, Role: "reviewer",
			Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
			Parent: subagent.ParentRef{ConversationID: "parent"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return prepared.(*preparedSubagentTask)
	}
	first, second := prepare("task-isolation-a"), prepare("task-isolation-b")
	t.Cleanup(func() {
		first.runtime.close(errors.New("test complete"))
		second.runtime.close(errors.New("test complete"))
	})

	if first.runtime.Conversation == second.runtime.Conversation ||
		first.runtime.Authorizer == second.runtime.Authorizer ||
		first.runtime.Authorizer.Session == second.runtime.Authorizer.Session ||
		first.runtime.Confirm == second.runtime.Confirm ||
		first.runtime.ReadCache == second.runtime.ReadCache ||
		first.runtime.ActiveTools == second.runtime.ActiveTools ||
		first.runtime.HookSessionID == second.runtime.HookSessionID {
		t.Fatal("parallel tasks share a mutable runtime owner")
	}

	first.runtime.Conversation.Messages = append(first.runtime.Conversation.Messages, conversation.Message{
		Role: conversation.RoleAssistant, Content: testSafeText("first-only-message"), CreatedAt: time.Now().UTC(),
	})
	if got := len(first.runtime.Conversation.Messages) - len(second.runtime.Conversation.Messages); got != 1 {
		t.Fatalf("conversation mutation crossed task boundary: delta=%d", got)
	}
	first.runtime.Authorizer.Session.Add(permission.Rule{
		Tool: "Read", Pattern: "first-only", MatchType: string(permission.MatchExact), Effect: string(permission.EffectAllow),
	})
	if len(first.runtime.Authorizer.Session.Rules()) != 1 || len(second.runtime.Authorizer.Session.Rules()) != 0 {
		t.Fatal("session permission mutation crossed task boundary")
	}
	if err := first.runtime.beginIteration(1); err != nil {
		t.Fatal(err)
	}
	if _, err := first.runtime.addUsage(&provider.Usage{InputTokens: 7, OutputTokens: 3}); err != nil {
		t.Fatal(err)
	}
	if first.runtime.snapshotIteration() != 1 || second.runtime.snapshotIteration() != 0 ||
		first.runtime.snapshotUsage().InputTokens != 7 || second.runtime.snapshotUsage() != (provider.Usage{}) {
		t.Fatal("iteration or usage mutation crossed task boundary")
	}

	cacheKey := tool.ReadCacheKey{Tool: "Read", ArgumentsFingerprint: "first-only-fingerprint"}
	if err := first.runtime.ReadCache.Put(cacheKey, nil, tool.CachedReadResult{
		State: tool.Completed, Status: tool.StatusSuccess, Summary: testSafeText("cached"), Preview: testSafeText("first-only-cache"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := first.runtime.ReadCache.Get(cacheKey, nil); !ok {
		t.Fatal("first task did not retain its read cache entry")
	}
	if _, ok := second.runtime.ReadCache.Get(cacheKey, nil); ok {
		t.Fatal("read cache entry crossed task boundary")
	}
}

func TestRunnerFactoryFirstProviderRequestObserverIsExactlyOnce(t *testing.T) {
	factory, _ := newTaskRuntimeFactoryFixture(t, 3)
	prepared, err := factory.Prepare(context.Background(), "task-observer", subagent.SubmitInput{
		Task: "observe first request", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := prepared.(*preparedSubagentTask)
	type observerKey struct{}
	requestCtx := context.WithValue(context.Background(), observerKey{}, "first-request")
	calls := 0
	task.SetFirstProviderRequestObserver(func(ctx context.Context) {
		calls++
		if ctx.Value(observerKey{}) != "first-request" {
			t.Fatalf("observer received the wrong request context")
		}
	})
	task.observeFirstProviderRequest(requestCtx)
	task.observeFirstProviderRequest(context.Background())
	if calls != 1 {
		t.Fatalf("first request observer calls = %d", calls)
	}
}

func TestForkPreparationUsesFrozenParentSnapshotsAndForcesBackground(t *testing.T) {
	factory, roles := newTaskRuntimeFactoryFixture(t, 3)
	now := time.Date(2026, 8, 15, 11, 0, 0, 0, time.UTC)
	parentConversation := conversation.NewConversation("parent-fork", now)
	parentConversation.Messages = append(parentConversation.Messages, conversation.Message{
		Role: conversation.RoleUser, Content: testSafeText("PARENT-AUDIT-MESSAGE"), CreatedAt: now,
	})
	parentConversation.UpdatedAt = now
	conversationSnapshot, err := conversation.TakeSnapshot(parentConversation)
	if err != nil {
		t.Fatal(err)
	}
	promptSnapshot, err := provider.CapturePromptPrefix(provider.ChatRequest{
		Model: "model-parent",
		StableSystem: []provider.SystemBlock{{
			Name: "parent-stable", Content: testSafeText("PARENT-STABLE"), Cacheable: true, Scope: prompt.ScopeProject,
		}},
		DynamicSystem: []provider.SystemBlock{{
			Name: "parent-dynamic", Content: testSafeText("PARENT-DYNAMIC"), Cacheable: false, Scope: prompt.ScopeRuntime,
		}},
		Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: testSafeText("PARENT-PROVIDER-MESSAGE")}},
		Tools:    []provider.ToolDefinition{{Name: "ParentOnly", Description: "captured parent tool", Schema: tool.Schema{Type: "object"}}},
		Cache:    provider.CachePolicy{EnablePromptCache: true, SystemBreakpointName: "parent-stable", CacheTools: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	factory.options.ParentRuntime = func(context.Context, subagent.ParentRef) (ParentRuntimeSnapshot, error) {
		return ParentRuntimeSnapshot{
			Model: "model-parent", PermissionMode: permission.ModeDefault, Registry: factory.options.Registry,
			Conversation: conversationSnapshot, Prompt: promptSnapshot,
		}, nil
	}
	prepared, err := factory.Prepare(context.Background(), "task-fork", subagent.SubmitInput{
		Task: "FORK-TASK-MESSAGE", Type: subagent.TypeFork,
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent-fork", RequestGeneration: 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := prepared.(*preparedSubagentTask)
	rolePrepared, err := factory.Prepare(context.Background(), "task-fork-role", subagent.SubmitInput{
		Task: "FORK-WITH-ROLE", Type: subagent.TypeFork, Role: "reviewer",
		Placement: subagent.PlacementBackground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent-fork", RequestGeneration: 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.Prepare(context.Background(), "task-fork-placement", subagent.SubmitInput{
		Task: "invalid placement", Type: subagent.TypeFork, Placement: subagent.PlacementIntent("sideways"),
		Origin: subagent.OriginTUI, Parent: subagent.ParentRef{ConversationID: "parent-fork", RequestGeneration: 9},
	}); err == nil {
		t.Fatal("fork accepted an invalid placement intent")
	}

	// Mutations after Prepare must not change either the audit snapshot or the
	// Provider truth source retained by the child.
	parentConversation.Messages[0].Content = testSafeText("LATE-PARENT-CONVERSATION-MUTATION")
	promptSnapshot.Messages[0].Content = testSafeText("LATE-PARENT-PROMPT-MUTATION")
	promptSnapshot.DynamicSystem[0].Content = testSafeText("LATE-PARENT-SYSTEM-MUTATION")
	mutatedRole := roles.roles["reviewer"]
	mutatedRole.Definition.Instructions = testSafeText("LATE-FORK-ROLE-MUTATION")
	roles.roles["reviewer"] = mutatedRole

	request, err := task.firstRequest()
	if err != nil {
		t.Fatal(err)
	}
	stable, dynamic := requestSystemText(request)
	if !strings.Contains(stable, "PARENT-STABLE") || !strings.Contains(dynamic, "PARENT-DYNAMIC") {
		t.Fatalf("fork request lost frozen parent system: stable=%q dynamic=%q", stable, dynamic)
	}
	for _, forbidden := range []string{"LATE-PARENT-CONVERSATION-MUTATION", "LATE-PARENT-PROMPT-MUTATION", "LATE-PARENT-SYSTEM-MUTATION", "PARENT-AUDIT-MESSAGE"} {
		if strings.Contains(stable+dynamic, forbidden) {
			t.Fatalf("fork system leaked late/audit value %q", forbidden)
		}
	}
	if len(request.Messages) != 2 || request.Messages[0].Content.Text() != "PARENT-PROVIDER-MESSAGE" ||
		request.Messages[1].Content.Text() != "FORK-TASK-MESSAGE" {
		t.Fatalf("fork Provider messages = %#v", request.Messages)
	}
	if !request.Cache.EnablePromptCache || request.Cache.CacheTools {
		t.Fatalf("fork cache policy = %#v, want system cache retained and changed tool cache disabled", request.Cache)
	}
	if !task.background || task.runtime.ParentBridge != nil ||
		task.runtime.ActiveTools.Current().Fingerprint != task.runtime.Profile.BackgroundTools.Fingerprint {
		t.Fatalf("fork placement/runtime = background=%v bridge=%#v", task.background, task.runtime.ParentBridge)
	}
	if task.runtime.Role != nil || task.Metadata().Role != "" || task.runtime.Profile.Depth != 1 {
		t.Fatalf("role-less fork metadata/runtime = %#v %#v", task.Metadata(), task.runtime)
	}
	if len(task.runtime.Conversation.Messages) != 2 || task.runtime.Conversation.Messages[0].Content.Text() != "PARENT-AUDIT-MESSAGE" ||
		task.runtime.Conversation.Messages[1].Content.Text() != "FORK-TASK-MESSAGE" {
		t.Fatalf("fork audit conversation = %#v", task.runtime.Conversation.Messages)
	}
	roleRequest, err := rolePrepared.(*preparedSubagentTask).firstRequest()
	if err != nil {
		t.Fatal(err)
	}
	_, roleDynamic := requestSystemText(roleRequest)
	if !strings.Contains(roleDynamic, "ROLE-BODY-ONLY") || strings.Contains(roleDynamic, "LATE-FORK-ROLE-MUTATION") {
		t.Fatalf("fork optional role was not frozen/appended dynamically: %q", roleDynamic)
	}
}

func TestSubagentLoopCompletesWithUsageObserverAndPairedHooks(t *testing.T) {
	factory, _ := newTaskRuntimeFactoryFixture(t, 3)
	hooks := newSubagentLifecycleRecorder()
	providerImpl := &subagentScriptProvider{scripts: [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: testSafeText("child completed")},
		{Type: provider.StreamEventUsage, Usage: &provider.Usage{InputTokens: 5, OutputTokens: 2, CacheReadInputTokens: 1}},
		{Type: provider.StreamEventDone},
	}}}
	factory.options.Provider = providerImpl
	factory.options.Hooks = hooks
	prepared, err := factory.Prepare(context.Background(), "task-loop", subagent.SubmitInput{
		Task: "run one child turn", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := prepared.(*preparedSubagentTask)
	observerCalls := 0
	task.SetFirstProviderRequestObserver(func(ctx context.Context) {
		observerCalls++
		if ctx == nil || ctx.Err() != nil {
			t.Fatalf("first request observer context = %#v", ctx)
		}
	})
	providerImpl.beforeFirst = func(context.Context) {
		if observerCalls != 1 {
			t.Fatalf("StreamChat started before observer: calls=%d", observerCalls)
		}
	}
	var agentEvents []subagent.AgentEvent
	completion := task.Run(context.Background(), func(event subagent.AgentEvent) error {
		agentEvents = append(agentEvents, event.Clone())
		return nil
	})
	if completion.Status != subagent.StatusCompleted || completion.StopReason != subagent.StopCompleted ||
		completion.Error != nil || completion.Summary.Text() != "child completed" {
		t.Fatalf("subagent completion = %#v", completion)
	}
	if completion.Usage.InputTokens != 5 || completion.Usage.OutputTokens != 2 || completion.Usage.CacheReadInputTokens != 1 {
		t.Fatalf("subagent completion usage = %#v", completion.Usage)
	}
	if observerCalls != 1 || len(providerImpl.Requests()) != 1 {
		t.Fatalf("observer/provider calls = %d/%d", observerCalls, len(providerImpl.Requests()))
	}
	gotHooks := hooks.snapshot()
	wantHooks := []string{
		"session_start:subagent:task-loop:new",
		"turn_start:subagent:task-loop:isolated_skill:default",
		"turn_end:subagent:task-loop:completed:completed",
		"session_end:subagent:task-loop:exit",
	}
	if strings.Join(gotHooks, "|") != strings.Join(wantHooks, "|") {
		t.Fatalf("hook lifecycle = %#v, want %#v", gotHooks, wantHooks)
	}
	var progress, usage, textEvent bool
	for _, event := range agentEvents {
		if err := event.Validate(); err != nil {
			t.Fatalf("invalid projected AgentEvent: %#v err=%v", event, err)
		}
		switch event.Kind {
		case events.AgentProgressed:
			progress = true
		case events.UsageUpdated:
			usage = event.Payload.Usage != nil && event.Payload.Usage.InputTokens == 5
		case events.TextDelta:
			textEvent = event.Range != nil && event.Range.From == 0 && event.Range.To == int64(len("child completed"))
		}
	}
	if !progress || !usage || !textEvent {
		t.Fatalf("subagent AgentEvents missing progress/usage/ranged text: %#v", agentEvents)
	}
}

func TestSubagentLoopExecutesOrdinaryToolBatchThroughScopedRuntime(t *testing.T) {
	hooks := newSubagentLifecycleRecorder()
	providerImpl := &subagentScriptProvider{scripts: [][]provider.StreamEvent{
		{
			{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("call-echo", "Echo", `{"value":"hello"}`)},
			{Type: provider.StreamEventUsage, Usage: &provider.Usage{InputTokens: 4, OutputTokens: 1}},
			{Type: provider.StreamEventDone},
		},
		{
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("tool loop complete")},
			{Type: provider.StreamEventUsage, Usage: &provider.Usage{InputTokens: 3, OutputTokens: 2}},
			{Type: provider.StreamEventDone},
		},
	}}
	factory, echo := newTaskLoopToolFactory(t, providerImpl, hooks, 3)
	prepared, err := factory.Prepare(context.Background(), "task-tool-loop", subagent.SubmitInput{
		Task: "use echo once", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var emitted []subagent.AgentEvent
	completion := prepared.Run(context.Background(), func(event subagent.AgentEvent) error {
		emitted = append(emitted, event.Clone())
		return nil
	})
	if completion.Status != subagent.StatusCompleted || completion.Summary.Text() != "tool loop complete" ||
		completion.Usage.InputTokens != 7 || completion.Usage.OutputTokens != 3 {
		t.Fatalf("tool loop completion = %#v", completion)
	}
	if echo.Calls() != 1 {
		t.Fatalf("task ScopedExecutor tool calls = %d, want 1", echo.Calls())
	}
	requests := providerImpl.Requests()
	if len(requests) != 2 {
		t.Fatalf("tool loop Provider requests = %d, want 2", len(requests))
	}
	var sawCall, sawResult bool
	for _, message := range requests[1].Messages {
		switch message.Role {
		case provider.ModelMessageRoleToolCall:
			sawCall = message.ToolCallID == "call-echo" && message.ToolName == "Echo"
		case provider.ModelMessageRoleToolResult:
			sawResult = message.ToolCallID == "call-echo" && message.ToolName == "Echo" &&
				strings.Contains(message.ToolResult.Text(), "echo:hello")
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("second request omitted ordered tool call/result: %#v", requests[1].Messages)
	}
	var pending, running, success bool
	for _, event := range emitted {
		switch event.Kind {
		case events.ToolPending:
			pending = true
		case events.ToolRunning:
			running = true
		case events.ToolSuccess:
			success = true
		}
	}
	if !pending || !running || !success {
		t.Fatalf("tool lifecycle events missing: %#v", emitted)
	}
	turnStarts, turnEnds := 0, 0
	for _, call := range hooks.snapshot() {
		if strings.HasPrefix(call, "turn_start:") {
			turnStarts++
		}
		if strings.HasPrefix(call, "turn_end:") {
			turnEnds++
		}
	}
	if turnStarts != 2 || turnEnds != 2 {
		t.Fatalf("tool loop hook turns = %d/%d: %#v", turnStarts, turnEnds, hooks.snapshot())
	}
}

func TestSubagentLoopRejectsRecursiveAgentBeforeAnySystemSubmission(t *testing.T) {
	providerImpl := &subagentScriptProvider{scripts: [][]provider.StreamEvent{
		{
			{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("call-recursive", tool.AgentToolName, `{"task":"nested","type":"defined","role":"reviewer"}`)},
			{Type: provider.StreamEventDone},
		},
		{
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("recovered from recursive rejection")},
			{Type: provider.StreamEventDone},
		},
	}}
	factory, echo := newTaskLoopToolFactory(t, providerImpl, hook.Noop(), 3)
	prepared, err := factory.Prepare(context.Background(), "task-recursive", subagent.SubmitInput{
		Task: "do not recurse", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	completion := prepared.Run(context.Background(), func(subagent.AgentEvent) error { return nil })
	if completion.Status != subagent.StatusCompleted || completion.Summary.Text() != "recovered from recursive rejection" {
		t.Fatalf("recursive recovery completion = %#v", completion)
	}
	if echo.Calls() != 0 {
		t.Fatalf("recursive rejection executed an unrelated ordinary tool %d times", echo.Calls())
	}
	requests := providerImpl.Requests()
	if len(requests) != 2 {
		t.Fatalf("recursive recovery Provider requests = %d, want 2", len(requests))
	}
	var recursiveResult provider.ModelMessage
	for _, message := range requests[1].Messages {
		if message.Role == provider.ModelMessageRoleToolResult && message.ToolCallID == "call-recursive" {
			recursiveResult = message
		}
	}
	if recursiveResult.ToolName != tool.AgentToolName ||
		!strings.Contains(recursiveResult.ToolResult.Text(), string(subagent.ErrRecursiveDelegate)) {
		t.Fatalf("recursive result = %#v", recursiveResult)
	}
}

func TestSubagentLoopUsesTaskConfirmationBrokerAndResumes(t *testing.T) {
	providerImpl := &subagentScriptProvider{scripts: [][]provider.StreamEvent{
		{
			{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("call-confirm", "Echo", `{"value":"confirmed"}`)},
			{Type: provider.StreamEventDone},
		},
		{
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("confirmed tool complete")},
			{Type: provider.StreamEventDone},
		},
	}}
	factory, echo := newTaskLoopToolFactory(t, providerImpl, hook.Noop(), 3)
	prepared, err := factory.Prepare(context.Background(), "task-confirm-loop", subagent.SubmitInput{
		Task: "confirm echo", Type: subagent.TypeDefined, Role: "strict-reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := prepared.(*preparedSubagentTask)
	waiting := make(chan events.ToolConfirmationRequest, 1)
	resolved := make(chan error, 1)
	completed := make(chan subagent.Completion, 1)
	go func() {
		result := task.Run(context.Background(), func(event subagent.AgentEvent) error {
			if event.Kind == events.ToolWaitingConfirmation && event.Payload.Confirmation != nil {
				request := *event.Payload.Confirmation
				waiting <- request
				// Resolve synchronously inside the sink callback. The Broker pending
				// identity must be installed before this actionable event exists.
				resolved <- task.ResolveConfirmation(events.ToolConfirmationDecision{
					ConfirmationID: request.ConfirmationID, CallID: request.CallID,
					Action: events.PermissionAllowOnce, Allowed: true,
				})
			}
			return nil
		})
		completed <- task.Settle(context.Background(), result)
	}()
	var request events.ToolConfirmationRequest
	select {
	case request = <-waiting:
	case <-time.After(time.Second):
		t.Fatal("task confirmation request was not published")
	}
	if request.CallID != "call-confirm" || request.ConfirmationID == "" || request.AllowPermanent {
		t.Fatalf("task confirmation request = %#v", request)
	}
	if err := <-resolved; err != nil {
		t.Fatal(err)
	}
	select {
	case completion := <-completed:
		if completion.Status != subagent.StatusCompleted || completion.Summary.Text() != "confirmed tool complete" {
			t.Fatalf("confirmed task completion = %#v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmed task did not resume")
	}
	if echo.Calls() != 1 {
		t.Fatalf("confirmed task tool calls = %d, want 1", echo.Calls())
	}
}

func TestSubagentLoopStopsAtIterationLimitWithoutExtraProviderRequest(t *testing.T) {
	hooks := newSubagentLifecycleRecorder()
	providerImpl := &subagentScriptProvider{scripts: [][]provider.StreamEvent{
		{
			{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("call-recursive-1", tool.AgentToolName, `{"task":"nested","type":"defined"}`)},
			{Type: provider.StreamEventDone},
		},
		{
			{Type: provider.StreamEventToolCall, ToolCall: testSafeToolCall("call-recursive-2", tool.AgentToolName, `{"task":"nested-again","type":"defined"}`)},
			{Type: provider.StreamEventDone},
		},
	}}
	factory, _ := newTaskLoopToolFactory(t, providerImpl, hooks, 2)
	prepared, err := factory.Prepare(context.Background(), "task-max-loop", subagent.SubmitInput{
		Task: "reach max", Type: subagent.TypeDefined, Role: "reviewer",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	completion := prepared.Run(context.Background(), func(subagent.AgentEvent) error { return nil })
	if completion.Status != subagent.StatusLimitReached || completion.StopReason != subagent.StopMaxIterations ||
		completion.Error == nil || completion.Error.Code != string(subagent.ErrLimitReached) {
		t.Fatalf("max-iteration completion = %#v", completion)
	}
	if len(providerImpl.Requests()) != 2 {
		t.Fatalf("max-iteration Provider calls = %d, want 2", len(providerImpl.Requests()))
	}
	calls := hooks.snapshot()
	turnStarts, turnEnds, maxEnds := 0, 0, 0
	for _, call := range calls {
		if strings.HasPrefix(call, "turn_start:") {
			turnStarts++
		}
		if strings.HasPrefix(call, "turn_end:") {
			turnEnds++
		}
		if strings.Contains(call, ":max_iterations:max_iterations") {
			maxEnds++
		}
	}
	if turnStarts != 2 || turnEnds != 2 || maxEnds != 1 {
		t.Fatalf("max-iteration hook lifecycle = %#v", calls)
	}
}

func TestSubagentPrepareRejectsContextBudgetBeforeProvider(t *testing.T) {
	factory, _ := newTaskRuntimeFactoryFixture(t, 3)
	providerImpl := &subagentScriptProvider{}
	policy, err := NewSkillHistoryPolicy(1<<30, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	factory.options.RequestBudgeter = contextmgr.NewRequestBudgeter()
	factory.options.ContextPolicy = policy
	factory.options.Provider = providerImpl
	_, err = factory.Prepare(context.Background(), "task-budget", subagent.SubmitInput{
		Task: "this request cannot fit a one-token planning budget",
		Type: subagent.TypeDefined, Role: "reviewer", Placement: subagent.PlacementForeground,
		Origin: subagent.OriginTUI, Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	var safe *diagnostics.SafeError
	if !errors.As(err, &safe) || safe.Code != string(subagent.ErrContextBudgetExceeded) || !safe.Recoverable {
		t.Fatalf("Prepare() budget error = %#v, want recoverable context_budget_exceeded", err)
	}
	if len(providerImpl.Requests()) != 0 {
		t.Fatalf("over-budget Prepare started Provider %d times", len(providerImpl.Requests()))
	}
}

func TestSubagentTurnRejectsHookPromptBeyondFrozenContextBudget(t *testing.T) {
	providerImpl := &subagentScriptProvider{scripts: [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: testSafeText("must not start")},
		{Type: provider.StreamEventDone},
	}}}
	lease := &subagentPromptLease{blocks: []hook.PromptBlock{{
		Name: "oversized-hook", Content: strings.Repeat("H", 512*1024), Source: "fixture",
	}}}
	hooks := newSubagentLifecycleRecorder()
	hooks.prompts = lease
	factory, _ := newTaskRuntimeFactoryFixture(t, 3)
	factory.options.Provider = providerImpl
	factory.options.Hooks = hooks
	policy, err := NewSkillHistoryPolicy(1<<30, 100_001, 1)
	if err != nil {
		t.Fatal(err)
	}
	factory.options.ContextPolicy = policy
	prepared, err := factory.Prepare(context.Background(), "task-hook-budget", subagent.SubmitInput{
		Task: "prepare fits before the turn hook is acquired",
		Type: subagent.TypeDefined, Role: "reviewer", Placement: subagent.PlacementForeground,
		Origin: subagent.OriginTUI, Parent: subagent.ParentRef{ConversationID: "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	completion := prepared.Run(context.Background(), func(subagent.AgentEvent) error { return nil })
	if completion.Status != subagent.StatusFailed || completion.StopReason != subagent.StopInternalError ||
		completion.Error == nil || completion.Error.Code != string(subagent.ErrContextBudgetExceeded) || !completion.Error.Recoverable {
		t.Fatalf("over-budget hook completion = %#v", completion)
	}
	if len(providerImpl.Requests()) != 0 {
		t.Fatalf("over-budget hook started Provider %d times", len(providerImpl.Requests()))
	}
	if commits, releases := lease.counts(); commits != 0 || releases != 1 {
		t.Fatalf("over-budget hook lease commits/releases = %d/%d", commits, releases)
	}
	wantHooks := []string{
		"session_start:subagent:task-hook-budget:new",
		"turn_start:subagent:task-hook-budget:isolated_skill:default",
		"turn_end:subagent:task-hook-budget:error:request_error",
		"session_end:subagent:task-hook-budget:exit",
	}
	if got := hooks.snapshot(); strings.Join(got, "|") != strings.Join(wantHooks, "|") {
		t.Fatalf("over-budget hook lifecycle = %#v, want %#v", got, wantHooks)
	}
}

func requestSystemText(request provider.ChatRequest) (string, string) {
	var stable, dynamic []string
	for _, block := range request.StableSystem {
		stable = append(stable, block.Content.Text())
	}
	for _, block := range request.DynamicSystem {
		dynamic = append(dynamic, block.Content.Text())
	}
	for _, block := range request.System {
		if block.Cacheable {
			stable = append(stable, block.Content.Text())
		} else {
			dynamic = append(dynamic, block.Content.Text())
		}
	}
	return strings.Join(stable, "\n"), strings.Join(dynamic, "\n")
}
