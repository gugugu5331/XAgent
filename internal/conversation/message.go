package conversation

import (
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"xagent/internal/artifact"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

type MessageRole string

const (
	RoleUser            MessageRole = "user"
	RoleAssistant       MessageRole = "assistant"
	RoleThinking        MessageRole = "thinking"
	RoleToolCall        MessageRole = "tool_call"
	RoleToolResult      MessageRole = "tool_result"
	RoleContextSummary  MessageRole = "context_summary"
	RoleContextBoundary MessageRole = "context_boundary"
)

// Message is the only message shape reachable from primary Conversation.
type Message struct {
	Role      MessageRole
	Content   redact.SafeText
	CreatedAt time.Time
	Tool      *ToolState
}

type ToolState struct {
	CallID           string
	Name             string
	ArgumentsJSON    redact.SafeText
	State            tool.ExecutionState
	Status           tool.ResultStatus
	Summary          redact.SafeText
	Result           redact.SafeText
	Truncated        bool
	TruncationReason redact.SafeText
	Artifact         *artifact.Ref
	Error            *tool.SafeError
}

// ToolResultMessageInput is the persistence-only half of a projected tool
// result. ModelContent is deliberately not representable here: callers must
// pass the PersistedContent, UserView, and OutputMeta returned by the single
// ContextManager projection.
type ToolResultMessageInput struct {
	CallID           string
	Name             string
	PersistedContent redact.SafeText
	UserView         tool.UserView
	OutputMeta       tool.OutputMeta
}

// AppendProjectedToolResultMessage appends one tool-result message without
// retaining the current run's model preview. The persisted text is used for
// both message content fields; user-facing preview text is intentionally not
// copied into Conversation.
func AppendProjectedToolResultMessage(conversation *Conversation, input ToolResultMessageInput) (int, error) {
	if conversation == nil {
		return -1, errors.New("conversation is unavailable")
	}
	if input.CallID == "" || input.Name == "" || input.PersistedContent.Text() == "" ||
		!utf8.ValidString(input.PersistedContent.Text()) || !input.UserView.State.CanProduceResult() ||
		!validProjectedResultStatus(input.UserView.Status) || input.OutputMeta.CapturedBytes < 0 {
		return -1, errors.New("projected tool result is invalid")
	}
	if input.UserView.Truncated != input.OutputMeta.Truncated ||
		input.UserView.TruncationReason.Text() != input.OutputMeta.TruncationReason.Text() ||
		!sameArtifactRef(input.UserView.Artifact, input.OutputMeta.Artifact) {
		return -1, errors.New("projected tool result is inconsistent")
	}

	now := time.Now()
	messageIndex := len(conversation.Messages)
	conversation.Messages = append(conversation.Messages, Message{
		Role:      RoleToolResult,
		Content:   input.PersistedContent,
		CreatedAt: now,
		Tool: &ToolState{
			CallID:           input.CallID,
			Name:             input.Name,
			State:            input.UserView.State,
			Status:           input.UserView.Status,
			Summary:          input.UserView.Summary,
			Result:           input.PersistedContent,
			Truncated:        input.OutputMeta.Truncated,
			TruncationReason: input.OutputMeta.TruncationReason,
			Artifact:         cloneMessageArtifactRef(input.OutputMeta.Artifact),
			Error:            cloneMessageSafeError(input.UserView.Error),
		},
	})
	conversation.UpdatedAt = now
	return messageIndex, nil
}

func validProjectedResultStatus(status tool.ResultStatus) bool {
	switch status {
	case tool.StatusSuccess, tool.StatusError, tool.StatusDenied, tool.StatusTimeout:
		return true
	default:
		return false
	}
}

func sameArtifactRef(left, right *artifact.Ref) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func cloneMessageArtifactRef(reference *artifact.Ref) *artifact.Ref {
	if reference == nil {
		return nil
	}
	cloned := *reference
	return &cloned
}

func cloneMessageSafeError(safe *tool.SafeError) *tool.SafeError {
	if safe == nil {
		return nil
	}
	cloned := *safe
	return &cloned
}

// Compatibility helpers construct only safe primary messages. Structured raw
// legacy payloads are intentionally ignored; callers must use safe tool views.
func AppendToolCallMessage(conversation *Conversation, callID, name, rawArguments string) {
	if conversation == nil {
		return
	}
	now := time.Now()
	conversation.Messages = append(conversation.Messages, Message{Role: RoleToolCall, Content: compatibilitySafeText(name), CreatedAt: now, Tool: &ToolState{CallID: callID, Name: name, ArgumentsJSON: compatibilitySafeText(rawArguments), State: tool.Prepared}})
	conversation.UpdatedAt = now
}

func AppendToolResultMessage(conversation *Conversation, callID, name, status, summary, content, errorCode string, truncated bool, _ json.RawMessage, errorData json.RawMessage) {
	if conversation == nil {
		return
	}
	now := time.Now()
	resultStatus := tool.ResultStatus(status)
	state := tool.Completed
	var safeError *tool.SafeError
	if errorCode != "" {
		safeError = &tool.SafeError{Code: errorCode, Message: compatibilitySafeText(string(errorData)), Recoverable: true}
	}
	conversation.Messages = append(conversation.Messages, Message{Role: RoleToolResult, Content: compatibilitySafeText(content), CreatedAt: now, Tool: &ToolState{CallID: callID, Name: name, State: state, Status: resultStatus, Summary: compatibilitySafeText(summary), Result: compatibilitySafeText(content), Truncated: truncated, Error: safeError}})
	conversation.UpdatedAt = now
}
