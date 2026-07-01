package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/prompt"
	"xagent/internal/provider"
	"xagent/internal/resources"
	"xagent/internal/tool"
)

type Orchestrator struct {
	provider         provider.Provider
	store            conversation.ConversationStore
	resources        resources.PromptProvider
	thinking         config.ThinkingConfig
	registry         *tool.Registry
	readOnlyRegistry *tool.Registry
	executor         *tool.Executor
}

func New(provider provider.Provider, store conversation.ConversationStore, resources resources.PromptProvider, thinking config.ThinkingConfig) *Orchestrator {
	return &Orchestrator{provider: provider, store: store, resources: resources, thinking: thinking}
}

func NewWithTools(provider provider.Provider, store conversation.ConversationStore, resources resources.PromptProvider, thinking config.ThinkingConfig, registry *tool.Registry, executor *tool.Executor) *Orchestrator {
	var readOnlyRegistry *tool.Registry
	if executor != nil {
		readOnlyRegistry, _ = tool.NewReadOnlyRegistry(executor.ProjectRoot)
	}
	return &Orchestrator{provider: provider, store: store, resources: resources, thinking: thinking, registry: registry, readOnlyRegistry: readOnlyRegistry, executor: executor}
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
	bundle := prompt.Build(prompt.BuildRequest{Mode: promptRunMode(mode), Iteration: iteration, ProjectRoot: o.projectRoot()})
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

func (o *Orchestrator) consumeStream(ctx context.Context, conv *conversation.Conversation, out chan<- events.Event, stream <-chan provider.StreamEvent, start time.Time, allowTool bool) bool {
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
			return o.handleToolCall(ctx, conv, out, toolCalls[0], start)
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

func (o *Orchestrator) handleToolCall(ctx context.Context, conv *conversation.Conversation, out chan<- events.Event, call tool.Call, start time.Time) bool {
	display := newToolDisplay(call, events.ToolDisplayPending, "")
	out <- events.Event{Type: events.ToolPending, Tool: display}

	var result tool.Result
	if o.executor.NeedsConfirmation(call) {
		decisionCh := make(chan events.ToolConfirmationDecision, 1)
		confirmation := &events.ToolConfirmationRequest{
			CallID:    call.ID,
			Name:      call.Name,
			Arguments: call.ArgumentsJSON,
			Prompt:    formatToolConfirmation(call) + " 需要确认，按 y 允许，按 n 拒绝。",
			Decision:  decisionCh,
		}
		out <- events.Event{Type: events.ToolWaitingConfirmation, Tool: newToolDisplay(call, events.ToolDisplayWaitingConfirmation, "等待确认"), Confirmation: confirmation}
		select {
		case <-ctx.Done():
			out <- events.Event{Type: events.Error, Err: ctx.Err()}
			return true
		case decision := <-decisionCh:
			if !decision.Allowed {
				result = o.executor.Denied(call)
				out <- events.Event{Type: events.ToolDenied, Tool: resultDisplay(result)}
				o.appendToolMessages(conv, call, result)
				return o.finalReply(ctx, conv, out, start)
			}
		}
	}

	out <- events.Event{Type: events.ToolRunning, Tool: newToolDisplay(call, events.ToolDisplayRunning, "执行中")}
	result = o.executor.Execute(ctx, call)
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
	return o.consumeStream(ctx, conv, out, stream, start, false)
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
	conversation.AppendToolCallMessage(conv, call.ID, call.Name, call.ArgumentsJSON)
	conversation.AppendToolResultMessage(conv, call.ID, call.Name, string(result.Status), result.Summary, resultContent(result), resultErrorCode(result), result.Truncated, resultData(result))
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
	return &events.ToolDisplay{CallID: call.ID, Name: call.Name, Arguments: call.ArgumentsJSON, Summary: summary, Status: status}
}

func resultDisplay(result tool.Result) *events.ToolDisplay {
	status := events.ToolDisplaySuccess
	switch result.Status {
	case tool.StatusError, tool.StatusTimeout:
		status = events.ToolDisplayError
	case tool.StatusDenied:
		status = events.ToolDisplayDenied
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
		return fmt.Sprintf("Bash(command=%s, cwd=项目根目录)", stringArg(args, "command"))
	default:
		return formatToolCall(call)
	}
}

func previewArg(args map[string]any, key string) string {
	value := stringArg(args, key)
	const limit = 120
	if len([]rune(value)) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "..."
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

func resultErrorCode(result tool.Result) string {
	if result.Error == nil {
		return ""
	}
	return result.Error.Code
}
