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
