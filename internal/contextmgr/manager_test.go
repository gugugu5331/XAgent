package contextmgr

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/provider"
)

func TestPrepareExternalizesLargeToolResult(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	conversation.AppendToolResultMessage(conv, "call", "Bash", "success", "ok", strings.Repeat("x", 80), "", false, nil, nil)
	manager := New(&summaryProvider{summary: "unused"}, t.TempDir(), testConfig())
	result, err := manager.Prepare(context.Background(), conv, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if result.Externalized != 1 || !result.Changed {
		t.Fatalf("expected one externalized result: %#v", result)
	}
	message := conv.Messages[0]
	if !message.Externalized || message.ExternalPath == "" || !strings.Contains(message.ToolResultContent, "外置") {
		t.Fatalf("message not externalized: %#v", message)
	}
	if _, err := os.Stat(message.ExternalPath); err != nil {
		t.Fatalf("external file missing: %v", err)
	}
	contextMessages := conversation.ContextMessages(conv)
	if !strings.Contains(contextMessages[0].ToolResultContent, "重新读取") {
		t.Fatalf("context message missing reread hint: %#v", contextMessages[0])
	}
}

func TestPrepareExternalizesMultipleToolResultsByTotal(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	conversation.AppendToolResultMessage(conv, "a", "Read", "success", "", strings.Repeat("a", 30), "", false, nil, nil)
	conversation.AppendToolResultMessage(conv, "b", "Read", "success", "", strings.Repeat("b", 30), "", false, nil, nil)
	cfg := testConfig()
	cfg.ToolResultThresholdChars = 100
	cfg.ToolResultsThresholdChars = 40
	manager := New(&summaryProvider{summary: "unused"}, t.TempDir(), cfg)
	result, err := manager.Prepare(context.Background(), conv, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if result.Externalized != 1 {
		t.Fatalf("expected one externalized by total threshold: %#v", result)
	}
}

func TestPrepareIsIdempotentForExternalizedResults(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	conversation.AppendToolResultMessage(conv, "call", "Bash", "success", "ok", strings.Repeat("x", 80), "", false, nil, nil)
	manager := New(&summaryProvider{summary: "unused"}, t.TempDir(), testConfig())
	if _, err := manager.Prepare(context.Background(), conv, ModeAuto); err != nil {
		t.Fatal(err)
	}
	firstPath := conv.Messages[0].ExternalPath
	result, err := manager.Prepare(context.Background(), conv, ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if result.Externalized != 0 || conv.Messages[0].ExternalPath != firstPath {
		t.Fatalf("externalization not idempotent: %#v path=%s", result, conv.Messages[0].ExternalPath)
	}
}

func TestManualCompactSummarizesAndKeepsRecentMessages(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	for i := 0; i < 8; i++ {
		conversation.AppendUserMessage(conv, "user")
		conversation.AppendAssistantMessage(conv, "assistant")
	}
	manager := New(&summaryProvider{summary: "## 当前目标\n继续工作"}, t.TempDir(), testConfig())
	result, err := manager.CompactNow(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Summarized || conv.Messages[0].Role != conversation.RoleContextSummary || conv.Messages[1].Role != conversation.RoleContextBoundary {
		t.Fatalf("summary not applied: %#v messages=%#v", result, conv.Messages[:2])
	}
	if len(conv.Messages) < 7 {
		t.Fatalf("recent messages not preserved: %d", len(conv.Messages))
	}
}

func TestSummaryFailureCircuitBreaker(t *testing.T) {
	conv := conversation.NewConversation("conv", time.Now())
	for i := 0; i < 8; i++ {
		conversation.AppendUserMessage(conv, strings.Repeat("x", 20))
	}
	cfg := testConfig()
	cfg.SummaryFailureLimit = 3
	manager := New(&summaryProvider{err: errors.New("boom")}, t.TempDir(), cfg)
	for i := 0; i < 3; i++ {
		_, _ = manager.CompactNow(context.Background(), conv)
	}
	if conversation.EnsureContext(conv).SummaryFailureCount != 3 {
		t.Fatalf("expected three failures, got %#v", conversation.EnsureContext(conv))
	}
	_, err := manager.CompactNow(context.Background(), conv)
	if err == nil || !strings.Contains(err.Error(), "熔断") {
		t.Fatalf("expected circuit breaker error, got %v", err)
	}
}

func testConfig() config.ContextConfig {
	return config.ContextConfig{
		Enabled:                   true,
		ToolResultThresholdChars:  40,
		ToolResultsThresholdChars: 1000,
		ModelWindowTokens:         100000,
		AutoMarginTokens:          1000,
		ManualMarginTokens:        100,
		RecentKeepTokens:          10,
		RecentKeepMessages:        5,
		SummaryFailureLimit:       3,
		PreviewChars:              16,
	}
}

type summaryProvider struct {
	summary string
	err     error
	lastReq provider.ChatRequest
}

func (p *summaryProvider) Name() string { return "summary" }

func (p *summaryProvider) StreamChat(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.lastReq = req
	out := make(chan provider.StreamEvent, 2)
	go func() {
		defer close(out)
		if p.err != nil {
			out <- provider.StreamEvent{Type: provider.StreamEventError, Err: p.err}
			return
		}
		out <- provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: p.summary}
		out <- provider.StreamEvent{Type: provider.StreamEventDone}
	}()
	return out, nil
}
