package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/provider"
	"xagent/internal/resources"
	"xagent/internal/tool"
)

type fakeProvider struct {
	calls  int
	events [][]provider.StreamEvent
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) StreamChat(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	out := make(chan provider.StreamEvent, 4)
	p.calls++
	if len(p.events) >= p.calls {
		for _, event := range p.events[p.calls-1] {
			out <- event
		}
	} else if p.calls == 1 {
		out <- provider.StreamEvent{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "call_1", Name: "Read", ArgumentsJSON: `{"path":"note.txt"}`}}
	} else {
		out <- provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: "最终回复"}
		out <- provider.StreamEvent{Type: provider.StreamEventDone}
	}
	close(out)
	return out, nil
}

func TestOrchestratorExecutesToolAndRequestsFinalReply(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := conversation.NewFileStore(filepath.Join(root, "conversations"))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := store.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := tool.NewRegistry(root)
	executor := tool.NewExecutor(registry, root, time.Second, 1024)
	fp := &fakeProvider{}
	orch := NewWithTools(fp, store, resources.New(), config.ThinkingConfig{}, registry, executor)

	stream, err := orch.Send(context.Background(), conv, "读 note")
	if err != nil {
		t.Fatal(err)
	}
	seenTool := false
	seenFinal := false
	for event := range stream {
		if event.Type == events.ToolSuccess {
			seenTool = true
		}
		if event.Type == events.TextDelta && event.Text == "最终回复" {
			seenFinal = true
		}
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	if !seenTool || !seenFinal {
		t.Fatalf("seenTool=%v seenFinal=%v", seenTool, seenFinal)
	}
	if fp.calls != 2 {
		t.Fatalf("expected two model calls, got %d", fp.calls)
	}
	var hasToolCall, hasToolResult bool
	for _, message := range conv.Messages {
		hasToolCall = hasToolCall || message.Role == conversation.RoleToolCall
		hasToolResult = hasToolResult || message.Role == conversation.RoleToolResult
	}
	if !hasToolCall || !hasToolResult {
		t.Fatalf("tool messages missing: %#v", conv.Messages)
	}
}

func TestOrchestratorRejectsMultipleToolCallsWithoutExecuting(t *testing.T) {
	root := t.TempDir()
	store, err := conversation.NewFileStore(filepath.Join(root, "conversations"))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := store.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := tool.NewRegistry(root)
	executor := tool.NewExecutor(registry, root, time.Second, 1024)
	fp := &fakeProvider{events: [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCalls: []tool.Call{
			{ID: "call_1", Name: "Read", ArgumentsJSON: `{"path":"note.txt"}`},
			{ID: "call_2", Name: "Glob", ArgumentsJSON: `{"pattern":"*.go"}`},
		}}},
		{{Type: provider.StreamEventTextDelta, Delta: "请一次只调用一个工具"}, {Type: provider.StreamEventDone}},
	}}
	orch := NewWithTools(fp, store, resources.New(), config.ThinkingConfig{}, registry, executor)

	stream, err := orch.Send(context.Background(), conv, "同时调用两个工具")
	if err != nil {
		t.Fatal(err)
	}
	for event := range stream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	if fp.calls != 2 {
		t.Fatalf("expected final reply after multi-tool rejection, got %d model calls", fp.calls)
	}
	var unsupported int
	for _, message := range conv.Messages {
		if message.ToolErrorCode == tool.ErrMultipleToolCallsUnsupported {
			unsupported++
		}
	}
	if unsupported != 2 {
		t.Fatalf("expected one unsupported result per tool call, got %d messages=%#v", unsupported, conv.Messages)
	}
}

func TestOrchestratorStopsWhenFinalReplyRequestsTool(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := conversation.NewFileStore(filepath.Join(root, "conversations"))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := store.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := tool.NewRegistry(root)
	executor := tool.NewExecutor(registry, root, time.Second, 1024)
	fp := &fakeProvider{events: [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "call_1", Name: "Read", ArgumentsJSON: `{"path":"note.txt"}`}}},
		{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "call_2", Name: "Read", ArgumentsJSON: `{"path":"note.txt"}`}}},
	}}
	orch := NewWithTools(fp, store, resources.New(), config.ThinkingConfig{}, registry, executor)

	stream, err := orch.Send(context.Background(), conv, "读 note")
	if err != nil {
		t.Fatal(err)
	}
	for event := range stream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	if fp.calls != 2 {
		t.Fatalf("expected no recursive final reply, got %d model calls", fp.calls)
	}
}

func TestFormatToolConfirmationIncludesContentPreview(t *testing.T) {
	write := formatToolConfirmation(tool.Call{ID: "call_1", Name: "Write", ArgumentsJSON: `{"path":"config.yaml","content":"secret-change"}`})
	if !strings.Contains(write, "config.yaml") || !strings.Contains(write, "secret-change") {
		t.Fatalf("write confirmation missing content preview: %q", write)
	}

	edit := formatToolConfirmation(tool.Call{ID: "call_2", Name: "Edit", ArgumentsJSON: `{"path":"config.yaml","old_text":"old-secret","new_text":"new-secret"}`})
	if !strings.Contains(edit, "old-secret") || !strings.Contains(edit, "new-secret") {
		t.Fatalf("edit confirmation missing replacement preview: %q", edit)
	}
}
func TestAppendToolResultStoresTruncatedAndData(t *testing.T) {
	conv := conversation.NewConversation("test", time.Now())
	result := tool.Result{
		CallID:    "call_1",
		Name:      "Bash",
		Status:    tool.StatusSuccess,
		Summary:   "ok",
		Content:   "stdout",
		Data:      map[string]any{"exit_code": 0, "stderr": "warn"},
		Truncated: true,
	}
	orch := &Orchestrator{}
	orch.appendToolMessages(conv, tool.Call{ID: "call_1", Name: "Bash", ArgumentsJSON: `{"command":"echo hi"}`}, result)

	last := conv.Messages[len(conv.Messages)-1]
	if !last.ToolResultTruncated {
		t.Fatalf("expected truncated flag in message: %#v", last)
	}
	if len(last.ToolResultData) == 0 {
		t.Fatalf("expected tool result data in message: %#v", last)
	}
	if !strings.Contains(last.ToolResultContent, "truncated") || strings.Contains(last.ToolResultContent, "exit_code") {
		t.Fatalf("expected structured content without data payload, got %q", last.ToolResultContent)
	}
}
