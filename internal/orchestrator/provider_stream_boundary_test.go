package orchestrator

import (
	"context"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/provider"
)

func TestUnsafeToolArgumentsNeverReachAuthorizationOrExecutor(t *testing.T) {
	t.Run("provider_safe_error_stops_before_tool_pipeline", func(t *testing.T) {
		providerImpl := &scriptedSkillProvider{events: [][]provider.StreamEvent{{{
			Type: provider.StreamEventError,
			Error: &diagnostics.SafeError{
				Code:        "unsafe_tool_arguments",
				Source:      "provider.test",
				Message:     testSafeText("工具参数脱敏后不是有效 JSON，已拒绝工具调用"),
				Recoverable: false,
			},
		}}}}
		runUnsafeToolArgumentsBoundary(t, providerImpl)
	})

	t.Run("malformed_safe_tool_call_fails_closed", func(t *testing.T) {
		providerImpl := &scriptedSkillProvider{events: [][]provider.StreamEvent{
			{{
				Type: provider.StreamEventToolCall,
				ToolCall: &provider.SafeToolCall{
					ID:            "unsafe-call",
					Name:          "Recorder",
					ArgumentsJSON: testSafeText(`{"token":"[REDACTED]}`),
				},
			}},
			{{Type: provider.StreamEventDone}},
		}}
		runUnsafeToolArgumentsBoundary(t, providerImpl)
	})
}

func runUnsafeToolArgumentsBoundary(t *testing.T, providerImpl provider.Provider) {
	t.Helper()
	hooks := newToolHookRecorder(hook.Continue())
	orch, _, recorder := newHookToolFixture(t, hooks)
	orch.provider = providerImpl
	conv := conversation.NewConversation("unsafe-tool-arguments", time.Now())
	stream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "check unsafe tool arguments", Mode: RunModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	for event := range stream {
		if event.Type == events.ToolWaitingConfirmation || event.Type == events.ToolSuccess {
			t.Fatalf("unsafe tool arguments reached an authorization or execution event: %#v", event)
		}
	}
	before, after := hooks.counts()
	if before != 0 || after != 0 {
		t.Fatalf("unsafe tool arguments reached tool hooks: before=%d after=%d", before, after)
	}
	recorder.mu.Lock()
	executions := recorder.count
	recorder.mu.Unlock()
	if executions != 0 {
		t.Fatalf("unsafe tool arguments reached Executor: executions=%d", executions)
	}
	for _, message := range conv.Messages {
		if message.Role == conversation.RoleToolCall || message.Role == conversation.RoleToolResult {
			t.Fatalf("unsafe tool arguments entered tool history: %#v", conv.Messages)
		}
	}
}
