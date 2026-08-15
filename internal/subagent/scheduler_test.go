package subagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"xagent/internal/events"
	"xagent/internal/redact"
)

func TestSchedulerUsesFIFOQueueAndWaitingConfirmationKeepsConcurrencySlot(t *testing.T) {
	limits := managerTestLimits()
	limits.MaxConcurrent = 1
	limits.MaxQueued = 2
	limits.MaxRetainedTasks = 4
	startOrder := make(chan ID, 3)
	releases := make(map[ID]chan struct{})
	var releasesMu sync.Mutex
	redactor := managerTestSafe
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		release := make(chan struct{})
		releasesMu.Lock()
		releases[id] = release
		releasesMu.Unlock()
		return &managerTestPreparedTask{run: func(_ context.Context, sink EventSink) Completion {
			startOrder <- id
			if id == "task-fifo-1" {
				request := events.ToolConfirmationRequest{ConfirmationID: "confirmation-fifo", CallID: "call-fifo", Name: "Write"}
				if err := sink(AgentEvent{Kind: events.ToolWaitingConfirmation, Payload: events.Event{Type: events.ToolWaitingConfirmation, Confirmation: &request}}); err != nil {
					return managerTestInternalCompletion(id, "waiting failed")
				}
				<-release
				_ = sink(AgentEvent{Kind: events.ToolRunning, Payload: events.Event{Type: events.ToolRunning, Tool: &events.ToolDisplay{CallID: request.CallID, Name: request.Name}}})
			} else {
				<-release
			}
			return Completion{ID: id, Status: StatusCompleted, Summary: redactor("done"), StopReason: StopCompleted, EndedAt: time.Now().UTC()}
		}}, nil
	}}
	inbox := newManagerTestInbox()
	manager := newManagerUnderTest(t, ManagerOptions{
		Runner: factory, Limits: limits, Inbox: inbox, Redactor: newManagerRedactor(),
		IDGenerator: managerIDSequence("task-fifo-1", "task-fifo-2", "task-fifo-3", "task-fifo-4"), ShutdownTimeout: time.Second,
	})

	first := mustManagerSubmit(t, manager, managerModelInput(TypeDefined, PlacementForeground))
	if started := waitManagerStart(t, startOrder); started != first.ID {
		t.Fatalf("first start=%s", started)
	}
	waitManagerStatus(t, manager, first.ID, StatusWaitingConfirmation)
	second := mustManagerSubmit(t, manager, managerModelInput(TypeDefined, PlacementForeground))
	third := mustManagerSubmit(t, manager, managerModelInput(TypeDefined, PlacementForeground))
	if _, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementForeground)); managerErrorCode(err) != ErrQueueFull {
		t.Fatalf("full queue error=%v", err)
	}
	if factory.Calls() != 3 {
		t.Fatalf("queue-full submission reached Prepare %d times", factory.Calls())
	}
	select {
	case unexpected := <-startOrder:
		t.Fatalf("waiting_confirmation released the concurrency slot to %s", unexpected)
	case <-time.After(20 * time.Millisecond):
	}

	close(managerReleaseFor(t, &releasesMu, releases, first.ID))
	if outcome, err := manager.AwaitForeground(context.Background(), first.ID); err != nil || outcome.Completion == nil {
		t.Fatalf("first foreground outcome=%#v error=%v", outcome, err)
	}
	if started := waitManagerStart(t, startOrder); started != second.ID {
		t.Fatalf("FIFO second start=%s want=%s", started, second.ID)
	}
	close(managerReleaseFor(t, &releasesMu, releases, second.ID))
	if _, err := manager.AwaitForeground(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	if started := waitManagerStart(t, startOrder); started != third.ID {
		t.Fatalf("FIFO third start=%s want=%s", started, third.ID)
	}
	close(managerReleaseFor(t, &releasesMu, releases, third.ID))
	if _, err := manager.AwaitForeground(context.Background(), third.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerDefaultCapacityAdmitsFourRunsAndThirtyTwoQueuedExactly(t *testing.T) {
	limits := DefaultLimits()
	release := make(chan struct{})
	starts := make(chan ID, limits.MaxConcurrent+limits.MaxQueued)
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		return &managerTestPreparedTask{run: func(ctx context.Context, _ EventSink) Completion {
			starts <- id
			select {
			case <-release:
				return Completion{ID: id, Status: StatusCompleted, Summary: managerTestSafe("done"), StopReason: StopCompleted, EndedAt: time.Now().UTC()}
			case <-ctx.Done():
				return managerCancelledCandidate(id, StopCancelled)
			}
		}}, nil
	}}
	manager := newManagerUnderTest(t, ManagerOptions{
		Runner: factory, Limits: limits, Inbox: newManagerTestInbox(), Redactor: newManagerRedactor(),
		IDGenerator: managerCountingIDGenerator(), ShutdownTimeout: time.Second,
	})

	accepted := make([]Submission, 0, limits.MaxConcurrent+limits.MaxQueued)
	for index := 0; index < limits.MaxConcurrent+limits.MaxQueued; index++ {
		accepted = append(accepted, mustManagerSubmit(t, manager, managerModelInput(TypeDefined, PlacementBackground)))
	}
	for index := 0; index < limits.MaxConcurrent; index++ {
		_ = waitManagerStart(t, starts)
	}
	select {
	case unexpected := <-starts:
		t.Fatalf("scheduler exceeded the four-task running limit with %s", unexpected)
	case <-time.After(20 * time.Millisecond):
	}

	if _, err := manager.Submit(context.Background(), managerModelInput(TypeDefined, PlacementBackground)); managerErrorCode(err) != ErrQueueFull {
		t.Fatalf("submission beyond 4 running + 32 queued returned %v", err)
	}
	if factory.Calls() != len(accepted) {
		t.Fatalf("queue-full submission crossed Prepare: calls=%d accepted=%d", factory.Calls(), len(accepted))
	}
	listed, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	running, queued := 0, 0
	for _, task := range listed.Tasks {
		switch task.Status {
		case StatusRunning:
			running++
		case StatusQueued:
			queued++
		default:
			t.Fatalf("capacity task reached unexpected status %s", task.Status)
		}
	}
	if len(listed.Tasks) != len(accepted) || running != limits.MaxConcurrent || queued != limits.MaxQueued {
		t.Fatalf("capacity snapshot tasks=%d running=%d queued=%d", len(listed.Tasks), running, queued)
	}

	close(release)
	for _, submission := range accepted {
		completion, err := manager.Await(context.Background(), submission.ID)
		if err != nil || completion.Status != StatusCompleted {
			t.Fatalf("task %s completion=%#v error=%v", submission.ID, completion, err)
		}
	}
}

func TestSchedulerTaskDurationStartsAtRunningBoundaryNotWhileQueued(t *testing.T) {
	limits := managerTestLimits()
	limits.MaxConcurrent = 1
	limits.MaxQueued = 1
	limits.MaxRetainedTasks = 2
	limits.MaxTaskDuration = 40 * time.Millisecond
	firstStarted := make(chan struct{})
	secondStarted := make(chan error, 1)
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		return &managerTestPreparedTask{run: func(ctx context.Context, _ EventSink) Completion {
			switch id {
			case "task-timeout-1":
				close(firstStarted)
				<-ctx.Done()
				return Completion{ID: id, Status: StatusTimedOut, Summary: managerTestSafe("timed out"), StopReason: StopTaskTimeout, Error: SafeError(ErrTimedOut, managerTestSafe("timed out"), true), EndedAt: time.Now().UTC()}
			default:
				secondStarted <- ctx.Err()
				return Completion{ID: id, Status: StatusCompleted, Summary: managerTestSafe("done"), StopReason: StopCompleted, EndedAt: time.Now().UTC()}
			}
		}}, nil
	}}
	manager := newManagerUnderTest(t, ManagerOptions{
		Runner: factory, Limits: limits, Inbox: newManagerTestInbox(), Redactor: newManagerRedactor(),
		IDGenerator: managerIDSequence("task-timeout-1", "task-timeout-2", "task-timeout-notification", "task-second-notification"), ShutdownTimeout: time.Second,
	})
	first := mustManagerSubmit(t, manager, managerModelInput(TypeDefined, PlacementBackground))
	waitManagerSignal(t, firstStarted, "first running task")
	second := mustManagerSubmit(t, manager, managerModelInput(TypeDefined, PlacementBackground))
	firstCompletion, err := manager.Await(context.Background(), first.ID)
	if err != nil || firstCompletion.Status != StatusTimedOut {
		t.Fatalf("first completion=%#v error=%v", firstCompletion, err)
	}
	select {
	case startErr := <-secondStarted:
		if startErr != nil {
			t.Fatalf("queued task inherited elapsed duration: %v", startErr)
		}
	case <-time.After(time.Second):
		t.Fatal("queued task did not start after timeout released slot")
	}
	secondCompletion, err := manager.Await(context.Background(), second.ID)
	if err != nil || secondCompletion.Status != StatusCompleted {
		t.Fatalf("second completion=%#v error=%v", secondCompletion, err)
	}
}

func TestPlacementManualDetachStopsParentCancellationWithoutRestart(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	contextCancelled := make(chan struct{}, 1)
	var task *managerTestControlledTask
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		base := &managerTestPreparedTask{run: func(ctx context.Context, _ EventSink) Completion {
			close(started)
			select {
			case <-release:
				return Completion{ID: id, Status: StatusCompleted, Summary: managerTestSafe("done"), StopReason: StopCompleted, EndedAt: time.Now().UTC()}
			case <-ctx.Done():
				contextCancelled <- struct{}{}
				return managerCancelledCandidate(id, StopCancelled)
			}
		}}
		task = &managerTestControlledTask{managerTestPreparedTask: base}
		return task, nil
	}}
	inbox := newManagerTestInbox()
	manager := newManagerUnderTest(t, managerTestOptions(factory, inbox, managerIDSequence("task-manual", "notification-manual")))
	parentCtx, cancelParent := context.WithCancel(context.Background())
	submission := mustManagerSubmitContext(t, manager, parentCtx, managerModelInput(TypeDefined, PlacementForeground))
	waitManagerSignal(t, started, "foreground task start")
	if err := manager.MoveToBackground(context.Background(), submission.ID); err != nil {
		t.Fatal(err)
	}
	outcome, err := manager.AwaitForeground(context.Background(), submission.ID)
	if err != nil || !outcome.Detached || outcome.Completion != nil {
		t.Fatalf("detached foreground outcome=%#v error=%v", outcome, err)
	}
	cancelParent()
	select {
	case <-contextCancelled:
		t.Fatal("parent cancellation reached a detached task")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	completion, err := manager.Await(context.Background(), submission.ID)
	if err != nil || completion.Status != StatusCompleted {
		t.Fatalf("detached completion=%#v error=%v", completion, err)
	}
	if task.moveCalls.Load() != 1 {
		t.Fatalf("manual detach switched runtime %d times", task.moveCalls.Load())
	}
	if _, _, published := inbox.counts(); published != 1 {
		t.Fatalf("detached completion published %d notifications", published)
	}
}

func TestPlacementFailsClosedWithoutRuntimeController(t *testing.T) {
	release := make(chan struct{})
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		return &managerTestPreparedTask{run: func(context.Context, EventSink) Completion {
			<-release
			return Completion{ID: id, Status: StatusCompleted, Summary: managerTestSafe("done"), StopReason: StopCompleted, EndedAt: time.Now().UTC()}
		}}, nil
	}}
	manager := newManagerUnderTest(t, managerTestOptions(factory, newManagerTestInbox(), managerIDSequence("task-no-controller")))
	submission := mustManagerSubmit(t, manager, managerModelInput(TypeDefined, PlacementForeground))
	waitManagerStatus(t, manager, submission.ID, StatusRunning)
	if err := manager.MoveToBackground(context.Background(), submission.ID); managerErrorCode(err) != ErrInvalidTransition {
		t.Fatalf("missing placement controller error=%v", err)
	}
	detail, _ := manager.Get(context.Background(), submission.ID)
	if detail.Task.Placement != Foreground {
		t.Fatalf("failed detach changed snapshot placement: %#v", detail.Task)
	}
	close(release)
	if outcome, err := manager.AwaitForeground(context.Background(), submission.ID); err != nil || outcome.Completion == nil {
		t.Fatalf("foreground completion outcome=%#v error=%v", outcome, err)
	}
}

func TestPlacementForkIsForcedBackgroundAndDoesNotInstallParentBridge(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var controlled *managerTestControlledTask
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, input SubmitInput) (PreparedTask, error) {
		base := &managerTestPreparedTask{metadata: PreparedMetadata{Type: TypeFork}, run: func(ctx context.Context, _ EventSink) Completion {
			close(started)
			select {
			case <-ctx.Done():
				return managerCancelledCandidate(id, StopCancelled)
			case <-release:
				return Completion{ID: id, Status: StatusCompleted, Summary: managerTestSafe("fork done"), StopReason: StopCompleted, EndedAt: time.Now().UTC()}
			}
		}}
		if input.Type != TypeFork {
			return nil, errors.New("expected fork")
		}
		controlled = &managerTestControlledTask{managerTestPreparedTask: base}
		return controlled, nil
	}}
	manager := newManagerUnderTest(t, managerTestOptions(factory, newManagerTestInbox(), managerIDSequence("task-fork", "notification-fork")))
	parentCtx, cancelParent := context.WithCancel(context.Background())
	submission := mustManagerSubmitContext(t, manager, parentCtx, managerModelInput(TypeFork, PlacementForeground))
	if submission.Placement != Background {
		t.Fatalf("fork placement=%q", submission.Placement)
	}
	waitManagerSignal(t, started, "fork task start")
	cancelParent()
	time.Sleep(20 * time.Millisecond)
	detail, err := manager.Get(context.Background(), submission.ID)
	if err != nil || IsTerminal(detail.Task.Status) {
		t.Fatalf("parent cancellation stopped fork task: detail=%#v error=%v", detail, err)
	}
	if controlled.moveCalls.Load() != 0 {
		t.Fatalf("pre-background fork called runtime detach %d times", controlled.moveCalls.Load())
	}
	close(release)
	completion, err := manager.Await(context.Background(), submission.ID)
	if err != nil || completion.Status != StatusCompleted {
		t.Fatalf("fork completion=%#v error=%v", completion, err)
	}
}

func TestPlacementForegroundParentCancellationCancelsBeforeDetach(t *testing.T) {
	started := make(chan struct{})
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		return &managerTestControlledTask{managerTestPreparedTask: &managerTestPreparedTask{run: func(ctx context.Context, _ EventSink) Completion {
			close(started)
			<-ctx.Done()
			return managerCancelledCandidate(id, StopCancelled)
		}}}, nil
	}}
	manager := newManagerUnderTest(t, managerTestOptions(factory, newManagerTestInbox(), managerIDSequence("task-parent-cancel")))
	parentCtx, cancelParent := context.WithCancel(context.Background())
	submission := mustManagerSubmitContext(t, manager, parentCtx, managerModelInput(TypeDefined, PlacementForeground))
	waitManagerSignal(t, started, "parent-bound task start")
	cancelParent()
	completion, err := manager.Await(context.Background(), submission.ID)
	if err != nil || completion.Status != StatusCancelled || completion.StopReason != StopCancelled {
		t.Fatalf("parent cancellation completion=%#v error=%v", completion, err)
	}
	if err := manager.MoveToBackground(context.Background(), submission.ID); managerErrorCode(err) != ErrTaskTerminal {
		t.Fatalf("detach after parent cancellation error=%v", err)
	}
}

func TestPlacementAutomaticTimerCoversOnlyFirstProviderRequest(t *testing.T) {
	for _, test := range []struct {
		name       string
		requestEnd bool
		want       Placement
		moveCalls  int32
	}{
		{name: "request ends first", requestEnd: true, want: Foreground},
		{name: "threshold wins", requestEnd: false, want: Background, moveCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := managerTestLimits()
			limits.AutoBackgroundAfter = 20 * time.Millisecond
			started := make(chan struct{})
			release := make(chan struct{})
			var controlled *managerTestControlledTask
			factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
				base := &managerTestPreparedTask{run: func(ctx context.Context, _ EventSink) Completion {
					requestCtx, finishRequest := context.WithCancel(ctx)
					defer finishRequest()
					if !controlled.notifyFirstProviderRequest(requestCtx) {
						return managerTestInternalCompletion(id, "observer was not installed before Run")
					}
					if test.requestEnd {
						finishRequest()
					}
					close(started)
					<-release
					return Completion{ID: id, Status: StatusCompleted, Summary: managerTestSafe("done"), StopReason: StopCompleted, EndedAt: time.Now().UTC()}
				}}
				controlled = &managerTestControlledTask{managerTestPreparedTask: base}
				return controlled, nil
			}}
			manager := newManagerUnderTest(t, ManagerOptions{
				Runner: factory, Limits: limits, Inbox: newManagerTestInbox(), Redactor: newManagerRedactor(),
				IDGenerator: managerCountingIDGenerator(), ShutdownTimeout: time.Second,
			})
			submission := mustManagerSubmit(t, manager, managerModelInput(TypeDefined, PlacementForeground))
			waitManagerSignal(t, started, "first provider request")
			if test.want == Background {
				waitManagerPlacement(t, manager, submission.ID, Background)
			} else {
				time.Sleep(3 * limits.AutoBackgroundAfter)
			}
			detail, err := manager.Get(context.Background(), submission.ID)
			if err != nil || detail.Task.Placement != test.want {
				t.Fatalf("automatic placement=%#v error=%v", detail.Task, err)
			}
			if controlled.moveCalls.Load() != test.moveCalls {
				t.Fatalf("runtime detach calls=%d want=%d", controlled.moveCalls.Load(), test.moveCalls)
			}
			close(release)
			if test.want == Background {
				_, err = manager.Await(context.Background(), submission.ID)
			} else {
				_, err = manager.AwaitForeground(context.Background(), submission.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSchedulerAwaitCancellationDoesNotCancelTaskAndForegroundCompletionReleasesReservationOnce(t *testing.T) {
	release := make(chan struct{})
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		return &managerTestPreparedTask{run: func(context.Context, EventSink) Completion {
			<-release
			return Completion{ID: id, Status: StatusCompleted, Summary: managerTestSafe("done"), StopReason: StopCompleted, EndedAt: time.Now().UTC()}
		}}, nil
	}}
	inbox := newManagerTestInbox()
	manager := newManagerUnderTest(t, managerTestOptions(factory, inbox, managerIDSequence("task-await")))
	submission := mustManagerSubmit(t, manager, managerModelInput(TypeDefined, PlacementForeground))
	waitManagerStatus(t, manager, submission.ID, StatusRunning)
	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if _, err := manager.Await(waitCtx, submission.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Await error=%v", err)
	}
	detail, _ := manager.Get(context.Background(), submission.ID)
	if detail.Task.Status != StatusRunning {
		t.Fatalf("cancelled waiter changed task status: %#v", detail.Task)
	}
	close(release)
	first, err := manager.AwaitForeground(context.Background(), submission.ID)
	if err != nil || first.Completion == nil || first.Detached {
		t.Fatalf("foreground completion outcome=%#v error=%v", first, err)
	}
	second, err := manager.AwaitForeground(context.Background(), submission.ID)
	if err != nil || second.Completion == nil {
		t.Fatalf("idempotent foreground await=%#v error=%v", second, err)
	}
	reserved, released, published := inbox.counts()
	if reserved != 0 || released != 1 || published != 0 {
		t.Fatalf("foreground reservation lifecycle reserved=%d released=%d published=%d", reserved, released, published)
	}
}

func TestDetachRaceWithParentCancellationCommitsOneOutcome(t *testing.T) {
	for iteration := range 20 {
		t.Run(fmt.Sprintf("race-%d", iteration), func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
				return &managerTestControlledTask{managerTestPreparedTask: &managerTestPreparedTask{run: func(ctx context.Context, _ EventSink) Completion {
					close(started)
					select {
					case <-ctx.Done():
						return managerCancelledCandidate(id, StopCancelled)
					case <-release:
						return Completion{ID: id, Status: StatusCompleted, Summary: managerTestSafe("done"), StopReason: StopCompleted, EndedAt: time.Now().UTC()}
					}
				}}}, nil
			}}
			manager := newManagerUnderTest(t, managerTestOptions(factory, newManagerTestInbox(), managerCountingIDGenerator()))
			parentCtx, cancelParent := context.WithCancel(context.Background())
			submission := mustManagerSubmitContext(t, manager, parentCtx, managerModelInput(TypeDefined, PlacementForeground))
			waitManagerSignal(t, started, "race task start")
			start := make(chan struct{})
			moveResult := make(chan error, 1)
			go func() { <-start; moveResult <- manager.MoveToBackground(context.Background(), submission.ID) }()
			go func() { <-start; cancelParent() }()
			close(start)
			moveErr := <-moveResult
			close(release)
			completion, err := manager.Await(context.Background(), submission.ID)
			if err != nil {
				t.Fatal(err)
			}
			if moveErr == nil {
				if completion.Status != StatusCompleted {
					t.Fatalf("detach won but completion=%#v", completion)
				}
			} else if managerErrorCode(moveErr) != ErrTaskTerminal || completion.Status != StatusCancelled {
				t.Fatalf("parent cancellation race: move=%v completion=%#v", moveErr, completion)
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
				t.Fatalf("detach/cancel race emitted %d terminals", terminalCount)
			}
		})
	}
}

func TestSchedulerShutdownIsIdempotentBoundedAndNeverFabricatesSuccess(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
		return &managerTestPreparedTask{run: func(context.Context, EventSink) Completion {
			close(started)
			<-release
			return Completion{ID: id, Status: StatusCompleted, Summary: managerTestSafe("late success"), StopReason: StopCompleted, EndedAt: time.Now().UTC()}
		}}, nil
	}}
	options := managerTestOptions(factory, newManagerTestInbox(), managerIDSequence("task-shutdown", "notification-shutdown"))
	options.ShutdownTimeout = 20 * time.Millisecond
	manager, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	submission := mustManagerSubmit(t, manager, managerModelInput(TypeDefined, PlacementBackground))
	waitManagerSignal(t, started, "shutdown task start")
	if err := manager.Shutdown(context.Background()); managerErrorCode(err) != ErrTimedOut {
		t.Fatalf("bounded shutdown error=%v", err)
	}
	detail, err := manager.Get(context.Background(), submission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != StatusCancelled || detail.Task.StopReason != StopApplicationClosed {
		t.Fatalf("shutdown task state=%#v", detail.Task)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown after runner exit: %v", err)
	}
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatalf("idempotent shutdown: %v", err)
	}
	detail, err = manager.Get(context.Background(), submission.ID)
	if err != nil || detail.Task.Status != StatusCancelled {
		t.Fatalf("late runner success replaced shutdown completion: detail=%#v error=%v", detail, err)
	}
}

func mustManagerSubmit(t *testing.T, manager Service, input SubmitInput) Submission {
	t.Helper()
	return mustManagerSubmitContext(t, manager, context.Background(), input)
}

func mustManagerSubmitContext(t *testing.T, manager Service, ctx context.Context, input SubmitInput) Submission {
	t.Helper()
	submission, err := manager.Submit(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	return submission
}

func waitManagerStart(t *testing.T, starts <-chan ID) ID {
	t.Helper()
	select {
	case id := <-starts:
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scheduled task start")
		return ""
	}
}

func managerReleaseFor(t *testing.T, mu *sync.Mutex, releases map[ID]chan struct{}, id ID) chan struct{} {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	release := releases[id]
	if release == nil {
		t.Fatalf("release channel for %s is unavailable", id)
	}
	return release
}

func waitManagerPlacement(t *testing.T, manager Service, id ID, wanted Placement) TaskSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		detail, err := manager.Get(context.Background(), id)
		if err == nil && detail.Task.Placement == wanted {
			return detail.Task
		}
		time.Sleep(time.Millisecond)
	}
	detail, err := manager.Get(context.Background(), id)
	t.Fatalf("task %s did not reach placement %s: detail=%#v error=%v", id, wanted, detail, err)
	return TaskSnapshot{}
}

func managerCancelledCandidate(id ID, reason StopReason) Completion {
	return Completion{ID: id, Status: StatusCancelled, Summary: managerTestSafe("cancelled"), StopReason: reason, Error: SafeError(ErrCancelled, managerTestSafe("cancelled"), true), EndedAt: time.Now().UTC()}
}

func newManagerRedactor() *redact.RuntimeRedactor {
	return redact.NewRuntimeRedactor()
}
