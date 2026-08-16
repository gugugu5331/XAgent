package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
)

func TestManagerReservesBeforePrepareAndReleasesPrepareFailure(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	inbox := newManagerTestInbox()
	inbox.onReserve = func(ID) {
		orderMu.Lock()
		order = append(order, "reserve")
		orderMu.Unlock()
	}
	inbox.onRelease = func(ID) {
		orderMu.Lock()
		order = append(order, "release")
		orderMu.Unlock()
	}
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, _ ID, _ SubmitInput) (PreparedTask, error) {
		orderMu.Lock()
		order = append(order, "prepare")
		orderMu.Unlock()
		return nil, SafeError(ErrUnknownRole, managerTestSafe("unknown role"), true)
	}}
	manager := newManagerUnderTest(t, managerTestOptions(factory, inbox, managerIDSequence("task-prepare")))

	if _, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementForeground)); managerErrorCode(err) != ErrUnknownRole {
		t.Fatalf("prepare failure code=%q error=%v", managerErrorCode(err), err)
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if fmt.Sprint(gotOrder) != fmt.Sprint([]string{"reserve", "prepare", "release"}) {
		t.Fatalf("submission boundary order=%v", gotOrder)
	}
	listed, err := manager.List(context.Background())
	if err != nil || len(listed.Tasks) != 0 {
		t.Fatalf("failed preparation created a task: snapshot=%#v error=%v", listed, err)
	}
}

func TestManagerCompletionIsSingleProjectionSource(t *testing.T) {
	release := make(chan struct{})
	inbox := newManagerTestInbox()
	redactor := redact.NewRuntimeRedactor()
	want := Completion{
		Status:           StatusLimitReached,
		Summary:          redactor.Redact("bounded summary"),
		SummaryTruncated: true,
		TruncationReason: redactor.Redact("max_result_bytes"),
		StopReason:       StopMaxIterations,
		Usage:            Usage{InputTokens: 11, OutputTokens: 7, CacheCreationInputTokens: 3, CacheReadInputTokens: 2},
		Error:            SafeError(ErrLimitReached, redactor.Redact("iteration limit"), true),
		Workspace: WorkspaceSummary{
			WorkspaceID: strings.Repeat("a", 32), Isolation: "worktree", State: "retained",
			BaseOID: strings.Repeat("b", 40), Branch: "xagent/worktree/" + strings.Repeat("a", 32),
			Dirty: true, Cleanup: "retained", RetentionCause: "dirty_worktree",
		},
	}
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		return &managerTestPreparedTask{run: func(context.Context, EventSink) Completion {
			<-release
			completion := want.Clone()
			completion.ID = id
			completion.EndedAt = time.Now().UTC()
			return completion
		}}, nil
	}}
	manager := newManagerUnderTest(t, managerTestOptions(factory, inbox, managerIDSequence("task-completion", "notification-completion")))
	submission, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementBackground))
	if err != nil {
		t.Fatal(err)
	}
	streamContext, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	stream, err := manager.Subscribe(streamContext, 0)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	completion, err := manager.Await(context.Background(), submission.ID)
	if err != nil {
		t.Fatal(err)
	}
	terminal, published := waitManagerTerminalAndResultEvents(t, stream)
	detail, err := manager.Get(context.Background(), submission.ID)
	if err != nil {
		t.Fatal(err)
	}
	notification := inbox.waitPublished(t)
	if notification.Workspace != want.Workspace {
		t.Fatalf("notification workspace diverged from settled completion: got %#v want %#v", notification.Workspace, want.Workspace)
	}

	assertManagerCompletionProjection(t, completion, detail.Task, *terminal.Completion, notification)
	if terminal.Snapshot == nil || terminal.Snapshot.Revision != terminal.Revision || notification.CompletionRevision != terminal.Revision || notification.CompletionSequence != terminal.Sequence {
		t.Fatalf("terminal revision projection diverged: event=%#v notification=%#v", terminal, notification)
	}
	if published.Result == nil || published.Result.CompletionRevision != terminal.Revision || published.Result.NotificationID != notification.NotificationID {
		t.Fatalf("result-published event diverged from inbox notification: %#v", published)
	}

	completion.Error.Code = "mutated"
	if terminal.Completion.Error != nil {
		terminal.Completion.Error.Code = "mutated"
	}
	notification.Error.Code = "mutated"
	stable, err := manager.Get(context.Background(), submission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stable.Task.Error == nil || stable.Task.Error.Code != string(ErrLimitReached) || stable.Task.Summary.Text() != "bounded summary" {
		t.Fatalf("returned completion projections aliased manager state: %#v", stable.Task)
	}
	if err := manager.Cancel(context.Background(), submission.ID); managerErrorCode(err) != ErrTaskTerminal {
		t.Fatalf("terminal task cancellation error=%v", err)
	}
}

func TestManagerRetriesResultPublicationWithoutRegeneratingCompletionOrDiagnostic(t *testing.T) {
	const publishSecret = "raw-inbox-publish-secret"
	release := make(chan struct{})
	inbox := newManagerTestInbox()
	inbox.setPublishError(errors.New("temporary publication failure: " + publishSecret))
	redactor := redact.NewRuntimeRedactor()
	want := Completion{
		Status:     StatusCompleted,
		Summary:    redactor.Redact("stable completion"),
		StopReason: StopCompleted,
		Usage:      Usage{InputTokens: 17, OutputTokens: 9, CacheReadInputTokens: 4},
	}
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		return &managerTestPreparedTask{run: func(context.Context, EventSink) Completion {
			<-release
			completion := want.Clone()
			completion.ID = id
			completion.EndedAt = time.Now().UTC()
			return completion
		}}, nil
	}}
	manager := newManagerUnderTest(t, managerTestOptions(factory, inbox, managerIDSequence("task-retry", "notification-retry")))
	submission, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementBackground))
	if err != nil {
		t.Fatal(err)
	}
	streamContext, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	stream, err := manager.Subscribe(streamContext, 0)
	if err != nil {
		t.Fatal(err)
	}
	close(release)

	// Keep publication failing across multiple retry ticks. The terminal
	// Completion and its NotificationID must remain frozen, and only one safe
	// diagnostic may be emitted for the whole retry episode.
	inbox.waitPublishAttempts(t, 3)
	inbox.setPublishError(nil)
	notification := inbox.waitPublished(t)
	completion, err := manager.Await(context.Background(), submission.ID)
	if err != nil {
		t.Fatal(err)
	}

	var (
		terminal        Event
		published       Event
		diagnosticCount int
	)
	deadline := time.After(2 * time.Second)
	for published.Result == nil {
		select {
		case event, ok := <-stream:
			if !ok {
				t.Fatal("manager event stream closed before retry succeeded")
			}
			switch {
			case terminalEventKind(event.Kind):
				terminal = event
			case event.Kind == EventDiagnostic:
				diagnosticCount++
				if event.Agent == nil || event.Agent.Payload.Diagnostic == nil ||
					event.Agent.Payload.Diagnostic.Code != string(ErrResultPublishFailed) ||
					strings.Contains(event.Agent.Payload.Diagnostic.Message.Text(), publishSecret) {
					t.Fatalf("unsafe result publication diagnostic: %#v", event)
				}
			case event.Kind == EventResultPublished:
				published = event
			}
		case <-deadline:
			t.Fatal("timed out waiting for retried result publication event")
		}
	}
	if diagnosticCount != 1 {
		t.Fatalf("result publication diagnostics = %d, want 1", diagnosticCount)
	}
	if terminal.Completion == nil || published.Result.NotificationID != notification.NotificationID ||
		notification.NotificationID != "notification-retry" {
		t.Fatalf("retry regenerated terminal projection: terminal=%#v published=%#v notification=%#v", terminal, published, notification)
	}
	detail, err := manager.Get(context.Background(), submission.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertManagerCompletionProjection(t, completion, detail.Task, *terminal.Completion, notification)
}

func TestManagerMapsPanicAndInvalidCompletionToInternalExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(ID) Completion
	}{
		{name: "panic", run: func(ID) Completion { panic("provider secret must not escape") }},
		{name: "invalid", run: func(id ID) Completion {
			return Completion{ID: id, Status: StatusCompleted, StopReason: StopProviderError, EndedAt: time.Now().UTC()}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			inbox := newManagerTestInbox()
			factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
				return &managerTestPreparedTask{run: func(context.Context, EventSink) Completion { return test.run(id) }}, nil
			}}
			manager := newManagerUnderTest(t, managerTestOptions(factory, inbox, managerIDSequence(ID("task-"+test.name), ID("notification-"+test.name))))
			submission, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementBackground))
			if err != nil {
				t.Fatal(err)
			}
			completion, err := manager.Await(context.Background(), submission.ID)
			if err != nil {
				t.Fatal(err)
			}
			if completion.Status != StatusFailed || completion.StopReason != StopInternalError || completion.Error == nil || completion.Error.Code != string(ErrInternal) || completion.Error.Source != "subagent" {
				t.Fatalf("unsafe runner outcome was not mapped to internal: %#v", completion)
			}
			detail, err := manager.Get(context.Background(), submission.ID)
			if err != nil {
				t.Fatal(err)
			}
			terminalCount := 0
			for _, event := range detail.RecentEvents {
				if terminalEventKind(event.Kind) {
					terminalCount++
				}
			}
			if terminalCount != 1 {
				t.Fatalf("runner failure produced %d terminal events: %#v", terminalCount, detail.RecentEvents)
			}
		})
	}
}

func TestManagerEventReducerOwnsConfirmationProgressAndMonotonicUsage(t *testing.T) {
	waitingSent := make(chan struct{})
	resolved := make(chan struct{})
	invalidUsage := make(chan error, 1)
	redactor := redact.NewRuntimeRedactor()
	request := events.ToolConfirmationRequest{
		ConfirmationID: "confirmation-manager",
		CallID:         "call-manager",
		Name:           "Write",
		Arguments:      redactor.Redact(`{"path":"safe.txt"}`),
		Scopes:         []events.ConfirmationScopeDisplay{{Scope: "once", Available: true, Description: redactor.Redact("once")}},
	}
	var controlled *managerTestControlledTask
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		base := &managerTestPreparedTask{metadata: PreparedMetadata{Type: TypeDefined, MaxIterations: 5}}
		controlled = &managerTestControlledTask{managerTestPreparedTask: base}
		controlled.resolve = func(decision events.ToolConfirmationDecision) error {
			if decision.ConfirmationID != request.ConfirmationID || decision.CallID != request.CallID {
				return errors.New("wrong confirmation")
			}
			close(resolved)
			return nil
		}
		base.run = func(_ context.Context, sink EventSink) Completion {
			if err := sink(AgentEvent{Kind: events.ToolWaitingConfirmation, Payload: events.Event{Type: events.ToolWaitingConfirmation, Confirmation: &request}}); err != nil {
				return managerTestInternalCompletion(id, "waiting event failed")
			}
			close(waitingSent)
			<-resolved
			_ = sink(AgentEvent{Kind: events.ToolRunning, Payload: events.Event{Type: events.ToolRunning, Tool: &events.ToolDisplay{CallID: request.CallID, Name: request.Name}}})
			_ = sink(AgentEvent{Kind: events.AgentProgressed, Payload: events.Event{Type: events.AgentProgressed, Progress: &events.AgentProgress{Iteration: 2, Max: 5}}})
			_ = sink(AgentEvent{Kind: events.UsageUpdated, Payload: events.Event{Type: events.UsageUpdated, Usage: &events.UsageDisplay{InputTokens: 10, OutputTokens: 4}}})
			invalidUsage <- sink(AgentEvent{Kind: events.UsageUpdated, Payload: events.Event{Type: events.UsageUpdated, Usage: &events.UsageDisplay{InputTokens: 9, OutputTokens: 4}}})
			return Completion{ID: id, Status: StatusCompleted, Summary: redactor.Redact("done"), StopReason: StopCompleted, Usage: Usage{InputTokens: 10, OutputTokens: 4}, EndedAt: time.Now().UTC()}
		}
		return controlled, nil
	}}
	manager := newManagerUnderTest(t, managerTestOptions(factory, newManagerTestInbox(), managerIDSequence("task-reducer")))
	submission, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementForeground))
	if err != nil {
		t.Fatal(err)
	}
	waitManagerSignal(t, waitingSent, "waiting confirmation event")
	waitManagerStatus(t, manager, submission.ID, StatusWaitingConfirmation)
	detail, err := manager.Get(context.Background(), submission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.PendingConfirmation == nil || detail.Task.PendingConfirmation.CallID != request.CallID {
		t.Fatalf("reducer did not retain pending confirmation: %#v", detail.Task)
	}
	decision := events.ToolConfirmationDecision{ConfirmationID: request.ConfirmationID, CallID: request.CallID, Action: events.PermissionAllowOnce, Allowed: true}
	if err := manager.ResolveConfirmation(context.Background(), submission.ID, decision); err != nil {
		t.Fatal(err)
	}
	outcome, err := manager.AwaitForeground(context.Background(), submission.ID)
	if err != nil || outcome.Completion == nil {
		t.Fatalf("foreground outcome=%#v error=%v", outcome, err)
	}
	completion := outcome.Completion.Clone()
	if err := <-invalidUsage; managerErrorCode(err) != ErrInvalidTransition {
		t.Fatalf("non-monotonic usage error=%v", err)
	}
	if completion.Usage != (Usage{InputTokens: 10, OutputTokens: 4}) {
		t.Fatalf("completion usage changed: %#v", completion.Usage)
	}
	detail, err = manager.Get(context.Background(), submission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != StatusCompleted || detail.Task.PendingConfirmation != nil || detail.Task.Iteration != 2 || detail.Task.MaxIterations != 5 || detail.Task.Usage != completion.Usage {
		t.Fatalf("reducer state diverged from terminal completion: %#v", detail.Task)
	}
	if controlled.resolveCalls.Load() != 1 {
		t.Fatalf("confirmation controller called %d times", controlled.resolveCalls.Load())
	}
}

func TestManagerListGetStableTerminalEvictionAndTombstones(t *testing.T) {
	limits := managerTestLimits()
	limits.MaxConcurrent = 1
	limits.MaxQueued = 1
	limits.MaxRetainedTasks = 2
	limits.MaxTaskTombstones = 1
	baseTime := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	var runIndex atomic.Int32
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		return &managerTestPreparedTask{run: func(context.Context, EventSink) Completion {
			index := runIndex.Add(1)
			return Completion{ID: id, Status: StatusCompleted, Summary: managerTestSafe(string(id)), StopReason: StopCompleted, EndedAt: baseTime.Add(time.Duration(index) * time.Minute)}
		}}, nil
	}}
	manager := newManagerUnderTest(t, ManagerOptions{
		Runner: factory, Limits: limits, Inbox: newManagerTestInbox(), Redactor: redact.NewRuntimeRedactor(),
		Clock: func() time.Time { return baseTime }, IDGenerator: managerCountingIDGenerator(), ShutdownTimeout: time.Second,
	})
	var ids []ID
	for range 4 {
		submission, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementBackground))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, submission.ID)
		if _, err := manager.Await(context.Background(), submission.ID); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tasks) != 2 || listed.Tasks[0].ID != ids[3] || listed.Tasks[1].ID != ids[2] || listed.Watermark == 0 {
		t.Fatalf("retained terminal order is unstable: %#v", listed)
	}
	if _, err := manager.Get(context.Background(), ids[1]); managerErrorCode(err) != ErrTaskExpired {
		t.Fatalf("newest tombstone error=%v", err)
	}
	if _, err := manager.Get(context.Background(), ids[0]); managerErrorCode(err) != ErrTaskNotFound {
		t.Fatalf("expired tombstone did not leave bounded history: %v", err)
	}
	if _, err := manager.Get(context.Background(), "never-seen"); managerErrorCode(err) != ErrTaskNotFound {
		t.Fatalf("unknown task error=%v", err)
	}
}

func assertManagerCompletionProjection(t *testing.T, completion Completion, snapshot TaskSnapshot, terminal Completion, notification ResultNotification) {
	t.Helper()
	for label, candidate := range map[string]Completion{
		"snapshot": {
			ID: snapshot.ID, Status: snapshot.Status, Summary: snapshot.Summary, SummaryTruncated: snapshot.SummaryTruncated,
			TruncationReason: snapshot.TruncationReason, StopReason: snapshot.StopReason, Usage: snapshot.Usage, Error: snapshot.Error, EndedAt: valueOrZero(snapshot.EndedAt),
		},
		"terminal": terminal,
		"notification": {
			ID: notification.TaskID, Status: notification.Status, Summary: notification.Summary, SummaryTruncated: notification.SummaryTruncated,
			TruncationReason: notification.TruncationReason, StopReason: notification.StopReason, Usage: notification.Usage, Error: notification.Error, EndedAt: notification.CreatedAt,
		},
	} {
		if candidate.ID != completion.ID || candidate.Status != completion.Status || candidate.Summary.Text() != completion.Summary.Text() ||
			candidate.SummaryTruncated != completion.SummaryTruncated || candidate.TruncationReason.Text() != completion.TruncationReason.Text() ||
			candidate.StopReason != completion.StopReason || candidate.Usage != completion.Usage || !managerSafeErrorsEqual(candidate.Error, completion.Error) ||
			!candidate.EndedAt.Equal(completion.EndedAt) {
			t.Fatalf("%s projection diverged:\ncompletion=%#v\ncandidate=%#v", label, completion, candidate)
		}
	}
}

func managerSafeErrorsEqual(left, right *diagnostics.SafeError) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Code == right.Code && left.Source == right.Source && left.Message.Text() == right.Message.Text() && left.Recoverable == right.Recoverable
}

func valueOrZero(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

func waitManagerTerminalAndResultEvents(t *testing.T, stream <-chan Event) (Event, Event) {
	t.Helper()
	var terminal Event
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event, ok := <-stream:
			if !ok {
				t.Fatal("manager event stream closed before result publication")
			}
			if terminalEventKind(event.Kind) {
				terminal = event
			}
			if event.Kind == EventResultPublished {
				if terminal.Completion == nil {
					t.Fatal("result event arrived before terminal event")
				}
				return terminal, event
			}
		case <-deadline:
			t.Fatal("timed out waiting for terminal/result events")
		}
	}
}

func managerTestInternalCompletion(id ID, summary string) Completion {
	return Completion{ID: id, Status: StatusFailed, Summary: managerTestSafe(summary), StopReason: StopInternalError, Error: SafeError(ErrInternal, managerTestSafe(summary), false), EndedAt: time.Now().UTC()}
}
