package provider

import (
	"context"
	"net/http/httptrace"
	"strings"
	"sync"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/tool"
)

type ChatRequest struct {
	Model         string
	System        []SystemBlock
	StableSystem  []SystemBlock
	DynamicSystem []SystemBlock
	SystemPrompt  string
	Messages      []conversation.Message
	Thinking      config.ThinkingConfig
	Tools         []ToolDefinition
	ToolDefs      *tool.Registry
	Cache         CachePolicy
	Observer      RequestObserver
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
	EnablePromptCache    bool
	SystemBreakpointName string
	CacheTools           bool
}

// RequestObserver observes one Provider request attempt. Providers call
// MarkSent when an outbound transport crosses the irreversible write
// boundary, and call Finish exactly once on every return path.
//
// Finish(false) is only valid after the transport and all related callbacks
// are quiescent; a later MarkSent is forbidden. Callers may pass nil.
type RequestObserver interface {
	MarkSent()
	Finish(sent bool)
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
	if len(req.System) > 0 {
		return nonEmptySystemBlocks(req.System)
	}
	blocks := make([]SystemBlock, 0, len(req.StableSystem)+len(req.DynamicSystem)+1)
	blocks = append(blocks, nonEmptySystemBlocks(req.StableSystem)...)
	blocks = append(blocks, nonEmptySystemBlocks(req.DynamicSystem)...)
	if len(blocks) == 0 && strings.TrimSpace(req.SystemPrompt) != "" {
		blocks = append(blocks, SystemBlock{Name: "legacy-system-prompt", Content: strings.TrimSpace(req.SystemPrompt), Cacheable: true})
	}
	return blocks
}

func usesOrderedSystem(req ChatRequest) bool {
	return len(req.System) > 0
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

type requestAttempt struct {
	observer  RequestObserver
	mu        sync.Mutex
	cond      *sync.Cond
	sent      bool
	notifying bool
	finished  bool
}

func newRequestAttempt(observer RequestObserver) *requestAttempt {
	attempt := &requestAttempt{observer: observer}
	attempt.cond = sync.NewCond(&attempt.mu)
	return attempt
}

func (a *requestAttempt) traceContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) {
			a.markSent()
		},
	})
}

func (a *requestAttempt) markSent() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.finished {
		observer := a.observer
		a.mu.Unlock()
		// Forward a contract violation so strict test observers can detect a
		// late callback. Production observers are required to be defensive.
		if observer != nil {
			observer.MarkSent()
		}
		return
	}
	if a.sent {
		a.mu.Unlock()
		return
	}
	a.sent = true
	observer := a.observer
	if observer == nil {
		a.mu.Unlock()
		return
	}
	a.notifying = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.notifying = false
		a.cond.Broadcast()
		a.mu.Unlock()
	}()
	observer.MarkSent()
}

func (a *requestAttempt) finish() {
	if a == nil {
		return
	}
	a.mu.Lock()
	for a.notifying {
		a.cond.Wait()
	}
	if a.finished {
		a.mu.Unlock()
		return
	}
	a.finished = true
	sent := a.sent
	observer := a.observer
	a.mu.Unlock()
	if observer != nil {
		observer.Finish(sent)
	}
}
