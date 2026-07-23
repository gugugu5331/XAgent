package hook

import (
	"context"
	"time"
)

type Event string

const (
	EventSystemStart   Event = "system_start"
	EventSystemStop    Event = "system_stop"
	EventSessionStart  Event = "session_start"
	EventSessionEnd    Event = "session_end"
	EventTurnStart     Event = "turn_start"
	EventTurnEnd       Event = "turn_end"
	EventMessageBefore Event = "message_before"
	EventMessageAfter  Event = "message_after"
	EventToolBefore    Event = "tool_before"
	EventToolAfter     Event = "tool_after"
	EventCompactBefore Event = "compact_before"
	EventCompactAfter  Event = "compact_after"
)

var allEvents = []Event{
	EventSystemStart, EventSystemStop, EventSessionStart, EventSessionEnd,
	EventTurnStart, EventTurnEnd, EventMessageBefore, EventMessageAfter,
	EventToolBefore, EventToolAfter, EventCompactBefore, EventCompactAfter,
}

type ExecutionKind string

const (
	ExecutionMain          ExecutionKind = "main"
	ExecutionIsolatedSkill ExecutionKind = "isolated_skill"
)

type HookMode string

const (
	ModeDefault HookMode = "default"
	ModePlan    HookMode = "plan"
)

type SessionState string

const (
	SessionNew     SessionState = "new"
	SessionResumed SessionState = "resumed"
)

type SessionEndReason string

const (
	SessionEndSwitch SessionEndReason = "switch"
	SessionEndExit   SessionEndReason = "exit"
)

type TurnStatus string

const (
	TurnCompleted     TurnStatus = "completed"
	TurnError         TurnStatus = "error"
	TurnCanceled      TurnStatus = "canceled"
	TurnMaxIterations TurnStatus = "max_iterations"
)

type MessageRole string

const (
	MessageUser      MessageRole = "user"
	MessageAssistant MessageRole = "assistant"
)

type ToolStatus string

const (
	ToolSuccess     ToolStatus = "success"
	ToolErrorStatus ToolStatus = "error"
	ToolTimeout     ToolStatus = "timeout"
)

type CompactReason string

const (
	CompactAuto   CompactReason = "auto"
	CompactManual CompactReason = "manual"
)

type CompactStatus string

const (
	CompactSuccess     CompactStatus = "success"
	CompactErrorStatus CompactStatus = "error"
)

type ExecutionRef struct {
	SessionID   string
	ExecutionID string
	TurnID      string
	Kind        ExecutionKind
	Mode        HookMode
}

type MessageToken struct {
	ref   ExecutionRef
	event *frozenEvent
}

type ToolInput struct {
	CallID    string
	Name      string
	Arguments map[string]any
}

type ToolError struct {
	Code        string
	Message     string
	Recoverable bool
}

type ToolOutput struct {
	Status  ToolStatus
	Content string
	Error   *ToolError
}

type CompactBinding struct {
	SessionID string
	Execution *ExecutionRef
}

type CompactStats struct {
	Messages        int `json:"messages"`
	EstimatedTokens int `json:"estimated_tokens"`
}

type CompactInput struct {
	Reason CompactReason
	Before CompactStats
}

type CompactOutput struct {
	Status CompactStatus
	After  *CompactStats
	Error  string
}

type CompactToken struct {
	binding CompactBinding
	event   *frozenEvent
}

type ToolDecisionKind string

const (
	DecisionContinue ToolDecisionKind = "continue"
	DecisionDeny     ToolDecisionKind = "deny"
)

type ToolDecision struct {
	Kind   ToolDecisionKind
	Reason string
}

func Continue() ToolDecision          { return ToolDecision{Kind: DecisionContinue} }
func Deny(reason string) ToolDecision { return ToolDecision{Kind: DecisionDeny, Reason: reason} }
func (d ToolDecision) IsDeny() bool   { return d.Kind == DecisionDeny }

type PromptBlock struct {
	Name    string
	Content string
	Source  string
}

type PromptLease interface {
	Blocks() []PromptBlock
	Commit()
	Release()
}

type Runtime interface {
	SystemStart(context.Context)
	Shutdown(context.Context) error
	SessionStart(context.Context, string, SessionState)
	SessionEnd(context.Context, string, SessionEndReason)
	BeginTurn(context.Context, string, ExecutionKind, HookMode) ExecutionRef
	EndTurn(context.Context, ExecutionRef, TurnStatus, string)
	BeginMessage(context.Context, ExecutionRef, MessageRole, string) MessageToken
	EndMessage(context.Context, MessageToken)
	BeforeTool(context.Context, ExecutionRef, ToolInput) ToolDecision
	AfterTool(context.Context, ExecutionRef, ToolInput, ToolOutput, time.Duration)
	BeforeCompact(context.Context, CompactBinding, CompactInput) CompactToken
	AfterCompact(context.Context, CompactToken, CompactOutput)
	AcquirePrompts(context.Context, ExecutionRef) (PromptLease, error)
}
