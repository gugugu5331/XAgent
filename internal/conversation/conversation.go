package conversation

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

type Conversation struct {
	ID        string           `json:"id"`
	Title     string           `json:"title"`
	Messages  []Message        `json:"messages"`
	Context   *ContextMetadata `json:"context,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

type ContextMetadata struct {
	Summary                 string     `json:"summary,omitempty"`
	LastBoundary            string     `json:"last_boundary,omitempty"`
	LastCompressionAt       *time.Time `json:"last_compression_at,omitempty"`
	SummaryFailureCount     int        `json:"summary_failure_count,omitempty"`
	LastInputTokens         int64      `json:"last_input_tokens,omitempty"`
	LastOutputTokens        int64      `json:"last_output_tokens,omitempty"`
	LastEstimatedTokens     int64      `json:"last_estimated_tokens,omitempty"`
	LastEstimatedCharacters int        `json:"last_estimated_characters,omitempty"`
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

func AppendContextSummaryMessage(conversation *Conversation, text string) {
	appendMessage(conversation, RoleContextSummary, text)
	ensureContext(conversation).Summary = text
}

func AppendContextBoundaryMessage(conversation *Conversation, text string) {
	appendMessage(conversation, RoleContextBoundary, text)
	ensureContext(conversation).LastBoundary = text
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

func AppendToolResultMessage(conversation *Conversation, callID string, name string, status string, summary string, content string, errorCode string, truncated bool, data json.RawMessage, errorData json.RawMessage) {
	appendToolMessage(conversation, Message{
		Role:                RoleToolResult,
		Content:             content,
		ToolCallID:          callID,
		ToolName:            name,
		ToolResultContent:   content,
		ToolResultStatus:    status,
		ToolResultSummary:   summary,
		ToolResultTruncated: truncated,
		ToolResultData:      data,
		ToolResultError:     errorData,
		ToolErrorCode:       errorCode,
	})
}

func ContextMessages(conversation *Conversation) []Message {
	if conversation == nil {
		return nil
	}
	messages := make([]Message, 0, len(conversation.Messages))
	for _, message := range conversation.Messages {
		if message.Role == RoleUser || message.Role == RoleAssistant || message.Role == RoleToolCall || message.Role == RoleToolResult || message.Role == RoleContextSummary || message.Role == RoleContextBoundary {
			messages = append(messages, contextMessage(message))
		}
	}
	return messages
}

func contextMessage(message Message) Message {
	if !message.Externalized {
		return message
	}
	content := externalizedContent(message)
	message.Content = content
	message.ToolResultContent = content
	return message
}

func externalizedContent(message Message) string {
	var builder strings.Builder
	if strings.TrimSpace(message.ExternalPreview) != "" {
		builder.WriteString(message.ExternalPreview)
	}
	if builder.Len() > 0 {
		builder.WriteString("\n\n")
	}
	builder.WriteString("[工具结果已外置保存")
	if message.ExternalBytes > 0 {
		builder.WriteString(", 大小 ")
		builder.WriteString(formatBytes(message.ExternalBytes))
	}
	if strings.TrimSpace(message.ExternalPath) != "" {
		builder.WriteString(", 路径: ")
		builder.WriteString(message.ExternalPath)
	}
	builder.WriteString("。如需完整细节，请重新读取该文件，不要根据预览或摘要脑补。]")
	return builder.String()
}

func formatBytes(size int64) string {
	return strconv.FormatInt(size, 10) + " bytes"
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

func EnsureContext(conversation *Conversation) *ContextMetadata {
	return ensureContext(conversation)
}

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
