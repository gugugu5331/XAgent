package tui

import (
	"encoding/json"
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

func TestMessagesViewRestoresCancelledToolFromHistory(t *testing.T) {
	view := NewMessagesView(false)
	view.SetMessages([]conversation.Message{
		{Role: conversation.RoleToolCall, ToolCallID: "call_1", ToolName: "Bash", RawToolArguments: `{"command":"git status"}`},
		{Role: conversation.RoleToolResult, ToolCallID: "call_1", ToolName: "Bash", ToolResultStatus: "cancelled", ToolResultSummary: "用户取消工具确认"},
	})
	output := view.View()
	if !strings.Contains(output, "● Bash(git status)") || !strings.Contains(output, "用户取消工具确认") {
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

func TestMessagesViewRendersToolErrorDetails(t *testing.T) {
	view := NewMessagesView(false)
	view.UpsertTool(events.ToolDisplay{
		CallID:            "call_1",
		Name:              "Bash",
		Arguments:         `{"command":"go test ./..."}`,
		Summary:           "Command exited 1",
		Status:            events.ToolDisplayError,
		ErrorCode:         "command_failed",
		Stdout:            "ok package",
		Stderr:            "failed package",
		Truncated:         true,
		Recoverable:       true,
		ArtifactID:        "call_1",
		ArtifactBytes:     123,
		ArtifactAvailable: true,
	})
	output := view.View()
	for _, want := range []string{"● Bash(go test ./...)", "error: command_failed", "stdout: ok package", "stderr: failed package", "output truncated", "recoverable", "artifact: call_1 (123 bytes)"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
}

func TestMessagesViewRestoresToolErrorDetailsFromHistory(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"stdout": "ok package", "stderr": "failed package", "artifact_id": "call_1", "artifact_bytes": 123, "artifact_available": true})
	errorData, _ := json.Marshal(map[string]any{"code": "command_failed", "recoverable": true})
	view := NewMessagesView(false)
	view.SetMessages([]conversation.Message{
		{Role: conversation.RoleToolCall, ToolCallID: "call_1", ToolName: "Bash", RawToolArguments: `{"command":"go test ./..."}`},
		{Role: conversation.RoleToolResult, ToolCallID: "call_1", ToolName: "Bash", ToolResultStatus: "error", ToolResultSummary: "Command exited 1", ToolErrorCode: "command_failed", ToolResultTruncated: true, ToolResultData: data, ToolResultError: errorData},
	})
	output := view.View()
	for _, want := range []string{"error: command_failed", "stdout: ok package", "stderr: failed package", "output truncated", "recoverable", "artifact: call_1 (123 bytes)"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
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
	output := Status{Provider: "fake", Model: "test", Streaming: true, WaitingConfirmation: true, AgentIteration: 3, AgentMaxIterations: 10}.View()
	if !strings.Contains(output, "等待工具确认") || strings.Contains(output, "第 3/10 轮") {
		t.Fatalf("unexpected status: %q", output)
	}
}

func TestStatusShowsAgentProgress(t *testing.T) {
	output := Status{Provider: "fake", Model: "test", Streaming: true, AgentIteration: 3, AgentMaxIterations: 10}.View()
	if !strings.Contains(output, "第 3/10 轮") {
		t.Fatalf("unexpected status: %q", output)
	}
}

func TestStatusShowsStopReasonAndUsage(t *testing.T) {
	output := Status{Provider: "fake", Model: "test", StopReason: "max_iterations", InputTokens: 12, OutputTokens: 34}.View()
	if !strings.Contains(output, "达到迭代上限") || !strings.Contains(output, "Tokens: 12 in / 34 out") {
		t.Fatalf("unexpected status: %q", output)
	}
}

func TestStatusShowsCacheUsageOnlyWhenPresent(t *testing.T) {
	withoutCache := Status{Provider: "fake", Model: "test", InputTokens: 12, OutputTokens: 34}.View()
	if strings.Contains(withoutCache, "Cache:") {
		t.Fatalf("cache segment should be hidden when zero: %q", withoutCache)
	}
	withCache := Status{Provider: "fake", Model: "test", CacheCreationInputTokens: 56, CacheReadInputTokens: 78}.View()
	if !strings.Contains(withCache, "Cache: 56 create / 78 read") {
		t.Fatalf("cache segment missing: %q", withCache)
	}
}

func TestStatusShowsMCPStatus(t *testing.T) {
	output := Status{Provider: "fake", Model: "test", MCP: "1 ready, 1 failed, bad: timeout"}.View()
	if !strings.Contains(output, "MCP: 1 ready, 1 failed, bad: timeout") {
		t.Fatalf("unexpected status: %q", output)
	}
}

func TestMessagesViewCanAppendAfterValueCopy(t *testing.T) {
	view := NewMessagesView(true)
	view.AppendAssistantDelta("hello")
	copied := view
	copied.AppendAssistantDelta(" world")
	copied.AppendThinkingDelta("thinking")
	output := copied.View()
	if !strings.Contains(output, "hello world") || !strings.Contains(output, "thinking") {
		t.Fatalf("unexpected copied view output: %q", output)
	}
}
