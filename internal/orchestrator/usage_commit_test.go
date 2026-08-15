package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/resources"
	"xagent/internal/skill"
)

func TestUsageAndToolResultsHaveSingleOrderedCommit(t *testing.T) {
	t.Run("usage", testUsageHasSingleOrderedCommit)
	t.Run("tool_results", testToolResultsHaveSingleOrderedCommit)
	t.Run("tool_execution_path", runToolResultsOrderedCommitChecks)
	t.Run("ordered_tool_terminal_does_not_save_again", testOrderedToolTerminalDoesNotSaveAgain)
}

func testUsageHasSingleOrderedCommit(t *testing.T) {
	t.Helper()
	disabled := false
	manager, err := contextmgr.New(nil, contextmgr.ManagerOptions{
		Context: config.ContextConfig{
			Enabled:                   &disabled,
			ToolResultThresholdChars:  40,
			ToolResultsThresholdChars: 1000,
			ModelWindowTokens:         100000,
			AutoMarginTokens:          1000,
			ManualMarginTokens:        100,
			RecentKeepTokens:          10,
			RecentKeepMessages:        5,
			SummaryFailureLimit:       3,
			PreviewChars:              16,
		},
		InlineOutputBytes: 8,
		RuntimeRedactor:   redact.NewRuntimeRedactor(),
	})
	if err != nil {
		t.Fatal(err)
	}

	published := &provider.Usage{
		InputTokens:              101,
		OutputTokens:             29,
		CacheCreationInputTokens: 17,
		CacheReadInputTokens:     13,
	}
	stream := newOrchestratorTestChatStream(
		provider.StreamEvent{Type: provider.StreamEventUsage, Usage: published},
		provider.StreamEvent{Type: provider.StreamEventDone},
	)
	out := make(chan events.Event, 2)
	collector, reason, err := collectProviderStream(context.Background(), stream, out)
	if err != nil || reason != StopReasonCompleted || !collector.Done {
		t.Fatalf("usage collection failed: collector=%#v reason=%s err=%v", collector, reason, err)
	}
	if len(out) != 0 {
		t.Fatalf("stream collector committed usage before the ordered commit point: events=%d", len(out))
	}

	want := *published
	published.InputTokens = 999
	published.OutputTokens = 999
	orchestrator := &Orchestrator{contextManager: manager}
	conv := conversation.NewConversation("usage-single-commit", time.Now())
	if err := orchestrator.commitProviderUsage(context.Background(), conv, collector.Usage, out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("usage event commits = %d, want 1", len(out))
	}
	event := <-out
	if event.Usage == nil {
		t.Fatalf("usage event omitted the Provider final snapshot: %#v", event)
	}
	got := provider.Usage{
		InputTokens:              event.Usage.InputTokens,
		OutputTokens:             event.Usage.OutputTokens,
		CacheCreationInputTokens: event.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     event.Usage.CacheReadInputTokens,
	}
	if event.Type != events.UsageUpdated || !reflect.DeepEqual(got, want) {
		t.Fatalf("usage event did not use the Provider final snapshot: type=%s got=%#v want=%#v", event.Type, got, want)
	}
	if conv.Context == nil || conv.Context.LastInputTokens != want.InputTokens || conv.Context.LastOutputTokens != want.OutputTokens || conv.Context.LastEstimatedTokens != want.InputTokens {
		t.Fatalf("ContextManager did not receive the same Provider snapshot: %#v", conv.Context)
	}

	final := provider.Usage{
		InputTokens:              211,
		OutputTokens:             37,
		CacheCreationInputTokens: 19,
		CacheReadInputTokens:     23,
	}
	providerRecorder := &fakeProvider{events: [][]provider.StreamEvent{{
		{Type: provider.StreamEventUsage, Usage: &final},
		{Type: provider.StreamEventDone},
	}}}
	orchestrator = NewWithOptions(OrchestratorOptions{
		Provider:       providerRecorder,
		Resources:      resources.New(),
		ContextManager: manager,
	})
	conv = conversation.NewConversation("usage-agent-loop-commit", time.Now())
	eventStream, err := orchestrator.SendRequest(context.Background(), conv, RunRequest{UserText: "commit usage once", Mode: RunModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	usageEvents := 0
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatalf("agent loop failed: %#v", event.Err)
		}
		if event.Type != events.UsageUpdated {
			continue
		}
		usageEvents++
		if event.Usage == nil || !reflect.DeepEqual(provider.Usage{
			InputTokens:              event.Usage.InputTokens,
			OutputTokens:             event.Usage.OutputTokens,
			CacheCreationInputTokens: event.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     event.Usage.CacheReadInputTokens,
		}, final) {
			t.Fatalf("agent loop usage event did not preserve the final snapshot: %#v", event.Usage)
		}
	}
	if usageEvents != 1 {
		t.Fatalf("agent loop usage event commits = %d, want 1", usageEvents)
	}
	if conv.Context == nil || conv.Context.LastInputTokens != final.InputTokens || conv.Context.LastOutputTokens != final.OutputTokens || conv.Context.LastEstimatedTokens != final.InputTokens {
		t.Fatalf("agent loop ContextManager usage differs from its event: %#v", conv.Context)
	}

	invalidCases := []struct {
		name   string
		events []provider.StreamEvent
	}{
		{name: "duplicate", events: []provider.StreamEvent{{Type: provider.StreamEventUsage, Usage: &final}, {Type: provider.StreamEventUsage, Usage: &final}, {Type: provider.StreamEventDone}}},
		{name: "late", events: []provider.StreamEvent{{Type: provider.StreamEventDone}, {Type: provider.StreamEventUsage, Usage: &final}}},
		{name: "negative", events: []provider.StreamEvent{{Type: provider.StreamEventUsage, Usage: &provider.Usage{InputTokens: -1}}, {Type: provider.StreamEventDone}}},
		{name: "overflow", events: []provider.StreamEvent{{Type: provider.StreamEventUsage, Usage: &provider.Usage{InputTokens: int64(^uint64(0) >> 1), OutputTokens: 1}}, {Type: provider.StreamEventDone}}},
	}
	for _, testCase := range invalidCases {
		t.Run(testCase.name+" fails closed", func(t *testing.T) {
			conv := conversation.NewConversation("usage-invalid-"+testCase.name, time.Now())
			orchestrator := NewWithOptions(OrchestratorOptions{
				Provider:  &fakeProvider{events: [][]provider.StreamEvent{testCase.events}},
				Resources: resources.New(), ContextManager: manager,
			})
			stream, err := orchestrator.Send(context.Background(), conv, "invalid usage")
			if err != nil {
				t.Fatal(err)
			}
			commits := 0
			for event := range stream {
				if event.Type == events.UsageUpdated {
					commits++
				}
			}
			if commits != 0 || conv.Context != nil {
				t.Fatalf("invalid usage partially committed: events=%d context=%#v", commits, conv.Context)
			}
		})
	}
}

func testOrderedToolTerminalDoesNotSaveAgain(t *testing.T) {
	t.Helper()
	store := &orderedCommitStore{}
	orchestrator := NewWithOptions(OrchestratorOptions{Store: store})
	conv := conversation.NewConversation("ordered-tool-terminal", time.Now())
	state := &executionState{profile: skill.ExecutionProfile{Persist: true}}
	wantErr := errors.New("already attempted ordered save")
	out := make(chan events.Event, 1)
	gotErr := orchestrator.stopRunWithError(
		context.Background(), conv, state, out, 1, 1, StopReasonProviderError,
		wantErr.Error(), &orderedToolCommitError{err: wantErr},
	)
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("ordered terminal error = %v, want %v", gotErr, wantErr)
	}
	if saves, _ := store.snapshot(); saves != 0 {
		t.Fatalf("ordered terminal retried Save %d times", saves)
	}
	if len(out) != 1 || (<-out).Type != events.AgentProgressed {
		t.Fatal("ordered terminal did not publish its stop progress")
	}
}
