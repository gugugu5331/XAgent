package tui

import (
	"strings"
	"testing"

	"xagent/internal/conversation"
	"xagent/internal/events"
)

func TestMessagesViewRendersToolDisplay(t *testing.T) {
	view := NewMessagesView(false)
	view.UpsertTool(events.ToolDisplay{
		CallID:    "call_1",
		Name:      "Read",
		Arguments: `{"path":"go.mod"}`,
		Summary:   "Read 10 bytes",
		Status:    events.ToolDisplaySuccess,
	})
	output := view.View()
	if !strings.Contains(output, "● Read(go.mod)") || !strings.Contains(output, "Read 10 bytes") {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestMessagesViewRestoresToolDisplayFromHistory(t *testing.T) {
	view := NewMessagesView(false)
	view.SetMessages([]conversation.Message{
		{Role: conversation.RoleToolCall, ToolCallID: "call_1", ToolName: "Read", RawToolArguments: `{"path":"go.mod"}`},
		{Role: conversation.RoleToolResult, ToolCallID: "call_1", ToolName: "Read", ToolResultStatus: "success", ToolResultSummary: "Read 10 bytes"},
	})
	output := view.View()
	if !strings.Contains(output, "● Read(go.mod)") || !strings.Contains(output, "Read 10 bytes") {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestMessagesViewKeepsLiveToolInTimeline(t *testing.T) {
	view := NewMessagesView(false)
	view.AppendUser("读文件")
	view.AppendAssistantDelta("我先读取文件。")
	view.UpsertTool(events.ToolDisplay{CallID: "call_1", Name: "Read", Arguments: `{"path":"go.mod"}`, Status: events.ToolDisplayRunning})
	view.UpsertTool(events.ToolDisplay{CallID: "call_1", Name: "Read", Status: events.ToolDisplaySuccess, Summary: "完成"})
	view.AppendAssistantDelta("读取完成。")
	view.CommitAssistant()

	output := view.View()
	before := strings.Index(output, "我先读取文件。")
	toolLine := strings.Index(output, "● Read(go.mod)")
	after := strings.Index(output, "读取完成。")
	if before < 0 || toolLine < 0 || after < 0 || !(before < toolLine && toolLine < after) {
		t.Fatalf("unexpected timeline order: %q", output)
	}
}

func TestMessagesViewRendersPendingToolAsPending(t *testing.T) {
	view := NewMessagesView(false)
	view.UpsertTool(events.ToolDisplay{CallID: "call_1", Name: "Read", Arguments: `{"path":"go.mod"}`, Status: events.ToolDisplayPending})
	output := view.View()
	if !strings.Contains(output, "准备执行") || strings.Contains(output, "完成") {
		t.Fatalf("unexpected pending output: %q", output)
	}
}
func TestStatusShowsWaitingConfirmation(t *testing.T) {
	output := Status{Provider: "fake", Model: "test", Streaming: true, WaitingConfirmation: true}.View()
	if !strings.Contains(output, "等待工具确认") || strings.Contains(output, "正在响应") {
		t.Fatalf("unexpected status: %q", output)
	}
}
