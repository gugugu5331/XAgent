package tui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/tool"
)

func TestMessagesViewRendersToolDisplay(t *testing.T) {
	view := NewMessagesView(false)
	view.UpsertTool(events.ToolDisplay{
		CallID:    "call_1",
		Name:      "Read",
		Arguments: safeDisplayText(`{"path":"go.mod"}`),
		Summary:   safeDisplayText("Read 10 bytes"),
		Status:    events.ToolDisplaySuccess,
	})
	output := view.View()
	if !strings.Contains(output, "● Read(go.mod)") || !strings.Contains(output, "Read 10 bytes") {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestToolResultMessageUsesOnlyUserView(t *testing.T) {
	createdAt := time.Date(2026, time.August, 3, 14, 0, 0, 0, time.UTC)
	reference := &artifact.Ref{ID: strings.Repeat("d", 64), Bytes: 64, CreatedAt: createdAt, Available: true, Complete: true}
	userView := tool.UserView{
		State:            tool.Completed,
		Status:           tool.StatusError,
		Summary:          safeDisplayText("safe display summary"),
		Preview:          safeDisplayText("live-preview-canary"),
		Artifact:         reference,
		Truncated:        true,
		TruncationReason: safeDisplayText("inline_preview_limit"),
		Error:            &tool.SafeError{Code: "command_failed", Message: safeDisplayText("safe display error"), Recoverable: true},
	}
	view := NewMessagesView(false)
	view.UpsertToolResult("call-1", "Bash", safeDisplayText(`{"command":"go test"}`), userView)
	output := view.View()
	for _, expected := range []string{"● Bash(go test)", "safe display summary", "stdout: live-preview-canary", "safe display error", "Artifact ID=" + reference.ID, "64 bytes", "Available", "Complete", "/artifact " + reference.ID} {
		if !strings.Contains(output, expected) {
			t.Fatalf("UserView display missing %q: %q", expected, output)
		}
	}
	if len(view.messages) != 1 || !view.messages[0].isTool {
		t.Fatalf("display marker missing: %#v", view.messages)
	}
	marker := view.messages[0]
	if marker.tool.Stdout.Text() != userView.Preview.Text() || marker.tool.Artifact == nil ||
		marker.tool.Artifact.ID != reference.ID || marker.tool.ErrorCode != "command_failed" {
		t.Fatalf("TUI did not retain the detached safe UserView projection: %#v", marker)
	}
	reference.ID = strings.Repeat("e", 64)
	userView.Error.Code = "mutated"
	if refreshed := view.View(); strings.Contains(refreshed, reference.ID) || strings.Contains(refreshed, "mutated") {
		t.Fatalf("TUI aliases mutable UserView fields: %q", refreshed)
	}
}

func TestMessagesViewRestoresToolDisplayFromHistory(t *testing.T) {
	view := NewMessagesView(false)
	view.SetMessages([]conversation.Message{
		historyToolMessage(conversation.RoleToolCall, "call_1", "Read", `{"path":"go.mod"}`, tool.Prepared, "", ""),
		historyToolMessage(conversation.RoleToolResult, "call_1", "Read", "", tool.Completed, tool.StatusSuccess, "Read 10 bytes"),
	})
	output := view.View()
	if !strings.Contains(output, "● Read(go.mod)") || !strings.Contains(output, "Read 10 bytes") {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestMessagesViewProjectsConversationBeforeCaching(t *testing.T) {
	source := []conversation.Message{
		{Role: conversation.RoleUser, Content: safeDisplayText("detached user")},
		historyToolMessage(conversation.RoleToolCall, "call-1", "Read", `{"path":"go.mod"}`, tool.Prepared, "", ""),
		historyToolMessage(conversation.RoleToolResult, "call-1", "Read", "", tool.Completed, tool.StatusSuccess, "detached result"),
	}
	view := NewMessagesView(false)
	view.SetMessages(source)

	source[0].Content = safeDisplayText("mutated user")
	source[1].Tool.Name = "mutated tool"
	source[2].Tool.Summary = safeDisplayText("mutated result")
	output := view.View()
	for _, want := range []string{"detached user", "● Read(go.mod)", "detached result"} {
		if !strings.Contains(output, want) {
			t.Fatalf("projected output missing %q: %q", want, output)
		}
	}
	for _, unwanted := range []string{"mutated user", "mutated tool", "mutated result"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("projected output aliases %q: %q", unwanted, output)
		}
	}
	if reflect.TypeOf(view.messages).Elem() == reflect.TypeOf(conversation.Message{}) {
		t.Fatal("MessagesView retained conversation.Message objects")
	}
}

func TestMessagesViewRestoresCancelledToolFromHistory(t *testing.T) {
	view := NewMessagesView(false)
	view.SetMessages([]conversation.Message{
		historyToolMessage(conversation.RoleToolCall, "call_1", "Bash", `{"command":"git status"}`, tool.Prepared, "", ""),
		historyToolMessage(conversation.RoleToolResult, "call_1", "Bash", "", tool.CancelledAfterStart, tool.StatusError, "用户取消工具确认"),
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
	view.UpsertTool(events.ToolDisplay{CallID: "call_1", Name: "Read", Arguments: safeDisplayText(`{"path":"go.mod"}`), Status: events.ToolDisplayRunning})
	view.UpsertTool(events.ToolDisplay{CallID: "call_1", Name: "Read", Status: events.ToolDisplaySuccess, Summary: safeDisplayText("完成")})
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
	artifactID := strings.Repeat("a", 64)
	view := NewMessagesView(false)
	view.UpsertTool(events.ToolDisplay{
		CallID:      "call_1",
		Name:        "Bash",
		Arguments:   safeDisplayText(`{"command":"go test ./..."}`),
		Summary:     safeDisplayText("Command exited 1"),
		Status:      events.ToolDisplayError,
		ErrorCode:   "command_failed",
		Stdout:      safeDisplayText("ok package"),
		Stderr:      safeDisplayText("failed package"),
		Truncated:   true,
		Recoverable: true,
		Artifact:    &events.ArtifactRef{ID: artifactID, Bytes: 123, Available: true},
	})
	output := view.View()
	for _, want := range []string{"● Bash(go test ./...)", "error: command_failed", "stdout: ok package", "stderr: failed package", "output truncated", "recoverable", "Artifact ID=" + artifactID, "123 bytes", "Available", "Incomplete", "/artifact " + artifactID} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
}

func TestMessagesViewRestoresToolErrorDetailsFromHistory(t *testing.T) {
	artifactID := strings.Repeat("b", 64)
	view := NewMessagesView(false)
	call := historyToolMessage(conversation.RoleToolCall, "call_1", "Bash", `{"command":"go test ./..."}`, tool.Prepared, "", "")
	result := historyToolMessage(conversation.RoleToolResult, "call_1", "Bash", "", tool.Completed, tool.StatusError, "Command exited 1")
	result.Tool.Result = safeDisplayText("ok package")
	result.Tool.Truncated = true
	result.Tool.Artifact = &artifact.Ref{ID: artifactID, Bytes: 123, Available: true, Complete: true}
	result.Tool.Error = &tool.SafeError{Code: "command_failed", Message: safeDisplayText("failed package"), Recoverable: true}
	view.SetMessages([]conversation.Message{
		call,
		result,
	})
	output := view.View()
	for _, want := range []string{"error: command_failed", "stderr: failed package", "output truncated", "recoverable", "Artifact ID=" + artifactID, "123 bytes", "Available", "Complete", "/artifact " + artifactID} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q: %q", want, output)
		}
	}
	if strings.Contains(output, "stdout: ok package") {
		t.Fatalf("persisted tool content was rendered as a live preview: %q", output)
	}
}

func TestMessagesViewRendersPendingToolAsPending(t *testing.T) {
	view := NewMessagesView(false)
	view.UpsertTool(events.ToolDisplay{CallID: "call_1", Name: "Read", Arguments: safeDisplayText(`{"path":"go.mod"}`), Status: events.ToolDisplayPending})
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

func TestMessagesViewToolUpdateAfterValueCopyDoesNotModifySource(t *testing.T) {
	view := NewMessagesView(false)
	view.UpsertTool(events.ToolDisplay{
		CallID: "call-1", Name: "Read", Arguments: safeDisplayText(`{"path":"go.mod"}`), Status: events.ToolDisplayRunning,
	})
	copied := view
	copied.UpsertTool(events.ToolDisplay{
		CallID: "call-1", Status: events.ToolDisplaySuccess, Summary: safeDisplayText("copied complete"),
	})

	if source := view.View(); !strings.Contains(source, "执行中") || strings.Contains(source, "copied complete") {
		t.Fatalf("copied tool update modified source view: %q", source)
	}
	if output := copied.View(); !strings.Contains(output, "copied complete") || strings.Contains(output, "执行中") {
		t.Fatalf("copied tool update was not isolated: %q", output)
	}
}

func TestMessagesViewClearDoesNotModifySourceAndCanAppend(t *testing.T) {
	messages := []conversation.Message{{Role: conversation.RoleUser, Content: safeDisplayText("keep")}}
	view := NewMessagesView(true)
	view.SetMessages(messages)
	view.AppendAssistantDelta("assistant")
	view.AppendThinkingDelta("thinking")
	view.Clear()
	if output := view.View(); output != "" {
		t.Fatalf("cleared view is not empty: %q", output)
	}
	if len(messages) != 1 || messages[0].Content.Text() != "keep" {
		t.Fatalf("source messages changed: %#v", messages)
	}
	view.AppendUser("new")
	if output := view.View(); !strings.Contains(output, "new") || strings.Contains(output, "keep") {
		t.Fatalf("unexpected output after append: %q", output)
	}
}

func TestTransientMessagesRenderAndClearWithoutPersistence(t *testing.T) {
	view := NewMessagesView(true)
	view.SetMessages([]conversation.Message{{Role: conversation.RoleUser, Content: safeDisplayText("/review current")}})
	view.AppendTransientThinkingDelta("run-1", "checking")
	view.AppendTransientAssistantDelta("run-1", "I will inspect.")
	view.UpsertTransientTool("run-1", events.ToolDisplay{
		CallID: "call-1", Name: "Read", Arguments: safeDisplayText(`{"path":"main.go"}`), Status: events.ToolDisplayRunning,
	})

	output := view.View()
	for _, want := range []string{"/review current", "checking", "I will inspect.", "● Read(main.go)", "执行中"} {
		if !strings.Contains(output, want) {
			t.Fatalf("live transient output missing %q: %q", want, output)
		}
	}
	if len(view.messages) != 1 {
		t.Fatalf("transient trace entered persistent messages: %#v", view.messages)
	}

	view.UpsertTransientTool("run-1", events.ToolDisplay{CallID: "call-1", Status: events.ToolDisplaySuccess, Summary: safeDisplayText("done")})
	output = view.View()
	if !strings.Contains(output, "● Read(main.go)") || !strings.Contains(output, "done") || strings.Contains(output, "执行中") {
		t.Fatalf("transient tool was not updated by CallID: %q", output)
	}

	view.ClearTransient("run-1")
	output = view.View()
	for _, gone := range []string{"checking", "I will inspect.", "● Read(main.go)", "done"} {
		if strings.Contains(output, gone) {
			t.Fatalf("cleared transient output still contains %q: %q", gone, output)
		}
	}
	if !strings.Contains(output, "/review current") || len(view.messages) != 1 {
		t.Fatalf("clearing transient changed persistent history: output=%q messages=%#v", output, view.messages)
	}
}

func TestTransientMessagesAreIsolatedByIndependentID(t *testing.T) {
	view := NewMessagesView(false)
	view.AppendTransientAssistantDelta("run-1", "first")
	view.AppendTransientAssistantDelta("run-2", "second")
	view.ClearTransient("run-1")
	output := view.View()
	if strings.Contains(output, "first") || !strings.Contains(output, "second") {
		t.Fatalf("transient traces were not isolated: %q", output)
	}
	view.Clear()
	if output := view.View(); output != "" {
		t.Fatalf("Clear retained transient trace: %q", output)
	}
}

func TestTransientThinkingHonorsVisibility(t *testing.T) {
	view := NewMessagesView(false)
	view.AppendTransientThinkingDelta("run-1", "secret reasoning")
	if output := view.View(); strings.Contains(output, "secret reasoning") {
		t.Fatalf("hidden thinking was rendered: %q", output)
	}
}

func TestSetMessagesDropsTransientTrace(t *testing.T) {
	view := NewMessagesView(false)
	view.AppendAssistantDelta("parent preamble")
	view.AppendThinkingDelta("parent thinking")
	view.AppendTransientAssistantDelta("run-1", "temporary")
	view.SetMessages([]conversation.Message{{Role: conversation.RoleAssistant, Content: safeDisplayText("saved")}})
	output := view.View()
	if strings.Contains(output, "temporary") || strings.Contains(output, "parent preamble") || strings.Contains(output, "parent thinking") || !strings.Contains(output, "saved") {
		t.Fatalf("history reload retained transient trace: %q", output)
	}
}

func historyToolMessage(
	role conversation.MessageRole,
	callID string,
	name string,
	arguments string,
	state tool.ExecutionState,
	status tool.ResultStatus,
	summary string,
) conversation.Message {
	return conversation.Message{
		Role:    role,
		Content: safeDisplayText(summary),
		Tool: &conversation.ToolState{
			CallID:        callID,
			Name:          name,
			ArgumentsJSON: safeDisplayText(arguments),
			State:         state,
			Status:        status,
			Summary:       safeDisplayText(summary),
		},
	}
}
