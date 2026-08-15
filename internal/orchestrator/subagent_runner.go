package orchestrator

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"xagent/internal/agentrole"
	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
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

// ParentRuntimeSnapshot is the small immutable boundary required to prepare a
// Defined child. It intentionally excludes parent messages, Skill Activity,
// mutable authorization state, confirmation state and usage accounting.
type ParentRuntimeSnapshot struct {
	Model          string
	PermissionMode permission.Mode
	PlanMode       bool
	ReadRoots      []string
	Registry       *tool.Registry
	// Conversation is the detached audit/display snapshot captured at the
	// parent's ordered commit boundary. Prompt is the exact already-budgeted
	// Provider truth source used by Fork children.
	Conversation conversation.ConversationSnapshot
	Prompt       provider.PromptPrefixSnapshot
	// ParentContext is the delegation request only. Manager supplies the
	// independent task lifecycle context later to PreparedTask.Run.
	ParentContext context.Context
}

// StableContextPreparer captures standard project instructions without
// compacting or otherwise reading a parent Conversation.
type StableContextPreparer interface {
	PrepareStable(context.Context) sessionctx.PreparedContext
}

type ParentRuntimeSnapshotter func(context.Context, subagent.ParentRef) (ParentRuntimeSnapshot, error)

// SubagentRunnerOptions contains shared immutable infrastructure and explicit
// snapshot seams. Per-task mutable objects are always allocated by Prepare.
// Provider/ContextManager/Hooks are retained here for the T25 non-interactive
// loop wiring; T22/T23 do not invoke them while preparing a task.
type SubagentRunnerOptions struct {
	Provider          provider.Provider
	Registry          *tool.Registry
	Executor          *tool.Executor
	ResultFactory     *tool.ResultFactory
	Roles             agentrole.Manager
	Models            agentrole.ModelCatalog
	Authorizer        *permission.Authorizer
	BackgroundPolicy  tool.BackgroundPolicy
	GlobalDenied      map[string]struct{}
	Limits            subagent.Limits
	RunOptions        RunOptions
	SessionContext    StableContextPreparer
	ContextManager    *contextmgr.Manager
	RequestBudgeter   contextmgr.RequestBudgeter
	ContextPolicy     SkillHistoryPolicy
	Thinking          config.ThinkingConfig
	Hooks             hook.Runtime
	RuntimeRedactor   *redact.RuntimeRedactor
	ParentRuntime     ParentRuntimeSnapshotter
	Clock             func() time.Time
	ChatStreamOptions provider.ChatStreamOptions
}

type SubagentRunnerFactory struct {
	options SubagentRunnerOptions
}

var (
	errSubagentContextBudgetExceeded = errors.New("subagent request exceeds its context budget")
	errSubagentFatalToolResult       = errors.New("subagent tool returned an unrecoverable result")
)

// NewSubagentRunnerFactory validates the shared assembly boundary. In
// particular, Registry must already be sealed and Executor/ResultFactory must
// be the same safe-candidate chain used to construct every ScopedExecutor.
func NewSubagentRunnerFactory(options SubagentRunnerOptions) (*SubagentRunnerFactory, error) {
	if options.Registry == nil || !options.Registry.IsSealed() {
		return nil, errors.New("subagent runner registry is unavailable or unsealed")
	}
	if options.Executor == nil || options.Executor.Registry != options.Registry || options.ResultFactory == nil {
		return nil, errors.New("subagent runner executor boundary is inconsistent")
	}
	if options.Roles == nil || options.Authorizer == nil {
		return nil, errors.New("subagent runner role or permission boundary is unavailable")
	}
	if options.RuntimeRedactor == nil {
		options.RuntimeRedactor = redact.NewRuntimeRedactor()
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	if options.Hooks == nil {
		options.Hooks = hook.Noop()
	}
	if options.Limits.MaxTaskBytes == 0 {
		options.Limits = subagent.DefaultLimits()
	}
	if err := options.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("subagent runner limits are invalid: %w", err)
	}
	if options.RunOptions.MaxIterations < 0 || options.RunOptions.MaxUnknownToolCalls <= 0 {
		return nil, errors.New("subagent runner loop limits are invalid")
	}
	if err := options.RequestBudgeter.Validate(); err != nil {
		return nil, fmt.Errorf("subagent runner request budgeter is invalid: %w", err)
	}
	if err := options.ContextPolicy.requireValid(); err != nil {
		return nil, fmt.Errorf("subagent runner context policy is invalid: %w", err)
	}
	if err := options.BackgroundPolicy.Validate(options.Registry); err != nil {
		return nil, fmt.Errorf("subagent runner background policy is invalid: %w", err)
	}
	options.BackgroundPolicy.AllowedNames = append([]string(nil), options.BackgroundPolicy.AllowedNames...)
	options.GlobalDenied = cloneDeniedNames(options.GlobalDenied)
	return &SubagentRunnerFactory{options: options}, nil
}

func (factory *SubagentRunnerFactory) Prepare(ctx context.Context, id subagent.ID, input subagent.SubmitInput) (subagent.PreparedTask, error) {
	if factory == nil || ctx == nil {
		return nil, errors.New("subagent runner factory is unavailable")
	}
	if !validRunnerIdentifier(string(id), factory.options.Limits.MaxIDBytes) {
		return nil, factory.safeError(subagent.ErrInvalidTask, "subagent task identity is invalid", true)
	}
	switch input.Type {
	case subagent.TypeDefined:
		return factory.prepareDefined(ctx, id, input)
	case subagent.TypeFork:
		return factory.prepareFork(ctx, id, input)
	default:
		return nil, factory.safeError(subagent.ErrInvalidType, "subagent execution type is invalid", true)
	}
}

func (factory *SubagentRunnerFactory) prepareDefined(ctx context.Context, id subagent.ID, input subagent.SubmitInput) (subagent.PreparedTask, error) {
	taskText := strings.TrimSpace(input.Task)
	if taskText == "" {
		return nil, factory.safeError(subagent.ErrInvalidTask, "defined task content is invalid", true)
	}
	if int64(len(taskText)) > factory.options.Limits.MaxTaskBytes {
		return nil, factory.safeError(subagent.ErrTaskTooLarge, "defined task content exceeds its limit", true)
	}
	placement := input.Placement
	if placement == subagent.PlacementDefault {
		placement = subagent.PlacementForeground
	}
	if placement != subagent.PlacementForeground && placement != subagent.PlacementBackground {
		return nil, factory.safeError(subagent.ErrInvalidPlacement, "defined task placement is invalid", true)
	}
	roleName := strings.ToLower(strings.TrimSpace(input.Role))
	if !validRunnerIdentifier(roleName, factory.options.Limits.MaxRoleNameBytes) {
		return nil, factory.safeError(subagent.ErrUnknownRole, "defined task role is invalid", true)
	}
	resolved, ok := factory.options.Roles.Resolve(roleName)
	if !ok {
		return nil, factory.safeError(subagent.ErrUnknownRole, "defined task role is unavailable", true)
	}
	role := cloneResolvedRole(resolved)

	parent, err := factory.parentRuntime(ctx, input.Parent)
	if err != nil {
		return nil, factory.safeError(subagent.ErrParentSnapshotUnavailable, "defined parent runtime snapshot is unavailable", true)
	}
	modelAlias := role.Definition.Model
	if modelAlias == "" {
		modelAlias = agentrole.ModelInherit
	}
	model, err := factory.options.Models.Resolve(modelAlias, parent.Model)
	if err != nil {
		return nil, factory.safeError(subagent.ErrModelAliasUnavailable,
			fmt.Sprintf("role %s model alias %s is unavailable", roleName, modelAlias), true)
	}
	rolePermission := role.Definition.PermissionMode
	if rolePermission == "" {
		rolePermission = agentrole.PermissionInherit
	}
	effectivePermission, err := permission.RestrictMode(parent.PermissionMode, rolePermission)
	if err != nil {
		return nil, factory.safeError(subagent.ErrPermissionEscalation,
			fmt.Sprintf("role %s permission mode cannot widen its parent", roleName), true)
	}

	foreground, background, err := tool.BuildCapabilityViews(
		parent.Registry, &role.Definition, factory.options.BackgroundPolicy,
		factory.options.GlobalDenied, parent.PlanMode,
	)
	if err != nil {
		return nil, factory.safeError(subagent.ErrInternal, "defined capability views are unavailable", false)
	}
	activeTools, err := tool.NewCapabilitySwitch(foreground, background)
	if err != nil {
		return nil, factory.safeError(subagent.ErrInternal, "defined capability switch is unavailable", false)
	}
	taskScope, err := factory.options.Authorizer.NewTaskScope(permission.TaskScopeOptions{
		ScopeID: string(id), Mode: effectivePermission, AllowPermanent: false,
	})
	if err != nil {
		return nil, factory.safeError(subagent.ErrInternal, "defined permission scope is unavailable", false)
	}
	confirmation, err := subagent.NewTaskConfirmationBroker(id)
	if err != nil {
		return nil, err
	}
	readCache, err := tool.NewReadCache(tool.ReadCacheLimits{
		MaxEntries:              factory.options.Limits.ReadCacheMaxEntries,
		MaxBytes:                factory.options.Limits.ReadCacheMaxBytes,
		MaxValueBytes:           factory.options.Limits.ReadCacheMaxValueBytes,
		MaxDependenciesPerEntry: factory.options.Limits.ReadCacheMaxDependenciesPerEntry,
	}, factory.options.ResultFactory)
	if err != nil {
		confirmation.Close(err)
		return nil, factory.safeError(subagent.ErrInternal, "defined read cache is unavailable", false)
	}
	scopedExecutor, err := tool.NewScopedExecutor(factory.options.Executor, taskScope.Verifier, string(id), activeTools, readCache)
	if err != nil {
		readCache.Close()
		confirmation.Close(err)
		return nil, factory.safeError(subagent.ErrInternal, "defined scoped executor is unavailable", false)
	}

	maxIterations := effectiveMaxIterations(factory.options.RunOptions.MaxIterations, role.Definition.MaxIterations)
	now := factory.options.Clock()
	hookSessionID := "subagent:" + string(id)
	childConversation := conversation.NewConversation(hookSessionID, now)
	childConversation.Messages = append(childConversation.Messages, conversation.Message{
		Role: conversation.RoleUser, Content: factory.options.RuntimeRedactor.Redact(taskText), CreatedAt: now,
	})
	childConversation.UpdatedAt = now

	runtimeCtx, cancel := context.WithCancelCause(context.Background())
	profile := RuntimeProfile{
		Model: model, PermissionMode: effectivePermission, PlanMode: parent.PlanMode, Depth: 1,
		ReadRoots: append([]string(nil), parent.ReadRoots...), Persist: false, UpdateMemory: false,
		MaxUnknownToolCalls: factory.options.RunOptions.MaxUnknownToolCalls,
		ForegroundTools:     foreground.Clone(), BackgroundTools: background.Clone(), MaxIterations: maxIterations,
		MaxRequestBytes:          factory.options.ContextPolicy.maxSessionBytes,
		MaxRequestPlanningTokens: factory.options.ContextPolicy.modelWindowTokens - factory.options.ContextPolicy.autoMarginTokens,
	}
	runtime := &TaskRuntimeState{
		TaskID: id, Parent: input.Parent, Role: role, Conversation: childConversation, Profile: profile,
		ActiveTools: activeTools, Authorizer: taskScope.Authorizer, Executor: scopedExecutor,
		Confirm: confirmation, ReadCache: readCache, HookSessionID: hookSessionID,
		Context: runtimeCtx, Cancel: cancel, RequestBudgeter: factory.options.RequestBudgeter,
	}
	backgroundTask := placement == subagent.PlacementBackground
	if !backgroundTask && parent.ParentContext != nil {
		runtime.ParentBridge = newParentCancelBridge(parent.ParentContext, cancel)
	}
	if backgroundTask {
		changed, _ := runtime.ActiveTools.MoveToBackground()
		if !changed {
			runtime.close(errors.New("background capability switch failed"))
			return nil, factory.safeError(subagent.ErrInternal, "defined background capability switch is unavailable", false)
		}
	}
	prefix, err := factory.definedPromptPrefix(ctx, runtime)
	if err != nil {
		runtime.close(err)
		return nil, factory.safeError(subagent.ErrContextBudgetExceeded, "defined prompt prefix is invalid", true)
	}
	runtime.Prompt = prefix

	task := &preparedSubagentTask{
		factory: factory, runtime: runtime, projectRoot: factory.options.Executor.ProjectRoot,
		metadata: preparedMetadata(input.Type, roleName, role, profile), background: backgroundTask,
	}
	if err := task.validatePreparedBudget(ctx); err != nil {
		runtime.close(err)
		if errors.Is(err, errSubagentContextBudgetExceeded) {
			return nil, factory.safeError(subagent.ErrContextBudgetExceeded, "defined child request exceeds its context budget", true)
		}
		return nil, factory.safeError(subagent.ErrInternal, "defined child request budget could not be measured", false)
	}
	return task, nil
}

func (factory *SubagentRunnerFactory) prepareFork(ctx context.Context, id subagent.ID, input subagent.SubmitInput) (subagent.PreparedTask, error) {
	taskText := strings.TrimSpace(input.Task)
	if taskText == "" {
		return nil, factory.safeError(subagent.ErrInvalidTask, "fork task content is invalid", true)
	}
	if int64(len(taskText)) > factory.options.Limits.MaxTaskBytes {
		return nil, factory.safeError(subagent.ErrTaskTooLarge, "fork task content exceeds its limit", true)
	}
	if input.Placement != subagent.PlacementDefault && input.Placement != subagent.PlacementForeground && input.Placement != subagent.PlacementBackground {
		return nil, factory.safeError(subagent.ErrInvalidPlacement, "fork task placement is invalid", true)
	}
	parent, err := factory.parentRuntime(ctx, input.Parent)
	if err != nil {
		return nil, factory.safeError(subagent.ErrParentSnapshotUnavailable, "fork parent runtime snapshot is unavailable", true)
	}
	if err := parent.Prompt.Validate(); err != nil {
		return nil, factory.safeError(subagent.ErrParentSnapshotUnavailable, "fork prompt snapshot is unavailable", true)
	}

	roleName := strings.ToLower(strings.TrimSpace(input.Role))
	var role *agentrole.ResolvedRole
	if roleName != "" {
		if !validRunnerIdentifier(roleName, factory.options.Limits.MaxRoleNameBytes) {
			return nil, factory.safeError(subagent.ErrUnknownRole, "fork task role is invalid", true)
		}
		resolved, ok := factory.options.Roles.Resolve(roleName)
		if !ok {
			return nil, factory.safeError(subagent.ErrUnknownRole, "fork task role is unavailable", true)
		}
		role = cloneResolvedRole(resolved)
	}

	model := parent.Prompt.Model
	effectivePermission := parent.PermissionMode
	maxIterations := factory.options.RunOptions.MaxIterations
	var roleDefinition *agentrole.Definition
	if role != nil {
		roleDefinition = &role.Definition
		modelAlias := role.Definition.Model
		if modelAlias == "" {
			modelAlias = agentrole.ModelInherit
		}
		model, err = factory.options.Models.Resolve(modelAlias, parent.Prompt.Model)
		if err != nil {
			return nil, factory.safeError(subagent.ErrModelAliasUnavailable,
				fmt.Sprintf("role %s model alias %s is unavailable", roleName, modelAlias), true)
		}
		rolePermission := role.Definition.PermissionMode
		if rolePermission == "" {
			rolePermission = agentrole.PermissionInherit
		}
		effectivePermission, err = permission.RestrictMode(parent.PermissionMode, rolePermission)
		if err != nil {
			return nil, factory.safeError(subagent.ErrPermissionEscalation,
				fmt.Sprintf("role %s permission mode cannot widen its parent", roleName), true)
		}
		maxIterations = effectiveMaxIterations(maxIterations, role.Definition.MaxIterations)
	}

	foreground, background, err := tool.BuildCapabilityViews(
		parent.Registry, roleDefinition, factory.options.BackgroundPolicy,
		factory.options.GlobalDenied, parent.PlanMode,
	)
	if err != nil {
		return nil, factory.safeError(subagent.ErrInternal, "fork capability views are unavailable", false)
	}
	activeTools, err := tool.NewCapabilitySwitch(foreground, background)
	if err != nil {
		return nil, factory.safeError(subagent.ErrInternal, "fork capability switch is unavailable", false)
	}
	changed, _ := activeTools.MoveToBackground()
	if !changed {
		return nil, factory.safeError(subagent.ErrInternal, "fork background capability switch is unavailable", false)
	}
	taskScope, err := factory.options.Authorizer.NewTaskScope(permission.TaskScopeOptions{
		ScopeID: string(id), Mode: effectivePermission, AllowPermanent: false,
	})
	if err != nil {
		return nil, factory.safeError(subagent.ErrInternal, "fork permission scope is unavailable", false)
	}
	confirmation, err := subagent.NewTaskConfirmationBroker(id)
	if err != nil {
		return nil, err
	}
	readCache, err := tool.NewReadCache(tool.ReadCacheLimits{
		MaxEntries:              factory.options.Limits.ReadCacheMaxEntries,
		MaxBytes:                factory.options.Limits.ReadCacheMaxBytes,
		MaxValueBytes:           factory.options.Limits.ReadCacheMaxValueBytes,
		MaxDependenciesPerEntry: factory.options.Limits.ReadCacheMaxDependenciesPerEntry,
	}, factory.options.ResultFactory)
	if err != nil {
		confirmation.Close(err)
		return nil, factory.safeError(subagent.ErrInternal, "fork read cache is unavailable", false)
	}
	scopedExecutor, err := tool.NewScopedExecutor(factory.options.Executor, taskScope.Verifier, string(id), activeTools, readCache)
	if err != nil {
		readCache.Close()
		confirmation.Close(err)
		return nil, factory.safeError(subagent.ErrInternal, "fork scoped executor is unavailable", false)
	}

	now := factory.options.Clock()
	hookSessionID := "subagent:" + string(id)
	childConversation := parent.Conversation.MaterializeEphemeral(hookSessionID, now)
	if childConversation == nil {
		readCache.Close()
		confirmation.Close(conversation.ErrConversationSnapshotInvalid)
		return nil, factory.safeError(subagent.ErrParentSnapshotUnavailable, "fork conversation snapshot is unavailable", true)
	}
	messageOffset := len(childConversation.Messages)
	childConversation.Messages = append(childConversation.Messages, conversation.Message{
		Role: conversation.RoleUser, Content: factory.options.RuntimeRedactor.Redact(taskText), CreatedAt: now,
	})
	childConversation.UpdatedAt = now

	prefix, err := cloneForkPromptPrefix(parent.Prompt, model)
	if err != nil {
		readCache.Close()
		confirmation.Close(err)
		return nil, factory.safeError(subagent.ErrParentSnapshotUnavailable, "fork prompt snapshot is invalid", true)
	}
	runtimeCtx, cancel := context.WithCancelCause(context.Background())
	profile := RuntimeProfile{
		Model: model, PermissionMode: effectivePermission, PlanMode: parent.PlanMode, Depth: 1,
		ReadRoots: append([]string(nil), parent.ReadRoots...), Persist: false, UpdateMemory: false,
		MaxUnknownToolCalls: factory.options.RunOptions.MaxUnknownToolCalls,
		ForegroundTools:     foreground.Clone(), BackgroundTools: background.Clone(), MaxIterations: maxIterations,
		MaxRequestBytes:          factory.options.ContextPolicy.maxSessionBytes,
		MaxRequestPlanningTokens: factory.options.ContextPolicy.modelWindowTokens - factory.options.ContextPolicy.autoMarginTokens,
	}
	runtime := &TaskRuntimeState{
		TaskID: id, Parent: input.Parent, Role: role, Conversation: childConversation, Profile: profile,
		ActiveTools: activeTools, Authorizer: taskScope.Authorizer, Executor: scopedExecutor,
		Confirm: confirmation, ReadCache: readCache, HookSessionID: hookSessionID,
		Context: runtimeCtx, Cancel: cancel, Prompt: prefix, RequestBudgeter: factory.options.RequestBudgeter,
	}
	task := &preparedSubagentTask{
		factory: factory, runtime: runtime, projectRoot: factory.options.Executor.ProjectRoot,
		metadata: preparedMetadata(input.Type, roleName, role, profile), background: true,
		conversationMessageOffset: messageOffset, appendRoleToPrompt: role != nil,
	}
	if err := task.validatePreparedBudget(ctx); err != nil {
		runtime.close(err)
		if errors.Is(err, errSubagentContextBudgetExceeded) {
			return nil, factory.safeError(subagent.ErrContextBudgetExceeded, "fork child request exceeds its context budget", true)
		}
		return nil, factory.safeError(subagent.ErrInternal, "fork child request budget could not be measured", false)
	}
	return task, nil
}

func cloneForkPromptPrefix(snapshot provider.PromptPrefixSnapshot, model string) (provider.PromptPrefixSnapshot, error) {
	cloned, err := clonePromptPrefixSnapshot(snapshot)
	if err != nil {
		return provider.PromptPrefixSnapshot{}, err
	}
	request := cloned.BuildChild(nil, nil, cloned.Tools)
	request.Model = model
	return provider.CapturePromptPrefix(request)
}

func (factory *SubagentRunnerFactory) parentRuntime(ctx context.Context, parentRef subagent.ParentRef) (ParentRuntimeSnapshot, error) {
	parent := ParentRuntimeSnapshot{
		Model: factory.options.Models.Default, PermissionMode: permission.ModeDefault,
		Registry: factory.options.Registry,
	}
	if factory.options.Executor.ProjectRoot != "" {
		parent.ReadRoots = []string{factory.options.Executor.ProjectRoot}
	}
	if factory.options.ParentRuntime != nil {
		captured, err := factory.options.ParentRuntime(ctx, parentRef)
		if err != nil {
			return ParentRuntimeSnapshot{}, err
		}
		parent = captured
		if parent.Model == "" {
			parent.Model = factory.options.Models.Default
		}
		if parent.PermissionMode == "" {
			parent.PermissionMode = permission.ModeDefault
		}
		if parent.Registry == nil {
			parent.Registry = factory.options.Registry
		}
		parent.ReadRoots = append([]string(nil), parent.ReadRoots...)
	}
	if parent.Registry == nil || !parent.Registry.IsSealed() {
		return ParentRuntimeSnapshot{}, errors.New("parent registry is unavailable")
	}
	return parent, nil
}

func (factory *SubagentRunnerFactory) definedPromptPrefix(ctx context.Context, runtime *TaskRuntimeState) (provider.PromptPrefixSnapshot, error) {
	optional := []prompt.Section(nil)
	if factory.options.SessionContext != nil {
		prepared := factory.options.SessionContext.PrepareStable(ctx)
		optional = append(optional, prepared.StableSections...)
	}
	sections := prompt.StableSections(redactStableSections(factory.options.RuntimeRedactor, optional))
	stable := make([]provider.SystemBlock, 0, len(sections))
	for _, section := range sections {
		stable = append(stable, provider.SystemBlock{
			Name: section.Name, Content: factory.options.RuntimeRedactor.Redact(section.Content), Cacheable: true,
		})
	}
	dynamic := []provider.SystemBlock{{
		Name: "subagent-role", Content: runtime.Role.Definition.Instructions, Cacheable: false,
	}}
	current := runtime.ActiveTools.Current()
	request := provider.ChatRequest{
		Model: runtime.Profile.Model, StableSystem: stable, DynamicSystem: dynamic,
		Thinking: factory.options.Thinking, Tools: toolDefinitionsFromRegistry(current.Registry),
		Cache: provider.CachePolicy{EnablePromptCache: true, CacheTools: true},
	}
	return provider.CapturePromptPrefix(request)
}

type preparedSubagentTask struct {
	factory     *SubagentRunnerFactory
	runtime     *TaskRuntimeState
	projectRoot string
	metadata    subagent.PreparedMetadata
	// Fork requests start from Prompt.Messages, not from the separately
	// materialized audit Conversation. Only the child-owned suffix at and after
	// this offset is appended on every round.
	conversationMessageOffset int
	appendRoleToPrompt        bool

	mu                   sync.Mutex
	run                  bool
	firstRequestObserver func(context.Context)
	firstObserved        bool
	confirmationSequence uint64

	placementMu sync.Mutex
	background  bool
}

// taskEventBridge continuously drains the shared Agent Loop event channel,
// strips the main-session envelope, and projects only the bounded child event
// contract. A failed consumer cancels the task but never leaves Provider or
// tool producers blocked behind an abandoned channel.
type taskEventBridge struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	sink   subagent.EventSink
	limit  int64
	out    chan events.Event
	done   chan struct{}

	mu       sync.Mutex
	err      error
	offsets  map[events.Type]int64
	finished bool
}

func newTaskEventBridge(ctx context.Context, cancel context.CancelCauseFunc, sink subagent.EventSink, limit int64, buffer int) *taskEventBridge {
	if buffer < 1 {
		buffer = 1
	}
	bridge := &taskEventBridge{
		ctx: ctx, cancel: cancel, sink: sink, limit: limit,
		out: make(chan events.Event, buffer), done: make(chan struct{}), offsets: make(map[events.Type]int64),
	}
	go bridge.drain()
	return bridge
}

func (bridge *taskEventBridge) drain() {
	defer close(bridge.done)
	for event := range bridge.out {
		bridge.project(event)
	}
}

func (bridge *taskEventBridge) project(event events.Event) {
	bridge.mu.Lock()
	failed := bridge.err != nil
	bridge.mu.Unlock()
	if failed || bridge.sink == nil {
		return
	}
	event.IndependentID = ""
	if event.Type == events.TextDelta || event.Type == events.ThinkingDelta {
		value := event.Text.Text()
		for len(value) > 0 {
			part, rest := boundedUTF8Prefix(value, bridge.limit)
			if part == "" {
				part, rest = value, ""
			}
			bridge.mu.Lock()
			from := bridge.offsets[event.Type]
			to := from + int64(len(part))
			bridge.offsets[event.Type] = to
			bridge.mu.Unlock()
			projected := events.Clone(event)
			projected.Text = redact.NewRuntimeRedactor().Redact(part)
			if err := bridge.sink(subagent.AgentEvent{
				Kind: projected.Type, Payload: projected, Range: &subagent.DeltaRange{From: from, To: to},
			}); err != nil {
				bridge.fail(err)
				return
			}
			value = rest
		}
		return
	}
	if err := bridge.sink(subagent.AgentEvent{Kind: event.Type, Payload: events.Clone(event)}); err != nil {
		bridge.fail(err)
	}
}

func (bridge *taskEventBridge) fail(err error) {
	if err == nil {
		return
	}
	bridge.mu.Lock()
	if bridge.err == nil {
		bridge.err = err
	}
	bridge.mu.Unlock()
	if bridge.cancel != nil {
		bridge.cancel(err)
	}
}

func (bridge *taskEventBridge) emit(event events.Event) bool {
	if bridge == nil {
		return false
	}
	select {
	case bridge.out <- event:
		return true
	case <-bridge.ctx.Done():
		return false
	}
}

func (bridge *taskEventBridge) close() error {
	if bridge == nil {
		return nil
	}
	bridge.mu.Lock()
	if !bridge.finished {
		bridge.finished = true
		close(bridge.out)
	}
	bridge.mu.Unlock()
	<-bridge.done
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.err
}

func boundedUTF8Prefix(value string, limit int64) (string, string) {
	if value == "" || limit <= 0 || int64(len(value)) <= limit {
		return value, ""
	}
	end := int(limit)
	for end > 0 && value[end]&0xc0 == 0x80 {
		end--
	}
	if end == 0 {
		_, size := utf8.DecodeRuneInString(value)
		end = size
	}
	return value[:end], value[end:]
}

func (task *preparedSubagentTask) Metadata() subagent.PreparedMetadata {
	if task == nil {
		return subagent.PreparedMetadata{}
	}
	return task.metadata
}

func (task *preparedSubagentTask) MoveToBackground() bool {
	if task == nil || task.runtime == nil || task.runtime.ActiveTools == nil {
		return false
	}
	task.placementMu.Lock()
	defer task.placementMu.Unlock()
	if task.background {
		return false
	}
	// Detachment must win before the capability pointer is changed. If parent
	// cancellation has already begun, stop() fails and the task remains
	// foreground/cancelled. Run's independent lifecycle bridge is untouched.
	if task.runtime.ParentBridge != nil && !task.runtime.ParentBridge.Detach() {
		return false
	}
	changed, _ := task.runtime.ActiveTools.MoveToBackground()
	if !changed {
		return false
	}
	task.background = true
	return true
}

func (task *preparedSubagentTask) ResolveConfirmation(decision events.ToolConfirmationDecision) error {
	if task == nil || task.runtime == nil || task.runtime.Confirm == nil {
		return errors.New("task confirmation broker is unavailable")
	}
	return task.runtime.Confirm.Resolve(decision)
}

func (task *preparedSubagentTask) SetFirstProviderRequestObserver(observer func(context.Context)) {
	if task == nil {
		return
	}
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.run || task.firstObserved {
		return
	}
	task.firstRequestObserver = observer
}

// observeFirstProviderRequest is the exact T25 handoff seam. The loop calls it
// synchronously once immediately before its first StreamChat invocation.
func (task *preparedSubagentTask) observeFirstProviderRequest(ctx context.Context) {
	if task == nil {
		return
	}
	task.mu.Lock()
	if task.firstObserved {
		task.mu.Unlock()
		return
	}
	task.firstObserved = true
	observer := task.firstRequestObserver
	task.firstRequestObserver = nil
	task.mu.Unlock()
	if observer != nil {
		observer(ctx)
	}
}

// firstRequest constructs the current placement-aware request solely from the
// frozen prefix and the task-owned Conversation. It never re-reads the parent,
// role Manager, Skill Activity, global authorization or global usage.
func (task *preparedSubagentTask) firstRequest() (provider.ChatRequest, error) {
	return task.requestForIteration(1)
}

func (task *preparedSubagentTask) validatePreparedBudget(ctx context.Context) error {
	request, err := task.requestForIteration(1)
	if err != nil {
		return err
	}
	return task.validateRequestBudget(ctx, request)
}

func (task *preparedSubagentTask) validateRequestBudget(ctx context.Context, request provider.ChatRequest) error {
	if task == nil || task.runtime == nil {
		return errors.New("subagent request budget is unavailable")
	}
	if ctx == nil {
		return errors.New("subagent request budget context is nil")
	}
	measure, err := task.runtime.RequestBudgeter.MeasureRequest(ctx, request)
	if err != nil {
		return err
	}
	if measure.Bytes < 0 || measure.PlanningTokens < 0 {
		return errors.New("subagent request budget measure is negative")
	}
	profile := task.runtime.Profile
	if profile.MaxRequestBytes <= 0 || profile.MaxRequestPlanningTokens <= 0 {
		return errors.New("subagent request budget limits are invalid")
	}
	if measure.Bytes > profile.MaxRequestBytes || measure.PlanningTokens > profile.MaxRequestPlanningTokens {
		return fmt.Errorf("%w: bytes=%d/%d planning_tokens=%d/%d",
			errSubagentContextBudgetExceeded,
			measure.Bytes, profile.MaxRequestBytes,
			measure.PlanningTokens, profile.MaxRequestPlanningTokens,
		)
	}
	return nil
}

func (task *preparedSubagentTask) requestForIteration(iteration int) (provider.ChatRequest, error) {
	if task == nil || task.runtime == nil || task.factory == nil {
		return provider.ChatRequest{}, errors.New("prepared task is unavailable")
	}
	blocks, err := prompt.DynamicBlocksFromSafe(prompt.SafeDynamicRequest{
		Mode: promptMode(task.runtime.Profile.PlanMode), Iteration: iteration,
		ProjectRoot: task.factory.options.RuntimeRedactor.Redact(task.projectRoot),
	})
	if err != nil {
		return provider.ChatRequest{}, err
	}
	dynamic := make([]provider.SystemBlock, 0, len(blocks))
	for _, block := range blocks {
		dynamic = append(dynamic, provider.SystemBlock{
			Name: block.Name, Content: task.factory.options.RuntimeRedactor.Redact(block.Content), Cacheable: false,
		})
	}
	if task.appendRoleToPrompt && task.runtime.Role != nil {
		dynamic = append([]provider.SystemBlock{{
			Name: "subagent-role", Content: task.runtime.Role.Definition.Instructions, Cacheable: false,
		}}, dynamic...)
	}
	messages, err := taskModelMessagesFrom(task.runtime.Conversation, task.conversationMessageOffset)
	if err != nil {
		return provider.ChatRequest{}, err
	}
	for index := range messages {
		if messages[index].Role != provider.ModelMessageRoleToolResult {
			continue
		}
		if modelContent, ok := task.runtime.modelToolResult(messages[index].ToolCallID); ok {
			messages[index].Content = modelContent
			messages[index].ToolResult = modelContent
		}
	}
	current := task.runtime.ActiveTools.Current()
	request := task.runtime.Prompt.BuildChild(dynamic, messages, toolDefinitionsFromRegistry(current.Registry))
	if err := request.Validate(); err != nil {
		return provider.ChatRequest{}, err
	}
	return request, nil
}

func (task *preparedSubagentTask) requestForTurn(ctx context.Context, iteration int, ref hook.ExecutionRef) (provider.ChatRequest, error) {
	request, err := task.requestForIteration(iteration)
	if err != nil {
		return provider.ChatRequest{}, err
	}
	if ref.ExecutionID == "" {
		return task.finalizeTurnRequest(ctx, request, nil)
	}
	lease, err := task.factory.options.Hooks.AcquirePrompts(ctx, ref)
	if err != nil {
		return provider.ChatRequest{}, err
	}
	if lease == nil {
		return task.finalizeTurnRequest(ctx, request, nil)
	}
	blocks := lease.Blocks()
	if len(blocks) == 0 {
		lease.Release()
		return task.finalizeTurnRequest(ctx, request, nil)
	}
	appended := make([]provider.SystemBlock, 0, len(blocks))
	for _, block := range blocks {
		name := strings.TrimSpace(task.factory.options.RuntimeRedactor.Text(block.Name))
		content := task.factory.options.RuntimeRedactor.Redact(block.Content)
		if name == "" || strings.TrimSpace(content.Text()) == "" {
			continue
		}
		appended = append(appended, provider.SystemBlock{Name: name, Content: content, Cacheable: false})
	}
	if len(appended) == 0 {
		lease.Release()
		return task.finalizeTurnRequest(ctx, request, nil)
	}
	if request.System != nil {
		request.System = append(request.System, appended...)
	} else {
		request.DynamicSystem = append(request.DynamicSystem, appended...)
	}
	return task.finalizeTurnRequest(ctx, request, lease)
}

func (task *preparedSubagentTask) finalizeTurnRequest(
	ctx context.Context,
	request provider.ChatRequest,
	lease hook.PromptLease,
) (provider.ChatRequest, error) {
	if err := request.Validate(); err != nil {
		if lease != nil {
			lease.Release()
		}
		return provider.ChatRequest{}, err
	}
	if err := task.validateRequestBudget(ctx, request); err != nil {
		if lease != nil {
			lease.Release()
		}
		return provider.ChatRequest{}, err
	}
	if lease != nil {
		request.Observer = newPromptLeaseObserver(lease)
	}
	return request, nil
}

func (task *preparedSubagentTask) streamChat(ctx context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	if task == nil || task.factory == nil || task.factory.options.Provider == nil {
		return nil, errors.New("subagent Provider is unavailable")
	}
	if ctx == nil {
		return nil, errors.New("subagent Provider context is unavailable")
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	if configured, ok := task.factory.options.Provider.(provider.ChatStreamOptionsProvider); ok {
		return configured.StreamChatWithOptions(ctx, request, task.factory.options.ChatStreamOptions)
	}
	return task.factory.options.Provider.StreamChat(ctx, request)
}

func (task *preparedSubagentTask) Run(ctx context.Context, sink subagent.EventSink) subagent.Completion {
	if task == nil || task.runtime == nil || task.factory == nil {
		return subagent.Completion{}
	}
	task.mu.Lock()
	if task.run {
		task.mu.Unlock()
		return task.failureCompletion(subagent.StopInternalError, subagent.ErrInternal, "task was already run", false)
	}
	task.run = true
	task.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	stopRunBridge := context.AfterFunc(ctx, func() { task.runtime.Cancel(context.Cause(ctx)) })
	defer stopRunBridge()
	if cause := context.Cause(ctx); cause != nil {
		task.runtime.Cancel(cause)
	}
	defer task.runtime.close(context.Cause(task.runtime.Context))
	bridge := newTaskEventBridge(
		task.runtime.Context, task.runtime.Cancel, sink,
		task.factory.options.Limits.MaxEventBytes, task.factory.options.Limits.MaxSubscriberBuffer,
	)
	hooks := task.factory.options.Hooks
	hooks.SessionStart(task.runtime.Context, task.runtime.HookSessionID, hook.SessionNew)

	var completion subagent.Completion
	if task.runtime.Profile.MaxIterations == 0 {
		completion = task.failureCompletion(subagent.StopMaxIterations, subagent.ErrLimitReached, "subagent reached its iteration limit", true)
	} else {
		select {
		case <-task.runtime.Context.Done():
			completion = task.failureCompletion(subagent.StopCancelled, subagent.ErrCancelled, "subagent task was cancelled", true)
		default:
			completion = task.runNonInteractiveLoop(bridge)
		}
	}
	if sinkErr := bridge.close(); sinkErr != nil && completion.Status == subagent.StatusCompleted {
		completion = task.failureCompletion(subagent.StopInternalError, subagent.ErrInternal, "subagent event consumer failed", false)
	}
	hooks.SessionEnd(context.WithoutCancel(task.runtime.Context), task.runtime.HookSessionID, hook.SessionEndExit)
	return completion
}

func (task *preparedSubagentTask) runNonInteractiveLoop(bridge *taskEventBridge) subagent.Completion {
	unknownToolCalls := 0
	for iteration := 1; iteration <= task.runtime.Profile.MaxIterations; iteration++ {
		if err := context.Cause(task.runtime.Context); err != nil {
			return task.failureCompletion(subagent.StopCancelled, subagent.ErrCancelled, "subagent task was cancelled", true)
		}
		if err := task.runtime.beginIteration(iteration); err != nil {
			return task.failureCompletion(subagent.StopInternalError, subagent.ErrInternal, "subagent iteration state is invalid", false)
		}
		if !bridge.emit(events.Event{Type: events.AgentProgressed, Progress: &events.AgentProgress{
			Iteration: iteration, Max: task.runtime.Profile.MaxIterations,
		}}) {
			return task.failureCompletion(subagent.StopCancelled, subagent.ErrCancelled, "subagent event stream was cancelled", true)
		}
		ref := task.factory.options.Hooks.BeginTurn(
			task.runtime.Context, task.runtime.HookSessionID, hook.ExecutionIsolatedSkill,
			hookMode(runModeFromPlan(task.runtime.Profile.PlanMode)),
		)
		task.runtime.mu.Lock()
		task.runtime.HookExecution = ref
		task.runtime.mu.Unlock()

		request, err := task.requestForTurn(task.runtime.Context, iteration, ref)
		if err != nil {
			if context.Cause(task.runtime.Context) != nil {
				task.endTurn(ref, hook.TurnCanceled, "request canceled")
				return task.failureCompletion(subagent.StopCancelled, subagent.ErrCancelled, "subagent task was cancelled", true)
			}
			task.endTurn(ref, hook.TurnError, "request_error")
			if errors.Is(err, errSubagentContextBudgetExceeded) {
				return task.failureCompletion(subagent.StopInternalError, subagent.ErrContextBudgetExceeded, "subagent request exceeds its context budget", true)
			}
			return task.failureCompletion(subagent.StopInternalError, subagent.ErrInternal, "subagent request could not be constructed", false)
		}
		if context.Cause(task.runtime.Context) != nil {
			task.endTurn(ref, hook.TurnCanceled, "request canceled")
			return task.failureCompletion(subagent.StopCancelled, subagent.ErrCancelled, "subagent task was cancelled", true)
		}
		// The manager's first-request timer begins at the exact handoff boundary,
		// synchronously and immediately before StreamChat.
		task.observeFirstProviderRequest(task.runtime.Context)
		if context.Cause(task.runtime.Context) != nil {
			task.endTurn(ref, hook.TurnCanceled, "request canceled")
			return task.failureCompletion(subagent.StopCancelled, subagent.ErrCancelled, "subagent task was cancelled", true)
		}
		stream, startErr := task.streamChat(task.runtime.Context, request)
		collector, reason, streamErr := ownProviderStreamWithRedactor(
			task.runtime.Context, stream, startErr, bridge.out,
			task.factory.options.RuntimeRedactor.Text, 64,
		)
		if startErr != nil || streamErr != nil {
			if context.Cause(task.runtime.Context) != nil || reason == StopReasonCancelled {
				task.endTurn(ref, hook.TurnCanceled, "request canceled")
				return task.failureCompletion(subagent.StopCancelled, subagent.ErrCancelled, "subagent task was cancelled", true)
			}
			task.endTurn(ref, hook.TurnError, "provider_error")
			return task.failureCompletion(subagent.StopProviderError, subagent.ErrProviderFailed, "subagent Provider request failed", true)
		}
		if context.Cause(task.runtime.Context) != nil {
			task.endTurn(ref, hook.TurnCanceled, "request canceled")
			return task.failureCompletion(subagent.StopCancelled, subagent.ErrCancelled, "subagent task was cancelled", true)
		}
		usage, err := task.runtime.addUsage(collector.Usage)
		if err != nil {
			task.endTurn(ref, hook.TurnError, "usage_error")
			return task.failureCompletion(subagent.StopInternalError, subagent.ErrInternal, "subagent Provider usage is invalid", false)
		}
		if collector.Usage != nil && !bridge.emit(events.Event{Type: events.UsageUpdated, Usage: usageDisplay(&usage)}) {
			task.endTurn(ref, hook.TurnCanceled, "event canceled")
			return task.failureCompletion(subagent.StopCancelled, subagent.ErrCancelled, "subagent event stream was cancelled", true)
		}
		if task.factory.options.Thinking.Show {
			thinking := strings.TrimSpace(collector.ThinkingText.String())
			if thinking != "" {
				task.appendConversationMessage(conversation.RoleThinking, thinking, nil)
			}
		}
		assistantText := collector.AssistantText.String()
		if assistantText != "" {
			message := task.factory.options.Hooks.BeginMessage(task.runtime.Context, ref, hook.MessageAssistant, assistantText)
			task.appendConversationMessage(conversation.RoleAssistant, assistantText, nil)
			task.factory.options.Hooks.EndMessage(task.runtime.Context, message)
		}
		if len(collector.ToolCalls) == 0 {
			task.endTurn(ref, hook.TurnCompleted, "completed")
			bridge.emit(events.Event{Type: events.AgentProgressed, Progress: &events.AgentProgress{
				Iteration: iteration, Max: task.runtime.Profile.MaxIterations,
				StopReason: string(subagent.StopCompleted), Message: task.factory.options.RuntimeRedactor.Redact("completed"),
			}})
			return task.completedCompletion(assistantText)
		}
		if context.Cause(task.runtime.Context) != nil {
			task.endTurn(ref, hook.TurnCanceled, "tool batch canceled")
			return task.failureCompletion(subagent.StopCancelled, subagent.ErrCancelled, "subagent task was cancelled", true)
		}
		// Ordinary tool batches and recursive Agent rejection are connected below
		// at the same task-owned capability/permission boundary.
		unknown, err := task.executeTaskToolBatch(task.runtime.Context, iteration, ref, collector.ToolCalls, bridge)
		unknownToolCalls += unknown
		if err != nil {
			if context.Cause(task.runtime.Context) != nil {
				task.endTurn(ref, hook.TurnCanceled, "tool batch canceled")
				return task.failureCompletion(subagent.StopCancelled, subagent.ErrCancelled, "subagent task was cancelled", true)
			}
			task.endTurn(ref, hook.TurnError, "tool_error")
			return task.failureCompletion(subagent.StopToolError, subagent.ErrToolFailed, "subagent tool batch failed", true)
		}
		if unknownToolCalls >= task.runtime.Profile.MaxUnknownToolCalls {
			task.endTurn(ref, hook.TurnError, "unknown_tool_limit")
			return task.failureCompletion(subagent.StopUnknownToolLimit, subagent.ErrLimitReached, "subagent reached its unknown tool limit", true)
		}
		if iteration == task.runtime.Profile.MaxIterations {
			task.endTurn(ref, hook.TurnMaxIterations, "max_iterations")
			return task.failureCompletion(subagent.StopMaxIterations, subagent.ErrLimitReached, "subagent reached its iteration limit", true)
		}
		task.endTurn(ref, hook.TurnCompleted, "continue")
	}
	return task.failureCompletion(subagent.StopMaxIterations, subagent.ErrLimitReached, "subagent reached its iteration limit", true)
}

type taskToolOutcome struct {
	call       tool.Call
	result     tool.Result
	projection contextmgr.ToolResultProjection
	unknown    bool
	hookInput  hook.ToolInput
	duration   time.Duration
	afterHook  bool
	fatal      bool
}

func (task *preparedSubagentTask) executeTaskToolBatch(
	ctx context.Context,
	_ int,
	ref hook.ExecutionRef,
	calls []tool.Call,
	bridge *taskEventBridge,
) (int, error) {
	outcomes := make([]taskToolOutcome, 0, len(calls))
	unknown := 0
	fatal := false
	for _, call := range calls {
		if cause := context.Cause(ctx); cause != nil {
			return unknown, cause
		}
		outcome, err := task.executeTaskTool(ctx, ref, call, bridge)
		if err != nil {
			return unknown, err
		}
		if outcome.unknown {
			unknown++
		}
		outcomes = append(outcomes, outcome)
		if outcome.fatal {
			fatal = true
			break
		}
	}
	// Conversation commits remain in model call order even when a later
	// scheduler revision executes safe calls concurrently.
	for _, outcome := range outcomes {
		arguments := task.factory.options.RuntimeRedactor.Redact(redactedArguments(outcome.call))
		task.appendConversationMessage(conversation.RoleToolCall, outcome.call.Name, &conversation.ToolState{
			CallID: outcome.call.ID, Name: outcome.call.Name, ArgumentsJSON: arguments, State: tool.Prepared,
		})
		if _, err := conversation.AppendProjectedToolResultMessage(task.runtime.Conversation, conversation.ToolResultMessageInput{
			CallID: outcome.call.ID, Name: outcome.call.Name,
			PersistedContent: outcome.projection.PersistedContent,
			UserView:         outcome.projection.UserView,
			OutputMeta:       outcome.projection.OutputMeta,
		}); err != nil {
			return unknown, err
		}
		if err := task.runtime.setModelToolResult(outcome.call.ID, outcome.projection.ModelContent); err != nil {
			return unknown, err
		}
		if outcome.afterHook {
			task.factory.options.Hooks.AfterTool(
				context.WithoutCancel(ctx), ref, outcome.hookInput,
				hook.ToolOutputFromUserView(outcome.projection.UserView), outcome.duration,
			)
		}
		if !bridge.emit(events.ToolResultEventFromUserView(
			outcome.call.ID, outcome.call.Name, arguments, outcome.projection.UserView,
		)) {
			return unknown, context.Cause(ctx)
		}
	}
	if fatal {
		return unknown, errSubagentFatalToolResult
	}
	return unknown, nil
}

func (task *preparedSubagentTask) executeTaskTool(
	ctx context.Context,
	ref hook.ExecutionRef,
	call tool.Call,
	bridge *taskEventBridge,
) (taskToolOutcome, error) {
	outcome := taskToolOutcome{call: call}
	if cause := context.Cause(ctx); cause != nil {
		return outcome, cause
	}
	if !bridge.emit(events.Event{Type: events.ToolPending, Tool: task.safeToolDisplay(call, events.ToolDisplayPending, "")}) {
		return outcome, context.Cause(ctx)
	}
	if call.Name == tool.AgentToolName {
		result, err := task.syntheticToolResult(call, tool.StatusDenied,
			"Recursive delegation is unavailable inside a subagent.", string(subagent.ErrRecursiveDelegate))
		if err != nil {
			return outcome, err
		}
		return task.projectTaskToolResult(call, result)
	}
	current := task.runtime.ActiveTools.Current()
	if current.Registry == nil {
		return outcome, errors.New("task capability registry is unavailable")
	}
	if !current.Allows(call.Name) {
		_, known := task.factory.options.Registry.Get(call.Name)
		outcome.unknown = !known
		code := tool.ErrToolFiltered
		message := "Tool is unavailable in the current task capability view: " + string(current.FilterReason(call.Name))
		if !known {
			code = tool.ErrToolNotFound
			message = "Tool is not registered in the task capability snapshot."
		}
		result, err := task.syntheticToolResult(call, tool.StatusDenied, message, code)
		if err != nil {
			return outcome, err
		}
		projected, err := task.projectTaskToolResult(call, result)
		projected.unknown = outcome.unknown
		return projected, err
	}
	descriptor, ok := current.Registry.Descriptor(call.Name)
	if !ok || descriptor.Route == tool.RouteSystem {
		result, err := task.syntheticToolResult(call, tool.StatusDenied,
			"System-routed tools are unavailable inside a subagent.", tool.ErrInternalRoutingRequired)
		if err != nil {
			return outcome, err
		}
		return task.projectTaskToolResult(call, result)
	}

	execCtx, err := task.taskToolContext(ctx)
	if err != nil {
		return outcome, err
	}
	validated, err := task.factory.options.Executor.PrepareCall(execCtx, call)
	if err != nil {
		result, buildErr := task.syntheticToolResult(call, tool.StatusError,
			"Tool arguments are invalid.", tool.ErrInvalidArguments)
		if buildErr != nil {
			return outcome, buildErr
		}
		return task.projectTaskToolResult(call, result)
	}
	permissionContext := permission.Context{
		ProjectRoot: task.projectRoot, ReadRoots: append([]string(nil), task.runtime.Profile.ReadRoots...),
		Mode: task.runtime.Profile.PermissionMode, PlanMode: task.runtime.Profile.PlanMode,
		ConversationID: task.runtime.HookSessionID,
	}
	normalized, err := permission.NormalizeArguments(permissionCall(call), validated.Arguments, permissionContext)
	if err != nil {
		result, buildErr := task.syntheticToolResult(call, tool.StatusDenied,
			"Tool arguments could not be normalized for permission checking.", tool.ErrPermissionDenied)
		if buildErr != nil {
			return outcome, buildErr
		}
		return task.projectTaskToolResult(call, result)
	}
	identity, err := task.factory.options.Executor.CallIdentity(validated)
	if err != nil {
		return outcome, err
	}
	permissionContext.Identity = identity
	if hard := task.runtime.Authorizer.CheckHard(normalized, permissionContext); hard != nil {
		result, buildErr := task.permissionDeniedToolResult(call, *hard)
		if buildErr != nil {
			return outcome, buildErr
		}
		return task.projectTaskToolResult(call, result)
	}
	hookInput := hook.NewToolInput(call.ID, call.Name, validated.Arguments)
	if hookDecision := task.factory.options.Hooks.BeforeTool(ctx, ref, hookInput); hookDecision.IsDeny() {
		result, buildErr := task.syntheticToolResult(call, tool.StatusDenied,
			"Tool execution was denied by a task hook.", tool.ErrHookDenied)
		if buildErr != nil {
			return outcome, buildErr
		}
		return task.projectTaskToolResult(call, result)
	}
	decision := task.runtime.Authorizer.DecideOrdinary(normalized, permissionContext)
	if decision.Kind == permission.DecisionAsk {
		confirmationID := task.nextConfirmationID()
		request := task.safeConfirmationRequest(call, decision, confirmationID)
		if request == nil {
			return outcome, errors.New("task confirmation request is unavailable")
		}
		userDecision, requestErr := task.requestTaskConfirmation(ctx, call, request, bridge)
		if requestErr != nil {
			return outcome, requestErr
		}
		decision = task.runtime.Authorizer.ResolveNormalizedUserDecision(
			normalized, permissionContext, permissionAction(userDecision),
		)
	}
	if decision.Kind != permission.DecisionAllow || !decision.Ticket.Issued() {
		result, buildErr := task.permissionDeniedToolResult(call, decision)
		if buildErr != nil {
			return outcome, buildErr
		}
		return task.projectTaskToolResult(call, result)
	}
	if cause := context.Cause(ctx); cause != nil {
		return outcome, cause
	}
	if !bridge.emit(events.Event{Type: events.ToolRunning, Tool: task.safeToolDisplay(call, events.ToolDisplayRunning, "running")}) {
		return outcome, context.Cause(ctx)
	}
	started := time.Now()
	result := task.runtime.Executor.ExecuteValidatedAuthorized(execCtx, validated, decision.Ticket)
	if result.CallID == "" {
		return outcome, errors.New("task scoped executor returned no result")
	}
	projected, err := task.projectTaskToolResult(call, result)
	if err != nil {
		return outcome, err
	}
	projected.hookInput = hookInput
	projected.duration = time.Since(started)
	projected.afterHook = true
	return projected, nil
}

type taskConfirmationResult struct {
	decision events.ToolConfirmationDecision
	err      error
}

// requestTaskConfirmation establishes the broker's pending identity before
// publishing the actionable event. A consumer may therefore resolve directly
// from its sink callback without racing a not-yet-installed broker entry.
func (task *preparedSubagentTask) requestTaskConfirmation(
	ctx context.Context,
	call tool.Call,
	request *events.ToolConfirmationRequest,
	bridge *taskEventBridge,
) (events.ToolConfirmationDecision, error) {
	result := make(chan taskConfirmationResult, 1)
	go func() {
		decision, err := task.runtime.Confirm.Request(ctx, *request)
		result <- taskConfirmationResult{decision: decision, err: err}
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for task.runtime.Confirm.Pending() == nil {
		select {
		case resolved := <-result:
			return resolved.decision, resolved.err
		case <-ctx.Done():
			return events.ToolConfirmationDecision{}, context.Cause(ctx)
		case <-ticker.C:
		}
	}
	if !bridge.emit(events.Event{
		Type:         events.ToolWaitingConfirmation,
		Tool:         task.safeToolDisplay(call, events.ToolDisplayWaitingConfirmation, "waiting for confirmation"),
		Confirmation: request,
	}) {
		if task.runtime.Cancel != nil {
			task.runtime.Cancel(errors.New("task confirmation event publication failed"))
		}
	}
	resolved := <-result
	return resolved.decision, resolved.err
}

func (task *preparedSubagentTask) taskToolContext(ctx context.Context) (context.Context, error) {
	scope, err := tool.NewPinnedReadScope(task.projectRoot, task.runtime.Profile.ReadRoots)
	if err != nil {
		return nil, err
	}
	return tool.WithReadScope(ctx, scope), nil
}

func (task *preparedSubagentTask) projectTaskToolResult(call tool.Call, result tool.Result) (taskToolOutcome, error) {
	outcome := taskToolOutcome{call: call, result: result}
	projection, err := projectToolResult(task.factory.options.ContextManager, result)
	if err != nil {
		return outcome, err
	}
	outcome.projection = projection
	outcome.fatal = projection.UserView.Error != nil && !projection.UserView.Error.Recoverable
	return outcome, nil
}

func (task *preparedSubagentTask) syntheticToolResult(call tool.Call, status tool.ResultStatus, message, code string) (tool.Result, error) {
	return task.factory.options.ResultFactory.Build(tool.ResultFactoryInput{
		CallID: call.ID, Name: call.Name, State: tool.Rejected, Status: status,
		Summary: message, Preview: message,
		Error: &tool.Error{Code: code, Message: message, Recoverable: true},
	})
}

func (task *preparedSubagentTask) permissionDeniedToolResult(call tool.Call, decision permission.Decision) (tool.Result, error) {
	message := permission.DeniedModelMessage(decision)
	if strings.TrimSpace(message) == "" {
		message = "Tool execution was denied by task permission policy."
	}
	return task.syntheticToolResult(call, tool.StatusDenied, message, tool.ErrPermissionDenied)
}

func (task *preparedSubagentTask) safeToolDisplay(call tool.Call, status events.ToolDisplayStatus, summary string) *events.ToolDisplay {
	return &events.ToolDisplay{
		CallID: call.ID, Name: call.Name,
		Arguments: task.factory.options.RuntimeRedactor.Redact(redactedArguments(call)),
		Summary:   task.factory.options.RuntimeRedactor.Redact(summary), Status: status,
	}
}

func (task *preparedSubagentTask) safeConfirmationRequest(call tool.Call, decision permission.Decision, confirmationID string) *events.ToolConfirmationRequest {
	request := confirmationRequest(call, decision, confirmationID)
	if request == nil {
		return nil
	}
	request.AllowPermanent = false
	request.Arguments = task.factory.options.RuntimeRedactor.Redact(request.Arguments.Text())
	request.Prompt = task.factory.options.RuntimeRedactor.Redact(request.Prompt.Text())
	request.Target = task.factory.options.RuntimeRedactor.Redact(request.Target.Text())
	request.ScopePreview = task.factory.options.RuntimeRedactor.Redact(request.ScopePreview.Text())
	request.RuleLocation = task.factory.options.RuntimeRedactor.Redact(request.RuleLocation.Text())
	request.Warning = task.factory.options.RuntimeRedactor.Redact(request.Warning.Text())
	request.RevokeHint = task.factory.options.RuntimeRedactor.Redact(request.RevokeHint.Text())
	for index := range request.Scopes {
		request.Scopes[index].Description = task.factory.options.RuntimeRedactor.Redact(request.Scopes[index].Description.Text())
		if request.Scopes[index].Scope == "permanent" {
			request.Scopes[index].Available = false
		}
	}
	return request
}

func (task *preparedSubagentTask) nextConfirmationID() string {
	task.mu.Lock()
	defer task.mu.Unlock()
	task.confirmationSequence++
	digest := sha256.Sum256([]byte(task.runtime.TaskID))
	return fmt.Sprintf("confirmation-%x-%d", digest[:16], task.confirmationSequence)
}

func (task *preparedSubagentTask) endTurn(ref hook.ExecutionRef, status hook.TurnStatus, detail string) {
	task.factory.options.Hooks.EndTurn(context.WithoutCancel(task.runtime.Context), ref, status, detail)
}

func runModeFromPlan(plan bool) RunMode {
	if plan {
		return RunModePlan
	}
	return RunModeDefault
}

func (task *preparedSubagentTask) appendConversationMessage(role conversation.MessageRole, text string, toolState *conversation.ToolState) {
	now := task.factory.options.Clock()
	task.runtime.Conversation.Messages = append(task.runtime.Conversation.Messages, conversation.Message{
		Role: role, Content: task.factory.options.RuntimeRedactor.Redact(text), CreatedAt: now, Tool: toolState,
	})
	task.runtime.Conversation.UpdatedAt = now
}

func (task *preparedSubagentTask) completedCompletion(summary string) subagent.Completion {
	safe, truncated, reason := task.boundedSummary(summary)
	usage := task.runtime.snapshotUsage()
	return subagent.Completion{
		ID: task.runtime.TaskID, Status: subagent.StatusCompleted, Summary: safe,
		SummaryTruncated: truncated, TruncationReason: reason, StopReason: subagent.StopCompleted,
		Usage: subagent.Usage{
			InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens, CacheReadInputTokens: usage.CacheReadInputTokens,
		},
		EndedAt: task.factory.options.Clock(),
	}
}

func (task *preparedSubagentTask) boundedSummary(value string) (redact.SafeText, bool, redact.SafeText) {
	safe := task.factory.options.RuntimeRedactor.Redact(value)
	limit := task.factory.options.Limits.MaxResultBytes
	if int64(len(safe.Text())) <= limit {
		return safe, false, redact.SafeText{}
	}
	prefix, _ := boundedUTF8Prefix(safe.Text(), limit)
	return task.factory.options.RuntimeRedactor.Redact(prefix), true,
		task.factory.options.RuntimeRedactor.Redact("max_result_bytes")
}

func (task *preparedSubagentTask) failureCompletion(reason subagent.StopReason, code subagent.ErrorCode, summary string, recoverable bool) subagent.Completion {
	status := subagent.StatusFailed
	if reason == subagent.StopMaxIterations || reason == subagent.StopUnknownToolLimit {
		status = subagent.StatusLimitReached
	} else if reason == subagent.StopCancelled || reason == subagent.StopApplicationClosed {
		status = subagent.StatusCancelled
	}
	safe, truncated, truncationReason := task.boundedSummary(summary)
	usage := task.runtime.snapshotUsage()
	return subagent.Completion{
		ID: task.runtime.TaskID, Status: status, Summary: safe,
		SummaryTruncated: truncated, TruncationReason: truncationReason, StopReason: reason,
		Usage: subagent.Usage{
			InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens, CacheReadInputTokens: usage.CacheReadInputTokens,
		},
		Error: subagent.SafeError(code, safe, recoverable), EndedAt: task.factory.options.Clock(),
	}
}

func preparedMetadata(taskType subagent.ExecutionType, roleName string, role *agentrole.ResolvedRole, profile RuntimeProfile) subagent.PreparedMetadata {
	metadata := subagent.PreparedMetadata{
		Type: taskType, Role: roleName, Model: profile.Model,
		MaxIterations: profile.MaxIterations, MaxUnknownToolCalls: profile.MaxUnknownToolCalls,
		PermissionMode: string(profile.PermissionMode), Depth: profile.Depth,
		ForegroundToolFingerprint: profile.ForegroundTools.Fingerprint,
		BackgroundToolFingerprint: profile.BackgroundTools.Fingerprint,
	}
	if role != nil {
		metadata.RoleSource = role.Definition.Source
		metadata.RoleSourceID = role.Definition.SourceID
		metadata.RoleProviderID = role.Definition.ProviderID
		metadata.RoleOrigin = role.Definition.Origin
		metadata.RoleGeneration = role.Generation
	}
	return metadata
}

func effectiveMaxIterations(global int, role *int) int {
	if global < 0 {
		global = 0
	}
	if role != nil && *role < global {
		return *role
	}
	return global
}

func cloneResolvedRole(source agentrole.ResolvedRole) *agentrole.ResolvedRole {
	return &agentrole.ResolvedRole{Generation: source.Generation, Definition: source.Definition.Clone()}
}

func redactStableSections(redactor *redact.RuntimeRedactor, sections []prompt.Section) []prompt.Section {
	result := make([]prompt.Section, 0, len(sections))
	for _, section := range sections {
		section.Name = strings.TrimSpace(redactor.Text(section.Name))
		section.Content = strings.TrimSpace(redactor.Text(section.Content))
		if section.Name == "" || section.Content == "" || !section.Stable {
			continue
		}
		result = append(result, section)
	}
	return result
}

func taskModelMessages(child *conversation.Conversation) ([]provider.ModelMessage, error) {
	return taskModelMessagesFrom(child, 0)
}

func taskModelMessagesFrom(child *conversation.Conversation, offset int) ([]provider.ModelMessage, error) {
	if child == nil {
		return nil, errors.New("task conversation is unavailable")
	}
	if offset < 0 || offset > len(child.Messages) {
		return nil, errors.New("task conversation message offset is invalid")
	}
	result := make([]provider.ModelMessage, 0, len(child.Messages)-offset)
	for _, message := range child.Messages[offset:] {
		role, ok := providerMessageRole(message.Role)
		if !ok {
			continue
		}
		modelMessage := provider.ModelMessage{Role: role, Content: message.Content}
		if message.Tool != nil {
			modelMessage.ToolCallID = message.Tool.CallID
			modelMessage.ToolName = message.Tool.Name
			modelMessage.ArgumentsJSON = message.Tool.ArgumentsJSON
			modelMessage.ToolResult = message.Tool.Result
			modelMessage.ToolResultStatus = string(message.Tool.Status)
		}
		result = append(result, modelMessage)
	}
	return result, nil
}

func promptMode(planMode bool) prompt.RunMode {
	if planMode {
		return prompt.RunModePlan
	}
	return prompt.RunModeDefault
}

func cloneDeniedNames(source map[string]struct{}) map[string]struct{} {
	if source == nil {
		return nil
	}
	result := make(map[string]struct{}, len(source))
	for name := range source {
		result[name] = struct{}{}
	}
	return result
}

func validRunnerIdentifier(value string, maxBytes int64) bool {
	if value == "" || strings.TrimSpace(value) != value || int64(len(value)) > maxBytes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func (factory *SubagentRunnerFactory) safeError(code subagent.ErrorCode, message string, recoverable bool) error {
	return subagent.SafeError(code, factory.options.RuntimeRedactor.Redact(message), recoverable)
}

var _ subagent.RunnerFactory = (*SubagentRunnerFactory)(nil)
var _ subagent.PreparedTask = (*preparedSubagentTask)(nil)
var _ subagent.PreparedPlacementController = (*preparedSubagentTask)(nil)
var _ subagent.PreparedConfirmationController = (*preparedSubagentTask)(nil)
var _ subagent.FirstProviderRequestObservable = (*preparedSubagentTask)(nil)
