package conversation

import (
	"errors"
	"time"
	"unicode/utf8"

	"xagent/internal/redact"
)

const (
	conversationSnapshotMaxBytes    int64 = 256 << 20
	conversationSnapshotMaxMessages       = 1_000_000
)

var (
	ErrConversationSnapshotUnavailable = errors.New("conversation snapshot unavailable")
	ErrConversationSnapshotInvalid     = errors.New("conversation snapshot invalid")
	ErrConversationSnapshotTooLarge    = errors.New("conversation snapshot too large")
)

// ConversationSnapshot is a detached, immutable-by-API view captured at the
// parent's ordered commit boundary. Its fields remain private so callers can
// only obtain another deep copy through MaterializeEphemeral.
type ConversationSnapshot struct {
	id        string
	title     redact.SafeText
	messages  []Message
	context   *ContextMetadata
	createdAt time.Time
	updatedAt time.Time
}

// TakeSnapshot captures only model-visible persisted messages. Callers must
// invoke it while owning the conversation's ordered-commit boundary; this
// function intentionally does not race a mutable Messages slice with writers.
func TakeSnapshot(conversation *Conversation) (ConversationSnapshot, error) {
	if conversation == nil {
		return ConversationSnapshot{}, ErrConversationSnapshotUnavailable
	}
	if !validRecordIdentifier(conversation.ID) || !utf8.ValidString(conversation.Title.Text()) ||
		conversation.CreatedAt.IsZero() || conversation.UpdatedAt.IsZero() ||
		conversation.UpdatedAt.Before(conversation.CreatedAt) || len(conversation.Messages) > conversationSnapshotMaxMessages ||
		!validV2Context(conversation.Context) {
		return ConversationSnapshot{}, ErrConversationSnapshotInvalid
	}

	used, ok := boundedSnapshotStrings(conversationSnapshotMaxBytes, conversation.ID, conversation.Title.Text())
	if !ok {
		return ConversationSnapshot{}, ErrConversationSnapshotTooLarge
	}
	messages := make([]Message, 0, len(conversation.Messages))
	for index := range conversation.Messages {
		message := conversation.Messages[index]
		switch message.Role {
		case RoleUser, RoleAssistant, RoleToolCall, RoleToolResult, RoleContextSummary, RoleContextBoundary:
			if !validV2Messages([]Message{message}) {
				return ConversationSnapshot{}, ErrConversationSnapshotInvalid
			}
			messageBytes, ok := boundedSnapshotMessageBytes(message, conversationSnapshotMaxBytes-used)
			if !ok {
				return ConversationSnapshot{}, ErrConversationSnapshotTooLarge
			}
			used += messageBytes
			messages = append(messages, cloneV2MessageSlice([]Message{message})[0])
		case RoleThinking:
			if message.Tool != nil || message.Subagent != nil || message.CreatedAt.IsZero() {
				return ConversationSnapshot{}, ErrConversationSnapshotInvalid
			}
		case RoleSubagentNotification:
			if message.Tool != nil || message.Subagent == nil || message.Content.Text() != message.Subagent.Summary.Text() ||
				message.CreatedAt != message.Subagent.CreatedAt || !validSubagentNotification(*message.Subagent) {
				return ConversationSnapshot{}, ErrConversationSnapshotInvalid
			}
		default:
			return ConversationSnapshot{}, ErrConversationSnapshotInvalid
		}
	}

	var context *ContextMetadata
	if conversation.Context != nil {
		context = &ContextMetadata{
			Summary:      conversation.Context.Summary,
			LastBoundary: conversation.Context.LastBoundary,
		}
		if _, ok := boundedSnapshotStrings(conversationSnapshotMaxBytes-used, context.Summary.Text(), context.LastBoundary.Text()); !ok {
			return ConversationSnapshot{}, ErrConversationSnapshotTooLarge
		}
	}
	return ConversationSnapshot{
		id: conversation.ID, title: conversation.Title, messages: messages, context: context,
		createdAt: conversation.CreatedAt, updatedAt: conversation.UpdatedAt,
	}, nil
}

// MaterializeEphemeral creates a fresh task-local conversation. Parent
// timestamps and runtime accounting are intentionally not inherited.
func (snapshot ConversationSnapshot) MaterializeEphemeral(id string, now time.Time) *Conversation {
	if !validRecordIdentifier(id) || now.IsZero() || !validRecordIdentifier(snapshot.id) ||
		snapshot.createdAt.IsZero() || snapshot.updatedAt.IsZero() {
		return nil
	}
	materialized := &Conversation{
		ID: id, Title: snapshot.title, Messages: cloneV2MessageSlice(snapshot.messages),
		CreatedAt: now, UpdatedAt: now,
	}
	if snapshot.context != nil {
		materialized.Context = &ContextMetadata{
			Summary:      snapshot.context.Summary,
			LastBoundary: snapshot.context.LastBoundary,
		}
	}
	return materialized
}

func boundedSnapshotMessageBytes(message Message, remaining int64) (int64, bool) {
	if remaining < 0 {
		return 0, false
	}
	fields := []string{message.Content.Text()}
	if message.Tool != nil {
		fields = append(fields,
			message.Tool.CallID,
			message.Tool.Name,
			message.Tool.ArgumentsJSON.Text(),
			message.Tool.Summary.Text(),
			message.Tool.Result.Text(),
			message.Tool.TruncationReason.Text(),
		)
		if message.Tool.Artifact != nil {
			fields = append(fields, message.Tool.Artifact.ID)
		}
		if message.Tool.Error != nil {
			fields = append(fields, message.Tool.Error.Code, message.Tool.Error.Message.Text())
		}
	}
	return boundedSnapshotStrings(remaining, fields...)
}

func boundedSnapshotStrings(limit int64, fields ...string) (int64, bool) {
	if limit < 0 {
		return 0, false
	}
	var used int64
	for _, field := range fields {
		if !utf8.ValidString(field) || int64(len(field)) > limit-used {
			return 0, false
		}
		used += int64(len(field))
	}
	return used, true
}
