package events

import (
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

type Type string

const (
	UserSubmitted           Type = "user_submitted"
	TextDelta               Type = "text_delta"
	ThinkingDelta           Type = "thinking_delta"
	ToolPending             Type = "tool_pending"
	ToolWaitingConfirmation Type = "tool_waiting_confirmation"
	ToolRunning             Type = "tool_running"
	ToolSuccess             Type = "tool_success"
	ToolError               Type = "tool_error"
	ToolDenied              Type = "tool_denied"
	AgentProgressed         Type = "agent_progress"
	DiagnosticEmitted       Type = "diagnostic_emitted"
	UsageUpdated            Type = "usage_updated"
	MainTraceReset          Type = "main_trace_reset"
	Done                    Type = "done"
	Error                   Type = "error"
)

type ToolDisplayStatus string

const (
	ToolDisplayPending             ToolDisplayStatus = "pending"
	ToolDisplayWaitingConfirmation ToolDisplayStatus = "waiting_confirmation"
	ToolDisplayRunning             ToolDisplayStatus = "running"
	ToolDisplaySuccess             ToolDisplayStatus = "success"
	ToolDisplayError               ToolDisplayStatus = "error"
	ToolDisplayDenied              ToolDisplayStatus = "denied"
	ToolDisplayCancelled           ToolDisplayStatus = "cancelled"
)

type ToolDisplay struct {
	CallID           string
	Name             string
	Arguments        redact.SafeText
	Summary          redact.SafeText
	Status           ToolDisplayStatus
	ErrorCode        string
	Stdout           redact.SafeText
	Stderr           redact.SafeText
	Truncated        bool
	TruncationReason redact.SafeText
	Recoverable      bool
	Artifact         *ArtifactRef
}

// ArtifactRef is an opaque, path-free reference suitable for App and TUI
// boundaries. It intentionally does not expose the artifact domain object or
// its backing filesystem location.
type ArtifactRef struct {
	ID        string
	Bytes     int64
	Available bool
	Complete  bool
	CreatedAt time.Time
}

// ToolResultDisplayFromUserView is the only result-bearing Event adapter.
// Identity and already-safe arguments are supplied separately; every result
// field is copied exclusively from the projected UserView.
func ToolResultDisplayFromUserView(callID, name string, arguments redact.SafeText, view tool.UserView) ToolDisplay {
	display := ToolDisplay{
		CallID:           callID,
		Name:             name,
		Arguments:        arguments,
		Summary:          view.Summary,
		Status:           toolDisplayStatusFromUserView(view),
		Stdout:           view.Preview,
		Truncated:        view.Truncated,
		TruncationReason: view.TruncationReason,
	}
	if view.Error != nil {
		display.ErrorCode = view.Error.Code
		display.Stderr = view.Error.Message
		display.Recoverable = view.Error.Recoverable
	}
	if view.Artifact != nil {
		display.Artifact = &ArtifactRef{
			ID:        view.Artifact.ID,
			Bytes:     view.Artifact.Bytes,
			Available: view.Artifact.Available,
			Complete:  view.Artifact.Complete,
			CreatedAt: view.Artifact.CreatedAt,
		}
	}
	return display
}

// ToolResultEventFromUserView wraps the display without accepting a
// tool.Result or any model/persistence projection.
func ToolResultEventFromUserView(callID, name string, arguments redact.SafeText, view tool.UserView) Event {
	display := ToolResultDisplayFromUserView(callID, name, arguments, view)
	eventType := ToolSuccess
	switch display.Status {
	case ToolDisplayDenied:
		eventType = ToolDenied
	case ToolDisplayError, ToolDisplayCancelled:
		eventType = ToolError
	}
	return Event{Type: eventType, Tool: &display}
}

func toolDisplayStatusFromUserView(view tool.UserView) ToolDisplayStatus {
	switch view.State {
	case tool.Rejected:
		return ToolDisplayDenied
	case tool.CancelledBeforeStart, tool.CancelledAfterStart:
		return ToolDisplayCancelled
	}
	switch view.Status {
	case tool.StatusSuccess:
		return ToolDisplaySuccess
	case tool.StatusDenied:
		return ToolDisplayDenied
	default:
		return ToolDisplayError
	}
}

type DiagnosticDisplay struct {
	Code     string
	Severity string
	Source   string
	Message  redact.SafeText
	Hint     redact.SafeText
}

// ConfirmationScopeDisplay is a display-only projection of one authorization choice.
// Description is already redacted before crossing the Events boundary.
type ConfirmationScopeDisplay struct {
	Scope       string
	Available   bool
	Description redact.SafeText
}

type ToolConfirmationRequest struct {
	ConfirmationID string
	CallID         string
	Name           string
	Arguments      redact.SafeText
	Prompt         redact.SafeText
	Target         redact.SafeText
	Risk           string
	PermissionMode string
	ScopePreview   redact.SafeText
	Scopes         []ConfirmationScopeDisplay
	RuleLocation   redact.SafeText
	Warning        redact.SafeText
	RevokeHint     redact.SafeText
	AllowPermanent bool
}

type PermissionAction string

const (
	PermissionDeny           PermissionAction = "deny"
	PermissionAllowOnce      PermissionAction = "allow_once"
	PermissionAllowSession   PermissionAction = "allow_session"
	PermissionAllowPermanent PermissionAction = "allow_permanent"
	PermissionCancel         PermissionAction = "cancel"
)

type ToolConfirmationDecision struct {
	ConfirmationID string
	CallID         string
	Allowed        bool
	Action         PermissionAction
}

type AgentProgress struct {
	Iteration  int
	Max        int
	StopReason string
	Message    redact.SafeText
}

type UsageDisplay struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

type Event struct {
	Type          Type
	Text          redact.SafeText
	Duration      time.Duration
	Err           *diagnostics.SafeError
	Transient     bool
	IndependentID string
	Tool          *ToolDisplay
	Confirmation  *ToolConfirmationRequest
	Diagnostic    *DiagnosticDisplay
	Progress      *AgentProgress
	Usage         *UsageDisplay
}

// Clone returns a detached safe DTO graph. Event producers and App may run on
// different goroutines, so optional payload pointers must not alias mutable
// producer storage after crossing the channel boundary.
func Clone(source Event) Event {
	cloned := source
	if source.Err != nil {
		value := *source.Err
		cloned.Err = &value
	}
	if source.Tool != nil {
		value := *source.Tool
		if source.Tool.Artifact != nil {
			artifact := *source.Tool.Artifact
			value.Artifact = &artifact
		}
		cloned.Tool = &value
	}
	if source.Confirmation != nil {
		value := *source.Confirmation
		if source.Confirmation.Scopes != nil {
			value.Scopes = make([]ConfirmationScopeDisplay, len(source.Confirmation.Scopes))
			copy(value.Scopes, source.Confirmation.Scopes)
		}
		cloned.Confirmation = &value
	}
	if source.Diagnostic != nil {
		value := *source.Diagnostic
		cloned.Diagnostic = &value
	}
	if source.Progress != nil {
		value := *source.Progress
		cloned.Progress = &value
	}
	if source.Usage != nil {
		value := *source.Usage
		cloned.Usage = &value
	}
	return cloned
}
