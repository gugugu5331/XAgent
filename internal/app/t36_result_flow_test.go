package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/orchestrator"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/testutil"
	"xagent/internal/tui"
)

func TestT36ResultFlowKeepsOneCompletionThroughEventsPersistenceReleaseAndAck(t *testing.T) {
	const (
		conversationID = "conversation-t36"
		secret         = "t36-sensitive-canary"
	)
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(secret)
	clock := testutil.NewManualSubagentClock(time.Date(2026, time.August, 15, 14, 0, 0, 0, time.UTC))
	ids := testutil.NewScriptedSubagentIDGenerator(
		testutil.SubagentIDResult{ID: "task-t36"},
		testutil.SubagentIDResult{ID: "notification-t36"},
		testutil.SubagentIDResult{ID: "claim-t36-released"},
		testutil.SubagentIDResult{ID: "claim-t36-acked"},
	)
	confirmationGate := make(chan struct{})
	var resolveOnce sync.Once
	confirmation := events.ToolConfirmationRequest{
		ConfirmationID: "confirmation-t36", CallID: "call-t36", Name: "Write",
		Arguments: redactor.Redact(`{"path":"safe.txt"}`), Prompt: redactor.Redact("allow once"),
		Scopes: []events.ConfirmationScopeDisplay{{Scope: "once", Available: true, Description: redactor.Redact("this call")}},
	}
	wantUsage := subagent.Usage{
		InputTokens: 13, OutputTokens: 5, CacheCreationInputTokens: 3, CacheReadInputTokens: 2,
	}
	wantSummary := redactor.Redact("provider failed after safe partial result: " + secret)
	wantError := subagent.SafeError(
		subagent.ErrProviderFailed,
		redactor.Redact("provider failure: "+secret),
		true,
	)
	runner := testutil.NewScriptedSubagentRunner(clock.Now, testutil.SubagentRunStep{
		Metadata: subagent.PreparedMetadata{
			Type: subagent.TypeDefined, Role: "fixture", RoleSource: agentrole.SourceProject,
			RoleSourceID: "project-fixture", MaxIterations: 3,
		},
		BeforeWait: []subagent.AgentEvent{
			{
				Kind:    events.TextDelta,
				Payload: events.Event{Type: events.TextDelta, Text: redactor.Redact("visible delta " + secret)},
				Range:   &subagent.DeltaRange{From: 0, To: int64(len("visible delta [redacted]"))},
			},
			{
				Kind:    events.ThinkingDelta,
				Payload: events.Event{Type: events.ThinkingDelta, Text: redactor.Redact("thinking " + secret)},
				Range:   &subagent.DeltaRange{From: 0, To: int64(len("thinking [redacted]"))},
			},
			{
				Kind: events.ToolPending,
				Payload: events.Event{Type: events.ToolPending, Tool: &events.ToolDisplay{
					CallID: confirmation.CallID, Name: confirmation.Name, Status: events.ToolDisplayPending,
				}},
			},
			{Kind: events.ToolWaitingConfirmation, Payload: events.Event{Type: events.ToolWaitingConfirmation, Confirmation: &confirmation}},
		},
		Wait: confirmationGate,
		AfterWait: []subagent.AgentEvent{
			{
				Kind: events.ToolRunning,
				Payload: events.Event{Type: events.ToolRunning, Tool: &events.ToolDisplay{
					CallID: confirmation.CallID, Name: confirmation.Name, Status: events.ToolDisplayRunning,
				}},
			},
			{
				Kind: events.ToolSuccess,
				Payload: events.Event{Type: events.ToolSuccess, Tool: &events.ToolDisplay{
					CallID: confirmation.CallID, Name: confirmation.Name, Status: events.ToolDisplaySuccess,
					Summary: redactor.Redact("safe tool result " + secret),
				}},
			},
			{
				Kind:    events.AgentProgressed,
				Payload: events.Event{Type: events.AgentProgressed, Progress: &events.AgentProgress{Iteration: 2, Max: 3}},
			},
			{
				Kind: events.UsageUpdated,
				Payload: events.Event{Type: events.UsageUpdated, Usage: &events.UsageDisplay{
					InputTokens: wantUsage.InputTokens, OutputTokens: wantUsage.OutputTokens,
					CacheCreationInputTokens: wantUsage.CacheCreationInputTokens,
					CacheReadInputTokens:     wantUsage.CacheReadInputTokens,
				}},
			},
		},
		ResolveConfirmation: func(decision events.ToolConfirmationDecision) error {
			if decision.ConfirmationID != confirmation.ConfirmationID || decision.CallID != confirmation.CallID ||
				decision.Action != events.PermissionAllowOnce || !decision.Allowed {
				return errors.New("unexpected confirmation decision")
			}
			resolveOnce.Do(func() { close(confirmationGate) })
			return nil
		},
		Completion: subagent.Completion{
			Status: subagent.StatusFailed, Summary: wantSummary, StopReason: subagent.StopProviderError,
			Usage: wantUsage, Error: wantError,
		},
	})

	limits := subagent.DefaultLimits()
	limits.MaxConcurrent = 1
	limits.MaxQueued = 2
	limits.MaxRetainedTasks = 8
	limits.MaxTaskTombstones = 8
	limits.MaxGlobalEvents = 128
	limits.MaxEventsPerTask = 64
	limits.MaxSubscriberBuffer = 64
	limits.AutoBackgroundAfter = time.Hour
	inbox, err := subagent.NewResultInbox(subagent.ResultInboxOptions{Limits: limits, IDGenerator: ids.Generate})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := subagent.NewManager(subagent.ManagerOptions{
		Runner: runner, Limits: limits, Inbox: inbox, Redactor: redactor,
		Clock: clock.Now, IDGenerator: ids.Generate, ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	consumer, err := testutil.NewSubagentEventConsumer(context.Background(), manager, testutil.SubagentEventConsumerOptions{MaxEvents: 128})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(consumer.Close)

	parentCtx, cancelParent := context.WithCancel(context.Background())
	submission, err := manager.Submit(parentCtx, subagent.SubmitInput{
		Task: "exercise the complete result flow", Type: subagent.TypeDefined, Role: "fixture",
		Placement: subagent.PlacementForeground, Origin: subagent.OriginTUI,
		Parent: subagent.ParentRef{ConversationID: conversationID},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWait()
	requested, err := consumer.WaitForKind(waitCtx, subagent.EventConfirmationRequested)
	if err != nil || requested.Agent == nil || requested.Agent.Payload.Confirmation == nil {
		t.Fatalf("confirmation event = %#v, error = %v", requested, err)
	}
	if err := manager.MoveToBackground(context.Background(), submission.ID); err != nil {
		t.Fatal(err)
	}
	cancelParent()
	outcome, err := manager.AwaitForeground(context.Background(), submission.ID)
	if err != nil || !outcome.Detached || outcome.Completion != nil {
		t.Fatalf("foreground detach outcome = %#v, error = %v", outcome, err)
	}
	if err := manager.ResolveConfirmation(context.Background(), submission.ID, events.ToolConfirmationDecision{
		ConfirmationID: confirmation.ConfirmationID, CallID: confirmation.CallID,
		Action: events.PermissionAllowOnce, Allowed: true,
	}); err != nil {
		t.Fatal(err)
	}
	completion, err := manager.Await(context.Background(), submission.ID)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := consumer.WaitForKind(waitCtx, subagent.EventFailure)
	if err != nil {
		t.Fatal(err)
	}
	resultEvent, err := consumer.WaitForKind(waitCtx, subagent.EventResultPublished)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := manager.Get(context.Background(), submission.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertT36CompletionProjection(t, completion, detail.Task, terminal, resultEvent)
	assertT36MachineEventStream(t, consumer.Events(), submission.ID)

	active := conversation.NewConversation(conversationID, clock.Now().Add(-time.Minute))
	store := &taskNotificationStoreFake{err: errors.New("save failed: " + secret)}
	model := Model{
		deps: Deps{Store: store, RuntimeRedactor: redactor}, conversation: active,
		messages: tui.NewMessagesView(false),
	}
	firstErr := model.ApplyTaskEvent(context.Background(), resultEvent)
	if firstErr == nil || strings.Contains(firstErr.Error(), secret) || len(active.Messages) != 0 {
		t.Fatalf("failed notification commit = error:%v messages:%#v", firstErr, active.Messages)
	}
	store.err = nil
	if err := model.ApplyTaskEvent(context.Background(), resultEvent); err != nil {
		t.Fatalf("notification retry: %v", err)
	}
	if err := model.ApplyTaskEvent(context.Background(), resultEvent); err != nil {
		t.Fatalf("notification replay: %v", err)
	}
	if len(store.saves) != 1 || len(active.Messages) != 1 || active.Messages[0].Subagent == nil {
		t.Fatalf("notification dedup persistence = saves:%d messages:%#v", len(store.saves), active.Messages)
	}
	persisted := active.Messages[0].Subagent
	if persisted.NotificationID != resultEvent.Result.NotificationID || persisted.TaskID != string(completion.ID) ||
		persisted.Status != string(completion.Status) || persisted.Summary.Text() != completion.Summary.Text() ||
		persisted.StopReason != string(completion.StopReason) {
		t.Fatalf("persisted notification diverged from completion: notification=%#v completion=%#v", persisted, completion)
	}
	if strings.Contains(model.messages.View(), secret) || strings.Contains(persisted.Summary.Text(), secret) ||
		len(conversation.ContextMessages(active)) != 0 {
		t.Fatalf("notification crossed sensitive/Provider context boundary: view=%q message=%#v", model.messages.View(), active.Messages[0])
	}

	projector, err := orchestrator.NewResultProjector(orchestrator.ResultProjectorOptions{
		Service: manager, Budgeter: contextmgr.NewRequestBudgeter(), RuntimeRedactor: redactor,
		MaxRequestPlanningTokens: 100_000, MaxResultBytes: limits.MaxResultBytes,
		MaxResultsPerClaim: limits.MaxResultsPerClaim,
	})
	if err != nil {
		t.Fatal(err)
	}
	providerFixture := testutil.NewScriptedSubagentProvider(
		testutil.SubagentProviderStep{StartErr: errors.New("provider start failed")},
		testutil.SubagentProviderStep{Events: []provider.StreamEvent{{Type: provider.StreamEventDone}}},
	)
	if got := len(providerFixture.Requests()); got != 0 {
		t.Fatalf("task completion implicitly started %d Provider requests", got)
	}
	base := provider.ChatRequest{
		Model:    "model-main",
		Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleUser, Content: redactor.Redact("next user request")}},
	}
	firstOwner := subagent.ParentRef{ConversationID: conversationID, ExecutionID: "execution-release", RequestGeneration: 2}
	if _, err := projector.StreamChat(context.Background(), firstOwner, base, providerFixture.StreamChat); err == nil {
		t.Fatal("Provider start failure unexpectedly acknowledged the result")
	}
	secondOwner := subagent.ParentRef{ConversationID: conversationID, ExecutionID: "execution-ack", RequestGeneration: 3}
	stream, err := projector.StreamChat(context.Background(), secondOwner, base, providerFixture.StreamChat)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := providerFixture.Requests()
	if len(requests) != 2 {
		t.Fatalf("Release/retry Provider requests = %d, want 2", len(requests))
	}
	for index := range requests {
		assertT36ProviderResultProjection(t, requests[index], completion, conversationID, secret)
	}
	empty, err := manager.ClaimResults(context.Background(), subagent.ResultClaimOptions{
		Owner:            subagent.ParentRef{ConversationID: conversationID, ExecutionID: "execution-after-ack", RequestGeneration: 4},
		MaxNotifications: 1, MaxBytes: limits.MaxResultBytes,
	})
	if err != nil || empty.ClaimID != "" || len(empty.Notifications) != 0 {
		t.Fatalf("accepted result was not acknowledged exactly once: claim=%#v error=%v", empty, err)
	}
	if ids.Calls() != 4 {
		t.Fatalf("task/notification/released claim/acked claim ID calls = %d, want 4", ids.Calls())
	}
}

func assertT36CompletionProjection(
	t *testing.T,
	completion subagent.Completion,
	snapshot subagent.TaskSnapshot,
	terminal subagent.Event,
	result subagent.Event,
) {
	t.Helper()
	if terminal.Completion == nil || terminal.Snapshot == nil || result.Result == nil {
		t.Fatalf("terminal/result payloads are unavailable: terminal=%#v result=%#v", terminal, result)
	}
	for name, projection := range map[string]struct {
		status     subagent.Status
		summary    string
		stopReason subagent.StopReason
		usage      subagent.Usage
		errorCode  string
	}{
		"snapshot": {snapshot.Status, snapshot.Summary.Text(), snapshot.StopReason, snapshot.Usage, snapshot.Error.Code},
		"terminal": {terminal.Completion.Status, terminal.Completion.Summary.Text(), terminal.Completion.StopReason, terminal.Completion.Usage, terminal.Completion.Error.Code},
		"result":   {result.Result.Status, result.Result.Summary.Text(), result.Result.StopReason, result.Result.Usage, result.Result.Error.Code},
	} {
		if projection.status != completion.Status || projection.summary != completion.Summary.Text() ||
			projection.stopReason != completion.StopReason || projection.usage != completion.Usage ||
			completion.Error == nil || projection.errorCode != completion.Error.Code {
			t.Errorf("%s projection diverged from completion: %#v completion=%#v", name, projection, completion)
		}
	}
	if terminal.Revision != result.Result.CompletionRevision || terminal.Sequence != result.Result.CompletionSequence ||
		terminal.Revision >= result.Revision || terminal.Sequence >= result.Sequence {
		t.Errorf("terminal/result ordering diverged: terminal=%#v result=%#v", terminal, result)
	}
}

func assertT36MachineEventStream(t *testing.T, values []subagent.Event, taskID subagent.ID) {
	t.Helper()
	wanted := map[subagent.EventKind]bool{
		subagent.EventQueued: false, subagent.EventRunning: false,
		subagent.EventTextDelta: false, subagent.EventThinkingDelta: false, subagent.EventTool: false,
		subagent.EventWaitingConfirmation: false, subagent.EventConfirmationRequested: false,
		subagent.EventConfirmationResolved: false, subagent.EventPlacementChanged: false,
		subagent.EventProgress: false, subagent.EventUsage: false,
		subagent.EventFailure: false, subagent.EventResultPublished: false,
	}
	terminalCount := 0
	for index := range values {
		event := values[index]
		if err := event.ValidateOneOf(); err != nil {
			t.Errorf("event[%d] violates one-of: %#v error=%v", index, event, err)
		}
		if event.TaskID != taskID || event.Revision == 0 || event.Sequence == 0 || event.At.IsZero() {
			t.Errorf("event[%d] missing identity/order: %#v", index, event)
		}
		if index > 0 && (event.Revision <= values[index-1].Revision || event.Sequence <= values[index-1].Sequence) {
			t.Errorf("event stream is not strictly ordered at %d: previous=%#v current=%#v", index, values[index-1], event)
		}
		if _, exists := wanted[event.Kind]; exists {
			wanted[event.Kind] = true
		}
		switch event.Kind {
		case subagent.EventCompletion, subagent.EventFailure, subagent.EventCancellation, subagent.EventTimeout, subagent.EventLimit:
			terminalCount++
		}
	}
	for kind, seen := range wanted {
		if !seen {
			t.Errorf("machine event stream did not include %s: %#v", kind, values)
		}
	}
	if terminalCount != 1 {
		t.Errorf("machine event terminal count = %d, want 1", terminalCount)
	}
}

func assertT36ProviderResultProjection(
	t *testing.T,
	request provider.ChatRequest,
	completion subagent.Completion,
	conversationID string,
	secret string,
) {
	t.Helper()
	if len(request.Messages) != 2 || request.Messages[1].Role != provider.ModelMessageRoleSubagentResult {
		t.Fatalf("Provider result request messages = %#v", request.Messages)
	}
	result := request.Messages[1]
	if result.ToolCallID != "" || result.ToolName != "" || result.ArgumentsJSON.Text() != "" || result.ToolResult.Text() != "" {
		t.Errorf("subagent_result reused tool-call fields: %#v", result)
	}
	raw := result.Content.Text()
	if strings.Contains(raw, secret) || strings.Contains(raw, conversationID) || strings.Contains(raw, "confirmation-t36") {
		t.Errorf("Provider result leaked internal/sensitive fields: %s", raw)
	}
	var payload struct {
		SchemaVersion int    `json:"schema_version"`
		TaskID        string `json:"task_id"`
		Status        string `json:"status"`
		Summary       string `json:"summary"`
		StopReason    string `json:"stop_reason"`
		Usage         struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
		Error *struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			Recoverable bool   `json:"recoverable"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("decode fixed result schema: %v payload=%s", err, raw)
	}
	if payload.SchemaVersion != 1 || payload.TaskID != string(completion.ID) || payload.Status != string(completion.Status) ||
		payload.Summary != completion.Summary.Text() || payload.StopReason != string(completion.StopReason) ||
		payload.Usage.InputTokens != completion.Usage.InputTokens || payload.Usage.OutputTokens != completion.Usage.OutputTokens ||
		payload.Usage.CacheCreationInputTokens != completion.Usage.CacheCreationInputTokens ||
		payload.Usage.CacheReadInputTokens != completion.Usage.CacheReadInputTokens ||
		payload.Error == nil || completion.Error == nil ||
		payload.Error.Code != completion.Error.Code || payload.Error.Message != completion.Error.Message.Text() ||
		payload.Error.Recoverable != completion.Error.Recoverable {
		t.Errorf("Provider fixed result schema diverged from completion: payload=%#v completion=%#v", payload, completion)
	}
}
