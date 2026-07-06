package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/mcpclient"
	"xagent/internal/memory"
	"xagent/internal/permission"
	"xagent/internal/prompt"
	"xagent/internal/provider"
	"xagent/internal/resources"
	"xagent/internal/sessionctx"
	"xagent/internal/tool"
)

type OrchestratorOptions struct {
	Provider       provider.Provider
	Store          conversation.ConversationStore
	Resources      resources.PromptProvider
	Thinking       config.ThinkingConfig
	Registry       *tool.Registry
	Executor       *tool.Executor
	ContextManager *contextmgr.Manager
	SessionContext sessionPreparer
	Memory         memoryUpdater
}

type sessionPreparer interface {
	Prepare(ctx context.Context, conv *conversation.Conversation, mode sessionctx.PrepareMode) (sessionctx.PreparedContext, error)
}

type memoryUpdater interface {
	UpdateAsync(input memory.UpdateInput)
}

type Orchestrator struct {
	provider         provider.Provider
	store            conversation.ConversationStore
	resources        resources.PromptProvider
	thinking         config.ThinkingConfig
	registry         *tool.Registry
	readOnlyRegistry *tool.Registry
	executor         *tool.Executor
	authorizer       *permission.Authorizer
	contextManager   *contextmgr.Manager
	sessionContext   sessionPreparer
	memory           memoryUpdater
	permissionMode   permission.Mode
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
			Writer:     permission.Writer{ProjectRoot: options.Executor.ProjectRoot},
		}
	}
	return &Orchestrator{provider: options.Provider, store: options.Store, resources: options.Resources, thinking: options.Thinking, registry: options.Registry, readOnlyRegistry: readOnlyRegistry, executor: options.Executor, authorizer: authorizer, contextManager: options.ContextManager, sessionContext: options.SessionContext, memory: options.Memory, permissionMode: permission.ModeDefault}
}

func (o *Orchestrator) SetPermissionMode(mode permission.Mode) {
	if mode == "" {
		mode = permission.ModeDefault
	}
	o.permissionMode = mode
}

func (o *Orchestrator) CompactContext(ctx context.Context, conv *conversation.Conversation) (contextmgr.Result, error) {
	if o.contextManager == nil {
		return contextmgr.Result{}, fmt.Errorf("上下文管理未启用")
	}
	result, err := o.contextManager.CompactNow(ctx, conv)
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
	out := make(chan events.Event)
	conversation.AppendUserMessage(conv, req.UserText)

	go func() {
		defer close(out)
		start := time.Now()
		out <- events.Event{Type: events.UserSubmitted, Text: req.UserText}
		o.runAgentLoop(ctx, conv, req, out, start)
	}()
	return out, nil
}

func (o *Orchestrator) stream(ctx context.Context, conv *conversation.Conversation, mode RunMode, includeTools bool, iteration int) (<-chan provider.StreamEvent, error) {
	optionalSections := []prompt.Section{}
	if o.sessionContext != nil {
		prepared, err := o.sessionContext.Prepare(ctx, conv, sessionctx.PrepareAuto)
		if err != nil {
			return nil, err
		}
		optionalSections = append(optionalSections, prepared.StableSections...)
		if prepared.MessagesChanged {
			if err := o.store.Save(ctx, conv); err != nil {
				return nil, err
			}
		}
	} else if o.contextManager != nil {
		result, err := o.contextManager.Prepare(ctx, conv, contextmgr.ModeAuto)
		if err != nil {
			return nil, err
		}
		if result.Changed {
			if err := o.store.Save(ctx, conv); err != nil {
				return nil, err
			}
		}
	}
	bundle := prompt.Build(prompt.BuildRequest{Mode: promptRunMode(mode), Iteration: iteration, ProjectRoot: o.projectRoot(), OptionalStableSections: optionalSections})
	request := provider.ChatRequest{
		StableSystem:  providerStableBlocks(bundle.StableBlocks),
		DynamicSystem: providerDynamicBlocks(bundle.DynamicBlocks),
		Messages:      conversation.ContextMessages(conv),
		Thinking:      o.thinking,
		Cache:         provider.CachePolicy{EnablePromptCache: true},
	}
	if includeTools {
		registry, err := o.registryForMode(mode)
		if err != nil {
			return nil, err
		}
		request.Tools = toolDefinitionsFromRegistry(registry)
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

func (o *Orchestrator) consumeStream(ctx context.Context, conv *conversation.Conversation, out chan<- events.Event, stream <-chan provider.StreamEvent, start time.Time, mode RunMode, allowTool bool) bool {
	var assistantText strings.Builder
	var thinkingText strings.Builder
	for event := range stream {
		switch event.Type {
		case provider.StreamEventTextDelta:
			assistantText.WriteString(event.Delta)
			out <- events.Event{Type: events.TextDelta, Text: event.Delta}
		case provider.StreamEventThinkingDelta:
			thinkingText.WriteString(event.Delta)
			out <- events.Event{Type: events.ThinkingDelta, Text: event.Delta}
		case provider.StreamEventToolCall:
			if o.thinking.Show {
				conversation.AppendThinkingMessage(conv, thinkingText.String())
			}
			if assistantText.Len() > 0 {
				conversation.AppendAssistantMessage(conv, assistantText.String())
			}
			toolCalls := streamToolCalls(event)
			if len(toolCalls) == 0 {
				out <- events.Event{Type: events.Error, Err: fmt.Errorf("工具调用为空")}
				return true
			}
			if len(toolCalls) > 1 {
				result := multipleToolCallsResult(toolCalls)
				o.appendUnsupportedToolMessages(conv, toolCalls, result)
				out <- toolResultEvent(result)
				return o.finalReply(ctx, conv, out, start)
			}
			if !allowTool || o.executor == nil {
				result := unsupportedToolLoopResult(toolCalls[0])
				o.appendToolMessages(conv, toolCalls[0], result)
				out <- toolResultEvent(result)
				return o.finishWithoutMoreTools(ctx, conv, out, start, result.Content)
			}
			return o.handleToolCall(ctx, conv, out, mode, toolCalls[0], start)
		case provider.StreamEventUsage:
			if o.contextManager != nil {
				o.contextManager.UpdateUsage(conv, event.Usage)
			}
			if event.Usage != nil {
				out <- events.Event{Type: events.UsageUpdated, Usage: usageDisplay(event.Usage)}
			}
		case provider.StreamEventDone:
			if o.thinking.Show {
				conversation.AppendThinkingMessage(conv, thinkingText.String())
			}
			conversation.AppendAssistantMessage(conv, assistantText.String())
			if err := o.store.Save(ctx, conv); err != nil {
				out <- events.Event{Type: events.Error, Err: err}
				return true
			}
			out <- events.Event{Type: events.Done, Duration: time.Since(start)}
			return true
		case provider.StreamEventError:
			out <- events.Event{Type: events.Error, Err: event.Err}
			return true
		}
	}
	return false
}

func (o *Orchestrator) handleToolCall(ctx context.Context, conv *conversation.Conversation, out chan<- events.Event, mode RunMode, call tool.Call, start time.Time) bool {
	display := newToolDisplay(call, events.ToolDisplayPending, "")
	out <- events.Event{Type: events.ToolPending, Tool: display}

	var result tool.Result
	if o.authorizer == nil {
		result = tool.Result{CallID: call.ID, Name: call.Name, Status: tool.StatusError, Summary: "权限系统不可用", Error: &tool.Error{Code: tool.ErrPermissionDenied, Message: "权限系统不可用", Recoverable: true}}
		out <- toolResultEvent(result)
		o.appendToolMessages(conv, call, result)
		return o.finalReply(ctx, conv, out, start)
	}
	decision := o.authorizer.Decide(permissionCall(call), o.permissionContext(mode))
	switch decision.Kind {
	case permission.DecisionDeny:
		result = permissionDeniedResult(call, decision)
		out <- events.Event{Type: events.ToolDenied, Tool: resultDisplay(result)}
		o.appendToolMessages(conv, call, result)
		return o.finalReply(ctx, conv, out, start)
	case permission.DecisionAsk:
		decisionCh := make(chan events.ToolConfirmationDecision, 1)
		confirmation := &events.ToolConfirmationRequest{
			CallID:         call.ID,
			Name:           call.Name,
			Arguments:      redactedArguments(call),
			Prompt:         formatPermissionPrompt(call, decision),
			AllowPermanent: decision.Prompt != nil && decision.Prompt.AllowPermanent,
			Decision:       decisionCh,
		}
		out <- events.Event{Type: events.ToolWaitingConfirmation, Tool: newToolDisplay(call, events.ToolDisplayWaitingConfirmation, "等待确认"), Confirmation: confirmation}
		select {
		case <-ctx.Done():
			out <- events.Event{Type: events.Error, Err: ctx.Err()}
			return true
		case userDecision := <-decisionCh:
			decision = o.authorizer.ResolveUserDecision(permissionCall(call), o.permissionContext(mode), permissionAction(userDecision))
			if decision.Kind == permission.DecisionDeny {
				result = permissionDeniedResult(call, decision)
				out <- events.Event{Type: events.ToolDenied, Tool: resultDisplay(result)}
				o.appendToolMessages(conv, call, result)
				return o.finalReply(ctx, conv, out, start)
			}
		}
	}

	out <- events.Event{Type: events.ToolRunning, Tool: newToolDisplay(call, events.ToolDisplayRunning, "执行中")}
	result = o.executor.ExecuteAuthorized(ctx, call, *decision.Grant)
	out <- toolResultEvent(result)
	o.appendToolMessages(conv, call, result)
	return o.finalReply(ctx, conv, out, start)
}

func (o *Orchestrator) finalReply(ctx context.Context, conv *conversation.Conversation, out chan<- events.Event, start time.Time) bool {
	stream, err := o.stream(ctx, conv, RunModeDefault, false, 1)
	if err != nil {
		out <- events.Event{Type: events.Error, Err: err}
		return true
	}
	return o.consumeStream(ctx, conv, out, stream, start, RunModeDefault, false)
}

func (o *Orchestrator) finishWithoutMoreTools(ctx context.Context, conv *conversation.Conversation, out chan<- events.Event, start time.Time, text string) bool {
	conversation.AppendAssistantMessage(conv, text)
	out <- events.Event{Type: events.TextDelta, Text: text}
	if err := o.store.Save(ctx, conv); err != nil {
		out <- events.Event{Type: events.Error, Err: err}
		return true
	}
	out <- events.Event{Type: events.Done, Duration: time.Since(start)}
	return true
}

func (o *Orchestrator) appendToolMessages(conv *conversation.Conversation, call tool.Call, result tool.Result) {
	conversation.AppendToolCallMessage(conv, call.ID, call.Name, redactedArguments(call))
	conversation.AppendToolResultMessage(conv, call.ID, call.Name, resultHistoryStatus(result), result.Summary, resultContent(result), resultErrorCode(result), result.Truncated, resultData(result), resultErrorData(result))
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
	for _, key := range []string{"command", "content", "old_text", "new_text"} {
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
	return &events.ToolDisplay{CallID: result.CallID, Name: result.Name, Summary: result.Summary, Status: status}
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

func formatPermissionPrompt(call tool.Call, decision permission.Decision) string {
	base := formatToolConfirmation(call)
	if decision.Prompt == nil {
		return base + " 需要权限确认，按 y 允许本次，按 s 本会话允许，按 p 永久允许，按 n 拒绝，Esc 取消。"
	}
	parts := []string{base, "需要权限确认"}
	if decision.Prompt.Reason != "" {
		parts = append(parts, "原因: "+decision.Prompt.Reason)
	}
	if decision.Prompt.Mode != "" {
		parts = append(parts, "模式: "+string(decision.Prompt.Mode))
	}
	if decision.Prompt.RulePreview != nil {
		parts = append(parts, "本次允许规则: "+decision.Prompt.RulePreview.Display())
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
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(api[_-]?key\s*=\s*)[^\s]+`),
		regexp.MustCompile(`(?i)(token\s*=\s*)[^\s]+`),
		regexp.MustCompile(`(?i)(secret\s*=\s*)[^\s]+`),
		regexp.MustCompile(`(?i)(password\s*=\s*)[^\s]+`),
	}
	redacted := value
	for _, pattern := range patterns {
		redacted = pattern.ReplaceAllString(redacted, `${1}[REDACTED]`)
	}
	return redacted
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
	data, err := json.Marshal(payload)
	if err != nil {
		if strings.TrimSpace(result.Content) != "" {
			return result.Content
		}
		return result.Summary
	}
	return string(data)
}

func resultData(result tool.Result) json.RawMessage {
	if result.Data == nil {
		return nil
	}
	data, err := json.Marshal(result.Data)
	if err != nil {
		return nil
	}
	return data
}

func resultErrorData(result tool.Result) json.RawMessage {
	if result.Error == nil {
		return nil
	}
	data, err := json.Marshal(result.Error)
	if err != nil {
		return nil
	}
	return data
}

func resultErrorCode(result tool.Result) string {
	if result.Error == nil {
		return ""
	}
	return result.Error.Code
}
