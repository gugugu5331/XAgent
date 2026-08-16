package subagent

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
)

func TestEventHubAssignsGlobalRevisionAndTaskSequence(t *testing.T) {
	hub := newTestEventHub(t, 16, 8, 8)
	t.Cleanup(hub.Close)

	first := publishEventHubEvent(t, hub, snapshotHubEvent("task-a", EventQueued))
	second := publishEventHubEvent(t, hub, snapshotHubEvent("task-b", EventQueued))
	third := publishEventHubEvent(t, hub, snapshotHubEvent("task-a", EventRunning))

	if first.Revision != 1 || first.Sequence != 1 || second.Revision != 2 || second.Sequence != 1 || third.Revision != 3 || third.Sequence != 2 {
		t.Fatalf("unexpected revision/sequence allocation: first=%#v second=%#v third=%#v", first, second, third)
	}
	if first.At.IsZero() || second.At.IsZero() || third.At.IsZero() {
		t.Fatal("EventHub did not assign event timestamps")
	}
	if first.Snapshot.Revision != first.Revision || third.Snapshot.Revision != third.Revision {
		t.Fatalf("snapshot revisions did not follow authoritative events: first=%#v third=%#v", first, third)
	}
	if got := hub.Watermark(); got != 3 {
		t.Fatalf("Watermark()=%d, want 3", got)
	}
}

func TestEventHubValidatesOneOfAndAgentProjectionBeforeMutation(t *testing.T) {
	hub := newTestEventHub(t, 16, 8, 8)
	t.Cleanup(hub.Close)

	invalid := []Event{
		{TaskID: "task", Kind: EventQueued},
		{TaskID: "task", Kind: EventQueued, Snapshot: &TaskSnapshot{ID: "task"}, Agent: &AgentEvent{}},
		{TaskID: "task", Kind: EventGap, Gap: &GapDescriptor{FromRevision: 1, ToRevision: 1}},
		{TaskID: "task", Kind: EventTextDelta, Agent: &AgentEvent{Kind: events.Done, Payload: events.Event{Type: events.Done}}},
		{TaskID: "task", Kind: EventThinkingDelta, Agent: agentHubEvent("task", "text", 0, 4).Agent},
		{TaskID: "", Kind: EventQueued, Snapshot: &TaskSnapshot{}},
	}
	for index, event := range invalid {
		if _, err := hub.Publish(event); err == nil {
			t.Errorf("invalid event %d was accepted: %#v", index, event)
		}
	}
	if got := hub.Watermark(); got != 0 {
		t.Fatalf("invalid events consumed revisions: watermark=%d", got)
	}
	if _, _, _, err := hub.TaskEvents("task"); err == nil {
		t.Fatal("invalid events created a task trajectory")
	}
}

func TestEventHubDeepClonesAgentPayloads(t *testing.T) {
	hub := newTestEventHub(t, 8, 4, 4)
	t.Cleanup(hub.Close)
	input := agentHubEvent("task", "safe delta", 0, 10)
	published := publishEventHubEvent(t, hub, input)
	input.Agent.Range.From = 9
	published.Agent.Range.To = 99

	_, recent, _, err := hub.TaskEvents("task")
	if err != nil || len(recent) != 1 || recent[0].Agent.Range.From != 0 || recent[0].Agent.Range.To != 10 ||
		recent[0].Agent.Payload.Text.Text() != "safe delta" {
		t.Fatalf("agent payload was not detached: events=%#v err=%v", recent, err)
	}
}

func TestEventHubClonesInputReplayLiveAndTaskViews(t *testing.T) {
	hub := newTestEventHub(t, 16, 8, 8)
	t.Cleanup(hub.Close)

	input := snapshotHubEvent("task", EventQueued)
	input.Snapshot.Role = "original"
	published := publishEventHubEvent(t, hub, input)
	input.Snapshot.Role = "mutated input"
	published.Snapshot.Role = "mutated return"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := hub.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("Subscribe replay: %v", err)
	}
	replayed := receiveHubEvent(t, stream)
	if replayed.Revision != 1 || replayed.Snapshot.Role != "original" {
		t.Fatalf("replay observed aliased event: %#v", replayed)
	}
	replayed.Snapshot.Role = "mutated subscriber"

	livePublished := publishEventHubEvent(t, hub, snapshotHubEvent("task", EventRunning))
	live := receiveHubEvent(t, stream)
	if live.Revision != livePublished.Revision || live.Sequence != livePublished.Sequence {
		t.Fatalf("live event did not follow replay seamlessly: live=%#v published=%#v", live, livePublished)
	}

	watermark, recent, dropped, err := hub.TaskEvents("task")
	if err != nil {
		t.Fatalf("TaskEvents: %v", err)
	}
	if watermark != 2 || dropped != 0 || len(recent) != 2 || recent[0].Snapshot.Role != "original" {
		t.Fatalf("unexpected task view: watermark=%d dropped=%d events=%#v", watermark, dropped, recent)
	}
	recent[0].Snapshot.Role = "mutated view"
	_, fresh, _, err := hub.TaskEvents("task")
	if err != nil || fresh[0].Snapshot.Role != "original" {
		t.Fatalf("TaskEvents returned aliased storage: err=%v events=%#v", err, fresh)
	}
}

func TestEventHubSubscribeCursorExpiryAndExclusiveReplay(t *testing.T) {
	hub := newTestEventHub(t, 2, 2, 4)
	t.Cleanup(hub.Close)
	for index := 0; index < 3; index++ {
		publishEventHubEvent(t, hub, snapshotHubEvent("task", EventProgress))
	}

	if _, err := hub.Subscribe(context.Background(), 0); err == nil {
		t.Fatal("expired cursor was accepted")
	} else {
		requireEventHubErrorCode(t, err, ErrEventCursorExpired)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := hub.Subscribe(ctx, 1)
	if err != nil {
		t.Fatalf("Subscribe at earliest predecessor: %v", err)
	}
	for _, revision := range []uint64{2, 3} {
		if event := receiveHubEvent(t, stream); event.Revision != revision {
			t.Fatalf("replayed revision=%d, want %d", event.Revision, revision)
		}
	}
	if _, err := hub.Subscribe(context.Background(), 4); err == nil {
		t.Fatal("cursor ahead of watermark was accepted")
	} else {
		requireEventHubErrorCode(t, err, ErrInvalidTransition)
	}
}

func TestEventHubReplayLargerThanSubscriberBufferStillDrainsBeforeLive(t *testing.T) {
	hub := newTestEventHub(t, 8, 4, 2)
	t.Cleanup(hub.Close)
	for index := 0; index < 4; index++ {
		publishEventHubEvent(t, hub, snapshotHubEvent("task", EventProgress))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := hub.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	for _, revision := range []uint64{1, 2, 3, 4} {
		event := receiveHubEvent(t, stream)
		if event.Kind == EventGap || event.Revision != revision {
			t.Fatalf("replay event=%#v, want authoritative revision %d", event, revision)
		}
	}
	published := publishEventHubEvent(t, hub, snapshotHubEvent("task", EventProgress))
	if live := receiveHubEvent(t, stream); live.Kind == EventGap || live.Revision != published.Revision {
		t.Fatalf("live event did not follow large replay: live=%#v published=%#v", live, published)
	}
}

func TestEventHubSlowSubscriberGetsExactlyOneLocalGapWithoutBlockingProducer(t *testing.T) {
	hub := newTestEventHub(t, 64, 32, 2)
	t.Cleanup(hub.Close)
	stream, err := hub.Subscribe(context.Background(), 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for index := 0; index < 32; index++ {
		publishEventHubEvent(t, hub, snapshotHubEvent("task", EventProgress))
	}
	var delivered []Event
	for event := range stream {
		delivered = append(delivered, event)
	}
	if len(delivered) == 0 {
		t.Fatal("slow subscriber closed without a Gap")
	}
	for index, event := range delivered[:len(delivered)-1] {
		if event.Kind == EventGap || event.Revision != uint64(index+1) {
			t.Fatalf("unexpected authoritative prefix: %#v", delivered)
		}
	}
	gap := delivered[len(delivered)-1]
	wantFrom := uint64(len(delivered))
	if gap.Kind != EventGap || gap.Revision < wantFrom || gap.TaskID != "" || gap.Sequence != 0 || gap.Gap == nil ||
		gap.Gap.FromRevision != wantFrom || gap.Gap.ToRevision != gap.Revision || gap.Gap.Reason != "slow_subscriber" {
		t.Fatalf("unexpected subscriber-local Gap: %#v", gap)
	}
	if hub.Watermark() != 32 {
		t.Fatalf("slow subscriber blocked producer or consumed a Gap revision: watermark=%d", hub.Watermark())
	}
	watermark, authoritative, _, err := hub.TaskEvents("task")
	if err != nil || watermark != 32 || len(authoritative) != 32 {
		t.Fatalf("Gap altered authoritative task log: watermark=%d events=%d err=%v", watermark, len(authoritative), err)
	}
	for _, event := range authoritative {
		if event.Kind == EventGap {
			t.Fatal("subscriber-local Gap entered authoritative task log")
		}
	}
}

func TestEventHubCancelledSubscriberClosesWithoutGap(t *testing.T) {
	hub := newTestEventHub(t, 16, 8, 1)
	t.Cleanup(hub.Close)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := hub.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()
	publishEventHubEvent(t, hub, snapshotHubEvent("task", EventQueued))
	select {
	case event, ok := <-stream:
		if ok {
			t.Fatalf("cancelled subscriber received an event instead of close: %#v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled subscriber did not close")
	}
}

func TestEventHubTaskTrajectoryIsBoundedButRetainsPlacementAndCompletion(t *testing.T) {
	hub := newTestEventHub(t, 16, 2, 8)
	t.Cleanup(hub.Close)

	publishEventHubEvent(t, hub, snapshotHubEvent("task", EventQueued))
	publishEventHubEvent(t, hub, snapshotHubEvent("task", EventRunning))
	publishEventHubEvent(t, hub, snapshotHubEvent("task", EventProgress))
	publishEventHubEvent(t, hub, placementHubEvent("task", Foreground, Background))
	publishEventHubEvent(t, hub, snapshotHubEvent("task", EventProgress))
	publishEventHubEvent(t, hub, terminalHubEvent("task"))

	watermark, recent, dropped, err := hub.TaskEvents("task")
	if err != nil {
		t.Fatalf("TaskEvents: %v", err)
	}
	if watermark != 6 || dropped != 2 {
		t.Fatalf("TaskEvents watermark/dropped=(%d,%d), want (6,2)", watermark, dropped)
	}
	if len(recent) != 4 {
		t.Fatalf("recent event count=%d, want two bounded trace plus placement and completion: %#v", len(recent), recent)
	}
	for index, sequence := range []uint64{3, 4, 5, 6} {
		if recent[index].Sequence != sequence {
			t.Fatalf("recent[%d].Sequence=%d, want %d: %#v", index, recent[index].Sequence, sequence, recent)
		}
	}
	if recent[1].Kind != EventPlacementChanged || recent[3].Kind != EventCompletion {
		t.Fatalf("special authoritative state was not retained: %#v", recent)
	}
}

func TestEventHubTaskEventsDistinguishesUnknownAndForgottenEmptyTrajectory(t *testing.T) {
	hub := newTestEventHub(t, 8, 4, 4)
	t.Cleanup(hub.Close)
	if _, _, _, err := hub.TaskEvents("unknown"); err == nil {
		t.Fatal("unknown task returned an empty successful trajectory")
	} else {
		requireEventHubErrorCode(t, err, ErrTaskNotFound)
	}
	publishEventHubEvent(t, hub, snapshotHubEvent("task", EventQueued))
	watermark := hub.Watermark()
	if err := hub.ForgetTask("task"); err != nil {
		t.Fatalf("ForgetTask: %v", err)
	}
	gotWatermark, recent, dropped, err := hub.TaskEvents("task")
	if err != nil || gotWatermark != watermark || len(recent) != 0 || recent == nil || dropped != 0 {
		t.Fatalf("forgotten task view=(%d,%#v,%d,%v), want known explicit-empty at watermark %d", gotWatermark, recent, dropped, err, watermark)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := hub.Subscribe(ctx, 0)
	if err != nil {
		t.Fatalf("Subscribe after ForgetTask: %v", err)
	}
	if replay := receiveHubEvent(t, stream); replay.Revision != 1 || replay.TaskID != "task" {
		t.Fatalf("ForgetTask changed global replay: %#v", replay)
	}
}

func TestEventHubForgetTaskDoesNotPermitSecondCompletion(t *testing.T) {
	hub := newTestEventHub(t, 8, 4, 4)
	t.Cleanup(hub.Close)
	publishEventHubEvent(t, hub, terminalHubEvent("task"))
	if err := hub.ForgetTask("task"); err != nil {
		t.Fatalf("ForgetTask: %v", err)
	}
	if _, err := hub.Publish(terminalHubEvent("task")); err == nil {
		t.Fatal("forgotten task accepted a second completion")
	} else {
		requireEventHubErrorCode(t, err, ErrTaskTerminal)
	}
	if hub.Watermark() != 1 {
		t.Fatalf("duplicate completion consumed a revision: watermark=%d", hub.Watermark())
	}
}

func TestEventHubCounterExhaustionReservesFinalRevisionForTerminalEvent(t *testing.T) {
	hub := newTestEventHub(t, 8, 4, 4)
	t.Cleanup(hub.Close)
	hub.mu.Lock()
	hub.revision = math.MaxUint64 - 1
	hub.mu.Unlock()

	if _, err := hub.Publish(snapshotHubEvent("task", EventQueued)); err == nil {
		t.Fatal("nonterminal event consumed the reserved terminal revision")
	} else {
		requireEventHubErrorCode(t, err, ErrInternal)
	}
	terminal := publishEventHubEvent(t, hub, terminalHubEvent("task"))
	if terminal.Revision != math.MaxUint64 || hub.Watermark() != math.MaxUint64 {
		t.Fatalf("terminal event did not receive final revision: event=%#v watermark=%d", terminal, hub.Watermark())
	}
	if _, err := hub.Publish(snapshotHubEvent("other", EventQueued)); err == nil {
		t.Fatal("event was allocated after global counter exhaustion")
	} else {
		requireEventHubErrorCode(t, err, ErrInternal)
	}
}

func TestEventHubTaskCounterExhaustionIsAtomic(t *testing.T) {
	hub := newTestEventHub(t, 8, 4, 4)
	t.Cleanup(hub.Close)
	publishEventHubEvent(t, hub, snapshotHubEvent("task", EventQueued))
	hub.mu.Lock()
	hub.tasks["task"].sequence = math.MaxUint64 - 1
	hub.mu.Unlock()

	if _, err := hub.Publish(snapshotHubEvent("task", EventProgress)); err == nil {
		t.Fatal("nonterminal event consumed the reserved task terminal sequence")
	} else {
		requireEventHubErrorCode(t, err, ErrInternal)
	}
	if hub.Watermark() != 1 {
		t.Fatalf("rejected task sequence consumed global revision: watermark=%d", hub.Watermark())
	}
	terminal := publishEventHubEvent(t, hub, terminalHubEvent("task"))
	if terminal.Revision != 2 || terminal.Sequence != math.MaxUint64 {
		t.Fatalf("terminal event counters=%d/%d, want 2/%d", terminal.Revision, terminal.Sequence, uint64(math.MaxUint64))
	}
	if _, err := hub.Publish(snapshotHubEvent("task", EventProgress)); err == nil {
		t.Fatal("event was allocated after task counter exhaustion")
	} else {
		requireEventHubErrorCode(t, err, ErrInternal)
	}
}

func TestEventHubConcurrentPublishMaintainsStrictCounters(t *testing.T) {
	hub := newTestEventHub(t, 256, 64, 8)
	t.Cleanup(hub.Close)
	const eventCount = 128
	results := make(chan Event, eventCount)
	errorsChannel := make(chan error, eventCount)
	var wait sync.WaitGroup
	for index := 0; index < eventCount; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			event, err := hub.Publish(snapshotHubEvent(ID("task-"+string(rune('a'+index%4))), EventProgress))
			if err != nil {
				errorsChannel <- err
				return
			}
			results <- event
		}()
	}
	wait.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		t.Fatalf("concurrent Publish: %v", err)
	}

	published := make([]Event, 0, eventCount)
	perTask := make(map[ID][]uint64)
	for event := range results {
		published = append(published, event)
		perTask[event.TaskID] = append(perTask[event.TaskID], event.Sequence)
	}
	sort.Slice(published, func(left, right int) bool { return published[left].Revision < published[right].Revision })
	for index, event := range published {
		if event.Revision != uint64(index+1) {
			t.Fatalf("revision[%d]=%d, want %d", index, event.Revision, index+1)
		}
	}
	for taskID, sequences := range perTask {
		sort.Slice(sequences, func(left, right int) bool { return sequences[left] < sequences[right] })
		for index, sequence := range sequences {
			if sequence != uint64(index+1) {
				t.Fatalf("task %q sequence[%d]=%d, want %d", taskID, index, sequence, index+1)
			}
		}
	}
}

func TestEventHubCloseIsIdempotentAndStopsAdmission(t *testing.T) {
	hub := newTestEventHub(t, 8, 4, 4)
	stream, err := hub.Subscribe(context.Background(), 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	hub.Close()
	hub.Close()
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("subscriber remained open after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not close")
	}
	if _, err := hub.Publish(snapshotHubEvent("task", EventQueued)); err == nil {
		t.Fatal("Publish succeeded after Close")
	} else {
		requireEventHubErrorCode(t, err, ErrShutdown)
	}
	if _, err := hub.Subscribe(context.Background(), 0); err == nil {
		t.Fatal("Subscribe succeeded after Close")
	} else {
		requireEventHubErrorCode(t, err, ErrShutdown)
	}
}

func newTestEventHub(t *testing.T, globalEvents, taskEvents, subscriberBuffer int) *EventHub {
	t.Helper()
	limits := DefaultLimits()
	limits.MaxGlobalEvents = globalEvents
	limits.MaxEventsPerTask = taskEvents
	limits.MaxSubscriberBuffer = subscriberBuffer
	now := time.Date(2026, time.August, 15, 10, 0, 0, 0, time.UTC)
	hub, err := NewEventHub(EventHubOptions{Limits: limits, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewEventHub: %v", err)
	}
	return hub
}

func snapshotHubEvent(taskID ID, kind EventKind) Event {
	status := StatusRunning
	switch kind {
	case EventQueued:
		status = StatusQueued
	case EventWaitingConfirmation:
		status = StatusWaitingConfirmation
	}
	return Event{TaskID: taskID, Kind: kind, Snapshot: &TaskSnapshot{ID: taskID, Status: status}}
}

func placementHubEvent(taskID ID, from, to Placement) Event {
	return Event{
		TaskID: taskID,
		Kind:   EventPlacementChanged,
		Snapshot: &TaskSnapshot{
			ID: taskID, Status: StatusRunning, Placement: to,
		},
		Placement: &PlacementChange{From: from, To: to, Reason: "manual"},
	}
}

func terminalHubEvent(taskID ID) Event {
	endedAt := time.Date(2026, time.August, 15, 10, 1, 0, 0, time.UTC)
	return Event{
		TaskID: taskID,
		Kind:   EventCompletion,
		Snapshot: &TaskSnapshot{
			ID: taskID, Status: StatusCompleted, EndedAt: &endedAt, StopReason: StopCompleted,
		},
		Completion: &Completion{ID: taskID, Status: StatusCompleted, StopReason: StopCompleted, EndedAt: endedAt},
	}
}

func agentHubEvent(taskID ID, text string, from, to int64) Event {
	safeText := redact.NewRuntimeRedactor().Redact(text)
	return Event{
		TaskID: taskID,
		Kind:   EventTextDelta,
		Agent: &AgentEvent{
			Kind: events.TextDelta, Payload: events.Event{Type: events.TextDelta, Text: safeText}, Range: &DeltaRange{From: from, To: to},
		},
	}
}

func publishEventHubEvent(t *testing.T, hub *EventHub, input Event) Event {
	t.Helper()
	published, err := hub.Publish(input)
	if err != nil {
		t.Fatalf("Publish(%q, %q): %v", input.TaskID, input.Kind, err)
	}
	return published
}

func receiveHubEvent(t *testing.T, stream <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-stream:
		if !ok {
			t.Fatal("event stream closed")
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("event stream did not deliver")
		return Event{}
	}
}

func requireEventHubErrorCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error=nil, want code %q", code)
	}
	var safe *diagnostics.SafeError
	if !errors.As(err, &safe) || safe.Code != string(code) {
		t.Fatalf("error=%T %v, want SafeError code %q", err, err, code)
	}
}
