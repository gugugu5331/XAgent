package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/memory"
	"xagent/internal/permission"
	"xagent/internal/prompt"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/resources"
	"xagent/internal/sessionctx"
	"xagent/internal/skill"
	"xagent/internal/subagent"
	"xagent/internal/tool"
)

type OrchestratorOptions struct {
	Provider  provider.Provider
	Store     conversation.Store
	Resources resources.PromptProvider
	Thinking  config.ThinkingConfig
	Registry  *tool.Registry
	Executor  *tool.Executor
	// CleanupTimeout and LifecycleDiagnostics are the C7 inputs for
	// request-owned Provider ChatStreams. Diagnostics remains the legacy
	// session/context Collector below; keeping the two fields distinct avoids
	// widening or replacing the established diagnostic projection API while
	// allowing the process root to inject its one bounded sink.
	CleanupTimeout       time.Duration
	LifecycleDiagnostics diagnostics.BoundedSink
	// Authorizer is the Assembly-owned permission boundary. When supplied,
	// NewWithOptions retains it instead of creating a second authority or
	// reloading rules from a process-level path.
	Authorizer          *permission.Authorizer
	ContextManager      *contextmgr.Manager
	ResultFactory       *tool.ResultFactory
	ResultProjector     *ResultProjector
	Subagents           subagent.Service
	SubagentLimits      subagent.Limits
	SkillHistoryPolicy  SkillHistoryPolicy
	RequestBudgeter     contextmgr.RequestBudgeter
	MaxRecordBytes      int64
	MaxSessionBytes     int64
	SessionContext      sessionPreparer
	Memory              memoryUpdater
	Diagnostics         *diagnostics.Collector
	Agent               config.AgentConfig
	SkillManager        *skill.Manager
	DefaultModel        string
	RuntimeRedactor     *redact.RuntimeRedactor
	Redact              func(string) string
	RedactionLookbehind int
	Hooks               hook.Runtime
}

type sessionPreparer interface {
	Prepare(ctx context.Context, conv *conversation.Conversation, mode sessionctx.PrepareMode) (sessionctx.PreparedContext, error)
}

type optionsSessionPreparer interface {
	PrepareWithOptions(ctx context.Context, conv *conversation.Conversation, opts contextmgr.PrepareOptions) (sessionctx.PreparedContext, error)
}

type memoryUpdater interface {
	UpdateAsync(input memory.UpdateInput)
}

type Orchestrator struct {
	provider             provider.Provider
	store                conversation.Store
	resources            resources.PromptProvider
	thinking             config.ThinkingConfig
	registry             *tool.Registry
	readOnlyRegistry     *tool.Registry
	executor             *tool.Executor
	authorizer           *permission.Authorizer
	contextManager       *contextmgr.Manager
	resultFactory        *tool.ResultFactory
	resultProjector      *ResultProjector
	systemToolRouter     *SystemToolRouter
	skillHistoryPolicy   SkillHistoryPolicy
	skillHistoryBudgeter contextmgr.RequestBudgeter
	maxRecordBytes       int64
	maxSessionBytes      int64
	requestGeneration    atomic.Uint64
	confirmationSequence atomic.Uint64
	confirmationMu       sync.Mutex
	pendingConfirmations map[string]*pendingConfirmation
	sessionContext       sessionPreparer
	memory               memoryUpdater
	diagnostics          *diagnostics.Collector
	chatStreamOptions    provider.ChatStreamOptions
	skillManager         *skill.Manager
	defaultModel         string
	runtimeRedactor      *redact.RuntimeRedactor
	redact               func(string) string
	redactionLookbehind  int
	permissionMode       permission.Mode
	runOptions           RunOptions
	runs                 *runTracker
	hooks                hook.Runtime
}

type pendingConfirmation struct {
	callID   string
	decision chan events.ToolConfirmationDecision
	resolved bool
}

func New(provider provider.Provider, store conversation.Store, resources resources.PromptProvider, thinking config.ThinkingConfig) *Orchestrator {
	return NewWithOptions(OrchestratorOptions{Provider: provider, Store: store, Resources: resources, Thinking: thinking})
}

func NewWithTools(provider provider.Provider, store conversation.Store, resources resources.PromptProvider, thinking config.ThinkingConfig, registry *tool.Registry, executor *tool.Executor) *Orchestrator {
	return NewWithToolsAndContext(provider, store, resources, thinking, registry, executor, nil)
}

func NewWithToolsAndContext(provider provider.Provider, store conversation.Store, resources resources.PromptProvider, thinking config.ThinkingConfig, registry *tool.Registry, executor *tool.Executor, contextManager *contextmgr.Manager) *Orchestrator {
	return NewWithOptions(OrchestratorOptions{Provider: provider, Store: store, Resources: resources, Thinking: thinking, Registry: registry, Executor: executor, ContextManager: contextManager})
}

func NewWithOptions(options OrchestratorOptions) *Orchestrator {
	runtimeRedactor := options.RuntimeRedactor
	if runtimeRedactor == nil {
		runtimeRedactor = redact.NewRuntimeRedactor()
	}
	redactor := options.Redact
	if redactor == nil {
		redactor = runtimeRedactor.Text
	}
	lookbehind := options.RedactionLookbehind
	if lookbehind < 64 {
		lookbehind = 64
	}
	var readOnlyRegistry *tool.Registry
	authorizer := options.Authorizer
	// Compatibility constructors historically handed ownership of a fully
	// populated registry to Orchestrator without an explicit finalization
	// step. Seal that boundary once here; production Assembly seals earlier so
	// registration/definition failures can still abort startup explicitly.
	if options.Registry != nil && !options.Registry.IsSealed() {
		_ = options.Registry.Seal()
	}
	if options.Executor != nil {
		if options.Registry != nil {
			// Assembly supplies one safe-candidate Registry. Derive an immutable
			// view over those exact tool instances instead of constructing a
			// second set of legacy Read/Glob/Grep producers.
			readOnlyRegistry, _ = options.Registry.View(tool.ViewOptions{ReadOnly: true})
		} else {
			// Compatibility constructors may still omit Registry until the
			// T4.29a atomic cutover removes their legacy construction path.
			readOnlyRegistry, _ = tool.NewReadOnlyRegistry(options.Executor.ProjectRoot)
		}
		if authorizer == nil {
			loaded := permission.LoadRules(options.Executor.ProjectRoot)
			authority, _ := permission.NewTicketAuthority()
			options.Executor.TicketVerifier = authority
			authorizer = &permission.Authorizer{
				Session:    permission.NewSession(),
				User:       loaded.User,
				Project:    loaded.Project,
				Local:      loaded.Local,
				LoadErrors: loaded.Errors,
				Writer:     permission.Writer{},
				Redact:     redactor,
				Issuer:     authority,
			}
		}
	}
	var systemToolRouter *SystemToolRouter
	if options.Subagents != nil && options.ResultFactory != nil {
		systemToolRouter, _ = NewSystemToolRouter(SystemToolRouterOptions{
			Service: options.Subagents, ResultFactory: options.ResultFactory, Limits: options.SubagentLimits,
		})
	}
	runOptions := runOptionsFromConfig(options.Agent)
	hooks := options.Hooks
	if hooks == nil {
		hooks = hook.Noop()
	}
	return &Orchestrator{
		provider: options.Provider, store: options.Store, resources: options.Resources, thinking: options.Thinking,
		registry: options.Registry, readOnlyRegistry: readOnlyRegistry, executor: options.Executor, authorizer: authorizer,
		contextManager: options.ContextManager, resultFactory: options.ResultFactory, resultProjector: options.ResultProjector,
		systemToolRouter:   systemToolRouter,
		skillHistoryPolicy: options.SkillHistoryPolicy, skillHistoryBudgeter: options.RequestBudgeter,
		maxRecordBytes: options.MaxRecordBytes, maxSessionBytes: options.MaxSessionBytes,
		sessionContext: options.SessionContext, memory: options.Memory, diagnostics: options.Diagnostics,
		chatStreamOptions: provider.ChatStreamOptions{
			CleanupTimeout: options.CleanupTimeout,
			Diagnostics:    options.LifecycleDiagnostics,
		},
		skillManager: options.SkillManager, defaultModel: strings.TrimSpace(options.DefaultModel),
		runtimeRedactor: runtimeRedactor, redact: redactor, redactionLookbehind: lookbehind,
		permissionMode: permission.ModeDefault, runOptions: runOptions, runs: newRunTracker(), hooks: hooks,
		pendingConfirmations: make(map[string]*pendingConfirmation),
	}
}

func (o *Orchestrator) candidateResultsEnabled() bool {
	return o != nil && o.resultFactory != nil
}

func (o *Orchestrator) configureCandidateState(state *executionState, conversationID string) error {
	if o == nil || state == nil {
		return fmt.Errorf("execution state is unavailable")
	}
	if state.requestGeneration != 0 {
		return fmt.Errorf("request generation is already assigned")
	}
	if o.candidateResultsEnabled() &&
		(o.contextManager == nil || o.maxRecordBytes <= 0 || o.maxSessionBytes <= 0 || o.maxRecordBytes > o.maxSessionBytes) {
		return fmt.Errorf("safe tool result candidate dependencies are incomplete")
	}
	generation := o.requestGeneration.Add(1)
	if generation == 0 {
		return fmt.Errorf("request generation overflow")
	}
	state.requestGeneration = generation
	if !o.candidateResultsEnabled() {
		return nil
	}
	return state.configureModelContentSlots(conversationID, generation, o.maxRecordBytes, o.maxSessionBytes)
}

// WaitIdle waits until every main or independent Agent run has completed its
// lifecycle finalizer. It is safe to call on an Orchestrator created by any of
// the compatibility constructors.
func (o *Orchestrator) WaitIdle(ctx context.Context) error {
	if o == nil || o.runs == nil {
		return nil
	}
	return o.runs.wait(ctx)
}

func (o *Orchestrator) hookRuntime() hook.Runtime {
	if o == nil || o.hooks == nil {
		return hook.Noop()
	}
	return o.hooks
}

func (o *Orchestrator) redactText(value string) string {
	if o == nil || o.redact == nil {
		return redact.Text(value)
	}
	return o.redact(value)
}

func (o *Orchestrator) safeText(value string) redact.SafeText {
	if o == nil || o.runtimeRedactor == nil {
		return redact.NewRuntimeRedactor().Redact(redact.Text(value))
	}
	return o.runtimeRedactor.Redact(o.redactText(value))
}

func (o *Orchestrator) safeError(code string, err error) *diagnostics.SafeError {
	if err == nil {
		return nil
	}
	return &diagnostics.SafeError{
		Code:        code,
		Source:      "orchestrator",
		Message:     o.safeText(err.Error()),
		Recoverable: false,
	}
}

func (o *Orchestrator) redactError(err error) error {
	if err == nil {
		return nil
	}
	safe := o.redactText(err.Error())
	if safe == err.Error() {
		return err
	}
	return fmt.Errorf("%s", safe)
}

func (o *Orchestrator) SetPermissionMode(mode permission.Mode) {
	if mode == "" {
		mode = permission.ModeDefault
	}
	o.permissionMode = mode
}

type PermissionStatus struct {
	Mode         string
	SessionRules int
	LocalRules   int
	ProjectRules int
	UserRules    int
	LoadErrors   int
}

func (o *Orchestrator) PermissionStatus() PermissionStatus {
	mode := o.permissionMode
	if mode == "" {
		mode = permission.ModeDefault
	}
	status := PermissionStatus{Mode: string(mode)}
	if o == nil || o.authorizer == nil {
		return status
	}
	if o.authorizer.Session != nil {
		status.SessionRules = len(o.authorizer.Session.Rules())
	}
	status.LocalRules = len(o.authorizer.Local.Rules)
	status.ProjectRules = len(o.authorizer.Project.Rules)
	status.UserRules = len(o.authorizer.User.Rules)
	status.LoadErrors = len(o.authorizer.LoadErrors)
	return status
}

func (o *Orchestrator) CompactContext(ctx context.Context, conv *conversation.Conversation) (contextmgr.Result, error) {
	if o.contextManager == nil {
		return contextmgr.Result{}, fmt.Errorf("上下文管理未启用")
	}
	binding := hook.CompactBinding{}
	if conv != nil {
		binding.SessionID = conv.ID
	}
	result, err := o.contextManager.PrepareWithOptions(ctx, conv, contextmgr.PrepareOptions{
		Mode:             contextmgr.ModeManual,
		PersistArtifacts: true,
		Observer:         hookCompactionObserver{runtime: o.hookRuntime(), binding: binding, redact: o.redactText},
	})
	if result.Changed {
		if _, saveErr := o.store.Save(ctx, conv); saveErr != nil {
			return result, saveErr
		}
	}
	return result, err
}

func (o *Orchestrator) Send(ctx context.Context, conv *conversation.Conversation, userText string) (<-chan events.Event, error) {
	req, err := parseRunRequest(userText)
	if err != nil {
		return nil, err
	}
	return o.SendWithMode(ctx, conv, req.UserText, req.Mode)
}

func (o *Orchestrator) SendWithMode(ctx context.Context, conv *conversation.Conversation, userText string, mode RunMode) (<-chan events.Event, error) {
	return o.SendRequest(ctx, conv, RunRequest{UserText: userText, Mode: mode})
}

func (o *Orchestrator) SendRequest(ctx context.Context, conv *conversation.Conversation, req RunRequest) (<-chan events.Event, error) {
	endRun := func() {}
	if o != nil && o.runs != nil {
		endRun = o.runs.begin()
	}
	runHandedOff := false
	defer func() {
		if !runHandedOff {
			endRun()
		}
	}()

	req.UserText = strings.TrimSpace(req.UserText)
	if req.UserText == "" {
		return nil, fmt.Errorf("请输入非空内容")
	}
	if req.Mode != RunModeDefault && req.Mode != RunModePlan && req.Mode != RunModeDo {
		return nil, fmt.Errorf("运行模式无效: %s", req.Mode)
	}
	if conv == nil {
		return nil, fmt.Errorf("会话不能为空")
	}
	state, err := o.newExecutionState(req)
	if err != nil {
		return nil, err
	}
	if err := o.configureCandidateState(state, conv.ID); err != nil {
		return nil, err
	}
	runtime := o.hookRuntime()
	state.ref = normalizeExecutionRef(
		runtime.BeginTurn(requestCtxOrBackground(ctx), conv.ID, hook.ExecutionMain, hookMode(req.Mode)),
		conv.ID,
		hook.ExecutionMain,
		hookMode(req.Mode),
		state.requestGeneration,
	)
	message := runtime.BeginMessage(requestCtxOrBackground(ctx), state.ref, hook.MessageUser, req.UserText)
	o.appendConversationMessage(conv, conversation.RoleUser, o.safeText(req.UserText), nil)
	runtime.EndMessage(requestCtxOrBackground(ctx), message)
	out := make(chan events.Event)

	go func() {
		defer close(out)
		defer endRun()
		defer state.clearModelContentSlots()
		start := time.Now()
		result := RunResult{}
		if !emitEvent(ctx, out, events.Event{Type: events.UserSubmitted, Text: o.safeText(req.UserText)}) {
			result = finishRunResult(result, start, StopReasonCancelled, ctx.Err())
		} else {
			result = o.runAgentLoop(ctx, conv, req, state, out, start)
		}
		o.endTurn(state.ref, result)
		o.emitTerminalEvent(ctx, out, result)
	}()
	runHandedOff = true
	return out, nil
}

func (o *Orchestrator) endTurn(ref hook.ExecutionRef, result RunResult) {
	o.hookRuntime().EndTurn(context.Background(), ref, hookTurnStatus(result), hookTurnError(result))
}

func hookTurnError(result RunResult) string {
	switch result.Reason {
	case StopReasonCancelled:
		return "request canceled"
	case StopReasonUnknownTool:
		return "unknown tool call limit reached"
	case StopReasonProviderError:
		return "agent run failed"
	default:
		if result.Err != nil {
			return "agent run failed"
		}
		return ""
	}
}

func hookMode(mode RunMode) hook.HookMode {
	if mode == RunModePlan {
		return hook.ModePlan
	}
	return hook.ModeDefault
}

func normalizeExecutionRef(ref hook.ExecutionRef, sessionID string, kind hook.ExecutionKind, mode hook.HookMode, generation uint64) hook.ExecutionRef {
	if ref.SessionID == "" {
		ref.SessionID = sessionID
	}
	if ref.ExecutionID == "" {
		ref.ExecutionID = fmt.Sprintf("request-%d", generation)
	}
	if ref.TurnID == "" {
		ref.TurnID = fmt.Sprintf("turn-%d", generation)
	}
	if ref.Kind == "" {
		ref.Kind = kind
	}
	if ref.Mode == "" {
		ref.Mode = mode
	}
	return ref
}

func hookTurnStatus(result RunResult) hook.TurnStatus {
	switch result.Reason {
	case StopReasonCompleted:
		return hook.TurnCompleted
	case StopReasonMaxIterations:
		return hook.TurnMaxIterations
	case StopReasonCancelled:
		return hook.TurnCanceled
	default:
		return hook.TurnError
	}
}

func (o *Orchestrator) emitTerminalEvent(ctx context.Context, out chan<- events.Event, result RunResult) {
	if result.Err != nil {
		emitEvent(ctx, out, events.Event{Type: events.Error, Err: o.safeError("orchestrator_run_failed", result.Err)})
		return
	}
	emitEvent(ctx, out, events.Event{Type: events.Done, Duration: result.Duration})
}

func (o *Orchestrator) stream(ctx context.Context, conv *conversation.Conversation, mode RunMode, includeTools bool, iteration int) (provider.ChatStream, error) {
	profile, err := o.buildExecutionProfile(mode, skill.NewActivity(), 0)
	if err != nil {
		return nil, err
	}
	return o.streamWithProfile(ctx, conv, mode, profile, includeTools, iteration)
}

func (o *Orchestrator) streamWithProfile(ctx context.Context, conv *conversation.Conversation, mode RunMode, profile skill.ExecutionProfile, includeTools bool, iteration int) (provider.ChatStream, error) {
	return o.streamWithExecution(ctx, conv, mode, profile, includeTools, iteration, hook.ExecutionRef{})
}

func (o *Orchestrator) streamWithExecution(ctx context.Context, conv *conversation.Conversation, mode RunMode, profile skill.ExecutionProfile, includeTools bool, iteration int, ref hook.ExecutionRef) (provider.ChatStream, error) {
	return o.streamWithExecutionState(ctx, conv, mode, profile, includeTools, iteration, ref, nil)
}

func (o *Orchestrator) streamWithExecutionState(ctx context.Context, conv *conversation.Conversation, mode RunMode, profile skill.ExecutionProfile, includeTools bool, iteration int, ref hook.ExecutionRef, state *executionState) (stream provider.ChatStream, err error) {
	if state != nil && o.candidateResultsEnabled() {
		if conv == nil {
			state.clearModelContentSlots()
			return nil, fmt.Errorf("conversation is unavailable")
		}
		slots := &state.modelContents
		slots.mu.Lock()
		generation := slots.requestGeneration
		slots.mu.Unlock()
		if err := state.beginModelContentIteration(conv.ID, generation, iteration); err != nil {
			return nil, err
		}
		defer func() {
			// StreamChat owns a value request after handoff. No model-only view may
			// remain reachable from executionState on success or error.
			state.clearModelContentSlots()
		}()
	}
	var beforeMessages []conversation.Message
	if conv != nil {
		beforeMessages = cloneMessages(conv.Messages)
	}
	optionalSections := []prompt.Section{}
	binding := hook.CompactBinding{}
	if conv != nil {
		binding.SessionID = conv.ID
	}
	if ref.ExecutionID != "" {
		refCopy := ref
		binding.SessionID = ref.SessionID
		binding.Execution = &refCopy
	}
	prepareOptions := contextmgr.PrepareOptions{
		Mode:             contextmgr.ModeAuto,
		PersistArtifacts: profile.Persist,
		Observer:         hookCompactionObserver{runtime: o.hookRuntime(), binding: binding, redact: o.redactText},
	}
	if o.resultProjector != nil && ref.Kind == hook.ExecutionMain {
		prepareOptions.ReservePlanningTokens = o.resultProjector.ReservePlanningTokens()
	}
	if o.sessionContext != nil {
		var prepared sessionctx.PreparedContext
		var err error
		if options, ok := o.sessionContext.(optionsSessionPreparer); ok {
			prepared, err = options.PrepareWithOptions(ctx, conv, prepareOptions)
		} else if !profile.Persist {
			if stable, ok := o.sessionContext.(sessionctx.StablePreparer); ok {
				prepared = stable.PrepareStable(ctx)
			}
		} else {
			prepared, err = o.sessionContext.Prepare(ctx, conv, sessionctx.PrepareAuto)
		}
		if err != nil {
			return nil, err
		}
		optionalSections = append(optionalSections, prepared.StableSections...)
		if o.diagnostics != nil {
			o.diagnostics.Add(prepared.Diagnostics...)
		}
		if prepared.MessagesChanged && state != nil && o.candidateResultsEnabled() {
			if err := state.rebaseModelContentSlots(uniqueMessageSurvivors(beforeMessages, conv.Messages)); err != nil {
				return nil, err
			}
		}
		if prepared.MessagesChanged && profile.Persist && o.store != nil {
			if _, err := o.store.Save(ctx, conv); err != nil {
				return nil, err
			}
		}
	} else if o.contextManager != nil {
		result, err := o.contextManager.PrepareWithOptions(ctx, conv, prepareOptions)
		if err != nil {
			return nil, err
		}
		if result.Changed && state != nil && o.candidateResultsEnabled() {
			if err := state.rebaseModelContentSlots(uniqueMessageSurvivors(beforeMessages, conv.Messages)); err != nil {
				return nil, err
			}
		}
		if result.Changed && profile.Persist && o.store != nil {
			if _, err := o.store.Save(ctx, conv); err != nil {
				return nil, err
			}
		}
	}
	var requestRegistry *tool.Registry
	if includeTools {
		var err error
		requestRegistry, err = o.registryForProfile(mode, profile)
		if err != nil {
			return nil, err
		}
	}
	var hookBlocks []prompt.Block
	var lease hook.PromptLease
	if ref.ExecutionID != "" {
		acquired, err := o.hookRuntime().AcquirePrompts(requestCtxOrBackground(ctx), ref)
		if err != nil {
			return nil, err
		}
		var blocks []hook.PromptBlock
		if acquired != nil {
			blocks = acquired.Blocks()
			if len(blocks) == 0 {
				acquired.Release()
			} else {
				lease = acquired
			}
		}
		for _, block := range blocks {
			hookBlocks = append(hookBlocks, prompt.Block{
				Name: block.Name, Content: o.redactText(block.Content), Stable: false, Scope: prompt.ScopeRuntime,
			})
		}
	}
	bundle := prompt.Build(prompt.BuildRequest{
		Mode:                   promptRunMode(mode),
		Iteration:              iteration,
		ProjectRoot:            o.projectRoot(),
		SkillCatalog:           skill.CatalogPromptWithRedactor(profile.Catalog, o.redactText),
		ActiveSkills:           skill.ActivePromptWithRedactor(profile.Activity, o.redactText),
		OptionalStableSections: optionalSections,
		HookBlocks:             hookBlocks,
	})
	messages, err := o.providerMessagesWithState(conv, state, iteration)
	if err != nil {
		return nil, err
	}
	request := provider.ChatRequest{
		Model:         profile.Model,
		StableSystem:  o.providerStableBlocks(bundle.StableBlocks),
		DynamicSystem: o.providerDynamicBlocks(bundle.DynamicBlocks),
		Messages:      messages,
		Thinking:      o.thinking,
		Cache:         provider.CachePolicy{EnablePromptCache: true, CacheTools: includeTools},
	}
	if includeTools {
		request.Tools = toolDefinitionsFromRegistry(requestRegistry)
	}
	if bundle.UsesOrderedBlocks() {
		request.System = o.providerOrderedBlocks(bundle.OrderedBlocks)
		request.StableSystem = nil
		request.DynamicSystem = nil
		request.Cache.SystemBreakpointName = bundle.SystemBreakpointName
		request.Cache.CacheTools = false
	}
	if lease != nil {
		request.Observer = newPromptLeaseObserver(lease)
	}
	request, err = o.applyIndependentSkillHistory(ctx, request)
	if err != nil {
		if lease != nil {
			lease.Release()
		}
		return nil, err
	}
	if err := requestContextError(ctx); err != nil {
		if lease != nil {
			lease.Release()
		}
		if hasIndependentSkillHistoryBinding(ctx) {
			err = wrapIndependentSkillHistoryError(err)
		}
		return nil, err
	}
	startProvider := func(startCtx context.Context, finalRequest provider.ChatRequest) (provider.ChatStream, error) {
		if shouldCaptureParentPrompt(state, ref) {
			snapshot, snapshotErr := provider.CapturePromptPrefix(finalRequest)
			if snapshotErr != nil {
				return nil, snapshotErr
			}
			if snapshotErr = state.captureParentPrompt(ref, snapshot); snapshotErr != nil {
				return nil, snapshotErr
			}
		}
		return o.streamChat(startCtx, finalRequest)
	}
	if o.resultProjector != nil && ref.Kind == hook.ExecutionMain {
		owner, ownerErr := subagentResultOwner(conv, state, ref)
		if ownerErr != nil {
			if lease != nil {
				lease.Release()
			}
			return nil, ownerErr
		}
		stream, projectionErr := o.resultProjector.StreamChat(ctx, owner, request, startProvider)
		if projectionErr != nil && lease != nil {
			lease.Release()
		}
		return stream, projectionErr
	}
	if shouldCaptureParentPrompt(state, ref) {
		snapshot, snapshotErr := provider.CapturePromptPrefix(request)
		if snapshotErr == nil {
			snapshotErr = state.captureParentPrompt(ref, snapshot)
		}
		if snapshotErr != nil {
			if lease != nil {
				lease.Release()
			}
			return nil, snapshotErr
		}
	}
	return o.streamChat(ctx, request)
}

func shouldCaptureParentPrompt(state *executionState, ref hook.ExecutionRef) bool {
	return state != nil && state.requestGeneration > 0 && ref.ExecutionID != "" && ref.SessionID != "" &&
		state.ref.ExecutionID == ref.ExecutionID && state.ref.SessionID == ref.SessionID
}

func subagentResultOwner(conv *conversation.Conversation, state *executionState, ref hook.ExecutionRef) (subagent.ParentRef, error) {
	if conv == nil || state == nil {
		return subagent.ParentRef{}, errors.New("subagent result owner state is unavailable")
	}
	owner := subagent.ParentRef{
		ConversationID:    conv.ID,
		ExecutionID:       ref.ExecutionID,
		RequestGeneration: state.requestGeneration,
	}
	if !validResultProjectionOwner(owner) {
		return subagent.ParentRef{}, errors.New("subagent result owner is invalid")
	}
	return owner, nil
}

// streamChat is the single Provider handoff. Concrete Providers that own a
// lifecycle-aware ChatStream receive the exact resolved C7 options; legacy or
// test Providers that only implement the original interface remain compatible
// and use their existing StreamChat path.
func (o *Orchestrator) streamChat(ctx context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	if o == nil || o.provider == nil {
		return nil, errors.New("Provider 不可用")
	}
	if configured, ok := o.provider.(provider.ChatStreamOptionsProvider); ok {
		return configured.StreamChatWithOptions(ctx, request, o.chatStreamOptions)
	}
	return o.provider.StreamChat(ctx, request)
}

func (o *Orchestrator) projectRoot() string {
	if o.executor == nil {
		return ""
	}
	return o.executor.ProjectRoot
}

func promptRunMode(mode RunMode) prompt.RunMode {
	switch mode {
	case RunModePlan:
		return prompt.RunModePlan
	case RunModeDo:
		return prompt.RunModeDo
	default:
		return prompt.RunModeDefault
	}
}

func (o *Orchestrator) providerStableBlocks(blocks []prompt.Block) []provider.SystemBlock {
	result := make([]provider.SystemBlock, 0, len(blocks))
	for _, block := range blocks {
		result = append(result, provider.SystemBlock{Name: block.Name, Content: o.safeText(block.Content), Cacheable: true, Scope: block.Scope})
	}
	return result
}

func (o *Orchestrator) providerDynamicBlocks(blocks []prompt.Block) []provider.SystemBlock {
	result := make([]provider.SystemBlock, 0, len(blocks))
	for _, block := range blocks {
		result = append(result, provider.SystemBlock{Name: block.Name, Content: o.safeText(block.Content), Cacheable: false, Scope: block.Scope})
	}
	return result
}

func (o *Orchestrator) providerOrderedBlocks(blocks []prompt.Block) []provider.SystemBlock {
	result := make([]provider.SystemBlock, 0, len(blocks))
	for _, block := range blocks {
		result = append(result, provider.SystemBlock{Name: block.Name, Content: o.safeText(block.Content), Cacheable: block.Stable, Scope: block.Scope})
	}
	return result
}

func (o *Orchestrator) providerMessages(messages []conversation.Message) []provider.ModelMessage {
	result, _ := o.providerMessagesFromSlice(messages, "", nil, 0)
	return result
}

func (o *Orchestrator) providerMessagesWithState(conv *conversation.Conversation, state *executionState, iteration int) ([]provider.ModelMessage, error) {
	if conv == nil {
		return nil, fmt.Errorf("conversation is unavailable")
	}
	return o.providerMessagesFromSlice(conv.Messages, conv.ID, state, iteration)
}

func (o *Orchestrator) providerMessagesFromSlice(messages []conversation.Message, conversationID string, state *executionState, iteration int) ([]provider.ModelMessage, error) {
	result := make([]provider.ModelMessage, 0, len(messages))
	for messageIndex, message := range messages {
		role, ok := providerMessageRole(message.Role)
		if !ok {
			continue
		}
		modelMessage := provider.ModelMessage{
			Role:    role,
			Content: message.Content,
		}
		if message.Tool != nil {
			modelMessage.ToolCallID = message.Tool.CallID
			modelMessage.ToolName = message.Tool.Name
			modelMessage.ArgumentsJSON = message.Tool.ArgumentsJSON
			modelMessage.ToolResult = message.Tool.Result
			modelMessage.ToolResultStatus = string(message.Tool.Status)
			if state != nil && message.Role == conversation.RoleToolResult {
				modelContent, found, err := state.lookupModelContentForMessage(conversationID, iteration, messageIndex, message.Tool.CallID)
				if err != nil {
					return nil, err
				}
				if found {
					modelMessage.Content = modelContent
					modelMessage.ToolResult = modelContent
				}
			}
		}
		result = append(result, modelMessage)
	}
	return result, nil
}

func uniqueMessageSurvivors(before, after []conversation.Message) map[int][]int {
	result := make(map[int][]int, len(before))
	for oldIndex := range before {
		for newIndex := range after {
			if sameConversationMessage(before[oldIndex], after[newIndex]) {
				result[oldIndex] = append(result[oldIndex], newIndex)
			}
		}
	}
	return result
}

func sameConversationMessage(left, right conversation.Message) bool {
	if left.Role != right.Role || left.Content.Text() != right.Content.Text() || !left.CreatedAt.Equal(right.CreatedAt) || (left.Tool == nil) != (right.Tool == nil) {
		return false
	}
	if left.Tool == nil {
		return true
	}
	l, r := left.Tool, right.Tool
	if l.CallID != r.CallID || l.Name != r.Name || l.ArgumentsJSON.Text() != r.ArgumentsJSON.Text() || l.State != r.State ||
		l.Status != r.Status || l.Summary.Text() != r.Summary.Text() || l.Result.Text() != r.Result.Text() ||
		l.Truncated != r.Truncated || l.TruncationReason.Text() != r.TruncationReason.Text() ||
		(l.Artifact == nil) != (r.Artifact == nil) || (l.Error == nil) != (r.Error == nil) {
		return false
	}
	if l.Artifact != nil && *l.Artifact != *r.Artifact {
		return false
	}
	return l.Error == nil || (l.Error.Code == r.Error.Code && l.Error.Message.Text() == r.Error.Message.Text() && l.Error.Recoverable == r.Error.Recoverable)
}

func providerMessageRole(role conversation.MessageRole) (provider.ModelMessageRole, bool) {
	switch role {
	case conversation.RoleUser:
		return provider.ModelMessageRoleUser, true
	case conversation.RoleAssistant:
		return provider.ModelMessageRoleAssistant, true
	case conversation.RoleToolCall:
		return provider.ModelMessageRoleToolCall, true
	case conversation.RoleToolResult:
		return provider.ModelMessageRoleToolResult, true
	case conversation.RoleContextSummary:
		return provider.ModelMessageRoleContextSummary, true
	case conversation.RoleContextBoundary:
		return provider.ModelMessageRoleContextBoundary, true
	default:
		return "", false
	}
}

type projectedToolPublication struct {
	executionIndex int
	execution      ToolExecution
	projection     contextmgr.ToolResultProjection
	arguments      redact.SafeText
}

// projectToolResult is the sole ContextManager projection authority for both
// the main loop and isolated subagent loops. Keeping one production call site
// prevents either path from bypassing the same safe candidate boundary.
func projectToolResult(manager *contextmgr.Manager, result tool.Result) (contextmgr.ToolResultProjection, error) {
	if manager == nil {
		return contextmgr.ToolResultProjection{}, errors.New("tool result projector is unavailable")
	}
	return manager.ProjectToolResult(result)
}

type orderedToolCommitError struct {
	err error
}

func (e *orderedToolCommitError) Error() string {
	if e == nil || e.err == nil {
		return "ordered tool commit failed"
	}
	return e.err.Error()
}

func (e *orderedToolCommitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func isOrderedToolCommitError(err error) bool {
	var commitErr *orderedToolCommitError
	return errors.As(err, &commitErr)
}

func (o *Orchestrator) publishToolExecutions(ctx context.Context, conv *conversation.Conversation, state *executionState, providerIteration int, executions []ToolExecution, out chan<- events.Event) ([]ToolExecution, error) {
	if !o.candidateResultsEnabled() {
		// The migration adapter already emitted the bounded compatibility
		// Event/Hook at execution time. It cannot project, persist, or stage.
		return executions, nil
	}
	if o.contextManager == nil || state == nil || conv == nil {
		return executions, fmt.Errorf("safe tool result publisher is unavailable")
	}
	// Results produced after Provider iteration N are consumed by request N+1.
	nextIteration := providerIteration + 1
	slots := &state.modelContents
	slots.mu.Lock()
	generation := slots.requestGeneration
	slots.mu.Unlock()
	if err := state.beginModelContentIteration(conv.ID, generation, nextIteration); err != nil {
		return executions, err
	}
	publications := make([]projectedToolPublication, 0, len(executions))
	for index := range executions {
		execution := executions[index]
		if execution.State == tool.CancelledBeforeStart {
			if execution.HasResult || execution.Result.CallID != "" {
				state.clearModelContentSlots()
				return executions, fmt.Errorf("tool cancelled before start cannot publish a result")
			}
			continue
		}
		if execution.State.Valid() && !execution.State.CanProduceResult() && execution.HasResult {
			state.clearModelContentSlots()
			return executions, fmt.Errorf("tool execution state cannot publish a result")
		}
		if execution.State.CanProduceResult() && !execution.HasResult {
			state.clearModelContentSlots()
			return executions, fmt.Errorf("terminal tool execution is missing its result")
		}
		if !execution.HasResult {
			continue
		}
		if resultState := execution.Result.ExecutionState(); resultState.Valid() && execution.State.Valid() && resultState != execution.State {
			state.clearModelContentSlots()
			return executions, fmt.Errorf("tool execution state does not match its result")
		}
		projection, err := projectToolResult(o.contextManager, execution.Result)
		if err != nil {
			state.clearModelContentSlots()
			return executions, err
		}
		publications = append(publications, projectedToolPublication{
			executionIndex: index, execution: execution, projection: projection, arguments: o.safeText(redactedArguments(execution.Call)),
		})
	}
	// Execution workers retain their original return slots. Only the durable
	// publication view is ordered by the Provider call ordinal.
	sort.SliceStable(publications, func(i, j int) bool {
		return publications[i].execution.Index < publications[j].execution.Index
	})
	if len(publications) == 0 {
		return executions, nil
	}
	baseMessageIndex := len(conv.Messages)
	for index := range publications {
		publication := &publications[index]
		messageIndex := baseMessageIndex + index*2 + 1
		execution := publication.execution
		if _, err := state.stageCurrentModelContent(nextIteration, messageIndex, execution.Call.ID, publication.projection.ModelContent); err != nil {
			state.clearModelContentSlots()
			return executions, err
		}
	}
	checkpoint := checkpointConversation(conv)
	for index := range publications {
		publication := &publications[index]
		call := publication.execution.Call
		o.appendConversationMessage(conv, conversation.RoleToolCall, o.safeText(call.Name), &conversation.ToolState{
			CallID: call.ID, Name: call.Name, ArgumentsJSON: publication.arguments, State: tool.Prepared,
		})
		messageIndex, err := conversation.AppendProjectedToolResultMessage(conv, conversation.ToolResultMessageInput{
			CallID: call.ID, Name: call.Name, PersistedContent: publication.projection.PersistedContent,
			UserView: publication.projection.UserView, OutputMeta: publication.projection.OutputMeta,
		})
		if err != nil || messageIndex != baseMessageIndex+index*2+1 {
			restoreConversation(conv, checkpoint)
			state.clearModelContentSlots()
			if err != nil {
				return executions, err
			}
			return executions, fmt.Errorf("projected tool result message index mismatch")
		}
		publication.execution.Projection = publication.projection
		publication.execution.Projected = true
		executions[publication.executionIndex] = publication.execution
	}
	var eventErr error
	for index := range publications {
		publication := publications[index]
		execution := publication.execution
		if execution.HookInput.CallID != "" {
			o.hookRuntime().AfterTool(hookLifecycleContext(ctx), execution.Ref, execution.HookInput, hook.ToolOutputFromUserView(publication.projection.UserView), execution.Duration)
		}
		if !emitEvent(ctx, out, events.ToolResultEventFromUserView(execution.Call.ID, execution.Call.Name, publication.arguments, publication.projection.UserView)) {
			eventErr = ctx.Err()
			if eventErr == nil {
				eventErr = fmt.Errorf("tool result event publication failed")
			}
			break
		}
	}
	// Conversation is the execution-fact owner once all safe projections have
	// been appended. Event cancellation must not roll it back or prevent the
	// one ordered save attempt.
	saveErr := o.saveConversationIfNeeded(ctx, conv, state)
	if eventErr != nil || saveErr != nil {
		return executions, &orderedToolCommitError{err: errors.Join(eventErr, saveErr)}
	}
	return executions, nil
}

func (o *Orchestrator) publishToolExecution(ctx context.Context, conv *conversation.Conversation, state *executionState, providerIteration int, execution ToolExecution, out chan<- events.Event) (ToolExecution, error) {
	published, err := o.publishToolExecutions(ctx, conv, state, providerIteration, []ToolExecution{execution}, out)
	if len(published) == 0 {
		return execution, err
	}
	return published[0], err
}

func (o *Orchestrator) appendConversationMessage(conv *conversation.Conversation, role conversation.MessageRole, content redact.SafeText, toolState *conversation.ToolState) {
	if conv == nil {
		return
	}
	now := time.Now()
	conv.Messages = append(conv.Messages, conversation.Message{Role: role, Content: content, CreatedAt: now, Tool: toolState})
	if role == conversation.RoleUser && conv.Title.Text() == "新会话" {
		conv.Title = o.safeText(conversationTitle(content.Text()))
	}
	conv.UpdatedAt = now
}

func conversationTitle(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	if value == "" {
		return "新会话"
	}
	runes := []rune(value)
	if len(runes) > 24 {
		return string(runes[:24]) + "..."
	}
	return value
}

func streamToolCalls(event provider.StreamEvent) []tool.Call {
	if event.ToolCall == nil {
		return nil
	}
	return []tool.Call{{
		ID:            event.ToolCall.ID,
		Name:          event.ToolCall.Name,
		ArgumentsJSON: event.ToolCall.ArgumentsJSON.Text(),
	}}
}

func toolResultEvent(result tool.Result) events.Event {
	eventType := events.ToolSuccess
	if result.Status == tool.StatusDenied {
		eventType = events.ToolDenied
	} else if result.Status != tool.StatusSuccess {
		eventType = events.ToolError
	}
	return events.Event{Type: eventType, Tool: resultDisplay(result)}
}

func (o *Orchestrator) safeToolResultEvent(result tool.Result) events.Event {
	event := toolResultEvent(result)
	if event.Tool != nil {
		o.redactToolDisplay(event.Tool)
	}
	return event
}

func (o *Orchestrator) safeToolDisplay(call tool.Call, status events.ToolDisplayStatus, summary string) *events.ToolDisplay {
	display := newToolDisplay(call, status, summary)
	o.redactToolDisplay(display)
	return display
}

func (o *Orchestrator) safeResultDisplay(result tool.Result) *events.ToolDisplay {
	display := resultDisplay(result)
	o.redactToolDisplay(display)
	return display
}

func (o *Orchestrator) redactToolDisplay(display *events.ToolDisplay) {
	if display == nil {
		return
	}
	display.Arguments = o.safeText(display.Arguments.Text())
	display.Summary = o.safeText(display.Summary.Text())
	display.Stdout = o.safeText(display.Stdout.Text())
	display.Stderr = o.safeText(display.Stderr.Text())
	if display.Artifact != nil {
		display.Artifact.ID = o.redactText(display.Artifact.ID)
	}
}

func (o *Orchestrator) safeConfirmationRequest(call tool.Call, decision permission.Decision, confirmationID string) *events.ToolConfirmationRequest {
	request := confirmationRequest(call, decision, confirmationID)
	if request == nil {
		return nil
	}
	request.Arguments = o.safeText(request.Arguments.Text())
	request.Prompt = o.safeText(request.Prompt.Text())
	request.Target = o.safeText(request.Target.Text())
	request.ScopePreview = o.safeText(request.ScopePreview.Text())
	for index := range request.Scopes {
		request.Scopes[index].Description = o.safeText(request.Scopes[index].Description.Text())
	}
	request.RuleLocation = o.safeText(request.RuleLocation.Text())
	request.Warning = o.safeText(request.Warning.Text())
	request.RevokeHint = o.safeText(request.RevokeHint.Text())
	return request
}

func newToolDisplay(call tool.Call, status events.ToolDisplayStatus, summary string) *events.ToolDisplay {
	redactor := redact.NewRuntimeRedactor()
	return &events.ToolDisplay{CallID: call.ID, Name: call.Name, Arguments: redactor.Redact(redactedArguments(call)), Summary: redactor.Redact(summary), Status: status}
}

func redactedArguments(call tool.Call) string {
	var args map[string]any
	if json.Unmarshal([]byte(call.ArgumentsJSON), &args) != nil {
		return redactSensitive(call.ArgumentsJSON)
	}
	if strings.HasPrefix(call.Name, "mcp__") {
		data, err := json.Marshal(redact.Map(args))
		if err != nil {
			return "{}"
		}
		return string(data)
	}
	for _, key := range []string{"command", "content", "old_text", "new_text", "path", "pattern", "name", "args"} {
		if value, ok := args[key].(string); ok {
			args[key] = redactSensitive(value)
		}
	}
	data, err := json.Marshal(args)
	if err != nil {
		return redactSensitive(call.ArgumentsJSON)
	}
	return string(data)
}

func resultHistoryStatus(result tool.Result) string {
	if result.Status == tool.StatusDenied && result.Data != nil && result.Data["reason"] == string(permission.ReasonUserCancelled) {
		return string(events.ToolDisplayCancelled)
	}
	return string(result.Status)
}

func resultDisplay(result tool.Result) *events.ToolDisplay {
	redactor := redact.NewRuntimeRedactor()
	status := events.ToolDisplaySuccess
	switch result.Status {
	case tool.StatusError, tool.StatusTimeout:
		status = events.ToolDisplayError
	case tool.StatusDenied:
		status = events.ToolDisplayDenied
		if result.Data != nil && result.Data["reason"] == string(permission.ReasonUserCancelled) {
			status = events.ToolDisplayCancelled
		}
	}
	display := &events.ToolDisplay{
		CallID:      result.CallID,
		Name:        result.Name,
		Summary:     redactor.Redact(redactSensitive(result.Summary)),
		Status:      status,
		ErrorCode:   resultErrorCode(result),
		Stdout:      redactor.Redact(stringData(result.Data, "stdout")),
		Stderr:      redactor.Redact(stringData(result.Data, "stderr")),
		Truncated:   result.Truncated,
		Recoverable: resultRecoverable(result),
	}
	if id := artifactID(result); id != "" {
		display.Artifact = &events.ArtifactRef{
			ID:        id,
			Bytes:     artifactBytes(result),
			Available: artifactAvailable(result),
		}
	}
	return display
}

func stringData(data map[string]any, key string) string {
	if data == nil {
		return ""
	}
	value, _ := data[key].(string)
	return redactSensitive(value)
}

func resultRecoverable(result tool.Result) bool {
	return result.Error != nil && result.Error.Recoverable
}

func artifactID(result tool.Result) string {
	return stringData(result.Data, "artifact_id")
}

func artifactAvailable(result tool.Result) bool {
	if result.Data == nil {
		return false
	}
	value, _ := result.Data["artifact_available"].(bool)
	return value
}

func artifactBytes(result tool.Result) int64 {
	if result.Data == nil {
		return 0
	}
	switch value := result.Data["artifact_bytes"].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	default:
		return 0
	}
}

func formatToolConfirmation(call tool.Call) string {
	var args map[string]any
	_ = json.Unmarshal([]byte(call.ArgumentsJSON), &args)
	switch call.Name {
	case "Write":
		return fmt.Sprintf("Write(path=%s, content=%q)", stringArg(args, "path"), previewArg(args, "content"))
	case "Edit":
		return fmt.Sprintf("Edit(path=%s, old=%q, new=%q)", stringArg(args, "path"), previewArg(args, "old_text"), previewArg(args, "new_text"))
	case "Bash":
		return fmt.Sprintf("Bash(command=%s, cwd=项目根目录)", redactSensitive(stringArg(args, "command")))
	default:
		if strings.HasPrefix(call.Name, "mcp__") {
			server, remote := mcpToolParts(call.Name)
			return fmt.Sprintf("MCP(server=%s, tool=%s, args=%s)", server, remote, previewText(redactedArguments(call), 160))
		}
		return formatToolCall(call)
	}
}

func confirmationRequest(call tool.Call, decision permission.Decision, confirmationID string) *events.ToolConfirmationRequest {
	redactor := redact.NewRuntimeRedactor()
	request := &events.ToolConfirmationRequest{
		ConfirmationID: confirmationID,
		CallID:         call.ID,
		Name:           call.Name,
		Arguments:      redactor.Redact(redactedArguments(call)),
		Prompt:         redactor.Redact(formatPermissionPrompt(call, decision)),
		AllowPermanent: decision.Prompt != nil && decision.Prompt.AllowPermanent,
	}
	if decision.Prompt == nil {
		return request
	}
	request.Risk = string(decision.Prompt.Risk)
	request.PermissionMode = string(decision.Prompt.Mode)
	request.Target = redactor.Redact(decision.Prompt.Target)
	if decision.Prompt.RulePreview != nil {
		request.ScopePreview = redactor.Redact(decision.Prompt.RulePreview.Display())
	}
	if decision.Prompt.Scopes != nil {
		request.Scopes = make([]events.ConfirmationScopeDisplay, len(decision.Prompt.Scopes))
		for index, scope := range decision.Prompt.Scopes {
			request.Scopes[index] = events.ConfirmationScopeDisplay{
				Scope:       string(scope.Scope),
				Available:   scope.Available,
				Description: redactor.Redact(scope.Description),
			}
		}
	}
	request.RuleLocation = redactor.Redact(decision.Prompt.RuleLocation)
	request.Warning = redactor.Redact(permissionWarning(call, decision.Prompt))
	request.RevokeHint = redactor.Redact(decision.Prompt.RevokeHint)
	return request
}

func (o *Orchestrator) openToolConfirmation(callID string) (string, <-chan events.ToolConfirmationDecision, func(), bool) {
	if o == nil || strings.TrimSpace(callID) == "" {
		return "", nil, func() {}, false
	}
	generation, ok := nextAtomicSequence(&o.confirmationSequence)
	if !ok {
		return "", nil, func() {}, false
	}
	confirmationID := fmt.Sprintf("confirmation-%d", generation)
	pending := &pendingConfirmation{callID: callID, decision: make(chan events.ToolConfirmationDecision, 1)}
	o.confirmationMu.Lock()
	if o.pendingConfirmations == nil {
		o.pendingConfirmations = make(map[string]*pendingConfirmation)
	}
	o.pendingConfirmations[confirmationID] = pending
	o.confirmationMu.Unlock()

	release := func() {
		o.confirmationMu.Lock()
		if o.pendingConfirmations[confirmationID] == pending {
			delete(o.pendingConfirmations, confirmationID)
		}
		o.confirmationMu.Unlock()
	}
	return confirmationID, pending.decision, release, true
}

func nextAtomicSequence(sequence *atomic.Uint64) (uint64, bool) {
	if sequence == nil {
		return 0, false
	}
	for {
		current := sequence.Load()
		if current == math.MaxUint64 {
			return 0, false
		}
		if sequence.CompareAndSwap(current, current+1) {
			return current + 1, true
		}
	}
}

// ResolveToolConfirmation is the narrow decision boundary used by App. The
// safe confirmation Event carries only opaque identity and display values;
// the writable decision channel remains owned by Orchestrator.
func (o *Orchestrator) ResolveToolConfirmation(decision events.ToolConfirmationDecision) bool {
	if o == nil || strings.TrimSpace(decision.ConfirmationID) == "" || strings.TrimSpace(decision.CallID) == "" {
		return false
	}
	o.confirmationMu.Lock()
	defer o.confirmationMu.Unlock()
	pending := o.pendingConfirmations[decision.ConfirmationID]
	if pending == nil || pending.callID != decision.CallID || pending.resolved {
		return false
	}
	select {
	case pending.decision <- decision:
		pending.resolved = true
		return true
	default:
		return false
	}
}

func permissionWarning(call tool.Call, prompt *permission.ConfirmationPrompt) string {
	if call.Name == "Bash" {
		return "Bash 不在沙箱中运行，请确认命令可信且作用范围符合预期。"
	}
	if prompt.Risk == permission.RiskHigh {
		return "高风险工具调用，请确认目标和影响范围。"
	}
	return ""
}

func formatPermissionPrompt(call tool.Call, decision permission.Decision) string {
	base := formatToolConfirmation(call)
	if decision.Prompt == nil {
		return base + " 需要权限确认，按 y 允许本次，按 s 本会话允许，按 p 永久允许，按 n 拒绝，Esc 取消。"
	}
	parts := []string{base, "需要权限确认"}
	parts = append(parts, "风险: "+string(decision.Prompt.Risk))
	if decision.Prompt.Reason != "" {
		parts = append(parts, "原因: "+decision.Prompt.Reason)
	}
	if decision.Prompt.Mode != "" {
		parts = append(parts, "模式: "+string(decision.Prompt.Mode))
	}
	if decision.Prompt.RulePreview != nil {
		parts = append(parts, "范围: "+decision.Prompt.RulePreview.Display())
	}
	if warning := permissionWarning(call, decision.Prompt); warning != "" {
		parts = append(parts, "警告: "+warning)
	}
	if hint := strings.TrimSpace(decision.Prompt.RevokeHint); hint != "" {
		parts = append(parts, hint)
	}
	shortcut := "按 y 允许本次，按 s 本会话允许，按 n 拒绝，Esc 取消。"
	if decision.Prompt.AllowPermanent {
		shortcut = "按 y 允许本次，按 s 本会话允许，按 p 永久允许，按 n 拒绝，Esc 取消。"
	}
	parts = append(parts, shortcut)
	return strings.Join(parts, "；")
}

func previewArg(args map[string]any, key string) string {
	return previewText(redactSensitive(stringArg(args, key)), 120)
}

func previewText(value string, limit int) string {
	if len([]rune(value)) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "..."
}

func mcpToolParts(name string) (string, string) {
	trimmed := strings.TrimPrefix(name, "mcp__")
	parts := strings.SplitN(trimmed, "__", 2)
	if len(parts) != 2 {
		return trimmed, ""
	}
	return parts[0], parts[1]
}

func redactSensitive(value string) string {
	return strings.ReplaceAll(redact.Text(value), "[redacted]", "[REDACTED]")
}

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, _ := args[key].(string)
	return value
}

func formatToolCall(call tool.Call) string {
	return fmt.Sprintf("%s(%s)", call.Name, call.ArgumentsJSON)
}

func resultContent(result tool.Result) string {
	payload := struct {
		Status    tool.ResultStatus `json:"status"`
		Summary   string            `json:"summary"`
		Content   string            `json:"content,omitempty"`
		Error     *tool.Error       `json:"error,omitempty"`
		Truncated bool              `json:"truncated,omitempty"`
	}{
		Status:    result.Status,
		Summary:   result.Summary,
		Content:   result.Content,
		Error:     result.Error,
		Truncated: result.Truncated,
	}
	data, err := marshalRedactedJSON(payload)
	if err != nil {
		if strings.TrimSpace(result.Content) != "" {
			return redactSensitive(result.Content)
		}
		return redactSensitive(result.Summary)
	}
	return string(data)
}

func marshalRedactedJSON(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	redacted := redact.Any(decoded)
	data, err = json.Marshal(redacted)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func resultErrorCode(result tool.Result) string {
	if result.Error == nil {
		return ""
	}
	return result.Error.Code
}
