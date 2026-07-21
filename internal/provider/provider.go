package provider

import (
	"context"
	"strings"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/tool"
)

type ChatRequest struct {
	Model         string
	StableSystem  []SystemBlock
	DynamicSystem []SystemBlock
	SystemPrompt  string
	Messages      []conversation.Message
	Thinking      config.ThinkingConfig
	Tools         []ToolDefinition
	ToolDefs      *tool.Registry
	Cache         CachePolicy
}

type SystemBlock struct {
	Name      string
	Content   string
	Cacheable bool
}

type ToolDefinition struct {
	Name        string
	Description string
	Schema      tool.Schema
}

type CachePolicy struct {
	EnablePromptCache bool
}

type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

type StreamEventType string

const (
	StreamEventTextDelta     StreamEventType = "text_delta"
	StreamEventThinkingDelta StreamEventType = "thinking_delta"
	StreamEventToolCall      StreamEventType = "tool_call"
	StreamEventUsage         StreamEventType = "usage"
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

func systemBlocks(req ChatRequest) []SystemBlock {
	blocks := make([]SystemBlock, 0, len(req.StableSystem)+len(req.DynamicSystem)+1)
	blocks = append(blocks, nonEmptySystemBlocks(req.StableSystem)...)
	blocks = append(blocks, nonEmptySystemBlocks(req.DynamicSystem)...)
	if len(blocks) == 0 && strings.TrimSpace(req.SystemPrompt) != "" {
		blocks = append(blocks, SystemBlock{Name: "legacy-system-prompt", Content: strings.TrimSpace(req.SystemPrompt), Cacheable: true})
	}
	return blocks
}

func joinedSystemBlocks(req ChatRequest) string {
	blocks := systemBlocks(req)
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.Content)
	}
	return strings.Join(parts, "\n\n")
}

func nonEmptySystemBlocks(blocks []SystemBlock) []SystemBlock {
	result := make([]SystemBlock, 0, len(blocks))
	for _, block := range blocks {
		block.Name = strings.TrimSpace(block.Name)
		block.Content = strings.TrimSpace(block.Content)
		if block.Content == "" {
			continue
		}
		result = append(result, block)
	}
	return result
}

func toolDefinitions(req ChatRequest) []ToolDefinition {
	if len(req.Tools) > 0 {
		return nonEmptyToolDefinitions(req.Tools)
	}
	if req.ToolDefs == nil {
		return nil
	}
	definitions := req.ToolDefs.OpenAIDefinitions()
	tools := make([]ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, ToolDefinition{Name: definition.Function.Name, Description: definition.Function.Description, Schema: definition.Function.Parameters})
	}
	return nonEmptyToolDefinitions(tools)
}

func nonEmptyToolDefinitions(definitions []ToolDefinition) []ToolDefinition {
	result := make([]ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		definition.Name = strings.TrimSpace(definition.Name)
		definition.Description = strings.TrimSpace(definition.Description)
		if definition.Name == "" {
			continue
		}
		result = append(result, definition)
	}
	return result
}

func requestModel(override string, fallback string) string {
	if model := strings.TrimSpace(override); model != "" {
		return model
	}
	return fallback
}

func emitStreamEvent(ctx context.Context, out chan<- StreamEvent, event StreamEvent) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}
