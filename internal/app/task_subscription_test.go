package app

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tui"
)

type taskSubscribeResult struct {
	events <-chan subagent.Event
	err    error
}

type taskStreamServiceFake struct {
	subagent.Service

	mu               sync.Mutex
	subscribeResults []taskSubscribeResult
	subscribeAfter   []uint64
	subscribeCtx     []context.Context
	listResult       subagent.TaskListSnapshot
	listErr          error
	listCalls        int
	getResult        subagent.TaskDetailSnapshot
	getErr           error
	getIDs           []subagent.ID
	shutdownCalls    int
	shutdownErr      error
	sequence         *t426Sequence
}

func (fake *taskStreamServiceFake) Subscribe(ctx context.Context, after uint64) (<-chan subagent.Event, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.subscribeAfter = append(fake.subscribeAfter, after)
	fake.subscribeCtx = append(fake.subscribeCtx, ctx)
	if len(fake.subscribeResults) == 0 {
		return nil, errors.New("unexpected Subscribe")
	}
	result := fake.subscribeResults[0]
	fake.subscribeResults = fake.subscribeResults[1:]
	return result.events, result.err
}

func (fake *taskStreamServiceFake) List(context.Context) (subagent.TaskListSnapshot, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.listCalls++
	return fake.listResult.Clone(), fake.listErr
}

func (fake *taskStreamServiceFake) Get(_ context.Context, id subagent.ID) (subagent.TaskDetailSnapshot, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.getIDs = append(fake.getIDs, id)
	return fake.getResult.Clone(), fake.getErr
}

func (fake *taskStreamServiceFake) Shutdown(context.Context) error {
	fake.mu.Lock()
	fake.shutdownCalls++
	fake.mu.Unlock()
	if fake.sequence != nil {
		fake.sequence.add("task_shutdown")
	}
	return fake.shutdownErr
}

func (fake *taskStreamServiceFake) snapshot() (after []uint64, contexts []context.Context, listCalls int, getIDs []subagent.ID, shutdownCalls int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]uint64(nil), fake.subscribeAfter...), append([]context.Context(nil), fake.subscribeCtx...), fake.listCalls,
		append([]subagent.ID(nil), fake.getIDs...), fake.shutdownCalls
}

func TestTaskEventSubscriptionDrainsOutsideNavigationEnvelope(t *testing.T) {
	now := time.Unix(500, 0)
	stream := make(chan subagent.Event, 2)
	service := &taskStreamServiceFake{subscribeResults: []taskSubscribeResult{{events: stream}}}
	store := &taskNotificationStoreFake{}
	active := conversation.NewConversation("conversation-1", now.Add(-time.Minute))
	model := Model{deps: Deps{Tasks: service, Store: store}, conversation: active, messages: tui.NewMessagesView(false)}

	start := model.Init()
	if start == nil {
		t.Fatal("task service did not start a permanent subscription")
	}
	updated, wait := model.Update(start())
	model = updated.(Model)
	if wait == nil {
		t.Fatal("successful Subscribe did not schedule a channel drain")
	}
	stream <- completedTaskResultEvent(now, "notification-stream", "conversation-1", "stream summary")
	message := wait()
	if _, staleEnvelope := message.(eventMsg); staleEnvelope {
		t.Fatalf("task event entered the ordinary request envelope: %#v", message)
	}
	updated, next := model.Update(message)
	model = updated.(Model)
	if next == nil {
		t.Fatal("task event drain stopped after one event")
	}
	if len(store.saves) != 1 || len(active.Messages) != 1 || active.Messages[0].Subagent == nil ||
		active.Messages[0].Subagent.NotificationID != "notification-stream" {
		t.Fatalf("task result was not committed through the dedicated adapter: saves=%d messages=%#v", len(store.saves), active.Messages)
	}
	if after, _, _, _, _ := service.snapshot(); !reflect.DeepEqual(after, []uint64{0}) {
		t.Fatalf("initial subscription cursors=%v, want [0]", after)
	}
}

func TestTaskEventSnapshotLiveReducerDeepCopiesListAndDetail(t *testing.T) {
	started := time.Unix(501, 0)
	wantStartedUnixMilli := started.UnixMilli()
	stream := make(chan subagent.Event, 1)
	service := &taskStreamServiceFake{subscribeResults: []taskSubscribeResult{{events: stream}}}
	model := Model{
		deps: Deps{Tasks: service},
		taskListView: projectTaskListView(subagent.TaskListSnapshot{Tasks: []subagent.TaskSnapshot{{
			ID: "task-2", Status: subagent.StatusWaitingConfirmation, Placement: subagent.Background, CreatedAt: time.Unix(400, 0),
		}}}),
		taskDetailView: projectTaskDetailView(subagent.TaskDetailSnapshot{
			Task: subagent.TaskSnapshot{ID: "task-1", Status: subagent.StatusQueued, Placement: subagent.Foreground, CreatedAt: time.Unix(300, 0)},
			RecentEvents: []subagent.Event{{
				Revision: 3, TaskID: "task-1", Sequence: 1, At: time.Unix(300, 0), Kind: subagent.EventQueued,
				Snapshot: &subagent.TaskSnapshot{ID: "task-1", Status: subagent.StatusQueued},
			}},
		}),
	}

	updated, wait := model.Update(model.Init()())
	model = updated.(Model)
	snapshot := &subagent.TaskSnapshot{
		ID: "task-1", Revision: 5, Type: subagent.TypeDefined, Origin: subagent.OriginTUI,
		Role: "explore", Placement: subagent.Background, Status: subagent.StatusRunning,
		CreatedAt: time.Unix(300, 0), StartedAt: &started, Iteration: 2, MaxIterations: 4,
		PendingConfirmation: &events.ToolConfirmationRequest{
			ConfirmationID: "confirmation-1", CallID: "call-1", Name: "Bash",
			Scopes: []events.ConfirmationScopeDisplay{{Scope: "session", Available: true, Description: redact.NewRuntimeRedactor().Redact("session only")}},
		},
	}
	stream <- subagent.Event{Revision: 5, TaskID: "task-1", Sequence: 2, At: started, Kind: subagent.EventRunning, Snapshot: snapshot}
	message := wait()
	// Reuse every producer-owned nested value before the App reducer runs.
	snapshot.Role = "mutated"
	*snapshot.StartedAt = time.Unix(999, 0)
	snapshot.PendingConfirmation.Scopes[0].Description = redact.NewRuntimeRedactor().Redact("mutated")
	updated, next := model.Update(message)
	model = updated.(Model)
	if next == nil {
		t.Fatal("live reducer stopped the permanent event drain")
	}

	tasks := model.taskListView.Tasks()
	if model.taskListView.Watermark() != 5 || len(tasks) != 2 || tasks[0].ID() != "task-1" || tasks[0].Role() != "explore" || tasks[0].Status() != string(subagent.StatusRunning) {
		t.Fatalf("snapshot was not stably upserted into task list: watermark=%d tasks=%#v", model.taskListView.Watermark(), tasks)
	}
	if got, ok := tasks[0].StartedAtUnixMilli(); !ok || got != wantStartedUnixMilli {
		t.Fatalf("snapshot time was not detached: got=(%d,%t)", got, ok)
	}
	detail := model.taskDetailView
	if detail.Watermark() != 5 || detail.Task().Role() != "explore" || len(detail.RecentEvents()) != 2 || detail.RecentEvents()[1].Revision() != 5 {
		t.Fatalf("snapshot did not update current detail and trajectory: detail=%#v events=%#v", detail, detail.RecentEvents())
	}
	confirmation, present := detail.Task().PendingConfirmation()
	if !present || confirmation.Scopes()[0].Description().Text() != "session only" {
		t.Fatalf("confirmation projection retained producer storage: present=%t confirmation=%#v", present, confirmation)
	}
	if _, _, listCalls, getIDs, _ := service.snapshot(); listCalls != 0 || len(getIDs) != 0 {
		t.Fatalf("snapshot event caused service refresh: list=%d get=%v", listCalls, getIDs)
	}
}

func TestTaskEventResultLiveReducerReflectsTerminalWithoutServiceQuery(t *testing.T) {
	now := time.Unix(502, 0)
	stream := make(chan subagent.Event, 1)
	service := &taskStreamServiceFake{subscribeResults: []taskSubscribeResult{{events: stream}}}
	model := Model{
		deps: Deps{Tasks: service}, conversation: conversation.NewConversation("current", now.Add(-time.Minute)),
		taskListView: projectTaskListView(subagent.TaskListSnapshot{Tasks: []subagent.TaskSnapshot{
			{ID: "task-1", Status: subagent.StatusRunning, Placement: subagent.Background, CreatedAt: time.Unix(300, 0)},
			{ID: "task-2", Status: subagent.StatusQueued, Placement: subagent.Background, CreatedAt: time.Unix(400, 0)},
		}}),
		taskDetailView: projectTaskDetailView(subagent.TaskDetailSnapshot{Task: subagent.TaskSnapshot{
			ID: "task-1", Status: subagent.StatusRunning, Placement: subagent.Background, CreatedAt: time.Unix(300, 0),
		}}),
	}

	updated, wait := model.Update(model.Init()())
	model = updated.(Model)
	result := &subagent.ResultNotification{
		NotificationID: "notification-terminal", CompletionRevision: 8, CompletionSequence: 3,
		CreatedAt: now, TaskID: "task-1", Parent: subagent.ParentRef{ConversationID: "other"},
		Status: subagent.StatusCompleted, Summary: redact.NewRuntimeRedactor().Redact("safe terminal"), StopReason: subagent.StopCompleted,
		Usage: subagent.Usage{InputTokens: 11, OutputTokens: 12},
	}
	stream <- subagent.Event{Revision: 9, TaskID: "task-1", Sequence: 4, At: now, Kind: subagent.EventResultPublished, Result: result}
	message := wait()
	result.Summary = redact.NewRuntimeRedactor().Redact("mutated")
	updated, next := model.Update(message)
	model = updated.(Model)
	if next == nil {
		t.Fatal("result reducer stopped the permanent event drain")
	}

	tasks := model.taskListView.Tasks()
	if len(tasks) != 2 || tasks[0].ID() != "task-2" || tasks[1].ID() != "task-1" || tasks[1].Status() != string(subagent.StatusCompleted) || tasks[1].Summary().Text() != "safe terminal" {
		t.Fatalf("result did not update and reorder terminal task: %#v", tasks)
	}
	if ended, ok := tasks[1].EndedAtUnixMilli(); !ok || ended != now.UnixMilli() || tasks[1].Usage().InputTokens() != 11 {
		t.Fatalf("terminal result fields were not projected: ended=(%d,%t) usage=%#v", ended, ok, tasks[1].Usage())
	}
	if model.taskDetailView.Watermark() != 9 || model.taskDetailView.Task().Summary().Text() != "safe terminal" ||
		len(model.taskDetailView.RecentEvents()) != 1 || model.taskDetailView.RecentEvents()[0].Kind() != string(subagent.EventResultPublished) {
		t.Fatalf("result did not update current detail: %#v", model.taskDetailView)
	}
	if _, _, listCalls, getIDs, _ := service.snapshot(); listCalls != 0 || len(getIDs) != 0 {
		t.Fatalf("result event caused service refresh: list=%d get=%v", listCalls, getIDs)
	}
}

func TestTaskEventLiveTrajectoryIsBounded(t *testing.T) {
	recent := make([]subagent.Event, taskDetailLiveEventLimit)
	for index := range recent {
		revision := uint64(index + 1)
		recent[index] = subagent.Event{
			Revision: revision, TaskID: "task-1", Sequence: revision, At: time.Unix(int64(revision), 0),
			Kind: subagent.EventProgress, Snapshot: &subagent.TaskSnapshot{ID: "task-1", Status: subagent.StatusRunning},
		}
	}
	model := Model{
		taskListView: projectTaskListView(subagent.TaskListSnapshot{
			Watermark: taskDetailLiveEventLimit,
			Tasks:     []subagent.TaskSnapshot{{ID: "task-1", Revision: taskDetailLiveEventLimit, Status: subagent.StatusRunning}},
		}),
		taskDetailView: projectTaskDetailView(subagent.TaskDetailSnapshot{
			Watermark:    taskDetailLiveEventLimit,
			Task:         subagent.TaskSnapshot{ID: "task-1", Revision: taskDetailLiveEventLimit, Status: subagent.StatusRunning},
			RecentEvents: recent,
		}),
	}
	event := subagent.Event{
		Revision: taskDetailLiveEventLimit + 1, TaskID: "task-1", Sequence: taskDetailLiveEventLimit + 1,
		At: time.Unix(taskDetailLiveEventLimit+1, 0), Kind: subagent.EventTextDelta,
		Agent: &subagent.AgentEvent{Kind: events.TextDelta, Payload: events.Event{
			Type: events.TextDelta, Text: redact.NewRuntimeRedactor().Redact("bounded delta"),
		}},
	}

	model.reduceTaskEventViews(event)
	got := model.taskDetailView.RecentEvents()
	if len(got) != taskDetailLiveEventLimit || got[0].Revision() != 2 || got[len(got)-1].Revision() != taskDetailLiveEventLimit+1 {
		t.Fatalf("live trajectory was not bounded FIFO: len=%d first=%d last=%d", len(got), got[0].Revision(), got[len(got)-1].Revision())
	}

	// Agent-only deltas can extend an already known task's detail, but cannot
	// manufacture an incomplete list row when no authoritative snapshot exists.
	unknown := event.Clone()
	unknown.Revision++
	unknown.Sequence = 1
	unknown.TaskID = "task-unknown"
	model.reduceTaskEventViews(unknown)
	if tasks := model.taskListView.Tasks(); len(tasks) != 1 || tasks[0].ID() != "task-1" {
		t.Fatalf("agent-only delta manufactured an incomplete task row: %#v", tasks)
	}
}

func TestTaskEventMalformedGapAndResultFailClosedWithoutStoppingDrain(t *testing.T) {
	now := time.Unix(503, 0)
	stream := make(chan subagent.Event, 2)
	service := &taskStreamServiceFake{subscribeResults: []taskSubscribeResult{{events: stream}}}
	model := Model{
		deps: Deps{Tasks: service}, conversation: conversation.NewConversation("current", now.Add(-time.Minute)),
		taskListView: projectTaskListView(subagent.TaskListSnapshot{Tasks: []subagent.TaskSnapshot{{
			ID: "task-1", Status: subagent.StatusRunning,
		}}}),
	}
	updated, wait := model.Update(model.Init()())
	model = updated.(Model)

	stream <- subagent.Event{
		Revision: 4, Kind: subagent.EventGap,
		Gap: &subagent.GapDescriptor{FromRevision: 5, ToRevision: 4, Reason: "malformed"},
	}
	updated, next := model.Update(wait())
	model = updated.(Model)
	if next == nil || model.status.Error == nil {
		t.Fatalf("malformed Gap was not rejected while preserving drain: next=%v error=%v", next, model.status.Error)
	}
	if _, _, listCalls, _, _ := service.snapshot(); listCalls != 0 {
		t.Fatalf("malformed Gap reached authoritative resync: list=%d", listCalls)
	}

	stream <- subagent.Event{
		Revision: 5, TaskID: "task-1", Sequence: 4, At: now, Kind: subagent.EventResultPublished,
		Result: &subagent.ResultNotification{
			CreatedAt: now, TaskID: "task-1", Parent: subagent.ParentRef{ConversationID: "other"},
			Status: subagent.StatusCompleted, StopReason: subagent.StopCompleted,
		},
	}
	updated, next = model.Update(next())
	model = updated.(Model)
	if next == nil || model.taskListView.Tasks()[0].Status() != string(subagent.StatusRunning) {
		t.Fatalf("malformed result changed task state or stopped drain: next=%v tasks=%#v", next, model.taskListView.Tasks())
	}
}

func TestTaskEventSubscriptionAndNotificationFailuresRetryFromLastCursor(t *testing.T) {
	now := time.Unix(504, 0)
	t.Run("subscribe failure", func(t *testing.T) {
		stream := make(chan subagent.Event)
		service := &taskStreamServiceFake{subscribeResults: []taskSubscribeResult{
			{err: errors.New("transient subscribe failure")}, {events: stream},
		}}
		model := Model{deps: Deps{Tasks: service}}

		updated, retry := model.Update(model.Init()())
		model = updated.(Model)
		if retry == nil || model.status.Error == nil {
			t.Fatalf("transient Subscribe failure stopped permanently: retry=%v error=%v", retry, model.status.Error)
		}
		updated, subscribe := model.Update(retry())
		model = updated.(Model)
		if subscribe == nil {
			t.Fatal("retry timer did not schedule Subscribe")
		}
		updated, wait := model.Update(subscribe())
		model = updated.(Model)
		if wait == nil {
			t.Fatal("retried Subscribe did not resume channel drain")
		}
		if after, _, _, _, _ := service.snapshot(); !reflect.DeepEqual(after, []uint64{0, 0}) {
			t.Fatalf("Subscribe retry cursors=%v, want [0 0]", after)
		}
	})

	t.Run("notification save failure", func(t *testing.T) {
		event := completedTaskResultEvent(now, "notification-replay", "conversation-1", "retry summary")
		first := make(chan subagent.Event, 1)
		second := make(chan subagent.Event, 1)
		first <- event
		second <- event
		service := &taskStreamServiceFake{subscribeResults: []taskSubscribeResult{{events: first}, {events: second}}}
		store := &taskNotificationStoreFake{err: errors.New("transient save failure")}
		active := conversation.NewConversation("conversation-1", now.Add(-time.Minute))
		model := Model{deps: Deps{Tasks: service, Store: store}, conversation: active, messages: tui.NewMessagesView(false)}

		updated, wait := model.Update(model.Init()())
		model = updated.(Model)
		updated, retry := model.Update(wait())
		model = updated.(Model)
		if retry == nil || model.taskEventCursor != 0 || len(active.Messages) != 0 {
			t.Fatalf("failed ordered commit consumed cursor or stopped retry: retry=%v cursor=%d messages=%#v", retry, model.taskEventCursor, active.Messages)
		}
		store.err = nil
		updated, subscribe := model.Update(retry())
		model = updated.(Model)
		updated, wait = model.Update(subscribe())
		model = updated.(Model)
		updated, _ = model.Update(wait())
		model = updated.(Model)
		if model.taskEventCursor != event.Revision || len(active.Messages) != 1 || len(store.saves) != 1 {
			t.Fatalf("notification replay did not commit exactly once: cursor=%d messages=%#v saves=%d", model.taskEventCursor, active.Messages, len(store.saves))
		}
		if after, _, _, _, _ := service.snapshot(); !reflect.DeepEqual(after, []uint64{0, 0}) {
			t.Fatalf("notification retry cursors=%v, want [0 0]", after)
		}
	})
}

func TestTaskEventGapResyncsListAndDetailBeforeWatermarkSubscribe(t *testing.T) {
	first := make(chan subagent.Event, 1)
	second := make(chan subagent.Event, 1)
	first <- subagent.Event{
		Revision: 8, Kind: subagent.EventGap,
		Gap: &subagent.GapDescriptor{FromRevision: 3, ToRevision: 8, Reason: "subscriber_slow"},
	}
	service := &taskStreamServiceFake{
		subscribeResults: []taskSubscribeResult{{events: first}, {events: second}},
		listResult: subagent.TaskListSnapshot{Watermark: 42, Tasks: []subagent.TaskSnapshot{{
			ID: "task-1", Status: subagent.StatusRunning, Placement: subagent.Background, CreatedAt: time.Unix(1, 0),
		}}},
		getResult: subagent.TaskDetailSnapshot{Watermark: 43, Task: subagent.TaskSnapshot{
			ID: "task-1", Status: subagent.StatusRunning, Placement: subagent.Background, CreatedAt: time.Unix(1, 0),
		}},
	}
	model := Model{
		deps:           Deps{Tasks: service},
		taskDetailView: tui.NewTaskDetailView(tui.TaskDetailViewSpec{Task: tui.TaskViewSpec{ID: "task-1"}}),
	}

	updated, wait := model.Update(model.Init()())
	model = updated.(Model)
	updated, resync := model.Update(wait())
	model = updated.(Model)
	if resync == nil {
		t.Fatal("EventGap did not schedule authoritative List/Get resync")
	}
	updated, subscribe := model.Update(resync())
	model = updated.(Model)
	if subscribe == nil {
		t.Fatal("resync did not schedule a replacement subscription")
	}
	updated, next := model.Update(subscribe())
	model = updated.(Model)
	if next == nil {
		t.Fatal("replacement subscription was not drained")
	}

	after, _, listCalls, getIDs, _ := service.snapshot()
	if !reflect.DeepEqual(after, []uint64{0, 42}) || listCalls != 1 || !reflect.DeepEqual(getIDs, []subagent.ID{"task-1"}) {
		t.Fatalf("gap resync calls mismatch: after=%v list=%d get=%v", after, listCalls, getIDs)
	}
	if model.taskListView.Watermark() != 42 || model.taskDetailView.Watermark() != 43 || model.taskListView.Tasks()[0].ID() != "task-1" {
		t.Fatalf("gap snapshots were not published before resubscribe: list=%#v detail=%#v", model.taskListView, model.taskDetailView)
	}
}

func TestTaskEventGapResyncClearsMissingDetailAndKeepsDraining(t *testing.T) {
	first := make(chan subagent.Event, 1)
	second := make(chan subagent.Event)
	first <- subagent.Event{
		Revision: 8, Kind: subagent.EventGap,
		Gap: &subagent.GapDescriptor{FromRevision: 3, ToRevision: 8, Reason: "subscriber_slow"},
	}
	service := &taskStreamServiceFake{
		subscribeResults: []taskSubscribeResult{{events: first}, {events: second}},
		listResult:       subagent.TaskListSnapshot{Watermark: 42},
		getErr:           subagent.SafeError(subagent.ErrTaskNotFound, redact.NewRuntimeRedactor().Redact("task missing"), true),
	}
	model := Model{
		deps:           Deps{Tasks: service},
		taskDetailView: tui.NewTaskDetailView(tui.TaskDetailViewSpec{Task: tui.TaskViewSpec{ID: "task-missing"}}),
	}

	updated, wait := model.Update(model.Init()())
	model = updated.(Model)
	updated, resync := model.Update(wait())
	model = updated.(Model)
	updated, subscribe := model.Update(resync())
	model = updated.(Model)
	if subscribe == nil {
		t.Fatal("missing detail stopped the permanent stream after List succeeded")
	}
	updated, next := model.Update(subscribe())
	model = updated.(Model)
	if next == nil || model.taskDetailView.Task().ID() != "" {
		t.Fatalf("missing detail was not cleared before resubscribe: next=%v detail=%#v", next, model.taskDetailView)
	}
	after, _, listCalls, getIDs, _ := service.snapshot()
	if !reflect.DeepEqual(after, []uint64{0, 42}) || listCalls != 1 || !reflect.DeepEqual(getIDs, []subagent.ID{"task-missing"}) {
		t.Fatalf("missing-detail resync mismatch: after=%v list=%d get=%v", after, listCalls, getIDs)
	}
}

func TestTaskEventResyncDoesNotOverwriteAChangedDetailSelection(t *testing.T) {
	state := newTaskEventStreamState()
	_, epoch, ok := state.beginResync()
	if !ok {
		t.Fatal("failed to establish resync epoch")
	}
	service := &taskStreamServiceFake{subscribeResults: []taskSubscribeResult{{events: make(chan subagent.Event)}}}
	model := Model{
		deps:           Deps{Tasks: service},
		taskEvents:     state,
		taskDetailView: projectTaskDetailView(subagent.TaskDetailSnapshot{Task: subagent.TaskSnapshot{ID: "task-new", Status: subagent.StatusRunning}}),
	}
	message := taskEventResyncMsg{
		state: state, epoch: epoch, detailID: "task-old",
		list:   subagent.TaskListSnapshot{Watermark: 50},
		detail: &subagent.TaskDetailSnapshot{Watermark: 50, Task: subagent.TaskSnapshot{ID: "task-old", Status: subagent.StatusRunning}},
	}
	if next := model.handleTaskEventResync(message); next == nil {
		t.Fatal("resync did not preserve permanent subscription")
	}
	if got := model.taskDetailView.Task().ID(); got != "task-new" {
		t.Fatalf("stale resync overwrote changed detail selection: got=%q", got)
	}
}

func TestTaskEventCursorExpiredUsesSnapshotWatermark(t *testing.T) {
	second := make(chan subagent.Event)
	service := &taskStreamServiceFake{
		subscribeResults: []taskSubscribeResult{
			{err: subagent.SafeError(subagent.ErrEventCursorExpired, redact.NewRuntimeRedactor().Redact("cursor expired"), true)},
			{events: second},
		},
		listResult: subagent.TaskListSnapshot{Watermark: 99},
	}
	model := Model{deps: Deps{Tasks: service}}
	updated, resync := model.Update(model.Init()())
	model = updated.(Model)
	if resync == nil {
		t.Fatal("cursor-expired Subscribe did not schedule resync")
	}
	updated, subscribe := model.Update(resync())
	model = updated.(Model)
	updated, wait := model.Update(subscribe())
	model = updated.(Model)
	if wait == nil {
		t.Fatal("cursor resync did not resume draining")
	}
	after, _, listCalls, _, _ := service.snapshot()
	if !reflect.DeepEqual(after, []uint64{0, 99}) || listCalls != 1 {
		t.Fatalf("cursor resync mismatch: after=%v list=%d", after, listCalls)
	}
}

func TestTaskEventSubscriptionIsOptionalAndCloseCancelsBeforeTaskShutdown(t *testing.T) {
	if cmd := (Model{}).Init(); cmd != nil {
		t.Fatalf("ordinary App unexpectedly started task subscription: %#v", cmd())
	}

	sequence := newT426Sequence("task_shutdown", "wait_idle", "resources_closed")
	stream := make(chan subagent.Event)
	service := &taskStreamServiceFake{sequence: sequence, subscribeResults: []taskSubscribeResult{{events: stream}}}
	waiter := &t426Waiter{sequence: sequence}
	model := t426Model(sequence, nil, waiter, nil, nil, RuntimeOptions{CleanupTimeout: time.Second})
	model.deps.Tasks = service
	updated, wait := model.Update(model.Init()())
	model = updated.(Model)
	if wait == nil {
		t.Fatal("task stream was not waiting before Close")
	}

	if err := model.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, contexts, _, _, shutdownCalls := service.snapshot()
	if shutdownCalls != 1 || len(contexts) != 1 {
		t.Fatalf("task lifecycle calls mismatch: shutdown=%d contexts=%d", shutdownCalls, len(contexts))
	}
	select {
	case <-contexts[0].Done():
	default:
		t.Fatal("Close did not cancel the permanent task subscription")
	}
	assertT426Order(t, sequence.snapshot(), "task_shutdown", "wait_idle", "resources_closed")
}
