package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/orchestrator"
	"xagent/internal/provider"
	"xagent/internal/resources"
	"xagent/internal/tool"
)

type smokeProvider struct {
	mode  string
	calls int
}

func (p *smokeProvider) Name() string { return "smoke" }

func (p *smokeProvider) StreamChat(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	out := make(chan provider.StreamEvent, 2)
	p.calls++
	if p.calls == 1 {
		call := tool.Call{ID: "call_1", Name: "Read", ArgumentsJSON: `{"path":"note.txt"}`}
		if p.mode == "deny" {
			call = tool.Call{ID: "call_2", Name: "Write", ArgumentsJSON: `{"path":"note.txt","content":"changed"}`}
		}
		out <- provider.StreamEvent{Type: provider.StreamEventToolCall, ToolCall: &call}
	} else {
		out <- provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: "工具结果已处理。"}
		out <- provider.StreamEvent{Type: provider.StreamEventDone}
	}
	close(out)
	return out, nil
}

func main() {
	root, err := os.MkdirTemp("", "xagent-tool-smoke-*")
	must(err)
	defer os.RemoveAll(root)
	must(os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello smoke"), 0o644))
	if err := runSafeTool(root); err != nil {
		panic(err)
	}
	if err := runDeniedTool(root); err != nil {
		panic(err)
	}
	fmt.Println("TOOL_SMOKE_OK")
}

func runSafeTool(root string) error {
	stream, conv, provider := start(root, "safe")
	var toolSuccess, finalReply bool
	for event := range stream {
		switch event.Type {
		case events.ToolSuccess:
			toolSuccess = true
		case events.TextDelta:
			finalReply = event.Text == "工具结果已处理。"
		case events.Error:
			return event.Err
		}
	}
	if provider.calls != 2 || !toolSuccess || !finalReply || !hasToolMessages(conv) {
		return fmt.Errorf("safe smoke failed: calls=%d tool=%v final=%v messages=%v", provider.calls, toolSuccess, finalReply, conv.Messages)
	}
	return nil
}

func runDeniedTool(root string) error {
	stream, conv, provider := start(root, "deny")
	var denied, finalReply bool
	for event := range stream {
		switch event.Type {
		case events.ToolWaitingConfirmation:
			event.Confirmation.Decision <- events.ToolConfirmationDecision{CallID: event.Confirmation.CallID, Allowed: false}
		case events.ToolDenied:
			denied = true
		case events.TextDelta:
			finalReply = event.Text == "工具结果已处理。"
		case events.Error:
			return event.Err
		}
	}
	if provider.calls != 2 || !denied || !finalReply || !hasToolMessages(conv) {
		return fmt.Errorf("denied smoke failed: calls=%d denied=%v final=%v messages=%v", provider.calls, denied, finalReply, conv.Messages)
	}
	return nil
}

func start(root string, mode string) (<-chan events.Event, *conversation.Conversation, *smokeProvider) {
	store, err := conversation.NewFileStore(filepath.Join(root, ".xagent", mode))
	must(err)
	conv, err := store.Create(context.Background())
	must(err)
	registry, err := tool.NewRegistry(root)
	must(err)
	executor := tool.NewExecutor(registry, root, time.Second, 4096)
	smoke := &smokeProvider{mode: mode}
	orch := orchestrator.NewWithTools(smoke, store, resources.New(), config.ThinkingConfig{}, registry, executor)
	stream, err := orch.Send(context.Background(), conv, "执行工具")
	must(err)
	return stream, conv, smoke
}

func hasToolMessages(conv *conversation.Conversation) bool {
	var call, result bool
	for _, message := range conv.Messages {
		call = call || message.Role == conversation.RoleToolCall
		result = result || message.Role == conversation.RoleToolResult
	}
	return call && result
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
