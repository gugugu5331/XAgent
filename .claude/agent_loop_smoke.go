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

type loopProvider struct {
	events [][]provider.StreamEvent
	calls  int
}

func (p *loopProvider) Name() string { return "loop-smoke" }

func (p *loopProvider) StreamChat(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	out := make(chan provider.StreamEvent, 4)
	p.calls++
	if len(p.events) >= p.calls {
		for _, event := range p.events[p.calls-1] {
			out <- event
		}
	} else {
		out <- provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: "done"}
		out <- provider.StreamEvent{Type: provider.StreamEventDone}
	}
	close(out)
	return out, nil
}

func main() {
	root, err := os.MkdirTemp("", "xagent-agent-loop-smoke-*")
	must(err)
	defer os.RemoveAll(root)
	must(os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello keyword"), 0o644))

	must(runMultiRound(root))
	must(runPlanBlocksWrite(root))
	must(runMaxIterations(root))
	fmt.Println("AGENT_LOOP_SMOKE_OK")
}

func runMultiRound(root string) error {
	provider := &loopProvider{events: [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "read", Name: "Read", ArgumentsJSON: `{"path":"note.txt"}`}}},
		{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "grep", Name: "Grep", ArgumentsJSON: `{"pattern":"keyword","path":"."}`}}},
		{{Type: provider.StreamEventTextDelta, Delta: "总结完成"}, {Type: provider.StreamEventDone}},
	}}
	stream, conv := start(root, provider, "读取并搜索")
	var done bool
	var progress int
	for event := range stream {
		if event.Progress != nil && event.Progress.StopReason == "" {
			progress++
		}
		if event.Type == events.Done {
			done = true
		}
		if event.Type == events.Error {
			return event.Err
		}
	}
	if !done || provider.calls != 3 || progress < 3 || countToolResults(conv) != 2 {
		return fmt.Errorf("multi-round failed: done=%v calls=%d progress=%d results=%d", done, provider.calls, progress, countToolResults(conv))
	}
	return nil
}

func runPlanBlocksWrite(root string) error {
	provider := &loopProvider{events: [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "write", Name: "Write", ArgumentsJSON: `{"path":"blocked.txt","content":"nope"}`}}},
		{{Type: provider.StreamEventTextDelta, Delta: "计划完成"}, {Type: provider.StreamEventDone}},
	}}
	stream, conv := start(root, provider, "/plan 写文件")
	for event := range stream {
		if event.Type == events.Error {
			return event.Err
		}
	}
	if _, err := os.Stat(filepath.Join(root, "blocked.txt")); !os.IsNotExist(err) {
		return fmt.Errorf("plan mode wrote blocked file")
	}
	if !hasErrorCode(conv, tool.ErrPermissionDenied) {
		return fmt.Errorf("plan mode missing denied result")
	}
	return nil
}

func runMaxIterations(root string) error {
	eventsByCall := make([][]provider.StreamEvent, 10)
	for i := range eventsByCall {
		eventsByCall[i] = []provider.StreamEvent{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: fmt.Sprintf("read_%d", i), Name: "Read", ArgumentsJSON: `{"path":"note.txt"}`}}}
	}
	provider := &loopProvider{events: eventsByCall}
	stream, _ := start(root, provider, "一直读")
	var max bool
	for event := range stream {
		if event.Progress != nil && event.Progress.StopReason == "max_iterations" {
			max = true
		}
		if event.Type == events.Error {
			return event.Err
		}
	}
	if !max || provider.calls != 10 {
		return fmt.Errorf("max iteration failed: max=%v calls=%d", max, provider.calls)
	}
	return nil
}

func start(root string, provider provider.Provider, text string) (<-chan events.Event, *conversation.Conversation) {
	store, err := conversation.NewFileStore(filepath.Join(root, ".xagent", fmt.Sprintf("%d", time.Now().UnixNano())))
	must(err)
	conv, err := store.Create(context.Background())
	must(err)
	registry, err := tool.NewRegistry(root)
	must(err)
	executor := tool.NewExecutor(registry, root, time.Second, 4096)
	orch := orchestrator.NewWithTools(provider, store, resources.New(), config.ThinkingConfig{}, registry, executor)
	stream, err := orch.Send(context.Background(), conv, text)
	must(err)
	return stream, conv
}

func countToolResults(conv *conversation.Conversation) int {
	count := 0
	for _, message := range conv.Messages {
		if message.Role == conversation.RoleToolResult {
			count++
		}
	}
	return count
}

func hasErrorCode(conv *conversation.Conversation, code string) bool {
	for _, message := range conv.Messages {
		if message.ToolErrorCode == code {
			return true
		}
	}
	return false
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
