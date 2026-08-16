package testutil

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/subagent"
)

func TestSubagentFixtureProviderScriptsMultiRoundStreamErrorAndSlowCancellation(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	started := make(chan struct{}, 1)
	slowGate := make(chan struct{})
	streamFailure := &diagnostics.SafeError{
		Code: "provider_stream_failed", Source: "fixture", Message: redactor.Redact("safe stream failure"), Recoverable: true,
	}
	fixture := NewScriptedSubagentProvider(
		SubagentProviderStep{Events: []provider.StreamEvent{
			{Type: provider.StreamEventToolCall, ToolCall: &provider.SafeToolCall{ID: "call-1", Name: "Read", ArgumentsJSON: redactor.Redact(`{"path":"README.md"}`)}},
			{Type: provider.StreamEventDone},
		}},
		SubagentProviderStep{Events: []provider.StreamEvent{
			{Type: provider.StreamEventTextDelta, Delta: redactor.Redact("final summary")},
			{Type: provider.StreamEventUsage, Usage: &provider.Usage{InputTokens: 3, OutputTokens: 2}},
			{Type: provider.StreamEventDone},
		}},
		SubagentProviderStep{Events: []provider.StreamEvent{{Type: provider.StreamEventError, Error: streamFailure}}},
		SubagentProviderStep{Started: started, Gate: slowGate},
	)

	request := provider.ChatRequest{Model: "fixture-model", Messages: []provider.ModelMessage{{
		Role: provider.ModelMessageRoleUser, Content: redactor.Redact("first request"),
	}}}
	first := drainSubagentProviderStream(t, fixture, context.Background(), request)
	if len(first) != 2 || first[0].ToolCall == nil || first[0].ToolCall.Name != "Read" {
		t.Fatalf("first scripted round mismatch: %#v", first)
	}
	request.Messages[0].Content = redactor.Redact("mutated after call")
	second := drainSubagentProviderStream(t, fixture, context.Background(), provider.ChatRequest{Model: "fixture-model"})
	if len(second) != 3 || second[0].Delta.Text() != "final summary" || second[1].Usage == nil || second[1].Usage.InputTokens != 3 {
		t.Fatalf("second scripted round mismatch: %#v", second)
	}
	third := drainSubagentProviderStream(t, fixture, context.Background(), provider.ChatRequest{Model: "fixture-model"})
	if len(third) != 1 || third[0].Error == nil || third[0].Error.Code != "provider_stream_failed" {
		t.Fatalf("stream error injection mismatch: %#v", third)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := fixture.StreamChat(ctx, provider.ChatRequest{Model: "fixture-model"})
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow Provider step did not signal start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("slow Provider cancellation error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow Provider step ignored cancellation")
	}

	requests := fixture.Requests()
	if len(requests) != 4 || requests[0].Messages[0].Content.Text() != "first request" {
		t.Fatalf("Provider request capture was incomplete or aliased: %#v", requests)
	}
	requests[0].Messages[0].Content = redactor.Redact("mutated returned copy")
	if fixture.Requests()[0].Messages[0].Content.Text() != "first request" {
		t.Fatal("Provider Requests returned shared storage")
	}
}

func TestSubagentFixtureRunnerClockIDsConsumerConfirmationAndResultInjection(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	clock := NewManualSubagentClock(time.Unix(1_000, 0).UTC())
	ids := NewScriptedSubagentIDGenerator(
		SubagentIDResult{ID: "task-fixture"},
		SubagentIDResult{ID: "notification-fixture"},
		SubagentIDResult{ID: "claim-fixture"},
	)
	resolved := make(chan struct{})
	var resolveOnce sync.Once
	request := events.ToolConfirmationRequest{
		ConfirmationID: "confirmation-fixture", CallID: "call-fixture", Name: "Write",
		Arguments: redactor.Redact(`{"path":"safe.txt"}`),
		Scopes:    []events.ConfirmationScopeDisplay{{Scope: "once", Available: true, Description: redactor.Redact("once")}},
	}
	runner := NewScriptedSubagentRunner(clock.Now, SubagentRunStep{
		Metadata: subagent.PreparedMetadata{Type: subagent.TypeDefined, Role: "fixture", RoleSource: agentrole.SourceProject, MaxIterations: 3},
		BeforeWait: []subagent.AgentEvent{{
			Kind:    events.ToolWaitingConfirmation,
			Payload: events.Event{Type: events.ToolWaitingConfirmation, Confirmation: &request},
		}},
		Wait: resolved,
		AfterWait: []subagent.AgentEvent{{
			Kind:    events.ToolRunning,
			Payload: events.Event{Type: events.ToolRunning, Tool: &events.ToolDisplay{CallID: request.CallID, Name: request.Name}},
		}},
		ResolveConfirmation: func(decision events.ToolConfirmationDecision) error {
			if decision.ConfirmationID != request.ConfirmationID || decision.CallID != request.CallID {
				return errors.New("wrong confirmation identity")
			}
			resolveOnce.Do(func() { close(resolved) })
			return nil
		},
		Completion: subagent.Completion{
			Status: subagent.StatusCompleted, Summary: redactor.Redact("fixture completed"), StopReason: subagent.StopCompleted,
		},
	})
	manager, inbox := newSubagentFixtureManager(t, runner, clock.Now, ids.Generate, 250*time.Millisecond)
	consumer := newSubagentFixtureConsumer(t, manager)
	defer consumer.Close()

	submission, err := manager.Submit(context.Background(), subagentFixtureInput(subagent.PlacementBackground))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	waiting, err := consumer.WaitForKind(waitCtx, subagent.EventWaitingConfirmation)
	if err != nil || waiting.TaskID != submission.ID || waiting.Snapshot == nil || waiting.Snapshot.PendingConfirmation == nil {
		t.Fatalf("confirmation event was not consumable: event=%#v err=%v", waiting, err)
	}
	clock.Advance(time.Second)
	decision := events.ToolConfirmationDecision{
		ConfirmationID: request.ConfirmationID, CallID: request.CallID, Action: events.PermissionAllowOnce, Allowed: true,
	}
	if err := manager.ResolveConfirmation(context.Background(), submission.ID, decision); err != nil {
		t.Fatal(err)
	}
	resultEvent, err := consumer.WaitForKind(waitCtx, subagent.EventResultPublished)
	if err != nil || resultEvent.Result == nil || resultEvent.Result.Summary.Text() != "fixture completed" {
		t.Fatalf("result event was not injected: event=%#v err=%v", resultEvent, err)
	}
	completion, err := manager.Await(context.Background(), submission.ID)
	if err != nil || completion.EndedAt != clock.Now() || completion.ID != submission.ID {
		t.Fatalf("Runner/Clock completion mismatch: completion=%#v err=%v", completion, err)
	}
	claim, err := inbox.Claim(context.Background(), subagent.ResultClaimOptions{
		Owner: subagent.ParentRef{
			ConversationID:    "conversation-fixture",
			ExecutionID:       "fixture-consumer",
			RequestGeneration: 1,
		},
		MaxNotifications: 1,
		MaxBytes:         64 << 10,
	})
	if err != nil || len(claim.Notifications) != 1 || claim.Notifications[0].TaskID != submission.ID {
		t.Fatalf("result inbox injection mismatch: claim=%#v err=%v", claim, err)
	}
	if calls := runner.Calls(); len(calls) != 1 || calls[0].ID != submission.ID || calls[0].Input.Task != "fixture task" {
		t.Fatalf("Runner calls=%#v", calls)
	}
	if ids.Calls() != 3 {
		t.Fatalf("ID generator calls=%d, want 3", ids.Calls())
	}
	eventsSnapshot := consumer.Events()
	if len(eventsSnapshot) < 5 || !strictlyIncreasingSubagentFixtureEvents(eventsSnapshot) {
		t.Fatalf("event consumer did not retain ordered detached events: %#v", eventsSnapshot)
	}
	eventsSnapshot[0].Kind = subagent.EventGap
	if consumer.Events()[0].Kind == subagent.EventGap {
		t.Fatal("event consumer returned shared event storage")
	}
}

func TestSubagentFixtureRunnerSupportsSlowFirstRequestDetachAndCancellation(t *testing.T) {
	t.Run("slow first request detaches", func(t *testing.T) {
		clock := NewManualSubagentClock(time.Unix(2_000, 0).UTC())
		ids := NewScriptedSubagentIDGenerator(
			SubagentIDResult{ID: "task-slow"}, SubagentIDResult{ID: "notification-slow"},
		)
		firstRequestGate := make(chan struct{})
		runner := NewScriptedSubagentRunner(clock.Now, SubagentRunStep{
			Metadata:         subagent.PreparedMetadata{Type: subagent.TypeDefined, Role: "fixture", RoleSource: agentrole.SourceProject, MaxIterations: 2},
			FirstRequestGate: firstRequestGate,
			Completion: subagent.Completion{
				Status: subagent.StatusCompleted, Summary: redact.NewRuntimeRedactor().Redact("detached completion"), StopReason: subagent.StopCompleted,
			},
		})
		manager, _ := newSubagentFixtureManager(t, runner, clock.Now, ids.Generate, 10*time.Millisecond)
		consumer := newSubagentFixtureConsumer(t, manager)
		defer consumer.Close()
		submission, err := manager.Submit(context.Background(), subagentFixtureInput(subagent.PlacementForeground))
		if err != nil {
			t.Fatal(err)
		}
		waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		placement, err := consumer.WaitForKind(waitCtx, subagent.EventPlacementChanged)
		if err != nil || placement.Placement == nil || placement.Placement.To != subagent.Background {
			t.Fatalf("slow first request did not detach: event=%#v err=%v", placement, err)
		}
		outcome, err := manager.AwaitForeground(context.Background(), submission.ID)
		if err != nil || !outcome.Detached || outcome.Completion != nil {
			t.Fatalf("foreground outcome=%#v err=%v", outcome, err)
		}
		close(firstRequestGate)
		if _, err := consumer.WaitForKind(waitCtx, subagent.EventResultPublished); err != nil {
			t.Fatalf("detached task result: %v", err)
		}
	})

	t.Run("cancel releases blocked runner", func(t *testing.T) {
		clock := NewManualSubagentClock(time.Unix(3_000, 0).UTC())
		ids := NewScriptedSubagentIDGenerator(
			SubagentIDResult{ID: "task-cancel"}, SubagentIDResult{ID: "notification-cancel"},
		)
		started := make(chan struct{}, 1)
		blocked := make(chan struct{})
		runner := NewScriptedSubagentRunner(clock.Now, SubagentRunStep{
			Metadata: subagent.PreparedMetadata{Type: subagent.TypeDefined, Role: "fixture", RoleSource: agentrole.SourceProject, MaxIterations: 2},
			Started:  started, Wait: blocked,
		})
		manager, _ := newSubagentFixtureManager(t, runner, clock.Now, ids.Generate, time.Second)
		consumer := newSubagentFixtureConsumer(t, manager)
		defer consumer.Close()
		submission, err := manager.Submit(context.Background(), subagentFixtureInput(subagent.PlacementBackground))
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("blocked Runner did not start")
		}
		if err := manager.Cancel(context.Background(), submission.ID); err != nil {
			t.Fatal(err)
		}
		completion, err := manager.Await(context.Background(), submission.ID)
		if err != nil || completion.Status != subagent.StatusCancelled || completion.StopReason != subagent.StopCancelled {
			t.Fatalf("cancel completion=%#v err=%v", completion, err)
		}
		waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := consumer.WaitForKind(waitCtx, subagent.EventCancellation); err != nil {
			t.Fatalf("cancellation event: %v", err)
		}
	})
}

func drainSubagentProviderStream(t *testing.T, fixture *ScriptedSubagentProvider, ctx context.Context, request provider.ChatRequest) []provider.StreamEvent {
	t.Helper()
	stream, err := fixture.StreamChat(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var result []provider.StreamEvent
	for event := range stream.Events() {
		result = append(result, event)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	return result
}

func newSubagentFixtureManager(
	t *testing.T,
	runner subagent.RunnerFactory,
	clock func() time.Time,
	generateID func() (subagent.ID, error),
	autoBackgroundAfter time.Duration,
) (subagent.Service, subagent.ResultInbox) {
	t.Helper()
	limits := subagent.DefaultLimits()
	limits.MaxConcurrent = 1
	limits.MaxQueued = 2
	limits.MaxRetainedTasks = 8
	limits.MaxTaskTombstones = 8
	limits.MaxGlobalEvents = 128
	limits.MaxEventsPerTask = 32
	limits.MaxSubscriberBuffer = 32
	limits.AutoBackgroundAfter = autoBackgroundAfter
	inbox, err := subagent.NewResultInbox(subagent.ResultInboxOptions{Limits: limits, IDGenerator: generateID})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := subagent.NewManager(subagent.ManagerOptions{
		Runner: runner, Limits: limits, Inbox: inbox, Redactor: redact.NewRuntimeRedactor(),
		Clock: clock, IDGenerator: generateID, ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	return manager, inbox
}

func newSubagentFixtureConsumer(t *testing.T, service subagent.Service) *SubagentEventConsumer {
	t.Helper()
	consumer, err := NewSubagentEventConsumer(context.Background(), service, SubagentEventConsumerOptions{MaxEvents: 128})
	if err != nil {
		t.Fatal(err)
	}
	return consumer
}

func subagentFixtureInput(placement subagent.PlacementIntent) subagent.SubmitInput {
	return subagent.SubmitInput{
		Task: "fixture task", Type: subagent.TypeDefined, Role: "fixture", Placement: placement, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: "conversation-fixture"},
	}
}

func strictlyIncreasingSubagentFixtureEvents(values []subagent.Event) bool {
	for index := 1; index < len(values); index++ {
		if values[index].Revision <= values[index-1].Revision {
			return false
		}
	}
	return true
}
