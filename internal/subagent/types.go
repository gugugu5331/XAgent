package subagent

import (
	"context"
	"errors"
	"time"
	"unicode"

	"xagent/internal/agentrole"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
)

type ID string

type ExecutionType string

const (
	TypeDefined ExecutionType = "defined"
	TypeFork    ExecutionType = "fork"
)

func (value ExecutionType) Valid() bool {
	return value == TypeDefined || value == TypeFork
}

type PlacementIntent string

const (
	PlacementDefault    PlacementIntent = "default"
	PlacementForeground PlacementIntent = "foreground"
	PlacementBackground PlacementIntent = "background"
)

func (value PlacementIntent) Valid() bool {
	return value == PlacementDefault || value == PlacementForeground || value == PlacementBackground
}

type Placement string

const (
	Foreground Placement = "foreground"
	Background Placement = "background"
)

func (value Placement) Valid() bool {
	return value == Foreground || value == Background
}

type Origin string

const (
	OriginModel Origin = "model"
	OriginTUI   Origin = "tui"
)

func (value Origin) Valid() bool {
	return value == OriginModel || value == OriginTUI
}

type ParentRef struct {
	ConversationID    string
	ExecutionID       string
	RequestGeneration uint64
}

type InvocationRef struct {
	ToolCallID string
}

type SubmitInput struct {
	Task       string
	Type       ExecutionType
	Role       string
	Placement  PlacementIntent
	Origin     Origin
	Parent     ParentRef
	Invocation InvocationRef
}

type Submission struct {
	ID        ID
	Type      ExecutionType
	Role      string
	Origin    Origin
	Parent    ParentRef
	Placement Placement
	Status    Status
	Revision  uint64
	CreatedAt time.Time
}

type Status string

const (
	StatusQueued              Status = "queued"
	StatusRunning             Status = "running"
	StatusWaitingConfirmation Status = "waiting_confirmation"
	StatusSettling            Status = "settling"
	StatusCompleted           Status = "completed"
	StatusFailed              Status = "failed"
	StatusCancelled           Status = "cancelled"
	StatusTimedOut            Status = "timed_out"
	StatusLimitReached        Status = "limit_reached"
)

func (status Status) Valid() bool {
	switch status {
	case StatusQueued,
		StatusRunning,
		StatusWaitingConfirmation,
		StatusSettling,
		StatusCompleted,
		StatusFailed,
		StatusCancelled,
		StatusTimedOut,
		StatusLimitReached:
		return true
	default:
		return false
	}
}

func IsTerminal(status Status) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusTimedOut, StatusLimitReached:
		return true
	default:
		return false
	}
}

func CanTransition(from, to Status) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	switch from {
	case StatusQueued:
		return to == StatusRunning || to == StatusSettling || to == StatusCancelled
	case StatusRunning:
		return to == StatusWaitingConfirmation || to == StatusSettling || IsTerminal(to)
	case StatusWaitingConfirmation:
		return to == StatusRunning || to == StatusSettling || to == StatusFailed || to == StatusCancelled || to == StatusTimedOut || to == StatusLimitReached
	case StatusSettling:
		return IsTerminal(to)
	default:
		return false
	}
}

func ValidateTransition(from, to Status) error {
	if !CanTransition(from, to) {
		return errors.New(string(ErrInvalidTransition))
	}
	return nil
}

type StopReason string

const (
	StopCompleted         StopReason = "completed"
	StopMaxIterations     StopReason = "max_iterations"
	StopCancelled         StopReason = "cancelled"
	StopApplicationClosed StopReason = "application_closed"
	StopTaskTimeout       StopReason = "task_timeout"
	StopProviderError     StopReason = "provider_error"
	StopToolError         StopReason = "tool_error"
	StopUnknownToolLimit  StopReason = "unknown_tool_limit"
	StopInternalError     StopReason = "internal_error"
)

func (reason StopReason) Valid() bool {
	switch reason {
	case StopCompleted,
		StopMaxIterations,
		StopCancelled,
		StopApplicationClosed,
		StopTaskTimeout,
		StopProviderError,
		StopToolError,
		StopUnknownToolLimit,
		StopInternalError:
		return true
	default:
		return false
	}
}

type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// RunResult captures the execution phase before task-owned resources settle.
// It deliberately has no EndedAt because the public task is not terminal yet.
type RunResult struct {
	Status     Status
	StopReason StopReason
	Summary    redact.SafeText
	Usage      Usage
	Error      *diagnostics.SafeError
}

func (result RunResult) Clone() RunResult {
	cloned := result
	cloned.Error = cloneSafeError(result.Error)
	return cloned
}

func (usage Usage) Validate() error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CacheCreationInputTokens < 0 || usage.CacheReadInputTokens < 0 {
		return errors.New("subagent usage is invalid")
	}
	return nil
}

// WorkspaceSummary is the path-free lifecycle projection attached to a final
// completion. Empty Isolation represents the existing shared workspace mode.
type WorkspaceSummary struct {
	WorkspaceID    string
	Isolation      string
	State          string
	BaseOID        string
	Branch         string
	Dirty          bool
	Unpushed       bool
	Cleanup        string
	RetentionCause string
	Error          *diagnostics.SafeError
}

func (summary WorkspaceSummary) Clone() WorkspaceSummary {
	cloned := summary
	cloned.Error = cloneSafeError(summary.Error)
	return cloned
}

func (summary WorkspaceSummary) Validate() error {
	if summary.Isolation == "" {
		if summary != (WorkspaceSummary{}) {
			return errors.New("shared subagent workspace summary contains isolated state")
		}
		return nil
	}
	if summary.Isolation != "worktree" ||
		!validLowerHex(summary.WorkspaceID, 32) ||
		!validWorkspaceTerminal(summary.State) ||
		!validGitOID(summary.BaseOID) ||
		summary.Branch != "xagent/worktree/"+summary.WorkspaceID ||
		!validWorkspaceTerminal(summary.Cleanup) {
		return errors.New("subagent workspace summary is invalid")
	}
	if summary.RetentionCause != "" && !validWorkspaceReason(summary.RetentionCause) {
		return errors.New("subagent workspace retention cause is invalid")
	}
	return nil
}

func validGitOID(value string) bool {
	return validLowerHex(value, 40) || validLowerHex(value, 64)
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validWorkspaceTerminal(value string) bool {
	switch value {
	case "deleted", "retained", "partial", "manual_attention":
		return true
	default:
		return false
	}
}

func validWorkspaceReason(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		if index > 0 && (character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}

type Completion struct {
	ID               ID
	Status           Status
	Summary          redact.SafeText
	SummaryTruncated bool
	TruncationReason redact.SafeText
	StopReason       StopReason
	Usage            Usage
	Error            *diagnostics.SafeError
	Workspace        WorkspaceSummary
	EndedAt          time.Time
}

func (completion Completion) Clone() Completion {
	cloned := completion
	cloned.Error = cloneSafeError(completion.Error)
	cloned.Workspace = completion.Workspace.Clone()
	return cloned
}

func (completion Completion) Validate() error {
	if !validIdentifier(string(completion.ID)) {
		return errors.New("subagent completion ID is invalid")
	}
	if !IsTerminal(completion.Status) || completion.EndedAt.IsZero() {
		return errors.New("subagent completion terminal state is invalid")
	}
	if err := completion.Usage.Validate(); err != nil {
		return err
	}
	if err := completion.Workspace.Validate(); err != nil {
		return err
	}
	if completion.SummaryTruncated != (completion.TruncationReason.Text() != "") {
		return errors.New("subagent completion truncation state is invalid")
	}

	expectedCode, needsError, ok := completionTerminalMapping(completion.Status, completion.StopReason)
	if !ok {
		return errors.New("subagent completion status and stop reason are inconsistent")
	}
	if !needsError {
		if completion.Error != nil {
			return errors.New("completed subagent has an error")
		}
		return nil
	}
	if completion.Error == nil || completion.Error.Code != string(expectedCode) {
		return errors.New("subagent completion error is inconsistent")
	}
	return nil
}

func ValidateCompletion(completion Completion) error {
	return completion.Validate()
}

func completionTerminalMapping(status Status, reason StopReason) (ErrorCode, bool, bool) {
	switch status {
	case StatusCompleted:
		return "", false, reason == StopCompleted
	case StatusLimitReached:
		return ErrLimitReached, true, reason == StopMaxIterations || reason == StopUnknownToolLimit
	case StatusCancelled:
		return ErrCancelled, true, reason == StopCancelled || reason == StopApplicationClosed
	case StatusTimedOut:
		return ErrTimedOut, true, reason == StopTaskTimeout
	case StatusFailed:
		switch reason {
		case StopProviderError:
			return ErrProviderFailed, true, true
		case StopToolError:
			return ErrToolFailed, true, true
		case StopInternalError:
			return ErrInternal, true, true
		}
	}
	return "", false, false
}

type TaskSnapshot struct {
	ID                  ID
	Revision            uint64
	Type                ExecutionType
	Origin              Origin
	Role                string
	RoleSource          agentrole.Source
	RoleSourceID        string
	RoleProviderID      string
	RoleOrigin          redact.SafeText
	RoleGeneration      uint64
	Placement           Placement
	Status              Status
	Parent              ParentRef
	CreatedAt           time.Time
	StartedAt           *time.Time
	EndedAt             *time.Time
	Iteration           int
	MaxIterations       int
	StopReason          StopReason
	Summary             redact.SafeText
	SummaryTruncated    bool
	TruncationReason    redact.SafeText
	Error               *diagnostics.SafeError
	Usage               Usage
	EventsDropped       uint64
	PendingConfirmation *events.ToolConfirmationRequest
}

func (snapshot TaskSnapshot) Clone() TaskSnapshot {
	cloned := snapshot
	cloned.StartedAt = cloneTime(snapshot.StartedAt)
	cloned.EndedAt = cloneTime(snapshot.EndedAt)
	cloned.Error = cloneSafeError(snapshot.Error)
	cloned.PendingConfirmation = cloneConfirmationRequest(snapshot.PendingConfirmation)
	return cloned
}

type TaskListSnapshot struct {
	Watermark uint64
	Tasks     []TaskSnapshot
}

func (snapshot TaskListSnapshot) Clone() TaskListSnapshot {
	cloned := snapshot
	if snapshot.Tasks != nil {
		cloned.Tasks = make([]TaskSnapshot, len(snapshot.Tasks))
		for index := range snapshot.Tasks {
			cloned.Tasks[index] = snapshot.Tasks[index].Clone()
		}
	}
	return cloned
}

type TaskDetailSnapshot struct {
	Watermark    uint64
	Task         TaskSnapshot
	RecentEvents []Event
}

func (snapshot TaskDetailSnapshot) Clone() TaskDetailSnapshot {
	cloned := snapshot
	cloned.Task = snapshot.Task.Clone()
	if snapshot.RecentEvents != nil {
		cloned.RecentEvents = make([]Event, len(snapshot.RecentEvents))
		for index := range snapshot.RecentEvents {
			cloned.RecentEvents[index] = snapshot.RecentEvents[index].Clone()
		}
	}
	return cloned
}

type AgentEvent struct {
	Kind    events.Type
	Payload events.Event
	Range   *DeltaRange
}

type DeltaRange struct {
	From int64
	To   int64
}

func (event AgentEvent) Clone() AgentEvent {
	cloned := event
	cloned.Payload = events.Clone(event.Payload)
	if event.Range != nil {
		rangeCopy := *event.Range
		cloned.Range = &rangeCopy
	}
	return cloned
}

func (event AgentEvent) Validate() error {
	if event.Kind != event.Payload.Type || event.Payload.IndependentID != "" || !allowedAgentEventType(event.Kind) {
		return errors.New("subagent Agent event is invalid")
	}
	if event.Range != nil {
		if event.Kind != events.TextDelta && event.Kind != events.ThinkingDelta {
			return errors.New("subagent Agent event range is not allowed")
		}
		if event.Range.From < 0 || event.Range.To < event.Range.From {
			return errors.New("subagent Agent event range is invalid")
		}
	}
	return nil
}

func allowedAgentEventType(eventType events.Type) bool {
	switch eventType {
	case events.TextDelta,
		events.ThinkingDelta,
		events.ToolPending,
		events.ToolWaitingConfirmation,
		events.ToolRunning,
		events.ToolSuccess,
		events.ToolError,
		events.ToolDenied,
		events.AgentProgressed,
		events.UsageUpdated,
		events.DiagnosticEmitted:
		return true
	default:
		return false
	}
}

type EventSink func(AgentEvent) error

type PreparedMetadata struct {
	Type                      ExecutionType
	Role                      string
	RoleSource                agentrole.Source
	RoleSourceID              string
	RoleProviderID            string
	RoleOrigin                redact.SafeText
	RoleGeneration            uint64
	Model                     string
	MaxIterations             int
	MaxUnknownToolCalls       int
	PermissionMode            string
	Depth                     int
	ForegroundToolFingerprint string
	BackgroundToolFingerprint string
}

type PreparedTask interface {
	Run(context.Context, EventSink) RunResult
	Settle(context.Context, RunResult) Completion
	Metadata() PreparedMetadata
}

// PreparedPlacementController is an optional task-runtime boundary used by
// TaskManager when a foreground task detaches. Returning true means the
// runtime atomically narrowed its CapabilitySwitch and detached its parent
// cancellation bridge. Implementations must be non-blocking and idempotent.
type PreparedPlacementController interface {
	MoveToBackground() bool
}

// PreparedConfirmationController is the optional task-local route to the
// runtime's isolated confirmation broker. TaskManager fails closed when a
// waiting task does not implement it.
type PreparedConfirmationController interface {
	ResolveConfirmation(events.ToolConfirmationDecision) error
}

// FirstProviderRequestObservable lets TaskManager install the automatic
// background observer before Run starts. The task invokes the observer once,
// immediately before its first Provider StreamChat call, with a request
// context that ends when that first request completes, fails, or is cancelled.
type FirstProviderRequestObservable interface {
	SetFirstProviderRequestObserver(func(context.Context))
}

type RunnerFactory interface {
	Prepare(context.Context, ID, SubmitInput) (PreparedTask, error)
}

type EventKind string

const (
	EventQueued                EventKind = "queued"
	EventRunning               EventKind = "running"
	EventWaitingConfirmation   EventKind = "waiting_confirmation"
	EventSettling              EventKind = "settling"
	EventIterationStarted      EventKind = "iteration_started"
	EventTextDelta             EventKind = "text_delta"
	EventThinkingDelta         EventKind = "thinking_delta"
	EventTool                  EventKind = "tool"
	EventConfirmationRequested EventKind = "confirmation_requested"
	EventConfirmationResolved  EventKind = "confirmation_resolved"
	EventProgress              EventKind = "progress"
	EventUsage                 EventKind = "usage"
	EventPlacementChanged      EventKind = "placement_changed"
	EventDiagnostic            EventKind = "diagnostic"
	EventCompletion            EventKind = "completed"
	EventFailure               EventKind = "failed"
	EventCancellation          EventKind = "cancelled"
	EventTimeout               EventKind = "timed_out"
	EventLimit                 EventKind = "limit_reached"
	EventResultPublished       EventKind = "result_published"
	EventGap                   EventKind = "event_gap"
)

func (kind EventKind) Valid() bool {
	switch kind {
	case EventQueued,
		EventRunning,
		EventWaitingConfirmation,
		EventSettling,
		EventIterationStarted,
		EventTextDelta,
		EventThinkingDelta,
		EventTool,
		EventConfirmationRequested,
		EventConfirmationResolved,
		EventProgress,
		EventUsage,
		EventPlacementChanged,
		EventDiagnostic,
		EventCompletion,
		EventFailure,
		EventCancellation,
		EventTimeout,
		EventLimit,
		EventResultPublished,
		EventGap:
		return true
	default:
		return false
	}
}

type Event struct {
	Revision   uint64
	TaskID     ID
	Sequence   uint64
	At         time.Time
	Kind       EventKind
	Agent      *AgentEvent
	Snapshot   *TaskSnapshot
	Completion *Completion
	Result     *ResultNotification
	Gap        *GapDescriptor
	Placement  *PlacementChange
	Decision   *ConfirmationDecisionDisplay
}

func (event Event) Clone() Event {
	cloned := event
	if event.Agent != nil {
		value := event.Agent.Clone()
		cloned.Agent = &value
	}
	if event.Snapshot != nil {
		value := event.Snapshot.Clone()
		cloned.Snapshot = &value
	}
	if event.Completion != nil {
		value := event.Completion.Clone()
		cloned.Completion = &value
	}
	if event.Result != nil {
		value := event.Result.Clone()
		cloned.Result = &value
	}
	if event.Gap != nil {
		value := *event.Gap
		cloned.Gap = &value
	}
	if event.Placement != nil {
		value := *event.Placement
		cloned.Placement = &value
	}
	if event.Decision != nil {
		value := *event.Decision
		cloned.Decision = &value
	}
	return cloned
}

func (event Event) ValidateOneOf() error {
	if !event.Kind.Valid() {
		return errors.New("subagent event kind is invalid")
	}
	var valid bool
	switch event.Kind {
	case EventQueued, EventRunning, EventWaitingConfirmation, EventSettling, EventIterationStarted, EventProgress:
		valid = event.Snapshot != nil && event.onlyPayloads(payloadSnapshot)
	case EventTextDelta, EventThinkingDelta, EventTool, EventConfirmationRequested, EventUsage, EventDiagnostic:
		valid = event.Agent != nil && event.onlyPayloads(payloadAgent)
	case EventConfirmationResolved:
		valid = event.Snapshot != nil && event.Decision != nil && event.onlyPayloads(payloadSnapshot|payloadDecision)
	case EventPlacementChanged:
		valid = event.Snapshot != nil && event.Placement != nil && event.onlyPayloads(payloadSnapshot|payloadPlacement)
	case EventCompletion, EventFailure, EventCancellation, EventTimeout, EventLimit:
		valid = event.Snapshot != nil && event.Completion != nil && event.onlyPayloads(payloadSnapshot|payloadCompletion)
	case EventResultPublished:
		valid = event.Result != nil && event.onlyPayloads(payloadResult)
	case EventGap:
		valid = event.Gap != nil && event.onlyPayloads(payloadGap)
	}
	if !valid {
		return errors.New("subagent event payload invariant failed")
	}
	return nil
}

const (
	payloadAgent uint8 = 1 << iota
	payloadSnapshot
	payloadCompletion
	payloadResult
	payloadGap
	payloadPlacement
	payloadDecision
)

func (event Event) onlyPayloads(expected uint8) bool {
	var actual uint8
	if event.Agent != nil {
		actual |= payloadAgent
	}
	if event.Snapshot != nil {
		actual |= payloadSnapshot
	}
	if event.Completion != nil {
		actual |= payloadCompletion
	}
	if event.Result != nil {
		actual |= payloadResult
	}
	if event.Gap != nil {
		actual |= payloadGap
	}
	if event.Placement != nil {
		actual |= payloadPlacement
	}
	if event.Decision != nil {
		actual |= payloadDecision
	}
	return actual == expected
}

type GapDescriptor struct {
	FromRevision uint64
	ToRevision   uint64
	Reason       string
}

type PlacementChange struct {
	From   Placement
	To     Placement
	Reason string
}

type ConfirmationDecisionDisplay struct {
	ConfirmationID string
	CallID         string
	Action         events.PermissionAction
	Allowed        bool
}

type ResultNotification struct {
	NotificationID     string
	CompletionRevision uint64
	CompletionSequence uint64
	CreatedAt          time.Time
	TaskID             ID
	Parent             ParentRef
	Status             Status
	Summary            redact.SafeText
	SummaryTruncated   bool
	TruncationReason   redact.SafeText
	StopReason         StopReason
	Usage              Usage
	Error              *diagnostics.SafeError
	Workspace          WorkspaceSummary
}

func (notification ResultNotification) Clone() ResultNotification {
	cloned := notification
	cloned.Error = cloneSafeError(notification.Error)
	cloned.Workspace = notification.Workspace.Clone()
	return cloned
}

type ForegroundOutcome struct {
	Completion *Completion
	Detached   bool
}

func (outcome ForegroundOutcome) Clone() ForegroundOutcome {
	cloned := outcome
	if outcome.Completion != nil {
		value := outcome.Completion.Clone()
		cloned.Completion = &value
	}
	return cloned
}

type Service interface {
	Submit(context.Context, SubmitInput) (Submission, error)
	List(context.Context) (TaskListSnapshot, error)
	Get(context.Context, ID) (TaskDetailSnapshot, error)
	Cancel(context.Context, ID) error
	MoveToBackground(context.Context, ID) error
	ResolveConfirmation(context.Context, ID, events.ToolConfirmationDecision) error
	Subscribe(context.Context, uint64) (<-chan Event, error)
	Await(context.Context, ID) (Completion, error)
	AwaitForeground(context.Context, ID) (ForegroundOutcome, error)
	ClaimResults(context.Context, ResultClaimOptions) (ResultClaim, error)
	AckResults(context.Context, string, ParentRef) error
	ReleaseResults(context.Context, string, ParentRef) error
	Shutdown(context.Context) error
}

type ResultInbox interface {
	Reserve(context.Context, ID, ParentRef) error
	ReleaseReservation(context.Context, ID, ParentRef) error
	Publish(context.Context, ResultNotification) error
	Claim(context.Context, ResultClaimOptions) (ResultClaim, error)
	Ack(context.Context, string, ParentRef) error
	Release(context.Context, string, ParentRef) error
}

type ResultClaim struct {
	ClaimID         string
	Owner           ParentRef
	SerializedBytes int64
	Notifications   []ResultNotification
}

func (claim ResultClaim) Clone() ResultClaim {
	cloned := claim
	if claim.Notifications != nil {
		cloned.Notifications = make([]ResultNotification, len(claim.Notifications))
		for index := range claim.Notifications {
			cloned.Notifications[index] = claim.Notifications[index].Clone()
		}
	}
	return cloned
}

type ResultClaimOptions struct {
	Owner            ParentRef
	MaxNotifications int
	MaxBytes         int64
}

type ConfirmationBroker interface {
	Request(context.Context, events.ToolConfirmationRequest) (events.ToolConfirmationDecision, error)
	Resolve(events.ToolConfirmationDecision) error
	Pending() *events.ToolConfirmationRequest
	Close(error)
}

func cloneTime(source *time.Time) *time.Time {
	if source == nil {
		return nil
	}
	cloned := *source
	return &cloned
}

func cloneConfirmationRequest(source *events.ToolConfirmationRequest) *events.ToolConfirmationRequest {
	if source == nil {
		return nil
	}
	cloned := *source
	if source.Scopes != nil {
		cloned.Scopes = make([]events.ConfirmationScopeDisplay, len(source.Scopes))
		copy(cloned.Scopes, source.Scopes)
	}
	return &cloned
}

func validIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
