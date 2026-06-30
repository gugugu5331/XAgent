package conversation

import (
	"strings"
	"time"
)

type Conversation struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Messages  []Message `json:"messages"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func NewConversation(id string, now time.Time) *Conversation {
	return &Conversation{
		ID:        id,
		Title:     "新会话",
		Messages:  []Message{},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func AppendUserMessage(conversation *Conversation, text string) {
	appendMessage(conversation, RoleUser, text)
	if conversation.Title == "新会话" {
		conversation.Title = titleFromText(text)
	}
}

func AppendAssistantMessage(conversation *Conversation, text string) {
	appendMessage(conversation, RoleAssistant, text)
}

func AppendThinkingMessage(conversation *Conversation, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	appendMessage(conversation, RoleThinking, text)
}

func AppendToolCallMessage(conversation *Conversation, callID string, name string, rawArguments string) {
	appendToolMessage(conversation, Message{
		Role:             RoleToolCall,
		Content:          name + "(" + rawArguments + ")",
		ToolCallID:       callID,
		ToolName:         name,
		RawToolArguments: rawArguments,
	})
}

func AppendToolResultMessage(conversation *Conversation, callID string, name string, status string, summary string, content string, errorCode string) {
	appendToolMessage(conversation, Message{
		Role:              RoleToolResult,
		Content:           content,
		ToolCallID:        callID,
		ToolName:          name,
		ToolResultContent: content,
		ToolResultStatus:  status,
		ToolResultSummary: summary,
		ToolErrorCode:     errorCode,
	})
}

func ContextMessages(conversation *Conversation) []Message {
	if conversation == nil {
		return nil
	}
	messages := make([]Message, 0, len(conversation.Messages))
	for _, message := range conversation.Messages {
		if message.Role == RoleUser || message.Role == RoleAssistant || message.Role == RoleToolCall || message.Role == RoleToolResult {
			messages = append(messages, message)
		}
	}
	return messages
}

func appendToolMessage(conversation *Conversation, message Message) {
	if conversation == nil {
		return
	}
	now := time.Now()
	message.CreatedAt = now
	conversation.Messages = append(conversation.Messages, message)
	conversation.UpdatedAt = now
}

func appendMessage(conversation *Conversation, role MessageRole, text string) {
	if conversation == nil {
		return
	}
	now := time.Now()
	conversation.Messages = append(conversation.Messages, Message{
		Role:      role,
		Content:   text,
		CreatedAt: now,
	})
	conversation.UpdatedAt = now
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
