package subagent

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"xagent/internal/events"
	"xagent/internal/redact"
)

// EventHubOptions configures the bounded authoritative event log. Limits must
// already be fully resolved; EventHub never widens an invalid configuration.
type EventHubOptions struct {
	Limits Limits
	Clock  func() time.Time
}

// EventHub assigns the sole authoritative event ordering and isolates event
// producers from replay and subscriber backpressure.
type EventHub struct {
	mu sync.RWMutex

	clock            func() time.Time
	global           eventRing
	maxTaskEvents    int
	subscriberBuffer int
	revision         uint64
	tasks            map[ID]*taskEventLog
	subscribers      map[uint64]*eventSubscriber
	nextSubscriberID uint64
	closed           bool
}

type taskEventLog struct {
	sequence      uint64
	trace         eventRing
	dropped       uint64
	lastPlacement *Event
	completion    *Event
	completed     bool
}

type eventSubscriber struct {
	id         uint64
	ctx        context.Context
	events     chan Event
	live       chan Event
	overflow   chan Event
	stop       chan struct{}
	buffer     int
	stopped    bool
	overflowed bool
}

type eventRing struct {
	limit  int
	start  int
	events []Event
}

func NewEventHub(options EventHubOptions) (*EventHub, error) {
	if err := options.Limits.Validate(); err != nil {
		return nil, err
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	return &EventHub{
		clock:            clock,
		global:           newEventRing(options.Limits.MaxGlobalEvents),
		maxTaskEvents:    options.Limits.MaxEventsPerTask,
		subscriberBuffer: options.Limits.MaxSubscriberBuffer,
		tasks:            make(map[ID]*taskEventLog),
		subscribers:      make(map[uint64]*eventSubscriber),
	}, nil
}

// Publish validates and detaches an event before assigning its global and
// task-local counters. Subscriber delivery is always non-blocking.
func (hub *EventHub) Publish(input Event) (Event, error) {
	event := input.Clone()
	if err := validateAuthoritativeEvent(event); err != nil {
		return Event{}, err
	}
	now := hub.clock()
	terminal := terminalEventKind(event.Kind)

	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed {
		return Event{}, eventHubError(ErrShutdown, "event hub is closed", false)
	}

	taskLog := hub.tasks[event.TaskID]
	if taskLog != nil && terminal && taskLog.completed {
		return Event{}, eventHubError(ErrTaskTerminal, "task completion is already published", false)
	}
	currentSequence := uint64(0)
	if taskLog != nil {
		currentSequence = taskLog.sequence
	}
	nextRevision, err := nextAuthoritativeCounter(hub.revision, terminal)
	if err != nil {
		return Event{}, err
	}
	nextSequence, err := nextAuthoritativeCounter(currentSequence, terminal)
	if err != nil {
		return Event{}, err
	}

	if taskLog == nil {
		taskLog = &taskEventLog{trace: newEventRing(hub.maxTaskEvents)}
		hub.tasks[event.TaskID] = taskLog
	}
	event.Revision = nextRevision
	event.Sequence = nextSequence
	event.At = now
	if event.Snapshot != nil {
		event.Snapshot.Revision = nextRevision
	}

	hub.revision = nextRevision
	taskLog.sequence = nextSequence
	hub.global.append(event)
	hub.appendTaskEventLocked(taskLog, event)
	hub.deliverLocked(event)
	return event.Clone(), nil
}

// Subscribe replays events strictly after the supplied global revision and
// then joins the live stream under the same lock, avoiding a replay/live gap.
func (hub *EventHub) Subscribe(ctx context.Context, after uint64) (<-chan Event, error) {
	if ctx == nil {
		return nil, eventHubError(ErrInvalidTransition, "subscriber context is nil", false)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		return nil, eventHubError(ErrShutdown, "event hub is closed", false)
	}
	if after > hub.revision {
		hub.mu.Unlock()
		return nil, eventHubError(ErrInvalidTransition, "event cursor is ahead of the watermark", true)
	}
	if earliest, ok := hub.global.firstRevision(); ok && after < earliest-1 {
		hub.mu.Unlock()
		return nil, eventHubError(ErrEventCursorExpired, "event cursor is older than retained replay", true)
	}

	replay := hub.global.after(after)
	for index := range replay {
		replay[index] = replay[index].Clone()
	}
	stream := make(chan Event, hub.subscriberBuffer+1)
	select {
	case <-ctx.Done():
		close(stream)
		hub.mu.Unlock()
		return stream, nil
	default:
	}
	if hub.nextSubscriberID == math.MaxUint64 {
		close(stream)
		hub.mu.Unlock()
		return nil, eventHubError(ErrInternal, "subscriber identifiers are exhausted", false)
	}
	hub.nextSubscriberID++
	subscriber := &eventSubscriber{
		id:       hub.nextSubscriberID,
		ctx:      ctx,
		events:   stream,
		live:     make(chan Event, hub.subscriberBuffer),
		overflow: make(chan Event, 1),
		stop:     make(chan struct{}),
		buffer:   hub.subscriberBuffer,
	}
	hub.subscribers[subscriber.id] = subscriber
	hub.mu.Unlock()

	go hub.runSubscriber(subscriber, after, replay)
	return stream, nil
}

// Watermark returns the largest authoritative global revision allocated so
// far. Subscriber-local Gap events never affect it.
func (hub *EventHub) Watermark() uint64 {
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	return hub.revision
}

// TaskEvents returns the global watermark, retained task trajectory and drop
// count from one read-side critical section. A known task with an explicitly
// empty trajectory is distinct from a task that never existed.
func (hub *EventHub) TaskEvents(taskID ID) (uint64, []Event, uint64, error) {
	if !validIdentifier(string(taskID)) {
		return 0, nil, 0, eventHubError(ErrInvalidTask, "task ID is invalid", false)
	}
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	taskLog, exists := hub.tasks[taskID]
	if !exists {
		return hub.revision, nil, 0, eventHubError(ErrTaskNotFound, "task was not found", true)
	}
	events := make([]Event, 0, taskLog.trace.len()+2)
	for _, event := range taskLog.trace.all() {
		events = append(events, event.Clone())
	}
	if taskLog.lastPlacement != nil {
		events = append(events, taskLog.lastPlacement.Clone())
	}
	if taskLog.completion != nil {
		events = append(events, taskLog.completion.Clone())
	}
	sort.Slice(events, func(left, right int) bool {
		return events[left].Sequence < events[right].Sequence
	})
	return hub.revision, events, taskLog.dropped, nil
}

// ForgetTask releases retained per-task payloads while intentionally leaving
// the bounded global replay untouched. Counter state remains so a mistaken ID
// reuse cannot reset task-local ordering.
func (hub *EventHub) ForgetTask(taskID ID) error {
	if !validIdentifier(string(taskID)) {
		return eventHubError(ErrInvalidTask, "task ID is invalid", false)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	taskLog, exists := hub.tasks[taskID]
	if !exists {
		return eventHubError(ErrTaskNotFound, "task was not found", true)
	}
	taskLog.trace.clear()
	taskLog.dropped = 0
	taskLog.lastPlacement = nil
	taskLog.completion = nil
	return nil
}

// Close is idempotent. It stops admission and closes every subscriber without
// waiting for consumers to drain buffered events.
func (hub *EventHub) Close() {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed {
		return
	}
	hub.closed = true
	for id, subscriber := range hub.subscribers {
		hub.stopSubscriberLocked(id, subscriber)
	}
}

func (hub *EventHub) appendTaskEventLocked(taskLog *taskEventLog, event Event) {
	switch event.Kind {
	case EventPlacementChanged:
		if taskLog.lastPlacement != nil {
			hub.appendTraceLocked(taskLog, taskLog.lastPlacement.Clone())
		}
		value := event.Clone()
		taskLog.lastPlacement = &value
	case EventCompletion, EventFailure, EventCancellation, EventTimeout, EventLimit:
		value := event.Clone()
		taskLog.completion = &value
		taskLog.completed = true
	default:
		hub.appendTraceLocked(taskLog, event)
	}
}

func (hub *EventHub) appendTraceLocked(taskLog *taskEventLog, event Event) {
	if taskLog.trace.append(event) && taskLog.dropped != math.MaxUint64 {
		taskLog.dropped++
	}
}

func (hub *EventHub) deliverLocked(event Event) {
	for id, subscriber := range hub.subscribers {
		if subscriber.overflowed {
			continue
		}
		select {
		case <-subscriber.ctx.Done():
			hub.stopSubscriberLocked(id, subscriber)
			continue
		default:
		}
		select {
		case subscriber.live <- event.Clone():
		default:
			subscriber.overflowed = true
			subscriber.overflow <- localGap(0, event.Revision, event.At, "slow_subscriber")
		}
	}
}

func (hub *EventHub) runSubscriber(subscriber *eventSubscriber, after uint64, replay []Event) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	defer func() {
		close(subscriber.events)
		hub.mu.Lock()
		if current, exists := hub.subscribers[subscriber.id]; exists && current == subscriber {
			delete(hub.subscribers, subscriber.id)
		}
		hub.mu.Unlock()
	}()

	lastDelivered := after
	for _, event := range replay {
		if !hub.sendSubscriberEvent(subscriber, event, &lastDelivered, ticker.C) {
			return
		}
	}
	for {
		select {
		case <-subscriber.ctx.Done():
			return
		case <-subscriber.stop:
			return
		case gap := <-subscriber.overflow:
			hub.sendSubscriberGap(subscriber, gap, lastDelivered)
			return
		case event := <-subscriber.live:
			if !hub.sendSubscriberEvent(subscriber, event, &lastDelivered, ticker.C) {
				return
			}
		}
	}
}

func (hub *EventHub) sendSubscriberEvent(subscriber *eventSubscriber, event Event, lastDelivered *uint64, tick <-chan time.Time) bool {
	for {
		if len(subscriber.events) < subscriber.buffer {
			select {
			case <-subscriber.ctx.Done():
				return false
			case <-subscriber.stop:
				return false
			case gap := <-subscriber.overflow:
				hub.sendSubscriberGap(subscriber, gap, *lastDelivered)
				return false
			case subscriber.events <- event:
				*lastDelivered = event.Revision
				return true
			}
		}
		select {
		case <-subscriber.ctx.Done():
			return false
		case <-subscriber.stop:
			return false
		case gap := <-subscriber.overflow:
			hub.sendSubscriberGap(subscriber, gap, *lastDelivered)
			return false
		case <-tick:
		}
	}
}

func (hub *EventHub) sendSubscriberGap(subscriber *eventSubscriber, gap Event, lastDelivered uint64) {
	select {
	case <-subscriber.ctx.Done():
		return
	case <-subscriber.stop:
		return
	default:
	}
	gap.Gap.FromRevision = lastDelivered + 1
	if gap.Gap.FromRevision > gap.Gap.ToRevision {
		return
	}
	select {
	case <-subscriber.ctx.Done():
	case <-subscriber.stop:
	case subscriber.events <- gap:
	}
}

func (hub *EventHub) stopSubscriberLocked(id uint64, subscriber *eventSubscriber) {
	if subscriber.stopped {
		return
	}
	subscriber.stopped = true
	delete(hub.subscribers, id)
	close(subscriber.stop)
}

func validateAuthoritativeEvent(event Event) error {
	if !validIdentifier(string(event.TaskID)) {
		return eventHubError(ErrInvalidTask, "event task ID is invalid", false)
	}
	if event.Kind == EventGap {
		return eventHubError(ErrInvalidTransition, "Gap events are subscriber-local", false)
	}
	if err := event.ValidateOneOf(); err != nil {
		return eventHubError(ErrInvalidTransition, "event payload invariant failed", false)
	}
	if event.Agent != nil {
		if err := event.Agent.Validate(); err != nil {
			return eventHubError(ErrInvalidTransition, "agent event projection is invalid", false)
		}
		if !agentEventKindMatches(event.Kind, event.Agent.Kind) {
			return eventHubError(ErrInvalidTransition, "agent event kind does not match its projection", false)
		}
	}
	if event.Snapshot != nil && event.Snapshot.ID != event.TaskID {
		return eventHubError(ErrInvalidTask, "snapshot task ID does not match event", false)
	}
	if event.Completion != nil && event.Completion.ID != event.TaskID {
		return eventHubError(ErrInvalidTask, "completion task ID does not match event", false)
	}
	if event.Result != nil && event.Result.TaskID != event.TaskID {
		return eventHubError(ErrInvalidTask, "result task ID does not match event", false)
	}
	return nil
}

func agentEventKindMatches(kind EventKind, agentKind events.Type) bool {
	switch kind {
	case EventTextDelta:
		return agentKind == events.TextDelta
	case EventThinkingDelta:
		return agentKind == events.ThinkingDelta
	case EventTool:
		switch agentKind {
		case events.ToolPending, events.ToolRunning, events.ToolSuccess, events.ToolError, events.ToolDenied:
			return true
		default:
			return false
		}
	case EventConfirmationRequested:
		return agentKind == events.ToolWaitingConfirmation
	case EventUsage:
		return agentKind == events.UsageUpdated
	case EventDiagnostic:
		return agentKind == events.DiagnosticEmitted
	default:
		return false
	}
}

func terminalEventKind(kind EventKind) bool {
	switch kind {
	case EventCompletion, EventFailure, EventCancellation, EventTimeout, EventLimit:
		return true
	default:
		return false
	}
}

func nextAuthoritativeCounter(current uint64, terminal bool) (uint64, error) {
	if current == math.MaxUint64 || current == math.MaxUint64-1 && !terminal {
		return 0, eventHubError(ErrInternal, "event counter is exhausted", false)
	}
	return current + 1, nil
}

func localGap(from, to uint64, at time.Time, reason string) Event {
	return Event{
		Revision: to,
		At:       at,
		Kind:     EventGap,
		Gap: &GapDescriptor{
			FromRevision: from,
			ToRevision:   to,
			Reason:       reason,
		},
	}
}

func eventHubError(code ErrorCode, message string, recoverable bool) error {
	return SafeError(code, redact.NewRuntimeRedactor().Redact(message), recoverable)
}

func newEventRing(limit int) eventRing {
	return eventRing{limit: limit, events: make([]Event, 0, limit)}
}

// append returns true when the oldest retained event was evicted.
func (ring *eventRing) append(event Event) bool {
	if len(ring.events) < ring.limit {
		ring.events = append(ring.events, event)
		return false
	}
	ring.events[ring.start] = event
	ring.start = (ring.start + 1) % ring.limit
	return true
}

func (ring *eventRing) len() int {
	return len(ring.events)
}

func (ring *eventRing) all() []Event {
	result := make([]Event, 0, len(ring.events))
	for index := 0; index < len(ring.events); index++ {
		result = append(result, ring.events[(ring.start+index)%len(ring.events)])
	}
	return result
}

func (ring *eventRing) after(revision uint64) []Event {
	result := make([]Event, 0, len(ring.events))
	for _, event := range ring.all() {
		if event.Revision > revision {
			result = append(result, event)
		}
	}
	return result
}

func (ring *eventRing) firstRevision() (uint64, bool) {
	if len(ring.events) == 0 {
		return 0, false
	}
	return ring.events[ring.start].Revision, true
}

func (ring *eventRing) clear() {
	ring.start = 0
	ring.events = make([]Event, 0, ring.limit)
}
