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
	"xagent/internal/provider"
	"xagent/internal/resources"
	"xagent/internal/tool"
)

type Orchestrator struct {
	provider  provider.Provider
	store     conversation.ConversationStore
	resources resources.PromptProvider
	thinking  config.ThinkingConfig
	registry  *tool.Registry
	executor  *tool.Executor
}

func New(provider provider.Provider, store conversation.ConversationStore, resources resources.PromptProvider, thinking config.ThinkingConfig) *Orchestrator {
	return &Orchestrator{provider: provider, store: store, resources: resources, thinking: thinking}
}

func NewWithTools(provider provider.Provider, store conversation.ConversationStore, resources resources.PromptProvider, thinking config.ThinkingConfig, registry *tool.Registry, executor *tool.Executor) *Orchestrator {
	return &Orchestrator{provider: provider, store: store, resources: resources, thinking: thinking, registry: registry, executor: executor}
}

func (o *Orchestrator) Send(ctx context.Context, conv *conversation.Conversation, userText string) (<-chan events.Event, error) {
	if strings.TrimSpace(userText) == "" {
		return nil, fmt.Errorf("请输入非空内容")
	}
	out := make(chan events.Event)
	conversation.AppendUserMessage(conv, userText)

	stream, err := o.stream(ctx, conv, true)
	if err != nil {
		return nil, err
	}

	go func() {
		defer close(out)
		start := time.Now()
		out <- events.Event{Type: events.UserSubmitted, Text: userText}
		if o.consumeStream(ctx, conv, out, stream, start, true) {
			return
		}
	}()
	return out, nil
}

func (o *Orchestrator) stream(ctx context.Context, conv *conversation.Conversation, includeTools bool) (<-chan provider.StreamEvent, error) {
	request := provider.ChatRequest{
		SystemPrompt: o.resources.SystemPrompt(),
		Messages:     conversation.ContextMessages(conv),
		Thinking:     o.thinking,
	}
	if includeTools {
		request.ToolDefs = o.registry
	}
	return o.provider.StreamChat(ctx, request)
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
			if assistantText.Len() > 0 {
				conversation.AppendAssistantMessage(conv, assistantText.String())
			}
			if o.thinking.Show {
				conversation.AppendThinkingMessage(conv, thinkingText.String())
			}
			if event.ToolCall == nil {
				out <- events.Event{Type: events.Error, Err: fmt.Errorf("工具调用为空")}
				return true
			}
			if !allowTool || o.executor == nil {
				result := unsupportedToolLoopResult(*event.ToolCall)
				o.appendToolMessages(conv, *event.ToolCall, result)
				out <- toolResultEvent(result)
				return o.finalReply(ctx, conv, out, start)
			}
			return o.handleToolCall(ctx, conv, out, *event.ToolCall, start)
		case provider.StreamEventDone:
			conversation.AppendAssistantMessage(conv, assistantText.String())
			if o.thinking.Show {
				conversation.AppendThinkingMessage(conv, thinkingText.String())
			}
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
			Prompt:    formatToolCall(call) + " 需要确认，按 y 允许，按 n 拒绝。",
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
	stream, err := o.stream(ctx, conv, false)
	if err != nil {
		out <- events.Event{Type: events.Error, Err: err}
		return true
	}
	return o.consumeStream(ctx, conv, out, stream, start, false)
}

func (o *Orchestrator) appendToolMessages(conv *conversation.Conversation, call tool.Call, result tool.Result) {
	conversation.AppendToolCallMessage(conv, call.ID, call.Name, call.ArgumentsJSON)
	conversation.AppendToolResultMessage(conv, call.ID, call.Name, string(result.Status), result.Summary, resultContent(result), resultErrorCode(result))
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

func formatToolCall(call tool.Call) string {
	return fmt.Sprintf("%s(%s)", call.Name, call.ArgumentsJSON)
}

func resultContent(result tool.Result) string {
	if strings.TrimSpace(result.Content) != "" {
		return result.Content
	}
	data, err := json.Marshal(result)
	if err != nil {
		return result.Summary
	}
	return string(data)
}

func resultErrorCode(result tool.Result) string {
	if result.Error == nil {
		return ""
	}
	return result.Error.Code
}
