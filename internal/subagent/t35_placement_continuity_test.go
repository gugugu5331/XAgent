package subagent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/events"
)

func TestT35AllBackgroundPathsPreserveIdentityAndEventContinuity(t *testing.T) {
	for _, test := range []struct {
		name               string
		intent             PlacementIntent
		automatic          bool
		wantPlacementEvent int
	}{
		{name: "explicit", intent: PlacementBackground},
		{name: "manual", intent: PlacementForeground, wantPlacementEvent: 1},
		{name: "automatic", intent: PlacementForeground, automatic: true, wantPlacementEvent: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := managerTestLimits()
			limits.AutoBackgroundAfter = 10 * time.Millisecond
			started := make(chan struct{})
			release := make(chan struct{})
			cancelled := make(chan struct{}, 1)
			var runCalls atomic.Int32
			var controlled *managerTestControlledTask
			factory := &managerTestRunnerFactory{prepare: func(_ context.Context, id ID, _ SubmitInput) (PreparedTask, error) {
				base := &managerTestPreparedTask{run: func(ctx context.Context, sink EventSink) Completion {
					runCalls.Add(1)
					if test.automatic {
						requestCtx, finishRequest := context.WithCancel(ctx)
						defer finishRequest()
						if !controlled.notifyFirstProviderRequest(requestCtx) {
							return managerTestInternalCompletion(id, "first request observer was not installed")
						}
					}
					if err := sink(t35ProgressEvent(1)); err != nil {
						return managerTestInternalCompletion(id, "first progress failed")
					}
					close(started)
					select {
					case <-ctx.Done():
						cancelled <- struct{}{}
						return managerCancelledCandidate(id, StopCancelled)
					case <-release:
					}
					if err := sink(t35ProgressEvent(2)); err != nil {
						return managerTestInternalCompletion(id, "second progress failed")
					}
					return Completion{
						ID: id, Status: StatusCompleted, Summary: managerTestSafe("done"),
						StopReason: StopCompleted, EndedAt: time.Now().UTC(),
					}
				}}
				controlled = &managerTestControlledTask{managerTestPreparedTask: base}
				return controlled, nil
			}}
			id := ID("task-t35-" + test.name)
			manager := newManagerUnderTest(t, ManagerOptions{
				Runner: factory, Limits: limits, Inbox: newManagerTestInbox(), Redactor: newManagerRedactor(),
				IDGenerator: managerIDSequence(id, ID("notification-t35-"+test.name)), ShutdownTimeout: time.Second,
			})
			parentCtx, cancelParent := context.WithCancel(context.Background())
			submission := mustManagerSubmitContext(t, manager, parentCtx, managerModelInput(TypeDefined, test.intent))
			waitManagerSignal(t, started, "task start")

			switch {
			case test.automatic:
				waitManagerPlacement(t, manager, id, Background)
			case test.intent == PlacementForeground:
				if err := manager.MoveToBackground(context.Background(), id); err != nil {
					t.Fatal(err)
				}
			}
			if outcome, err := manager.AwaitForeground(context.Background(), id); err != nil || !outcome.Detached || outcome.Completion != nil {
				t.Fatalf("background handoff outcome=%#v error=%v", outcome, err)
			}
			cancelParent()
			select {
			case <-cancelled:
				t.Fatal("parent cancellation crossed a committed background transition")
			case <-time.After(15 * time.Millisecond):
			}
			close(release)
			completion, err := manager.Await(context.Background(), id)
			if err != nil || completion.Status != StatusCompleted {
				t.Fatalf("background completion=%#v error=%v", completion, err)
			}
			detail, err := manager.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if submission.ID != id || detail.Task.ID != id || completion.ID != id || detail.Task.Placement != Background {
				t.Fatalf("task identity/placement changed: submission=%#v detail=%#v completion=%#v", submission, detail.Task, completion)
			}
			if runCalls.Load() != 1 {
				t.Fatalf("task restarted %d times", runCalls.Load())
			}
			assertT35ContinuousTrajectory(t, id, detail.RecentEvents, test.wantPlacementEvent)
		})
	}
}

func t35ProgressEvent(iteration int) AgentEvent {
	return AgentEvent{
		Kind: events.AgentProgressed,
		Payload: events.Event{Type: events.AgentProgressed, Progress: &events.AgentProgress{
			Iteration: iteration, Max: 4,
		}},
	}
}

func assertT35ContinuousTrajectory(t *testing.T, id ID, trajectory []Event, wantPlacement int) {
	t.Helper()
	previous := uint64(0)
	placements, terminals := 0, 0
	progress := make(map[int]bool, 2)
	for index, event := range trajectory {
		if event.TaskID != id || event.Sequence <= previous {
			t.Fatalf("event %d crossed identity/order boundary: previous=%d event=%#v", index, previous, event)
		}
		previous = event.Sequence
		switch event.Kind {
		case EventPlacementChanged:
			placements++
		case EventProgress:
			if event.Snapshot != nil {
				progress[event.Snapshot.Iteration] = true
			}
		case EventCompletion, EventFailure, EventCancellation, EventTimeout, EventLimit:
			terminals++
		}
	}
	if placements != wantPlacement || terminals != 1 || !progress[1] || !progress[2] {
		t.Fatalf("trajectory continuity mismatch: placements=%d/%d terminals=%d progress=%v events=%s",
			placements, wantPlacement, terminals, progress, fmt.Sprint(trajectory))
	}
}
