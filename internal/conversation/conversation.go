package conversation

import (
	"strings"
	"time"

	"xagent/internal/redact"
)

// Conversation is the only runtime and persistence state accepted by Store.
// Every text field has crossed the runtime redaction boundary.
type Conversation struct {
	ID        string
	Title     redact.SafeText
	Messages  []Message
	Context   *ContextMetadata
	CreatedAt time.Time
	UpdatedAt time.Time
}

type ContextMetadata struct {
	Summary                 redact.SafeText
	LastBoundary            redact.SafeText
	LastCompressionAt       *time.Time
	SummaryFailureCount     int
	LastInputTokens         int64
	LastOutputTokens        int64
	LastEstimatedTokens     int64
	LastEstimatedCharacters int
}

func NewConversation(id string, now time.Time) *Conversation {
	redactor := redact.NewRuntimeRedactor()
	return &Conversation{ID: id, Title: redactor.Redact("新会话"), Messages: []Message{}, CreatedAt: now, UpdatedAt: now}
}

func AppendUserMessage(conversation *Conversation, text string) {
	appendMessage(conversation, RoleUser, text)
	if conversation != nil && conversation.Title.Text() == "新会话" {
		conversation.Title = compatibilitySafeText(titleFromText(text))
	}
}

func AppendAssistantMessage(conversation *Conversation, text string) {
	appendMessage(conversation, RoleAssistant, text)
}

func AppendContextSummaryMessage(conversation *Conversation, text string) {
	appendMessage(conversation, RoleContextSummary, text)
	ensureContext(conversation).Summary = compatibilitySafeText(text)
}

func AppendContextBoundaryMessage(conversation *Conversation, text string) {
	appendMessage(conversation, RoleContextBoundary, text)
	ensureContext(conversation).LastBoundary = compatibilitySafeText(text)
}

func AppendThinkingMessage(conversation *Conversation, text string) {
	if strings.TrimSpace(text) != "" {
		appendMessage(conversation, RoleThinking, text)
	}
}

func ContextMessages(conversation *Conversation) []Message {
	if conversation == nil {
		return nil
	}
	result := make([]Message, 0, len(conversation.Messages))
	for _, message := range conversation.Messages {
		switch message.Role {
		case RoleUser, RoleAssistant, RoleToolCall, RoleToolResult, RoleContextSummary, RoleContextBoundary:
			result = append(result, cloneV2MessageSlice([]Message{message})[0])
		}
	}
	return result
}

func EnsureContext(conversation *Conversation) *ContextMetadata { return ensureContext(conversation) }

func ensureContext(conversation *Conversation) *ContextMetadata {
	if conversation == nil {
		return &ContextMetadata{}
	}
	if conversation.Context == nil {
		conversation.Context = &ContextMetadata{}
	}
	return conversation.Context
}

func appendMessage(conversation *Conversation, role MessageRole, text string) {
	if conversation == nil {
		return
	}
	now := time.Now()
	conversation.Messages = append(conversation.Messages, Message{Role: role, Content: compatibilitySafeText(text), CreatedAt: now})
	conversation.UpdatedAt = now
}

func compatibilitySafeText(value string) redact.SafeText {
	return redact.NewRuntimeRedactor().Redact(value)
}

func titleFromText(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if text == "" {
		return "新会话"
	}
	runes := []rune(text)
	if len(runes) > 24 {
		return string(runes[:24]) + "..."
	}
	return text
}
