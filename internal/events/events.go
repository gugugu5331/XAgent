package events

import "time"

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
	DiagnosticEmitted      Type = "diagnostic_emitted"
	UsageUpdated            Type = "usage_updated"
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
	CallID            string
	Name              string
	Arguments         string
	Summary           string
	Status            ToolDisplayStatus
	ErrorCode         string
	Stdout            string
	Stderr            string
	Truncated         bool
	Recoverable       bool
	ArtifactID        string
	ArtifactBytes     int64
	ArtifactAvailable bool
}

type DiagnosticDisplay struct {
	Code     string
	Severity string
	Source   string
	Path     string
	Message  string
	Hint     string
}

type ToolConfirmationRequest struct {
	CallID         string
	Name           string
	Arguments      string
	Prompt         string
	Risk           string
	PermissionMode string
	ScopePreview   string
	Warning        string
	RevokeHint     string
	AllowPermanent bool
	Decision       chan ToolConfirmationDecision
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
	CallID  string
	Allowed bool
	Action  PermissionAction
}

type AgentProgress struct {
	Iteration  int
	Max        int
	StopReason string
	Message    string
}

type UsageDisplay struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

type Event struct {
	Type         Type
	Text         string
	Duration     time.Duration
	Err          error
	Tool         *ToolDisplay
	Confirmation *ToolConfirmationRequest
	Diagnostic   *DiagnosticDisplay
	Progress     *AgentProgress
	Usage        *UsageDisplay
}
