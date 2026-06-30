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
)

type ToolDisplay struct {
	CallID    string
	Name      string
	Arguments string
	Summary   string
	Status    ToolDisplayStatus
}

type ToolConfirmationRequest struct {
	CallID    string
	Name      string
	Arguments string
	Prompt    string
	Decision  chan ToolConfirmationDecision
}

type ToolConfirmationDecision struct {
	CallID  string
	Allowed bool
}

type Event struct {
	Type         Type
	Text         string
	Duration     time.Duration
	Err          error
	Tool         *ToolDisplay
	Confirmation *ToolConfirmationRequest
}
