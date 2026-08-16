package app

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tui"
)

type EventType = events.Type

type Event = events.Event

const (
	EventUserSubmitted           = events.UserSubmitted
	EventTextDelta               = events.TextDelta
	EventThinkingDelta           = events.ThinkingDelta
	EventToolPending             = events.ToolPending
	EventToolWaitingConfirmation = events.ToolWaitingConfirmation
	EventToolRunning             = events.ToolRunning
	EventToolSuccess             = events.ToolSuccess
	EventToolError               = events.ToolError
	EventToolDenied              = events.ToolDenied
	EventAgentProgress           = events.AgentProgressed
	EventUsageUpdated            = events.UsageUpdated
	EventMainTraceReset          = events.MainTraceReset
	EventDone                    = events.Done
	EventError                   = events.Error
)

const (
	staleRequestEventDiagnosticCode   = "stale_request_event"
	staleRequestEventDiagnosticSource = "app.event_boundary"
	eventDiagnosticMaxItems           = int64(100)
	eventDiagnosticMaxItemBytes       = int64(2 << 10)
	eventDiagnosticMaxTotalBytes      = int64(2 << 20)
	taskDetailLiveEventLimit          = 1_024
	taskEventRetryDelay               = 25 * time.Millisecond
)

var (
	errEventBoundaryGenerationExhausted = errors.New("app: event boundary generation exhausted")
	errEventBoundaryConversationMissing = errors.New("app: event boundary conversation is missing")
)

// eventEnvelope binds one safe Event stream to the App request and
// conversation that created it. It contains identity only, never a service,
// channel, domain object, error, or event payload.
type eventEnvelope struct {
	Generation     uint64
	ConversationID string
}

// sealedEventEnvelope is the immutable App publication candidate for one
// safe Event. The identity is captured from the three-layer state and every
// pointer-bearing DTO is defensively copied before asynchronous delivery.
type sealedEventEnvelope struct {
	identity eventEnvelope
	value    Event
}

func sealStateEvent(request RequestState, conversation ConversationState, event Event) (*sealedEventEnvelope, bool) {
	conversationID := strings.TrimSpace(conversation.ActiveID)
	if request.Generation == 0 || conversationID == "" || conversation.SkillGeneration != request.Generation {
		return nil, false
	}
	return &sealedEventEnvelope{
		identity: eventEnvelope{Generation: request.Generation, ConversationID: conversationID},
		value:    cloneAppEvent(event),
	}, true
}

func (candidate *sealedEventEnvelope) snapshot() (eventEnvelope, Event, bool) {
	if candidate == nil {
		return eventEnvelope{}, Event{}, false
	}
	return candidate.identity, cloneAppEvent(candidate.value), true
}

// eventBoundaryState owns the bounded stale-event diagnostic boundary. The
// legacy fields keep the current Bubble Tea stream adapter coherent until the
// three-layer Model is published; applyStateEvent never reads them and treats
// RuntimeState/ConversationState/RequestState as the only authoritative state.
type eventBoundaryState struct {
	mu             sync.Mutex
	legacySequence uint64
	legacyCurrent  eventEnvelope
	diagnostics    diagnostics.BoundedSink
}

func newEventBoundaryState(sink diagnostics.BoundedSink) *eventBoundaryState {
	if sink == nil {
		panic("app: nil event boundary diagnostics sink")
	}
	return &eventBoundaryState{diagnostics: sink}
}

func newDefaultEventBoundaryState(redactor *redact.RuntimeRedactor) *eventBoundaryState {
	if redactor == nil {
		redactor = redact.NewRuntimeRedactor()
	}
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      eventDiagnosticMaxItems,
		MaxItemBytes:  eventDiagnosticMaxItemBytes,
		MaxTotalBytes: eventDiagnosticMaxTotalBytes,
	})
	if err != nil {
		panic(err)
	}
	return newEventBoundaryState(sink)
}

// newRuntimeEventBoundaryState reuses the App runtime's process-owned C7 sink
// whenever one was injected by Assembly. Compatibility callers that predate
// RuntimeOptions still receive the historical bounded local fallback.
func newRuntimeEventBoundaryState(options *RuntimeOptions, redactor *redact.RuntimeRedactor) *eventBoundaryState {
	if options != nil && options.Diagnostics != nil {
		return newEventBoundaryState(options.Diagnostics)
	}
	boundary := newDefaultEventBoundaryState(redactor)
	if options != nil {
		options.Diagnostics = boundary.diagnostics
	}
	return boundary
}

func (state *eventBoundaryState) begin(conversationID string) (eventEnvelope, error) {
	if state == nil {
		return eventEnvelope{}, errors.New("app: nil event boundary state")
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return eventEnvelope{}, errEventBoundaryConversationMissing
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.legacySequence == math.MaxUint64 {
		return eventEnvelope{}, errEventBoundaryGenerationExhausted
	}
	next := eventEnvelope{Generation: state.legacySequence + 1, ConversationID: conversationID}
	state.legacySequence = next.Generation
	state.legacyCurrent = next
	return next, nil
}

func (state *eventBoundaryState) accepts(candidate eventEnvelope) bool {
	if state == nil {
		return false
	}
	state.mu.Lock()
	accepted := candidate.Generation != 0 && candidate == state.legacyCurrent
	state.mu.Unlock()
	if !accepted {
		state.recordStale()
	}
	return accepted
}

func (state *eventBoundaryState) finish(candidate eventEnvelope) bool {
	if state == nil {
		return false
	}
	state.mu.Lock()
	accepted := candidate.Generation != 0 && candidate == state.legacyCurrent
	if accepted {
		state.legacyCurrent = eventEnvelope{}
	}
	state.mu.Unlock()
	if !accepted {
		state.recordStale()
	}
	return accepted
}

// applyStateEvent is the only T4.11 publication point into RuntimeState,
// ConversationState and RequestState. It validates the envelope against the
// current three-layer boundary before touching any field; a rejected event is
// reduced to one content-free bounded diagnostic.
func (state *eventBoundaryState) applyStateEvent(
	candidate *sealedEventEnvelope,
	runtime *RuntimeState,
	conversation *ConversationState,
	request *RequestState,
) bool {
	identity, event, ok := candidate.snapshot()
	if !ok || runtime == nil || conversation == nil || request == nil ||
		identity.Generation == 0 || strings.TrimSpace(identity.ConversationID) == "" ||
		runtime.RequestSequence != request.Generation || request.Generation != identity.Generation ||
		conversation.SkillGeneration != request.Generation || conversation.ActiveID != identity.ConversationID {
		state.recordStale()
		return false
	}

	if event.Transient && strings.TrimSpace(event.IndependentID) != "" {
		request.TransientIDs = appendUniqueEventID(request.TransientIDs, event.IndependentID)
	}
	switch event.Type {
	case EventUserSubmitted, EventTextDelta, EventThinkingDelta:
		if !event.Transient {
			conversation.Messages = append(conversation.Messages, event.Text)
		}
	case EventToolWaitingConfirmation:
		if event.Confirmation != nil {
			request.Confirmation = &ConfirmationState{
				CallID:         event.Confirmation.CallID,
				Name:           event.Confirmation.Name,
				Prompt:         event.Confirmation.Prompt,
				Target:         event.Confirmation.Target,
				Risk:           event.Confirmation.Risk,
				PermissionMode: event.Confirmation.PermissionMode,
				ScopePreview:   event.Confirmation.ScopePreview,
				RuleLocation:   event.Confirmation.RuleLocation,
				Scopes:         confirmationScopeStates(event.Confirmation.Scopes),
				Warning:        event.Confirmation.Warning,
				RevokeHint:     event.Confirmation.RevokeHint,
				AllowPermanent: event.Confirmation.AllowPermanent,
			}
		}
	case EventAgentProgress:
		if event.Progress != nil {
			request.StopReason = event.Progress.StopReason
		}
	case EventUsageUpdated:
		if event.Usage != nil {
			request.Tokens = Usage{InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens}
			request.Cache = CacheUsage{
				CacheCreationInputTokens: event.Usage.CacheCreationInputTokens,
				CacheReadInputTokens:     event.Usage.CacheReadInputTokens,
			}
		}
	case EventDone:
		request.Duration = event.Duration
	case EventError:
		request.LastError = cloneAppSafeError(event.Err)
	}
	return true
}

func appendUniqueEventID(source []string, value string) []string {
	for _, existing := range source {
		if existing == value {
			return source
		}
	}
	return append(append([]string(nil), source...), value)
}

func cloneAppEvent(source Event) Event {
	clone := source
	clone.Err = cloneAppSafeError(source.Err)
	if source.Tool != nil {
		toolClone := *source.Tool
		if source.Tool.Artifact != nil {
			artifactClone := *source.Tool.Artifact
			toolClone.Artifact = &artifactClone
		}
		clone.Tool = &toolClone
	}
	if source.Confirmation != nil {
		confirmationClone := *source.Confirmation
		confirmationClone.Scopes = cloneConfirmationScopeDisplays(source.Confirmation.Scopes)
		clone.Confirmation = &confirmationClone
	}
	if source.Diagnostic != nil {
		diagnosticClone := *source.Diagnostic
		clone.Diagnostic = &diagnosticClone
	}
	if source.Progress != nil {
		progressClone := *source.Progress
		clone.Progress = &progressClone
	}
	if source.Usage != nil {
		usageClone := *source.Usage
		clone.Usage = &usageClone
	}
	return clone
}

func cloneConfirmationScopeDisplays(source []events.ConfirmationScopeDisplay) []events.ConfirmationScopeDisplay {
	if source == nil {
		return nil
	}
	clone := make([]events.ConfirmationScopeDisplay, len(source))
	copy(clone, source)
	return clone
}

func confirmationScopeStates(source []events.ConfirmationScopeDisplay) []ConfirmationScopeState {
	if source == nil {
		return nil
	}
	states := make([]ConfirmationScopeState, len(source))
	for index, scope := range source {
		states[index] = ConfirmationScopeState{
			Scope: scope.Scope, Available: scope.Available, Description: scope.Description,
		}
	}
	return states
}

func cloneAppSafeError(source *diagnostics.SafeError) *diagnostics.SafeError {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}

func (state *eventBoundaryState) recordStale() {
	if state == nil || state.diagnostics == nil {
		return
	}
	state.diagnostics.Add(diagnostics.SanitizeInput{
		Code:     staleRequestEventDiagnosticCode,
		Source:   staleRequestEventDiagnosticSource,
		Severity: diagnostics.SeverityWarning,
	})
}

func (m *Model) acceptsEventEnvelope(candidate eventEnvelope) bool {
	if m == nil || m.eventBoundary == nil {
		return false
	}
	if !m.matchesActiveEventEnvelope(candidate) {
		m.eventBoundary.recordStale()
		return false
	}
	return m.eventBoundary.accepts(candidate)
}

func (m *Model) finishEventEnvelope(candidate eventEnvelope) bool {
	if m == nil || m.eventBoundary == nil {
		return false
	}
	if !m.matchesActiveEventEnvelope(candidate) {
		m.eventBoundary.recordStale()
		return false
	}
	return m.eventBoundary.finish(candidate)
}

func (m *Model) matchesActiveEventEnvelope(candidate eventEnvelope) bool {
	if candidate.Generation == 0 || strings.TrimSpace(candidate.ConversationID) == "" || m.request == nil || m.conversation == nil {
		return false
	}
	return m.request.Generation == candidate.Generation &&
		m.request.ConversationID == candidate.ConversationID &&
		strings.TrimSpace(m.conversation.ID) == candidate.ConversationID
}

// taskEventStreamState is shared by Bubble Tea Model values and owns only
// consumer cancellation. Its contexts are never passed to task Cancel, so
// closing the UI or abandoning AwaitForeground cannot cancel a task directly.
type taskEventStreamState struct {
	mu           sync.Mutex
	rootCtx      context.Context
	rootCancel   context.CancelFunc
	streamCancel context.CancelFunc
	epoch        uint64
	closed       bool
}

func newTaskEventStreamState() *taskEventStreamState {
	ctx, cancel := context.WithCancel(context.Background())
	return &taskEventStreamState{rootCtx: ctx, rootCancel: cancel}
}

func (state *taskEventStreamState) beginSubscription() (context.Context, uint64, bool) {
	if state == nil {
		return nil, 0, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed || state.epoch == math.MaxUint64 {
		return nil, 0, false
	}
	if state.streamCancel != nil {
		state.streamCancel()
	}
	state.epoch++
	ctx, cancel := context.WithCancel(state.rootCtx)
	state.streamCancel = cancel
	return ctx, state.epoch, true
}

func (state *taskEventStreamState) beginResync() (context.Context, uint64, bool) {
	if state == nil {
		return nil, 0, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed || state.epoch == math.MaxUint64 {
		return nil, 0, false
	}
	if state.streamCancel != nil {
		state.streamCancel()
		state.streamCancel = nil
	}
	state.epoch++
	return state.rootCtx, state.epoch, true
}

func (state *taskEventStreamState) accepts(epoch uint64) bool {
	if state == nil || epoch == 0 {
		return false
	}
	state.mu.Lock()
	accepted := !state.closed && state.epoch == epoch
	state.mu.Unlock()
	return accepted
}

func (state *taskEventStreamState) context() (context.Context, bool) {
	if state == nil {
		return nil, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return nil, false
	}
	return state.rootCtx, true
}

func (state *taskEventStreamState) stopped() bool {
	if state == nil {
		return true
	}
	state.mu.Lock()
	stopped := state.closed
	state.mu.Unlock()
	return stopped
}

func (state *taskEventStreamState) close() {
	if state == nil {
		return
	}
	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return
	}
	state.closed = true
	if state.streamCancel != nil {
		state.streamCancel()
		state.streamCancel = nil
	}
	state.rootCancel()
	state.mu.Unlock()
}

type taskSubscribedMsg struct {
	state  *taskEventStreamState
	epoch  uint64
	after  uint64
	events <-chan subagent.Event
	err    error
}

type taskEventMsg struct {
	state  *taskEventStreamState
	epoch  uint64
	events <-chan subagent.Event
	event  subagent.Event
}

type taskEventStreamClosedMsg struct {
	state *taskEventStreamState
	epoch uint64
}

type taskEventResyncMsg struct {
	state         *taskEventStreamState
	epoch         uint64
	list          subagent.TaskListSnapshot
	detail        *subagent.TaskDetailSnapshot
	detailID      subagent.ID
	detailMissing bool
	detailErr     error
	err           error
}

type taskForegroundOutcomeMsg struct {
	state          *taskEventStreamState
	taskID         subagent.ID
	conversationID string
	outcome        subagent.ForegroundOutcome
	err            error
}

type taskEventRetryMode uint8

const (
	taskEventRetrySubscribe taskEventRetryMode = iota + 1
	taskEventRetryResync
)

type taskEventRetryMsg struct {
	state    *taskEventStreamState
	epoch    uint64
	mode     taskEventRetryMode
	after    uint64
	detailID subagent.ID
}

func subscribeTaskEvents(state *taskEventStreamState, service subagent.Service, after uint64) tea.Cmd {
	if state == nil || service == nil {
		return nil
	}
	ctx, epoch, ok := state.beginSubscription()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		events, err := service.Subscribe(ctx, after)
		if err == nil && events == nil {
			err = errors.New("task subscription returned no event stream")
		}
		return taskSubscribedMsg{state: state, epoch: epoch, after: after, events: events, err: err}
	}
}

func waitTaskEvent(state *taskEventStreamState, epoch uint64, stream <-chan subagent.Event) tea.Cmd {
	if state == nil || stream == nil {
		return nil
	}
	ctx, ok := state.context()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return taskEventStreamClosedMsg{state: state, epoch: epoch}
		case event, open := <-stream:
			if !open {
				return taskEventStreamClosedMsg{state: state, epoch: epoch}
			}
			return taskEventMsg{state: state, epoch: epoch, events: stream, event: event.Clone()}
		}
	}
}

func resyncTaskEvents(state *taskEventStreamState, service subagent.Service, detailID subagent.ID) tea.Cmd {
	if state == nil || service == nil {
		return nil
	}
	ctx, epoch, ok := state.beginResync()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		list, err := service.List(ctx)
		if err != nil {
			return taskEventResyncMsg{state: state, epoch: epoch, detailID: detailID, err: err}
		}
		var detail *subagent.TaskDetailSnapshot
		if detailID != "" {
			value, getErr := service.Get(ctx, detailID)
			if getErr != nil {
				code := taskErrorCode(getErr)
				return taskEventResyncMsg{
					state: state, epoch: epoch, list: list.Clone(), detailID: detailID,
					detailMissing: code == subagent.ErrTaskNotFound || code == subagent.ErrTaskExpired,
					detailErr:     getErr,
				}
			}
			cloned := value.Clone()
			detail = &cloned
		}
		return taskEventResyncMsg{state: state, epoch: epoch, list: list.Clone(), detail: detail, detailID: detailID}
	}
}

func awaitForegroundTask(state *taskEventStreamState, service subagent.Service, taskID subagent.ID, conversationID string) tea.Cmd {
	if state == nil || service == nil || taskID == "" || strings.TrimSpace(conversationID) == "" {
		return nil
	}
	ctx, ok := state.context()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		outcome, err := service.AwaitForeground(ctx, taskID)
		return taskForegroundOutcomeMsg{
			state: state, taskID: taskID, conversationID: conversationID, outcome: outcome.Clone(), err: err,
		}
	}
}

func retryTaskEvents(state *taskEventStreamState, epoch uint64, mode taskEventRetryMode, after uint64, detailID subagent.ID) tea.Cmd {
	if state == nil || epoch == 0 {
		return nil
	}
	ctx, ok := state.context()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		timer := time.NewTimer(taskEventRetryDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return taskEventStreamClosedMsg{state: state, epoch: epoch}
		case <-timer.C:
			return taskEventRetryMsg{state: state, epoch: epoch, mode: mode, after: after, detailID: detailID}
		}
	}
}

func taskErrorCode(err error) subagent.ErrorCode {
	var safe *diagnostics.SafeError
	if errors.As(err, &safe) {
		return subagent.ErrorCode(safe.Code)
	}
	return ""
}

func (m *Model) ensureTaskEventStreamState() *taskEventStreamState {
	if m == nil {
		return nil
	}
	if m.taskEvents == nil || m.taskEvents.stopped() {
		m.taskEvents = newTaskEventStreamState()
	}
	return m.taskEvents
}

func (m *Model) currentTaskDetailID() subagent.ID {
	if m == nil {
		return ""
	}
	return subagent.ID(m.taskDetailView.Task().ID())
}

func (m *Model) handleTaskSubscribed(message taskSubscribedMsg) tea.Cmd {
	if m == nil || message.state == nil || !message.state.accepts(message.epoch) {
		return nil
	}
	m.taskEvents = message.state
	if message.err != nil {
		if taskErrorCode(message.err) == subagent.ErrEventCursorExpired {
			return resyncTaskEvents(message.state, m.deps.Tasks, m.currentTaskDetailID())
		}
		m.publishTaskStreamError(message.err)
		if taskErrorCode(message.err) == subagent.ErrShutdown {
			return nil
		}
		return retryTaskEvents(message.state, message.epoch, taskEventRetrySubscribe, message.after, m.currentTaskDetailID())
	}
	return waitTaskEvent(message.state, message.epoch, message.events)
}

func (m *Model) handleTaskEvent(message taskEventMsg) tea.Cmd {
	if m == nil || message.state == nil || !message.state.accepts(message.epoch) {
		return nil
	}
	event := message.event.Clone()
	if event.Kind == subagent.EventGap {
		if !validTaskGap(event) {
			m.publishTaskStreamError(subagent.SafeError(subagent.ErrInternal, redact.NewRuntimeRedactor().Redact("任务事件缺口无效"), false))
			return waitTaskEvent(message.state, message.epoch, message.events)
		}
		return resyncTaskEvents(message.state, m.deps.Tasks, m.currentTaskDetailID())
	}
	if event.Revision == 0 || event.Sequence == 0 || event.TaskID == "" || event.ValidateOneOf() != nil || !validTaskEventPayload(event) {
		m.publishTaskStreamError(subagent.SafeError(subagent.ErrInternal, redact.NewRuntimeRedactor().Redact("任务事件无效"), false))
		return waitTaskEvent(message.state, message.epoch, message.events)
	}
	if event.Revision <= m.taskEventCursor {
		return waitTaskEvent(message.state, message.epoch, message.events)
	}
	ctx, ok := message.state.context()
	if !ok {
		return nil
	}
	if err := m.ApplyTaskEvent(ctx, event); err != nil {
		m.reduceTaskEventViews(event)
		return retryTaskEvents(message.state, message.epoch, taskEventRetrySubscribe, m.taskEventCursor, m.currentTaskDetailID())
	}
	m.reduceTaskEventViews(event)
	m.taskEventCursor = event.Revision
	return waitTaskEvent(message.state, message.epoch, message.events)
}

func (m *Model) handleTaskEventStreamClosed(message taskEventStreamClosedMsg) tea.Cmd {
	if m == nil || message.state == nil || !message.state.accepts(message.epoch) || message.state.stopped() {
		return nil
	}
	return resyncTaskEvents(message.state, m.deps.Tasks, m.currentTaskDetailID())
}

func (m *Model) handleTaskEventResync(message taskEventResyncMsg) tea.Cmd {
	if m == nil || message.state == nil || !message.state.accepts(message.epoch) {
		return nil
	}
	if message.err != nil {
		m.publishTaskStreamError(message.err)
		if taskErrorCode(message.err) == subagent.ErrShutdown {
			return nil
		}
		return retryTaskEvents(message.state, message.epoch, taskEventRetryResync, m.taskEventCursor, message.detailID)
	}
	if message.list.Watermark >= m.taskListView.Watermark() {
		m.taskListView = projectTaskListView(message.list)
		m.applyTaskListStatus(message.list)
	}
	currentDetailID := m.currentTaskDetailID()
	if message.detail != nil && currentDetailID == message.detailID && message.detail.Watermark >= m.taskDetailView.Watermark() {
		m.taskDetailView = projectTaskDetailView(*message.detail)
	} else if message.detailMissing && currentDetailID == message.detailID && message.list.Watermark >= m.taskDetailView.Watermark() {
		m.taskDetailView = tui.TaskDetailView{}
	}
	resumeWatermark := message.list.Watermark
	if current := m.taskListView.Watermark(); current > resumeWatermark {
		resumeWatermark = current
	}
	m.taskEventCursor = resumeWatermark
	if message.detailErr != nil && !message.detailMissing {
		m.publishTaskStreamError(message.detailErr)
	} else {
		m.status.Notice = m.redactText("任务事件已按权威快照恢复")
		m.status.Error = nil
	}
	return subscribeTaskEvents(message.state, m.deps.Tasks, resumeWatermark)
}

func (m *Model) handleTaskEventRetry(message taskEventRetryMsg) tea.Cmd {
	if m == nil || message.state == nil || !message.state.accepts(message.epoch) {
		return nil
	}
	switch message.mode {
	case taskEventRetrySubscribe:
		return subscribeTaskEvents(message.state, m.deps.Tasks, message.after)
	case taskEventRetryResync:
		return resyncTaskEvents(message.state, m.deps.Tasks, message.detailID)
	default:
		return nil
	}
}

func (m *Model) handleTaskForegroundOutcome(message taskForegroundOutcomeMsg) tea.Cmd {
	if m == nil || message.state == nil || (m.taskEvents != nil && m.taskEvents != message.state) ||
		m.conversation == nil || m.conversation.ID != message.conversationID {
		return nil
	}
	m.taskEvents = message.state
	if message.err != nil {
		if message.state.stopped() || errors.Is(message.err, context.Canceled) {
			return nil
		}
		m.publishTaskStreamError(message.err)
		return nil
	}
	if message.outcome.Detached == (message.outcome.Completion != nil) {
		m.publishTaskStreamError(subagent.SafeError(subagent.ErrInternal, redact.NewRuntimeRedactor().Redact("前台任务结果无效"), false))
		return nil
	}
	if message.outcome.Detached {
		m.status.Notice = m.redactText("任务已转入后台继续运行: id=" + string(message.taskID))
		m.status.Error = nil
		return nil
	}
	completion := message.outcome.Completion.Clone()
	if completion.ID != message.taskID || completion.Validate() != nil {
		m.publishTaskStreamError(subagent.SafeError(subagent.ErrInternal, redact.NewRuntimeRedactor().Redact("前台任务完成结果无效"), false))
		return nil
	}
	m.status.Notice = m.redactText(tui.NewTaskNotification(tui.TaskNotificationViewSpec{
		TaskID: string(completion.ID), Status: string(completion.Status), Summary: completion.Summary,
		StopReason: string(completion.StopReason), Error: projectTaskSafeError(completion.Error),
	}).View())
	m.status.Error = nil
	return nil
}

func (m *Model) publishTaskStreamError(source error) {
	if m == nil || source == nil {
		return
	}
	err := projectAppTaskError(m, source)
	m.status.Notice = ""
	m.status.Error = err
	m.lastError = err
}

func (m *Model) applyTaskListStatus(snapshot subagent.TaskListSnapshot) {
	if m == nil {
		return
	}
	m.status.TaskCount = len(snapshot.Tasks)
	m.status.RunningTasks = 0
	m.status.WaitingTaskConfirmations = 0
	for _, task := range snapshot.Tasks {
		if task.Status == subagent.StatusRunning {
			m.status.RunningTasks++
		}
		if task.Status == subagent.StatusWaitingConfirmation {
			m.status.WaitingTaskConfirmations++
		}
	}
}

func validTaskGap(event subagent.Event) bool {
	return event.ValidateOneOf() == nil && event.TaskID == "" && event.Sequence == 0 && event.Revision > 0 &&
		event.Gap != nil && event.Gap.FromRevision > 0 && event.Gap.ToRevision >= event.Gap.FromRevision &&
		event.Revision == event.Gap.ToRevision && strings.TrimSpace(event.Gap.Reason) != ""
}

func validTaskEventPayload(event subagent.Event) bool {
	if event.Snapshot != nil && event.Snapshot.ID != event.TaskID {
		return false
	}
	if event.Snapshot != nil && !event.Snapshot.Status.Valid() {
		return false
	}
	if event.Completion != nil {
		if event.Completion.ID != event.TaskID || event.Completion.Validate() != nil {
			return false
		}
	}
	if event.Result != nil {
		result := event.Result
		if result.TaskID != event.TaskID || strings.TrimSpace(result.NotificationID) == "" ||
			result.CompletionRevision == 0 || result.CompletionSequence == 0 || strings.TrimSpace(result.Parent.ConversationID) == "" {
			return false
		}
		completion := subagent.Completion{
			ID: result.TaskID, Status: result.Status, Summary: result.Summary, SummaryTruncated: result.SummaryTruncated,
			TruncationReason: result.TruncationReason, StopReason: result.StopReason, Usage: result.Usage,
			Error: result.Error, EndedAt: result.CreatedAt,
		}
		if completion.Validate() != nil {
			return false
		}
	}
	return true
}

// reduceTaskEventViews keeps the TUI's capability-free task state current
// without consulting TaskManager for every event. Snapshot and result payloads
// have already crossed the EventHub's safe boundary; each is cloned again by
// projectTaskViewSpec before the App-owned views retain it.
func (m *Model) reduceTaskEventViews(event subagent.Event) {
	if m == nil || event.Revision == 0 || event.TaskID == "" {
		return
	}

	listSpecs := taskViewSpecsFromList(m.taskListView)
	if event.Revision > m.taskListView.Watermark() {
		listSpecs = reduceTaskViewSpecs(listSpecs, event)
		sortTaskViewSpecs(listSpecs)
		m.taskListView = tui.NewTaskListView(tui.TaskListViewSpec{
			Watermark: event.Revision,
			Tasks:     listSpecs,
			Notice:    m.taskListView.Notice(),
		})
		m.applyTaskViewStatus()
	}

	detailID := subagent.ID(m.taskDetailView.Task().ID())
	if detailID == "" || event.Revision <= m.taskDetailView.Watermark() {
		return
	}
	detailSpec := taskDetailViewSpecFromView(m.taskDetailView)
	detailSpec.Watermark = event.Revision
	if detailID == event.TaskID {
		reduced := reduceTaskViewSpecs([]tui.TaskViewSpec{detailSpec.Task}, event)
		if len(reduced) == 1 {
			detailSpec.Task = reduced[0]
		}
		if !containsTaskEventRevision(detailSpec.RecentEvents, event.Revision) {
			detailSpec.RecentEvents = append(detailSpec.RecentEvents, projectTaskEventViewSpec(event))
			if excess := len(detailSpec.RecentEvents) - m.taskDetailEventLimit(); excess > 0 {
				detailSpec.RecentEvents = append([]tui.TaskEventViewSpec(nil), detailSpec.RecentEvents[excess:]...)
			}
		}
	}
	m.taskDetailView = tui.NewTaskDetailView(detailSpec)
}

func (m *Model) taskDetailEventLimit() int {
	if m != nil && m.deps.Config != nil && m.deps.Config.Subagent.Limits.MaxEventsPerTask > 0 {
		return m.deps.Config.Subagent.Limits.MaxEventsPerTask
	}
	return taskDetailLiveEventLimit
}

func reduceTaskViewSpecs(specs []tui.TaskViewSpec, event subagent.Event) []tui.TaskViewSpec {
	if event.Snapshot == nil && event.Result == nil {
		return specs
	}
	index := -1
	for candidate := range specs {
		if specs[candidate].ID == string(event.TaskID) {
			index = candidate
			break
		}
	}
	if index >= 0 && specs[index].Revision >= event.Revision {
		return specs
	}

	var next tui.TaskViewSpec
	if index >= 0 {
		next = specs[index]
	} else {
		next.ID = string(event.TaskID)
	}
	if event.Snapshot != nil {
		next = projectTaskViewSpec(event.Snapshot.Clone())
	}
	if event.Result != nil {
		applyTaskResultToViewSpec(&next, event.Result.Clone())
	}
	next.Revision = event.Revision
	if index >= 0 {
		specs[index] = next
		return specs
	}
	return append(specs, next)
}

func applyTaskResultToViewSpec(spec *tui.TaskViewSpec, result subagent.ResultNotification) {
	if spec == nil {
		return
	}
	spec.ID = string(result.TaskID)
	spec.Status = string(result.Status)
	spec.StopReason = string(result.StopReason)
	spec.Summary = result.Summary
	spec.SummaryTruncated = result.SummaryTruncated
	spec.TruncationReason = result.TruncationReason
	spec.Error = projectTaskSafeError(result.Error)
	spec.Usage = tui.TaskUsageViewSpec{
		InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens,
		CacheCreationInputTokens: result.Usage.CacheCreationInputTokens, CacheReadInputTokens: result.Usage.CacheReadInputTokens,
	}
	if !result.CreatedAt.IsZero() {
		spec.HasEndedAt = true
		spec.EndedAtUnixMilli = result.CreatedAt.UnixMilli()
	}
	spec.PendingConfirmation = tui.ConfirmationViewSpec{}
}

func taskViewSpecsFromList(view tui.TaskListView) []tui.TaskViewSpec {
	tasks := view.Tasks()
	if tasks == nil {
		return nil
	}
	specs := make([]tui.TaskViewSpec, len(tasks))
	for index := range tasks {
		specs[index] = taskViewSpecFromView(tasks[index])
	}
	return specs
}

func taskViewSpecFromView(view tui.TaskView) tui.TaskViewSpec {
	startedAt, hasStartedAt := view.StartedAtUnixMilli()
	endedAt, hasEndedAt := view.EndedAtUnixMilli()
	errorView, hasError := view.Error()
	usage := view.Usage()
	spec := tui.TaskViewSpec{
		ID: view.ID(), Revision: view.Revision(), Type: view.Type(), Origin: view.Origin(), Role: view.Role(),
		RoleSource: view.RoleSource(), RoleSourceID: view.RoleSourceID(), RoleProviderID: view.RoleProviderID(),
		RoleOrigin: view.RoleOrigin(), RoleGeneration: view.RoleGeneration(), Placement: view.Placement(), Status: view.Status(),
		CreatedAtUnixMilli: view.CreatedAtUnixMilli(), HasStartedAt: hasStartedAt, StartedAtUnixMilli: startedAt,
		HasEndedAt: hasEndedAt, EndedAtUnixMilli: endedAt, Iteration: view.Iteration(), MaxIterations: view.MaxIterations(),
		StopReason: view.StopReason(), Summary: view.Summary(), SummaryTruncated: view.SummaryTruncated(),
		TruncationReason: view.TruncationReason(), EventsDropped: view.EventsDropped(),
		Error: tui.SafeErrorViewSpec{
			Present: hasError, Code: errorView.Code(), Source: errorView.Source(), Message: errorView.Message(), Recoverable: errorView.Recoverable(),
		},
		Usage: tui.TaskUsageViewSpec{
			InputTokens: usage.InputTokens(), OutputTokens: usage.OutputTokens(),
			CacheCreationInputTokens: usage.CacheCreationInputTokens(), CacheReadInputTokens: usage.CacheReadInputTokens(),
		},
	}
	if confirmation, present := view.PendingConfirmation(); present {
		spec.PendingConfirmation = confirmationViewSpecFromView(confirmation)
	}
	return spec
}

func confirmationViewSpecFromView(view tui.ConfirmationView) tui.ConfirmationViewSpec {
	scopes := view.Scopes()
	projected := make([]tui.ConfirmationScopeViewSpec, len(scopes))
	for index := range scopes {
		projected[index] = tui.ConfirmationScopeViewSpec{
			Scope: scopes[index].Scope(), Available: scopes[index].Available(), Description: scopes[index].Description(),
		}
	}
	return tui.ConfirmationViewSpec{
		Present: true, ConfirmationID: view.ConfirmationID(), CallID: view.CallID(), Name: view.Name(), Prompt: view.Prompt(),
		Target: view.Target(), Risk: view.Risk(), PermissionMode: view.PermissionMode(), ScopePreview: view.ScopePreview(),
		RuleLocation: view.RuleLocation(), Scopes: projected, Warning: view.Warning(), RevokeHint: view.RevokeHint(),
		// Child tasks cannot grant permanent authorization, even when rebuilding
		// a previously projected view.
		AllowPermanent: false,
	}
}

func taskDetailViewSpecFromView(view tui.TaskDetailView) tui.TaskDetailViewSpec {
	events := view.RecentEvents()
	projected := make([]tui.TaskEventViewSpec, len(events))
	for index := range events {
		projected[index] = taskEventViewSpecFromView(events[index])
	}
	return tui.TaskDetailViewSpec{Watermark: view.Watermark(), Task: taskViewSpecFromView(view.Task()), RecentEvents: projected}
}

func taskEventViewSpecFromView(view tui.TaskEventView) tui.TaskEventViewSpec {
	errorView, hasError := view.Error()
	return tui.TaskEventViewSpec{
		Revision: view.Revision(), Sequence: view.Sequence(), AtUnixMilli: view.AtUnixMilli(), Kind: view.Kind(),
		Status: view.Status(), Placement: view.Placement(), ToolName: view.ToolName(), Text: view.Text(),
		Error: tui.SafeErrorViewSpec{
			Present: hasError, Code: errorView.Code(), Source: errorView.Source(), Message: errorView.Message(), Recoverable: errorView.Recoverable(),
		},
	}
}

func containsTaskEventRevision(events []tui.TaskEventViewSpec, revision uint64) bool {
	for index := range events {
		if events[index].Revision == revision {
			return true
		}
	}
	return false
}

func sortTaskViewSpecs(tasks []tui.TaskViewSpec) {
	sort.Slice(tasks, func(left, right int) bool {
		leftTerminal := taskViewStatusTerminal(tasks[left].Status)
		rightTerminal := taskViewStatusTerminal(tasks[right].Status)
		if leftTerminal != rightTerminal {
			return !leftTerminal
		}
		if !leftTerminal {
			leftRank := taskViewActiveStatusRank(tasks[left].Status)
			rightRank := taskViewActiveStatusRank(tasks[right].Status)
			if leftRank != rightRank {
				return leftRank < rightRank
			}
			if tasks[left].CreatedAtUnixMilli != tasks[right].CreatedAtUnixMilli {
				return tasks[left].CreatedAtUnixMilli > tasks[right].CreatedAtUnixMilli
			}
			return tasks[left].ID < tasks[right].ID
		}
		if tasks[left].HasEndedAt && tasks[right].HasEndedAt && tasks[left].EndedAtUnixMilli != tasks[right].EndedAtUnixMilli {
			return tasks[left].EndedAtUnixMilli > tasks[right].EndedAtUnixMilli
		}
		return tasks[left].ID < tasks[right].ID
	})
}

func taskViewStatusTerminal(status string) bool {
	return subagent.IsTerminal(subagent.Status(status))
}

func taskViewActiveStatusRank(status string) int {
	switch subagent.Status(status) {
	case subagent.StatusQueued:
		return 0
	case subagent.StatusRunning:
		return 1
	case subagent.StatusWaitingConfirmation:
		return 2
	default:
		return 3
	}
}

func (m *Model) applyTaskViewStatus() {
	if m == nil {
		return
	}
	tasks := m.taskListView.Tasks()
	m.status.TaskCount = len(tasks)
	m.status.RunningTasks = 0
	m.status.WaitingTaskConfirmations = 0
	for index := range tasks {
		switch subagent.Status(tasks[index].Status()) {
		case subagent.StatusRunning:
			m.status.RunningTasks++
		case subagent.StatusWaitingConfirmation:
			m.status.WaitingTaskConfirmations++
		}
	}
}
