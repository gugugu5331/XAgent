package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/mcpclient"
	"xagent/internal/memory"
	"xagent/internal/permission"
	"xagent/internal/prompt"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/resources"
	"xagent/internal/sessionctx"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

type OrchestratorOptions struct {
	Provider            provider.Provider
	Store               conversation.ConversationStore
	Resources           resources.PromptProvider
	Thinking            config.ThinkingConfig
	Registry            *tool.Registry
	Executor            *tool.Executor
	ContextManager      *contextmgr.Manager
	SessionContext      sessionPreparer
	Memory              memoryUpdater
	Diagnostics         *diagnostics.Collector
	Agent               config.AgentConfig
	SkillManager        *skill.Manager
	DefaultModel        string
	Redact              func(string) string
	RedactionLookbehind int
	Hooks               hook.Runtime
}

type sessionPreparer interface {
	Prepare(ctx context.Context, conv *conversation.Conversation, mode sessionctx.PrepareMode) (sessionctx.PreparedContext, error)
}

type stableSessionPreparer interface {
	PrepareStable(ctx context.Context) sessionctx.PreparedContext
}

type optionsSessionPreparer interface {
	PrepareWithOptions(ctx context.Context, conv *conversation.Conversation, opts contextmgr.PrepareOptions) (sessionctx.PreparedContext, error)
}

type memoryUpdater interface {
	UpdateAsync(input memory.UpdateInput)
}

type Orchestrator struct {
	provider            provider.Provider
	store               conversation.ConversationStore
	resources           resources.PromptProvider
	thinking            config.ThinkingConfig
	registry            *tool.Registry
	readOnlyRegistry    *tool.Registry
	executor            *tool.Executor
	authorizer          *permission.Authorizer
	contextManager      *contextmgr.Manager
	sessionContext      sessionPreparer
	memory              memoryUpdater
	diagnostics         *diagnostics.Collector
	skillManager        *skill.Manager
	defaultModel        string
	redact              func(string) string
	redactionLookbehind int
	permissionMode      permission.Mode
	runOptions          RunOptions
	runs                *runTracker
	hooks               hook.Runtime
}

func New(provider provider.Provider, store conversation.ConversationStore, resources resources.PromptProvider, thinking config.ThinkingConfig) *Orchestrator {
	return NewWithOptions(OrchestratorOptions{Provider: provider, Store: store, Resources: resources, Thinking: thinking})
}

func NewWithTools(provider provider.Provider, store conversation.ConversationStore, resources resources.PromptProvider, thinking config.ThinkingConfig, registry *tool.Registry, executor *tool.Executor) *Orchestrator {
	return NewWithToolsAndContext(provider, store, resources, thinking, registry, executor, nil)
}

func NewWithToolsAndContext(provider provider.Provider, store conversation.ConversationStore, resources resources.PromptProvider, thinking config.ThinkingConfig, registry *tool.Registry, executor *tool.Executor, contextManager *contextmgr.Manager) *Orchestrator {
	return NewWithOptions(OrchestratorOptions{Provider: provider, Store: store, Resources: resources, Thinking: thinking, Registry: registry, Executor: executor, ContextManager: contextManager})
}

func NewWithOptions(options OrchestratorOptions) *Orchestrator {
	redactor := options.Redact
	if redactor == nil {
		redactor = redact.Text
	}
	lookbehind := options.RedactionLookbehind
	if lookbehind < 64 {
		lookbehind = 64
	}
	var readOnlyRegistry *tool.Registry
	var authorizer *permission.Authorizer
	if options.Executor != nil {
		readOnlyRegistry, _ = tool.NewReadOnlyRegistry(options.Executor.ProjectRoot)
		loaded := permission.LoadRules(options.Executor.ProjectRoot)
		authorizer = &permission.Authorizer{
			Session:    permission.NewSession(),
			User:       loaded.User,
			Project:    loaded.Project,
			Local:      loaded.Local,
			LoadErrors: loaded.Errors,
			Writer:     permission.Writer{},
			Redact:     redactor,
		}
	}
	runOptions := runOptionsFromConfig(options.Agent)
	hooks := options.Hooks
	if hooks == nil {
		hooks = hook.Noop()
	}
	return &Orchestrator{provider: options.Provider, store: options.Store, resources: options.Resources, thinking: options.Thinking, registry: options.Registry, readOnlyRegistry: readOnlyRegistry, executor: options.Executor, authorizer: authorizer, contextManager: options.ContextManager, sessionContext: options.SessionContext, memory: options.Memory, diagnostics: options.Diagnostics, skillManager: options.SkillManager, defaultModel: strings.TrimSpace(options.DefaultModel), redact: redactor, redactionLookbehind: lookbehind, permissionMode: permission.ModeDefault, runOptions: runOptions, runs: newRunTracker(), hooks: hooks}
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
		if saveErr := o.store.Save(ctx, conv); saveErr != nil {
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
	runtime := o.hookRuntime()
	state.ref = runtime.BeginTurn(requestCtxOrBackground(ctx), conv.ID, hook.ExecutionMain, hookMode(req.Mode))
	message := runtime.BeginMessage(requestCtxOrBackground(ctx), state.ref, hook.MessageUser, req.UserText)
	conversation.AppendUserMessage(conv, req.UserText)
	runtime.EndMessage(requestCtxOrBackground(ctx), message)
	out := make(chan events.Event)

	go func() {
		defer close(out)
		defer endRun()
		start := time.Now()
		result := RunResult{}
		if !emitEvent(ctx, out, events.Event{Type: events.UserSubmitted, Text: req.UserText}) {
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
		emitEvent(ctx, out, events.Event{Type: events.Error, Err: o.redactError(result.Err)})
		return
	}
	emitEvent(ctx, out, events.Event{Type: events.Done, Duration: result.Duration})
}

func (o *Orchestrator) stream(ctx context.Context, conv *conversation.Conversation, mode RunMode, includeTools bool, iteration int) (<-chan provider.StreamEvent, error) {
	profile, err := o.buildExecutionProfile(mode, skill.NewActivity(), 0)
	if err != nil {
		return nil, err
	}
	return o.streamWithProfile(ctx, conv, mode, profile, includeTools, iteration)
}

func (o *Orchestrator) streamWithProfile(ctx context.Context, conv *conversation.Conversation, mode RunMode, profile skill.ExecutionProfile, includeTools bool, iteration int) (<-chan provider.StreamEvent, error) {
	return o.streamWithExecution(ctx, conv, mode, profile, includeTools, iteration, hook.ExecutionRef{})
}

func (o *Orchestrator) streamWithExecution(ctx context.Context, conv *conversation.Conversation, mode RunMode, profile skill.ExecutionProfile, includeTools bool, iteration int, ref hook.ExecutionRef) (<-chan provider.StreamEvent, error) {
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
	if o.sessionContext != nil {
		var prepared sessionctx.PreparedContext
		var err error
		if options, ok := o.sessionContext.(optionsSessionPreparer); ok {
			prepared, err = options.PrepareWithOptions(ctx, conv, prepareOptions)
		} else if !profile.Persist {
			if stable, ok := o.sessionContext.(stableSessionPreparer); ok {
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
		if prepared.MessagesChanged && profile.Persist && o.store != nil {
			if err := o.store.Save(ctx, conv); err != nil {
				return nil, err
			}
		}
	} else if o.contextManager != nil {
		result, err := o.contextManager.PrepareWithOptions(ctx, conv, prepareOptions)
		if err != nil {
			return nil, err
		}
		if result.Changed && profile.Persist && o.store != nil {
			if err := o.store.Save(ctx, conv); err != nil {
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
			hookBlocks = append(hookBlocks, prompt.Block{Name: block.Name, Content: o.redactText(block.Content), Stable: false})
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
	request := provider.ChatRequest{
		Model:         profile.Model,
		StableSystem:  providerStableBlocks(bundle.StableBlocks),
		DynamicSystem: providerDynamicBlocks(bundle.DynamicBlocks),
		Messages:      conversation.ContextMessages(conv),
		Thinking:      o.thinking,
		Cache:         provider.CachePolicy{EnablePromptCache: true},
	}
	if includeTools {
		request.Tools = toolDefinitionsFromRegistry(requestRegistry)
	}
	if bundle.UsesOrderedBlocks() {
		request.System = providerOrderedBlocks(bundle.OrderedBlocks)
		request.StableSystem = nil
		request.DynamicSystem = nil
		request.Cache.SystemBreakpointName = bundle.SystemBreakpointName
		request.Cache.CacheTools = false
	}
	if lease != nil {
		request.Observer = newPromptLeaseObserver(lease)
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

func providerStableBlocks(blocks []prompt.Block) []provider.SystemBlock {
	result := make([]provider.SystemBlock, 0, len(blocks))
	for _, block := range blocks {
		result = append(result, provider.SystemBlock{Name: block.Name, Content: block.Content, Cacheable: true})
	}
	return result
}

func providerDynamicBlocks(blocks []prompt.Block) []provider.SystemBlock {
	result := make([]provider.SystemBlock, 0, len(blocks))
	for _, block := range blocks {
		result = append(result, provider.SystemBlock{Name: block.Name, Content: block.Content, Cacheable: false})
	}
	return result
}

func providerOrderedBlocks(blocks []prompt.Block) []provider.SystemBlock {
	result := make([]provider.SystemBlock, 0, len(blocks))
	for _, block := range blocks {
		result = append(result, provider.SystemBlock{Name: block.Name, Content: block.Content, Cacheable: block.Stable})
	}
	return result
}

func (o *Orchestrator) appendToolMessages(conv *conversation.Conversation, call tool.Call, result tool.Result) {
	data := resultData(result)
	errorData := resultErrorData(result)
	if len(data) > 0 {
		data = json.RawMessage(o.redactText(string(data)))
	}
	if len(errorData) > 0 {
		errorData = json.RawMessage(o.redactText(string(errorData)))
	}
	conversation.AppendToolCallMessage(conv, call.ID, call.Name, o.redactText(redactedArguments(call)))
	conversation.AppendToolResultMessage(conv, call.ID, call.Name, resultHistoryStatus(result), o.redactText(redactSensitive(result.Summary)), o.redactText(resultContent(result)), resultErrorCode(result), result.Truncated, data, errorData)
}

func (o *Orchestrator) appendUnsupportedToolMessages(conv *conversation.Conversation, calls []tool.Call, result tool.Result) {
	for _, call := range calls {
		callResult := result
		callResult.CallID = call.ID
		callResult.Name = call.Name
		o.appendToolMessages(conv, call, callResult)
	}
}

func streamToolCalls(event provider.StreamEvent) []tool.Call {
	if len(event.ToolCalls) > 0 {
		return event.ToolCalls
	}
	if event.ToolCall == nil {
		return nil
	}
	return []tool.Call{*event.ToolCall}
}

func multipleToolCallsResult(calls []tool.Call) tool.Result {
	callID := "multiple_tool_calls"
	if len(calls) > 0 && calls[0].ID != "" {
		callID = calls[0].ID
	}
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.Name)
	}
	return tool.Result{
		CallID:  callID,
		Name:    "multiple_tool_calls",
		Status:  tool.StatusError,
		Summary: "本阶段不支持一次返回多个工具调用",
		Content: "本阶段一次只能执行一个工具调用，请重新选择一个工具调用。",
		Data:    map[string]any{"tool_names": names, "count": len(calls)},
		Error:   &tool.Error{Code: tool.ErrMultipleToolCallsUnsupported, Message: "本阶段不支持一次返回多个工具调用", Recoverable: true},
	}
}

func unsupportedToolLoopResult(call tool.Call) tool.Result {
	return tool.Result{
		CallID:  call.ID,
		Name:    call.Name,
		Status:  tool.StatusError,
		Summary: "本阶段不支持连续工具调用",
		Content: "本阶段不支持 Agent Loop 中的第二次工具调用。",
		Error:   &tool.Error{Code: tool.ErrMultipleToolCallsUnsupported, Message: "本阶段不支持连续工具调用", Recoverable: true},
	}
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
	display.Arguments = o.redactText(display.Arguments)
	display.Summary = o.redactText(display.Summary)
	display.Stdout = o.redactText(display.Stdout)
	display.Stderr = o.redactText(display.Stderr)
	display.ArtifactID = o.redactText(display.ArtifactID)
}

func (o *Orchestrator) safeConfirmationRequest(call tool.Call, decision permission.Decision, decisionCh chan events.ToolConfirmationDecision) *events.ToolConfirmationRequest {
	request := confirmationRequest(call, decision, decisionCh)
	if request == nil {
		return nil
	}
	request.Arguments = o.redactText(request.Arguments)
	request.Prompt = o.redactText(request.Prompt)
	request.ScopePreview = o.redactText(request.ScopePreview)
	request.Warning = o.redactText(request.Warning)
	request.RevokeHint = o.redactText(request.RevokeHint)
	return request
}

func newToolDisplay(call tool.Call, status events.ToolDisplayStatus, summary string) *events.ToolDisplay {
	return &events.ToolDisplay{CallID: call.ID, Name: call.Name, Arguments: redactedArguments(call), Summary: summary, Status: status}
}

func redactedArguments(call tool.Call) string {
	var args map[string]any
	if json.Unmarshal([]byte(call.ArgumentsJSON), &args) != nil {
		return redactSensitive(call.ArgumentsJSON)
	}
	if strings.HasPrefix(call.Name, "mcp__") {
		data, err := json.Marshal(mcpclient.RedactArguments(args))
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
	return &events.ToolDisplay{
		CallID:            result.CallID,
		Name:              result.Name,
		Summary:           redactSensitive(result.Summary),
		Status:            status,
		ErrorCode:         resultErrorCode(result),
		Stdout:            stringData(result.Data, "stdout"),
		Stderr:            stringData(result.Data, "stderr"),
		Truncated:         result.Truncated,
		Recoverable:       resultRecoverable(result),
		ArtifactID:        artifactID(result),
		ArtifactBytes:     artifactBytes(result),
		ArtifactAvailable: artifactAvailable(result),
	}
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

func confirmationRequest(call tool.Call, decision permission.Decision, decisionCh chan events.ToolConfirmationDecision) *events.ToolConfirmationRequest {
	request := &events.ToolConfirmationRequest{
		CallID:         call.ID,
		Name:           call.Name,
		Arguments:      redactedArguments(call),
		Prompt:         formatPermissionPrompt(call, decision),
		AllowPermanent: decision.Prompt != nil && decision.Prompt.AllowPermanent,
		Decision:       decisionCh,
	}
	if decision.Prompt == nil {
		return request
	}
	request.Risk = string(decision.Prompt.Risk)
	request.PermissionMode = string(decision.Prompt.Mode)
	if decision.Prompt.RulePreview != nil {
		request.ScopePreview = decision.Prompt.RulePreview.Display()
	}
	request.Warning = permissionWarning(call, decision.Prompt)
	request.RevokeHint = permissionRevokeHint(request.AllowPermanent)
	return request
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

func permissionRevokeHint(allowPermanent bool) string {
	if allowPermanent {
		return "永久授权会写入本地权限规则，可稍后从权限配置中撤销。"
	}
	return "本次工具调用不支持永久授权。"
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
	parts = append(parts, permissionRevokeHint(decision.Prompt.AllowPermanent))
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

func resultData(result tool.Result) json.RawMessage {
	if result.Data == nil {
		return nil
	}
	data, err := marshalRedactedJSON(result.Data)
	if err != nil {
		return nil
	}
	return data
}

func resultErrorData(result tool.Result) json.RawMessage {
	if result.Error == nil {
		return nil
	}
	data, err := marshalRedactedJSON(result.Error)
	if err != nil {
		return nil
	}
	return data
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
