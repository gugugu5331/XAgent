package subagent

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"xagent/internal/agentrole"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
)

const (
	defaultManagerShutdownTimeout = 5 * time.Second
	resultRetryInterval           = 25 * time.Millisecond
	maxCompletionReasonBytes      = 256
)

var managerRolePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ManagerOptions configures the task lifecycle boundary. Limits are expected
// to be fully resolved before construction; Manager never silently widens an
// invalid policy.
type ManagerOptions struct {
	Runner          RunnerFactory
	Limits          Limits
	Inbox           ResultInbox
	Redactor        *redact.RuntimeRedactor
	Clock           func() time.Time
	IDGenerator     func() (ID, error)
	ShutdownTimeout time.Duration
}

// Manager owns task state, the FIFO scheduler and the authoritative event
// projection. Every TaskSnapshot mutation after registration happens while mu
// is held through one of the reduce*Locked methods below.
type Manager struct {
	admissionMu sync.Mutex
	mu          sync.Mutex
	idMu        sync.Mutex

	runner          RunnerFactory
	limits          Limits
	inbox           ResultInbox
	redactor        *redact.RuntimeRedactor
	clock           func() time.Time
	idGenerator     func() (ID, error)
	shutdownTimeout time.Duration
	hub             *EventHub

	lifecycleCtx    context.Context
	lifecycleCancel context.CancelCauseFunc
	accepting       bool
	shuttingDown    bool

	tasks      map[ID]*managedTask
	queue      []*managedTask
	running    int
	tombstones map[ID]taskTombstone
	usedIDs    map[string]struct{}

	runWG        sync.WaitGroup
	shutdownOnce sync.Once
	shutdownDone chan struct{}
}

type managedTask struct {
	input    SubmitInput
	prepared PreparedTask
	snapshot TaskSnapshot

	ctx    context.Context
	cancel context.CancelCauseFunc

	done           chan struct{}
	foregroundDone chan struct{}

	completion           *Completion
	terminalRevision     uint64
	terminalSequence     uint64
	foregroundSignalled  bool
	foregroundDetached   bool
	reservationReleased  bool
	resultPublished      bool
	resultRetryStarted   bool
	resultFailureEmitted bool
	pendingNotification  *ResultNotification

	runningSlot          bool
	durationTimer        *time.Timer
	firstRequestObserved bool
}

type taskTombstone struct {
	id        ID
	evictedAt time.Time
}

// NewManager constructs an isolated task lifecycle. Task contexts derive from
// the manager lifecycle, never from a Submit caller's deadline.
func NewManager(options ManagerOptions) (Service, error) {
	if options.Runner == nil || options.Inbox == nil || options.Redactor == nil || options.IDGenerator == nil {
		return nil, managerStaticError(ErrInternal, "subagent manager dependencies are unavailable", false)
	}
	if err := options.Limits.Validate(); err != nil {
		return nil, managerStaticError(ErrInternal, "subagent manager limits are invalid", false)
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	if options.ShutdownTimeout == 0 {
		options.ShutdownTimeout = defaultManagerShutdownTimeout
	}
	if options.ShutdownTimeout < 0 || options.ShutdownTimeout > 24*time.Hour {
		return nil, managerStaticError(ErrInternal, "subagent shutdown timeout is invalid", false)
	}
	hub, err := NewEventHub(EventHubOptions{Limits: options.Limits, Clock: options.Clock})
	if err != nil {
		return nil, managerStaticError(ErrInternal, "subagent event hub initialization failed", false)
	}
	lifecycleCtx, lifecycleCancel := context.WithCancelCause(context.Background())
	return &Manager{
		runner:          options.Runner,
		limits:          options.Limits,
		inbox:           options.Inbox,
		redactor:        options.Redactor,
		clock:           options.Clock,
		idGenerator:     options.IDGenerator,
		shutdownTimeout: options.ShutdownTimeout,
		hub:             hub,
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		accepting:       true,
		tasks:           make(map[ID]*managedTask),
		tombstones:      make(map[ID]taskTombstone),
		usedIDs:         make(map[string]struct{}),
		shutdownDone:    make(chan struct{}),
	}, nil
}

func (manager *Manager) Submit(ctx context.Context, input SubmitInput) (Submission, error) {
	if manager == nil {
		return Submission{}, managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	normalized, placement, err := manager.normalizeSubmit(ctx, input)
	if err != nil {
		return Submission{}, err
	}

	// Reserve and Prepare are one serial admission transaction. This keeps a
	// slow preparation from allowing concurrent callers to oversubscribe the
	// bounded queue while still allowing running workers to complete.
	manager.admissionMu.Lock()
	defer manager.admissionMu.Unlock()

	manager.mu.Lock()
	if !manager.accepting {
		manager.mu.Unlock()
		return Submission{}, manager.error(ErrShutdown, "subagent admission is closed", false)
	}
	manager.evictTerminalLocked(manager.limits.MaxRetainedTasks - 1)
	if len(manager.tasks) >= manager.limits.MaxRetainedTasks ||
		(manager.running >= manager.limits.MaxConcurrent && len(manager.queue) >= manager.limits.MaxQueued) {
		manager.mu.Unlock()
		return Submission{}, manager.error(ErrQueueFull, "subagent task queue is full", true)
	}
	manager.mu.Unlock()

	id, err := manager.generateID()
	if err != nil {
		return Submission{}, err
	}
	if err := manager.inbox.Reserve(ctx, id, normalized.Parent); err != nil {
		return Submission{}, manager.externalError(err, ErrInboxFull, "subagent result capacity is unavailable", true)
	}

	prepared, prepareErr := manager.prepareTask(ctx, id, normalized)
	if prepareErr != nil || prepared == nil {
		_ = manager.inbox.ReleaseReservation(context.Background(), id, normalized.Parent)
		if prepareErr == nil {
			prepareErr = manager.error(ErrInternal, "subagent preparation returned no task", false)
		}
		return Submission{}, manager.externalError(prepareErr, ErrInternal, "subagent preparation failed", false)
	}
	metadata, metadataErr := manager.preparedMetadata(prepared, normalized)
	if metadataErr != nil {
		_ = manager.inbox.ReleaseReservation(context.Background(), id, normalized.Parent)
		return Submission{}, metadataErr
	}

	taskCtx, taskCancel := context.WithCancelCause(manager.lifecycleCtx)
	createdAt := manager.clock()
	record := &managedTask{
		input:    normalized,
		prepared: prepared,
		snapshot: TaskSnapshot{
			ID:             id,
			Type:           metadata.Type,
			Origin:         normalized.Origin,
			Role:           metadata.Role,
			RoleSource:     metadata.RoleSource,
			RoleSourceID:   metadata.RoleSourceID,
			RoleProviderID: metadata.RoleProviderID,
			RoleOrigin:     metadata.RoleOrigin,
			RoleGeneration: metadata.RoleGeneration,
			Placement:      placement,
			Status:         StatusQueued,
			Parent:         normalized.Parent,
			CreatedAt:      createdAt,
			MaxIterations:  metadata.MaxIterations,
		},
		ctx:            taskCtx,
		cancel:         taskCancel,
		done:           make(chan struct{}),
		foregroundDone: make(chan struct{}),
	}
	if placement == Background {
		record.foregroundDetached = true
		record.foregroundSignalled = true
		close(record.foregroundDone)
	}
	if err := manager.installFirstRequestObserver(prepared, id); err != nil {
		taskCancel(err)
		_ = manager.inbox.ReleaseReservation(context.Background(), id, normalized.Parent)
		return Submission{}, err
	}

	manager.mu.Lock()
	if !manager.accepting {
		manager.mu.Unlock()
		taskCancel(manager.error(ErrShutdown, "subagent admission is closed", false))
		_ = manager.inbox.ReleaseReservation(context.Background(), id, normalized.Parent)
		return Submission{}, manager.error(ErrShutdown, "subagent admission is closed", false)
	}
	queued, publishErr := manager.publishSnapshotLocked(record, EventQueued)
	if publishErr != nil {
		manager.mu.Unlock()
		taskCancel(publishErr)
		_ = manager.inbox.ReleaseReservation(context.Background(), id, normalized.Parent)
		return Submission{}, manager.externalError(publishErr, ErrInternal, "subagent registration failed", false)
	}
	manager.tasks[id] = record
	manager.queue = append(manager.queue, record)
	manager.dispatchLocked()
	manager.mu.Unlock()

	if placement == Foreground {
		go manager.watchParentCancellation(id, ctx)
	}
	return Submission{
		ID:        id,
		Type:      metadata.Type,
		Role:      metadata.Role,
		Origin:    normalized.Origin,
		Parent:    normalized.Parent,
		Placement: placement,
		Status:    StatusQueued,
		Revision:  queued.Revision,
		CreatedAt: createdAt,
	}, nil
}

func (manager *Manager) List(ctx context.Context) (TaskListSnapshot, error) {
	if manager == nil {
		return TaskListSnapshot{}, managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	if err := validCallContext(ctx); err != nil {
		return TaskListSnapshot{}, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	tasks := make([]TaskSnapshot, 0, len(manager.tasks))
	for _, record := range manager.tasks {
		tasks = append(tasks, record.snapshot.Clone())
	}
	sortTaskSnapshots(tasks)
	return TaskListSnapshot{Watermark: manager.hub.Watermark(), Tasks: tasks}, nil
}

func (manager *Manager) Get(ctx context.Context, id ID) (TaskDetailSnapshot, error) {
	if manager == nil {
		return TaskDetailSnapshot{}, managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	if err := validCallContext(ctx); err != nil {
		return TaskDetailSnapshot{}, err
	}
	if !manager.validID(id) {
		return TaskDetailSnapshot{}, manager.error(ErrInvalidTask, "subagent task identity is invalid", true)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	record := manager.tasks[id]
	if record == nil {
		return TaskDetailSnapshot{}, manager.missingTaskErrorLocked(id)
	}
	watermark, recent, dropped, err := manager.hub.TaskEvents(id)
	if err != nil {
		return TaskDetailSnapshot{}, manager.externalError(err, ErrInternal, "subagent task trajectory is unavailable", false)
	}
	snapshot := record.snapshot.Clone()
	snapshot.EventsDropped = dropped
	return TaskDetailSnapshot{Watermark: watermark, Task: snapshot, RecentEvents: recent}, nil
}

func (manager *Manager) Subscribe(ctx context.Context, after uint64) (<-chan Event, error) {
	if manager == nil {
		return nil, managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	return manager.hub.Subscribe(ctx, after)
}

func (manager *Manager) Await(ctx context.Context, id ID) (Completion, error) {
	if manager == nil {
		return Completion{}, managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	if err := validCallContext(ctx); err != nil {
		return Completion{}, err
	}
	if !manager.validID(id) {
		return Completion{}, manager.error(ErrInvalidTask, "subagent task identity is invalid", true)
	}
	manager.mu.Lock()
	record := manager.tasks[id]
	if record == nil {
		err := manager.missingTaskErrorLocked(id)
		manager.mu.Unlock()
		return Completion{}, err
	}
	done := record.done
	manager.mu.Unlock()

	select {
	case <-ctx.Done():
		return Completion{}, context.Cause(ctx)
	case <-done:
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if record.completion == nil {
		return Completion{}, manager.error(ErrInternal, "subagent terminal projection is unavailable", false)
	}
	return record.completion.Clone(), nil
}

func (manager *Manager) AwaitForeground(ctx context.Context, id ID) (ForegroundOutcome, error) {
	if manager == nil {
		return ForegroundOutcome{}, managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	if err := validCallContext(ctx); err != nil {
		return ForegroundOutcome{}, err
	}
	if !manager.validID(id) {
		return ForegroundOutcome{}, manager.error(ErrInvalidTask, "subagent task identity is invalid", true)
	}
	manager.mu.Lock()
	record := manager.tasks[id]
	if record == nil {
		err := manager.missingTaskErrorLocked(id)
		manager.mu.Unlock()
		return ForegroundOutcome{}, err
	}
	foregroundDone := record.foregroundDone
	manager.mu.Unlock()

	select {
	case <-ctx.Done():
		return ForegroundOutcome{}, context.Cause(ctx)
	case <-foregroundDone:
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if record.foregroundDetached || record.snapshot.Placement == Background {
		return ForegroundOutcome{Detached: true}, nil
	}
	if record.completion == nil {
		return ForegroundOutcome{}, manager.error(ErrInternal, "subagent foreground outcome is unavailable", false)
	}
	if !record.reservationReleased {
		if err := manager.inbox.ReleaseReservation(context.Background(), id, record.snapshot.Parent); err != nil {
			return ForegroundOutcome{}, manager.externalError(err, ErrInternal, "subagent foreground reservation cleanup failed", false)
		}
		record.reservationReleased = true
	}
	completion := record.completion.Clone()
	return ForegroundOutcome{Completion: &completion}, nil
}

func (manager *Manager) ClaimResults(ctx context.Context, options ResultClaimOptions) (ResultClaim, error) {
	if manager == nil {
		return ResultClaim{}, managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	return manager.inbox.Claim(ctx, options)
}

func (manager *Manager) AckResults(ctx context.Context, claimID string, owner ParentRef) error {
	if manager == nil {
		return managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	return manager.inbox.Ack(ctx, claimID, owner)
}

func (manager *Manager) ReleaseResults(ctx context.Context, claimID string, owner ParentRef) error {
	if manager == nil {
		return managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	return manager.inbox.Release(ctx, claimID, owner)
}

func (manager *Manager) ResolveConfirmation(ctx context.Context, id ID, decision events.ToolConfirmationDecision) error {
	if manager == nil {
		return managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	if err := validCallContext(ctx); err != nil {
		return err
	}
	if !manager.validID(id) {
		return manager.error(ErrInvalidTask, "subagent task identity is invalid", true)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	record := manager.tasks[id]
	if record == nil {
		return manager.missingTaskErrorLocked(id)
	}
	if IsTerminal(record.snapshot.Status) {
		return manager.error(ErrTaskTerminal, "subagent task is terminal", true)
	}
	pending := record.snapshot.PendingConfirmation
	if record.snapshot.Status != StatusWaitingConfirmation || pending == nil {
		return manager.error(ErrConfirmationNotFound, "subagent confirmation was not found", true)
	}
	if pending.ConfirmationID != decision.ConfirmationID || pending.CallID != decision.CallID {
		return manager.error(ErrConfirmationNotFound, "subagent confirmation was not found", true)
	}
	if err := validateConfirmationDecision(*pending, decision); err != nil {
		return manager.externalError(err, ErrInvalidTransition, "subagent confirmation decision is invalid", true)
	}
	controller, ok := record.prepared.(PreparedConfirmationController)
	if !ok {
		return manager.error(ErrInvalidTransition, "subagent confirmation controller is unavailable", false)
	}
	if err := callConfirmationController(controller, decision); err != nil {
		return manager.externalError(err, ErrInvalidTransition, "subagent confirmation resolution failed", true)
	}
	previous := record.snapshot.Clone()
	record.snapshot.Status = StatusRunning
	record.snapshot.PendingConfirmation = nil
	display := &ConfirmationDecisionDisplay{
		ConfirmationID: decision.ConfirmationID,
		CallID:         decision.CallID,
		Action:         decision.Action,
		Allowed:        decision.Allowed,
	}
	published, err := manager.hub.Publish(Event{
		TaskID: id, Kind: EventConfirmationResolved, Snapshot: snapshotPointer(record.snapshot), Decision: display,
	})
	if err != nil {
		record.snapshot = previous
		return manager.externalError(err, ErrInternal, "subagent confirmation event failed", false)
	}
	record.snapshot.Revision = published.Revision
	return nil
}

func (manager *Manager) reduceAgentEvent(id ID, input AgentEvent) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	record := manager.tasks[id]
	if record == nil {
		return manager.missingTaskErrorLocked(id)
	}
	if IsTerminal(record.snapshot.Status) {
		return manager.error(ErrTaskTerminal, "subagent task is terminal", true)
	}
	if err := manager.validateAgentEvent(input); err != nil {
		return err
	}
	event := input.Clone()
	switch event.Kind {
	case events.ToolWaitingConfirmation:
		if record.snapshot.Status != StatusRunning || record.snapshot.PendingConfirmation != nil {
			return manager.error(ErrInvalidTransition, "subagent confirmation transition is invalid", true)
		}
		request := cloneConfirmationRequest(event.Payload.Confirmation)
		if err := validateConfirmationRequest(*request); err != nil {
			return manager.externalError(err, ErrInvalidTransition, "subagent confirmation request is invalid", true)
		}
		previous := record.snapshot.Clone()
		record.snapshot.Status = StatusWaitingConfirmation
		record.snapshot.PendingConfirmation = request
		if _, err := manager.publishSnapshotLocked(record, EventWaitingConfirmation); err != nil {
			record.snapshot = previous
			return manager.externalError(err, ErrInternal, "subagent waiting event failed", false)
		}
		if _, err := manager.hub.Publish(Event{TaskID: id, Kind: EventConfirmationRequested, Agent: agentEventPointer(event)}); err != nil {
			return manager.externalError(err, ErrInternal, "subagent confirmation event failed", false)
		}
		return nil
	case events.ToolRunning, events.ToolDenied:
		if record.snapshot.Status == StatusWaitingConfirmation {
			previous := record.snapshot.Clone()
			record.snapshot.Status = StatusRunning
			record.snapshot.PendingConfirmation = nil
			if _, err := manager.publishSnapshotLocked(record, EventRunning); err != nil {
				record.snapshot = previous
				return manager.externalError(err, ErrInternal, "subagent running event failed", false)
			}
		}
		_, err := manager.hub.Publish(Event{TaskID: id, Kind: EventTool, Agent: agentEventPointer(event)})
		return manager.externalEventError(err)
	case events.ToolPending, events.ToolSuccess, events.ToolError:
		_, err := manager.hub.Publish(Event{TaskID: id, Kind: EventTool, Agent: agentEventPointer(event)})
		return manager.externalEventError(err)
	case events.TextDelta:
		_, err := manager.hub.Publish(Event{TaskID: id, Kind: EventTextDelta, Agent: agentEventPointer(event)})
		return manager.externalEventError(err)
	case events.ThinkingDelta:
		_, err := manager.hub.Publish(Event{TaskID: id, Kind: EventThinkingDelta, Agent: agentEventPointer(event)})
		return manager.externalEventError(err)
	case events.DiagnosticEmitted:
		_, err := manager.hub.Publish(Event{TaskID: id, Kind: EventDiagnostic, Agent: agentEventPointer(event)})
		return manager.externalEventError(err)
	case events.AgentProgressed:
		progress := event.Payload.Progress
		if progress.Iteration < record.snapshot.Iteration || progress.Iteration < 0 || progress.Max < 0 ||
			(progress.Max > 0 && record.snapshot.MaxIterations > 0 && progress.Max != record.snapshot.MaxIterations) ||
			(progress.Max > 0 && progress.Iteration > progress.Max) {
			return manager.error(ErrInvalidTransition, "subagent progress is not monotonic", true)
		}
		previous := record.snapshot.Clone()
		record.snapshot.Iteration = progress.Iteration
		if progress.Max > 0 {
			record.snapshot.MaxIterations = progress.Max
		}
		if _, err := manager.publishSnapshotLocked(record, EventProgress); err != nil {
			record.snapshot = previous
			return manager.externalError(err, ErrInternal, "subagent progress event failed", false)
		}
		return nil
	case events.UsageUpdated:
		usage := usageFromDisplay(*event.Payload.Usage)
		if !usageMonotonic(record.snapshot.Usage, usage) {
			return manager.error(ErrInvalidTransition, "subagent usage is not monotonic", true)
		}
		record.snapshot.Usage = usage
		_, err := manager.hub.Publish(Event{TaskID: id, Kind: EventUsage, Agent: agentEventPointer(event)})
		return manager.externalEventError(err)
	default:
		return manager.error(ErrInvalidTransition, "subagent event is unsupported", true)
	}
}

func (manager *Manager) publishSnapshotLocked(record *managedTask, kind EventKind) (Event, error) {
	published, err := manager.hub.Publish(Event{TaskID: record.snapshot.ID, Kind: kind, Snapshot: snapshotPointer(record.snapshot)})
	if err == nil {
		record.snapshot.Revision = published.Revision
	}
	return published, err
}

func (manager *Manager) completeRunnerLocked(record *managedTask, candidate Completion, panicked bool) bool {
	if record.completion != nil {
		return false
	}
	if panicked || !manager.validRunnerCompletionLocked(record, candidate) {
		candidate = manager.internalCompletionLocked(record)
	}
	return manager.completeLocked(record, candidate)
}

func (manager *Manager) completeLocked(record *managedTask, completion Completion) bool {
	if record.completion != nil {
		return false
	}
	if err := completion.Validate(); err != nil || !CanTransition(record.snapshot.Status, completion.Status) {
		completion = manager.internalCompletionLocked(record)
	}
	if !CanTransition(record.snapshot.Status, completion.Status) {
		return false
	}

	previous := record.snapshot.Clone()
	record.snapshot.Status = completion.Status
	endedAt := completion.EndedAt
	record.snapshot.EndedAt = &endedAt
	record.snapshot.StopReason = completion.StopReason
	record.snapshot.Summary = completion.Summary
	record.snapshot.SummaryTruncated = completion.SummaryTruncated
	record.snapshot.TruncationReason = completion.TruncationReason
	record.snapshot.Error = cloneSafeError(completion.Error)
	record.snapshot.Usage = completion.Usage
	record.snapshot.PendingConfirmation = nil

	kind := terminalKind(completion.Status)
	published, err := manager.hub.Publish(Event{
		TaskID: record.snapshot.ID, Kind: kind, Snapshot: snapshotPointer(record.snapshot), Completion: completionPointer(completion),
	})
	if err != nil {
		record.snapshot = previous
		return false
	}
	record.snapshot.Revision = published.Revision
	stored := completion.Clone()
	record.completion = &stored
	record.terminalRevision = published.Revision
	record.terminalSequence = published.Sequence
	if record.durationTimer != nil {
		record.durationTimer.Stop()
	}
	record.cancel(errManagedTaskFinished)
	close(record.done)
	if record.snapshot.Placement == Foreground {
		if err := manager.inbox.ReleaseReservation(context.Background(), record.snapshot.ID, record.snapshot.Parent); err == nil {
			record.reservationReleased = true
		}
		manager.signalForegroundLocked(record, false)
	} else {
		manager.publishResultLocked(record)
	}
	manager.evictTerminalLocked(manager.limits.MaxRetainedTasks)
	return true
}

func (manager *Manager) validRunnerCompletionLocked(record *managedTask, completion Completion) bool {
	if completion.ID != record.snapshot.ID || completion.Validate() != nil || !CanTransition(record.snapshot.Status, completion.Status) {
		return false
	}
	if record.snapshot.StartedAt != nil && completion.EndedAt.Before(*record.snapshot.StartedAt) {
		return false
	}
	if !usageMonotonic(record.snapshot.Usage, completion.Usage) {
		return false
	}
	if !manager.validSafeText(completion.Summary, manager.limits.MaxResultBytes) ||
		!manager.validSafeText(completion.TruncationReason, maxCompletionReasonBytes) {
		return false
	}
	if completion.Error != nil {
		if completion.Error.Source != "subagent" || !manager.validIDText(completion.Error.Code) ||
			!manager.validSafeText(completion.Error.Message, manager.limits.MaxResultBytes) {
			return false
		}
	}
	return true
}

func (manager *Manager) internalCompletionLocked(record *managedTask) Completion {
	return Completion{
		ID:         record.snapshot.ID,
		Status:     StatusFailed,
		Summary:    manager.redactor.Redact("subagent task failed"),
		StopReason: StopInternalError,
		Usage:      record.snapshot.Usage,
		Error:      SafeError(ErrInternal, manager.redactor.Redact("subagent task failed internally"), false),
		EndedAt:    manager.clock(),
	}
}

func (manager *Manager) cancellationCompletionLocked(record *managedTask, reason StopReason) Completion {
	message := "subagent task was cancelled"
	if reason == StopApplicationClosed {
		message = "subagent task stopped because the application closed"
	}
	return Completion{
		ID: record.snapshot.ID, Status: StatusCancelled, Summary: manager.redactor.Redact(message), StopReason: reason,
		Usage: record.snapshot.Usage, Error: SafeError(ErrCancelled, manager.redactor.Redact(message), true), EndedAt: manager.clock(),
	}
}

func (manager *Manager) timeoutCompletionLocked(record *managedTask) Completion {
	return Completion{
		ID: record.snapshot.ID, Status: StatusTimedOut, Summary: manager.redactor.Redact("subagent task timed out"), StopReason: StopTaskTimeout,
		Usage: record.snapshot.Usage, Error: SafeError(ErrTimedOut, manager.redactor.Redact("subagent task timed out"), true), EndedAt: manager.clock(),
	}
}

func (manager *Manager) publishResultLocked(record *managedTask) {
	if record.resultPublished || record.completion == nil {
		return
	}
	if record.pendingNotification == nil {
		generated, err := manager.generateID()
		if err != nil {
			manager.markResultPublishFailureLocked(record)
			return
		}
		completion := record.completion
		record.pendingNotification = &ResultNotification{
			NotificationID:     string(generated),
			CompletionRevision: record.terminalRevision,
			CompletionSequence: record.terminalSequence,
			CreatedAt:          completion.EndedAt,
			TaskID:             completion.ID,
			Parent:             record.snapshot.Parent,
			Status:             completion.Status,
			Summary:            completion.Summary,
			SummaryTruncated:   completion.SummaryTruncated,
			TruncationReason:   completion.TruncationReason,
			StopReason:         completion.StopReason,
			Usage:              completion.Usage,
			Error:              cloneSafeError(completion.Error),
		}
	}
	notification := record.pendingNotification.Clone()
	if err := manager.inbox.Publish(context.Background(), notification); err != nil {
		manager.markResultPublishFailureLocked(record)
		return
	}
	record.resultPublished = true
	record.pendingNotification = nil
	_, _ = manager.hub.Publish(Event{TaskID: record.snapshot.ID, Kind: EventResultPublished, Result: &notification})
}

func (manager *Manager) markResultPublishFailureLocked(record *managedTask) {
	if !record.resultFailureEmitted {
		record.resultFailureEmitted = true
		diagnostic := AgentEvent{Kind: events.DiagnosticEmitted, Payload: events.Event{
			Type: events.DiagnosticEmitted,
			Diagnostic: &events.DiagnosticDisplay{
				Code: string(ErrResultPublishFailed), Severity: "error", Source: "subagent",
				Message: manager.redactor.Redact("subagent result publication failed"),
			},
		}}
		_, _ = manager.hub.Publish(Event{TaskID: record.snapshot.ID, Kind: EventDiagnostic, Agent: &diagnostic})
	}
	if record.resultRetryStarted || manager.shuttingDown {
		return
	}
	record.resultRetryStarted = true
	id := record.snapshot.ID
	go manager.retryResultPublication(id)
}

func (manager *Manager) retryResultPublication(id ID) {
	ticker := time.NewTicker(resultRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-manager.lifecycleCtx.Done():
			return
		case <-ticker.C:
			manager.mu.Lock()
			record := manager.tasks[id]
			if record == nil || record.resultPublished || manager.shuttingDown {
				manager.mu.Unlock()
				return
			}
			manager.publishResultLocked(record)
			done := record.resultPublished
			if done {
				manager.evictTerminalLocked(manager.limits.MaxRetainedTasks)
			}
			manager.mu.Unlock()
			if done {
				return
			}
		}
	}
}

func (manager *Manager) signalForegroundLocked(record *managedTask, detached bool) {
	if record.foregroundSignalled {
		return
	}
	record.foregroundSignalled = true
	record.foregroundDetached = detached
	close(record.foregroundDone)
}

func (manager *Manager) evictTerminalLocked(target int) {
	if target < 0 {
		target = 0
	}
	for len(manager.tasks) > target {
		var candidate *managedTask
		for _, record := range manager.tasks {
			if record.completion == nil || (!record.resultPublished && record.snapshot.Placement == Background) {
				continue
			}
			if candidate == nil || terminalOlder(record.snapshot, candidate.snapshot) {
				candidate = record
			}
		}
		if candidate == nil {
			return
		}
		delete(manager.tasks, candidate.snapshot.ID)
		_ = manager.hub.ForgetTask(candidate.snapshot.ID)
		manager.addTombstoneLocked(candidate.snapshot.ID)
	}
}

func (manager *Manager) addTombstoneLocked(id ID) {
	manager.tombstones[id] = taskTombstone{id: id, evictedAt: manager.clock()}
	for len(manager.tombstones) > manager.limits.MaxTaskTombstones {
		var oldest taskTombstone
		set := false
		for _, tombstone := range manager.tombstones {
			if !set || tombstone.evictedAt.Before(oldest.evictedAt) ||
				(tombstone.evictedAt.Equal(oldest.evictedAt) && tombstone.id < oldest.id) {
				oldest = tombstone
				set = true
			}
		}
		delete(manager.tombstones, oldest.id)
	}
}

func (manager *Manager) normalizeSubmit(ctx context.Context, input SubmitInput) (SubmitInput, Placement, error) {
	if err := validCallContext(ctx); err != nil {
		return SubmitInput{}, "", err
	}
	input.Task = strings.TrimSpace(input.Task)
	if input.Task == "" || !utf8.ValidString(input.Task) || containsUnsafeNUL(input.Task) {
		return SubmitInput{}, "", manager.error(ErrInvalidTask, "subagent task is invalid", true)
	}
	if int64(len(input.Task)) > manager.limits.MaxTaskBytes {
		return SubmitInput{}, "", manager.error(ErrTaskTooLarge, "subagent task exceeds its byte limit", true)
	}
	if !input.Type.Valid() {
		return SubmitInput{}, "", manager.error(ErrInvalidType, "subagent execution type is invalid", true)
	}
	if !input.Placement.Valid() {
		return SubmitInput{}, "", manager.error(ErrInvalidPlacement, "subagent placement is invalid", true)
	}
	if !input.Origin.Valid() {
		return SubmitInput{}, "", manager.error(ErrInvalidParent, "subagent origin is invalid", true)
	}
	input.Role = strings.ToLower(strings.TrimSpace(input.Role))
	if input.Role != "" && (!managerRolePattern.MatchString(input.Role) || int64(len(input.Role)) > manager.limits.MaxRoleNameBytes) {
		return SubmitInput{}, "", manager.error(ErrUnknownRole, "subagent role is invalid", true)
	}
	if !manager.validParent(input.Parent, input.Origin) {
		return SubmitInput{}, "", manager.error(ErrInvalidParent, "subagent parent is invalid", true)
	}
	if input.Origin == OriginModel {
		if !manager.validIDText(input.Invocation.ToolCallID) {
			return SubmitInput{}, "", manager.error(ErrInvalidParent, "subagent invocation is invalid", true)
		}
	} else if input.Invocation.ToolCallID != "" {
		return SubmitInput{}, "", manager.error(ErrInvalidParent, "subagent TUI invocation is invalid", true)
	}
	placement := Foreground
	if input.Type == TypeFork || input.Placement == PlacementBackground {
		placement = Background
		input.Placement = PlacementBackground
	} else {
		input.Placement = PlacementForeground
	}
	return input, placement, nil
}

func (manager *Manager) preparedMetadata(prepared PreparedTask, input SubmitInput) (metadata PreparedMetadata, err error) {
	defer func() {
		if recover() != nil {
			err = manager.error(ErrInternal, "subagent metadata inspection failed", false)
		}
	}()
	metadata = prepared.Metadata()
	if !metadata.Type.Valid() || metadata.Type != input.Type || metadata.MaxIterations < 0 ||
		metadata.Role != input.Role || int64(len(metadata.Role)) > manager.limits.MaxRoleNameBytes || !utf8.ValidString(metadata.Role) {
		return PreparedMetadata{}, manager.error(ErrInternal, "subagent prepared metadata is invalid", false)
	}
	if metadata.Role != "" {
		switch metadata.RoleSource {
		case agentrole.SourcePlugin, agentrole.SourceBuiltin, agentrole.SourceUser, agentrole.SourceProject:
		default:
			return PreparedMetadata{}, manager.error(ErrInternal, "subagent role provenance is invalid", false)
		}
	}
	if !manager.validSafeText(metadata.RoleOrigin, manager.limits.MaxEventBytes) {
		return PreparedMetadata{}, manager.error(ErrInternal, "subagent role origin is invalid", false)
	}
	return metadata, nil
}

func (manager *Manager) prepareTask(ctx context.Context, id ID, input SubmitInput) (prepared PreparedTask, err error) {
	defer func() {
		if recover() != nil {
			prepared = nil
			err = manager.error(ErrInternal, "subagent preparation failed internally", false)
		}
	}()
	return manager.runner.Prepare(ctx, id, input)
}

func (manager *Manager) installFirstRequestObserver(prepared PreparedTask, id ID) (err error) {
	observable, ok := prepared.(FirstProviderRequestObservable)
	if !ok {
		return nil
	}
	defer func() {
		if recover() != nil {
			err = manager.error(ErrInternal, "subagent request observer installation failed", false)
		}
	}()
	observable.SetFirstProviderRequestObserver(func(requestCtx context.Context) {
		manager.observeFirstProviderRequest(id, requestCtx)
	})
	return nil
}

func (manager *Manager) validateAgentEvent(event AgentEvent) error {
	if err := event.Validate(); err != nil {
		return manager.error(ErrInvalidTransition, "subagent event is invalid", true)
	}
	switch event.Kind {
	case events.TextDelta, events.ThinkingDelta:
		if !manager.validSafeText(event.Payload.Text, manager.limits.MaxEventBytes) {
			return manager.error(ErrInvalidTransition, "subagent text event is invalid", true)
		}
	case events.ToolPending, events.ToolRunning, events.ToolSuccess, events.ToolError, events.ToolDenied:
		if event.Payload.Tool == nil || !manager.validIDText(event.Payload.Tool.CallID) || !manager.validIDText(event.Payload.Tool.Name) {
			return manager.error(ErrInvalidTransition, "subagent tool event is invalid", true)
		}
	case events.ToolWaitingConfirmation:
		if event.Payload.Confirmation == nil || !manager.validIDText(event.Payload.Confirmation.ConfirmationID) ||
			!manager.validIDText(event.Payload.Confirmation.CallID) {
			return manager.error(ErrInvalidTransition, "subagent confirmation event is invalid", true)
		}
	case events.AgentProgressed:
		if event.Payload.Progress == nil {
			return manager.error(ErrInvalidTransition, "subagent progress event is invalid", true)
		}
	case events.UsageUpdated:
		if event.Payload.Usage == nil {
			return manager.error(ErrInvalidTransition, "subagent usage event is invalid", true)
		}
	case events.DiagnosticEmitted:
		if event.Payload.Diagnostic == nil || !manager.validSafeText(event.Payload.Diagnostic.Message, manager.limits.MaxEventBytes) {
			return manager.error(ErrInvalidTransition, "subagent diagnostic event is invalid", true)
		}
	}
	return nil
}

func (manager *Manager) validParent(parent ParentRef, origin Origin) bool {
	if !manager.validIDText(parent.ConversationID) {
		return false
	}
	if parent.ExecutionID != "" && !manager.validIDText(parent.ExecutionID) {
		return false
	}
	if origin == OriginModel {
		return parent.ExecutionID != "" && parent.RequestGeneration > 0
	}
	return true
}

func (manager *Manager) validID(id ID) bool {
	return manager.validIDText(string(id))
}

func (manager *Manager) validIDText(value string) bool {
	return value != "" && int64(len(value)) <= manager.limits.MaxIDBytes && utf8.ValidString(value) &&
		strings.TrimSpace(value) == value && validIdentifier(value)
}

func (manager *Manager) validSafeText(value redact.SafeText, maxBytes int64) bool {
	text := value.Text()
	return utf8.ValidString(text) && int64(len(text)) <= maxBytes && !containsUnsafeNUL(text)
}

func (manager *Manager) generateID() (ID, error) {
	manager.idMu.Lock()
	defer manager.idMu.Unlock()
	id, err := manager.idGenerator()
	if err != nil || !manager.validID(id) {
		return "", manager.error(ErrInternal, "subagent identity generation failed", false)
	}
	if _, duplicate := manager.usedIDs[string(id)]; duplicate {
		return "", manager.error(ErrInternal, "subagent identity generation was not unique", false)
	}
	manager.usedIDs[string(id)] = struct{}{}
	return id, nil
}

func (manager *Manager) missingTaskErrorLocked(id ID) error {
	if _, expired := manager.tombstones[id]; expired {
		return manager.error(ErrTaskExpired, "subagent task has expired", true)
	}
	return manager.error(ErrTaskNotFound, "subagent task was not found", true)
}

func (manager *Manager) error(code ErrorCode, message string, recoverable bool) error {
	return SafeError(code, manager.redactor.Redact(message), recoverable)
}

func (manager *Manager) externalError(err error, fallback ErrorCode, message string, recoverable bool) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var safe *diagnostics.SafeError
	if errors.As(err, &safe) && safe.Source == "subagent" {
		return cloneSafeError(safe)
	}
	return manager.error(fallback, message, recoverable)
}

func (manager *Manager) externalEventError(err error) error {
	return manager.externalError(err, ErrInternal, "subagent event publication failed", false)
}

func managerStaticError(code ErrorCode, message string, recoverable bool) error {
	return SafeError(code, redact.NewRuntimeRedactor().Redact(message), recoverable)
}

func validCallContext(ctx context.Context) error {
	if ctx == nil {
		return managerStaticError(ErrInvalidTransition, "subagent operation context is invalid", false)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return nil
}

func usageFromDisplay(display events.UsageDisplay) Usage {
	return Usage{
		InputTokens: display.InputTokens, OutputTokens: display.OutputTokens,
		CacheCreationInputTokens: display.CacheCreationInputTokens, CacheReadInputTokens: display.CacheReadInputTokens,
	}
}

func usageMonotonic(previous, next Usage) bool {
	return next.Validate() == nil && next.InputTokens >= previous.InputTokens && next.OutputTokens >= previous.OutputTokens &&
		next.CacheCreationInputTokens >= previous.CacheCreationInputTokens && next.CacheReadInputTokens >= previous.CacheReadInputTokens
}

func snapshotPointer(snapshot TaskSnapshot) *TaskSnapshot {
	value := snapshot.Clone()
	return &value
}

func completionPointer(completion Completion) *Completion {
	value := completion.Clone()
	return &value
}

func agentEventPointer(event AgentEvent) *AgentEvent {
	value := event.Clone()
	return &value
}

func terminalKind(status Status) EventKind {
	switch status {
	case StatusCompleted:
		return EventCompletion
	case StatusFailed:
		return EventFailure
	case StatusCancelled:
		return EventCancellation
	case StatusTimedOut:
		return EventTimeout
	default:
		return EventLimit
	}
}

func terminalOlder(left, right TaskSnapshot) bool {
	if left.EndedAt == nil {
		return false
	}
	if right.EndedAt == nil {
		return true
	}
	if !left.EndedAt.Equal(*right.EndedAt) {
		return left.EndedAt.Before(*right.EndedAt)
	}
	return left.ID < right.ID
}

func sortTaskSnapshots(tasks []TaskSnapshot) {
	sort.Slice(tasks, func(left, right int) bool {
		leftTerminal := IsTerminal(tasks[left].Status)
		rightTerminal := IsTerminal(tasks[right].Status)
		if leftTerminal != rightTerminal {
			return !leftTerminal
		}
		if !leftTerminal {
			leftRank := activeStatusRank(tasks[left].Status)
			rightRank := activeStatusRank(tasks[right].Status)
			if leftRank != rightRank {
				return leftRank < rightRank
			}
			if !tasks[left].CreatedAt.Equal(tasks[right].CreatedAt) {
				return tasks[left].CreatedAt.After(tasks[right].CreatedAt)
			}
			return tasks[left].ID < tasks[right].ID
		}
		leftEnded := tasks[left].EndedAt
		rightEnded := tasks[right].EndedAt
		if leftEnded != nil && rightEnded != nil && !leftEnded.Equal(*rightEnded) {
			return leftEnded.After(*rightEnded)
		}
		return tasks[left].ID < tasks[right].ID
	})
}

func activeStatusRank(status Status) int {
	switch status {
	case StatusQueued:
		return 0
	case StatusRunning:
		return 1
	case StatusWaitingConfirmation:
		return 2
	default:
		return 3
	}
}

func containsUnsafeNUL(value string) bool {
	return strings.ContainsRune(value, '\x00')
}

func callConfirmationController(controller PreparedConfirmationController, decision events.ToolConfirmationDecision) (err error) {
	defer func() {
		if recover() != nil {
			err = managerStaticError(ErrInternal, "subagent confirmation controller failed", false)
		}
	}()
	return controller.ResolveConfirmation(decision)
}

var (
	errManagedTaskFinished         = errors.New("subagent task finished")
	_                      Service = (*Manager)(nil)
)
