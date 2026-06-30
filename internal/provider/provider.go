package provider

import (
	"context"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/tool"
)

type ChatRequest struct {
	SystemPrompt string
	Messages     []conversation.Message
	Thinking     config.ThinkingConfig
	ToolDefs     *tool.Registry
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type StreamEventType string

const (
	StreamEventTextDelta     StreamEventType = "text_delta"
	StreamEventThinkingDelta StreamEventType = "thinking_delta"
	StreamEventToolCall      StreamEventType = "tool_call"
	StreamEventDone          StreamEventType = "done"
	StreamEventError         StreamEventType = "error"
)

type StreamEvent struct {
	Type      StreamEventType
	Delta     string
	Err       error
	Usage     *Usage
	ToolCall  *tool.Call
	ToolCalls []tool.Call
}

type Provider interface {
	StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
	Name() string
}
