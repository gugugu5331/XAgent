package orchestrator

import (
	"context"
	"os"
	"path/filepath"
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
	calls int
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) StreamChat(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	out := make(chan provider.StreamEvent, 2)
	p.calls++
	if p.calls == 1 {
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
