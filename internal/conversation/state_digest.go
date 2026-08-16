package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"xagent/internal/artifact"
	"xagent/internal/tool"
)

const (
	conversationSchemaVersion     = 2
	stateDigestEncodingVersion    = 1
	messagesDigestEncodingVersion = 1

	stateDigestDomain    = "xagent/conversation/state-digest"
	messagesDigestDomain = "xagent/conversation/messages-digest"
)

var errStateDigestEncoding = errors.New("conversation state cannot be canonicalized")

type canonicalStateV1 struct {
	Domain              string             `json:"domain"`
	EncodingVersion     int                `json:"encoding_version"`
	ConversationVersion int                `json:"conversation_version"`
	ID                  string             `json:"id"`
	Title               string             `json:"title"`
	CreatedAt           canonicalTimeV1    `json:"created_at"`
	UpdatedAt           canonicalTimeV1    `json:"updated_at"`
	MessageCount        int                `json:"message_count"`
	MessagesDigest      string             `json:"messages_digest"`
	Context             canonicalContextV1 `json:"context"`
}

type canonicalMessagesV1 struct {
	Domain              string               `json:"domain"`
	EncodingVersion     int                  `json:"encoding_version"`
	ConversationVersion int                  `json:"conversation_version"`
	Messages            []canonicalMessageV1 `json:"messages"`
}

type canonicalMessageV1 struct {
	Role      string                           `json:"role"`
	Content   string                           `json:"content"`
	CreatedAt canonicalTimeV1                  `json:"created_at"`
	Tool      canonicalToolV1                  `json:"tool"`
	Subagent  *canonicalSubagentNotificationV1 `json:"subagent_notification,omitempty"`
}

type canonicalSubagentNotificationV1 struct {
	NotificationID   string          `json:"notification_id"`
	TaskID           string          `json:"task_id"`
	Status           string          `json:"status"`
	Summary          string          `json:"summary"`
	SummaryTruncated bool            `json:"summary_truncated"`
	TruncationReason string          `json:"truncation_reason"`
	StopReason       string          `json:"stop_reason"`
	CreatedAt        canonicalTimeV1 `json:"created_at"`
}

type canonicalToolV1 struct {
	Present          bool                     `json:"present"`
	CallID           string                   `json:"call_id"`
	Name             string                   `json:"name"`
	ArgumentsJSON    string                   `json:"arguments_json"`
	State            string                   `json:"state"`
	Status           string                   `json:"status"`
	Summary          string                   `json:"summary"`
	Result           string                   `json:"result"`
	Truncated        bool                     `json:"truncated"`
	TruncationReason string                   `json:"truncation_reason"`
	Artifact         canonicalArtifactRefV1   `json:"artifact"`
	Error            canonicalToolSafeErrorV1 `json:"error"`
}

type canonicalArtifactRefV1 struct {
	Present   bool            `json:"present"`
	ID        string          `json:"id"`
	Bytes     int64           `json:"bytes"`
	CreatedAt canonicalTimeV1 `json:"created_at"`
	Available bool            `json:"available"`
	Complete  bool            `json:"complete"`
}

type canonicalToolSafeErrorV1 struct {
	Present     bool   `json:"present"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable"`
}

type canonicalContextV1 struct {
	Summary                 string                  `json:"summary"`
	LastBoundary            string                  `json:"last_boundary"`
	LastCompressionAt       canonicalOptionalTimeV1 `json:"last_compression_at"`
	SummaryFailureCount     int64                   `json:"summary_failure_count"`
	LastInputTokens         int64                   `json:"last_input_tokens"`
	LastOutputTokens        int64                   `json:"last_output_tokens"`
	LastEstimatedTokens     int64                   `json:"last_estimated_tokens"`
	LastEstimatedCharacters int64                   `json:"last_estimated_characters"`
}

type canonicalOptionalTimeV1 struct {
	Present bool            `json:"present"`
	Value   canonicalTimeV1 `json:"value"`
}

type canonicalTimeV1 struct {
	UnixSeconds int64 `json:"unix_seconds"`
	Nanoseconds int32 `json:"nanoseconds"`
}

// computePersistedState calculates both v2 digest domains without mutating
// the conversation. Revision belongs to the record chain and is copied into
// the result, but deliberately does not participate in either state digest.
func computePersistedState(conversation *Conversation, revision uint64) (PersistedState, error) {
	if conversation == nil {
		return PersistedState{}, errStateDigestEncoding
	}
	messagesDigest, err := computeMessagesDigest(conversation.Messages)
	if err != nil {
		return PersistedState{}, err
	}
	digest, err := computeStateDigest(conversation, len(conversation.Messages), messagesDigest)
	if err != nil {
		return PersistedState{}, err
	}
	return PersistedState{
		Revision:       revision,
		MessageCount:   len(conversation.Messages),
		Digest:         digest,
		MessagesDigest: messagesDigest,
	}, nil
}

func computeMessagesDigest(messages []Message) (StateDigest, error) {
	canonical := canonicalMessagesV1{
		Domain:              messagesDigestDomain,
		EncodingVersion:     messagesDigestEncodingVersion,
		ConversationVersion: conversationSchemaVersion,
		Messages:            make([]canonicalMessageV1, len(messages)),
	}
	for index := range messages {
		message, err := canonicalMessageFromV2(messages[index])
		if err != nil {
			return "", err
		}
		canonical.Messages[index] = message
	}
	return digestCanonicalMessages(canonical)
}

func computeStateDigest(conversation *Conversation, messageCount int, messagesDigest StateDigest) (StateDigest, error) {
	if conversation == nil || messageCount < 0 || !validCanonicalString(conversation.ID) || !validCanonicalString(conversation.Title.Text()) {
		return "", errStateDigestEncoding
	}
	context, err := canonicalContextFromV2(conversation.Context)
	if err != nil {
		return "", err
	}
	return digestCanonicalState(canonicalStateV1{
		Domain:              stateDigestDomain,
		EncodingVersion:     stateDigestEncodingVersion,
		ConversationVersion: conversationSchemaVersion,
		ID:                  conversation.ID,
		Title:               conversation.Title.Text(),
		CreatedAt:           canonicalTime(conversation.CreatedAt),
		UpdatedAt:           canonicalTime(conversation.UpdatedAt),
		MessageCount:        messageCount,
		MessagesDigest:      string(messagesDigest),
		Context:             context,
	})
}

func canonicalMessageFromV2(message Message) (canonicalMessageV1, error) {
	if !validCanonicalStrings(string(message.Role), message.Content.Text()) {
		return canonicalMessageV1{}, errStateDigestEncoding
	}
	toolState, err := canonicalToolFromV2(message.Tool)
	if err != nil {
		return canonicalMessageV1{}, err
	}
	subagent, err := canonicalSubagentNotificationFromV2(message.Subagent)
	if err != nil {
		return canonicalMessageV1{}, err
	}
	return canonicalMessageV1{
		Role:      string(message.Role),
		Content:   message.Content.Text(),
		CreatedAt: canonicalTime(message.CreatedAt),
		Tool:      toolState,
		Subagent:  subagent,
	}, nil
}

func canonicalSubagentNotificationFromV2(notification *SubagentNotificationMessage) (*canonicalSubagentNotificationV1, error) {
	if notification == nil {
		return nil, nil
	}
	if !validCanonicalStrings(
		notification.NotificationID,
		notification.TaskID,
		notification.Status,
		notification.Summary.Text(),
		notification.TruncationReason.Text(),
		notification.StopReason,
	) {
		return nil, errStateDigestEncoding
	}
	return &canonicalSubagentNotificationV1{
		NotificationID: notification.NotificationID, TaskID: notification.TaskID, Status: notification.Status,
		Summary: notification.Summary.Text(), SummaryTruncated: notification.SummaryTruncated,
		TruncationReason: notification.TruncationReason.Text(), StopReason: notification.StopReason,
		CreatedAt: canonicalTime(notification.CreatedAt),
	}, nil
}

func canonicalToolFromV2(state *ToolState) (canonicalToolV1, error) {
	if state == nil {
		return canonicalToolV1{}, nil
	}
	if !validCanonicalStrings(
		state.CallID,
		state.Name,
		state.ArgumentsJSON.Text(),
		string(state.State),
		string(state.Status),
		state.Summary.Text(),
		state.Result.Text(),
		state.TruncationReason.Text(),
	) {
		return canonicalToolV1{}, errStateDigestEncoding
	}
	artifactRef, err := canonicalArtifactFromRef(state.Artifact)
	if err != nil {
		return canonicalToolV1{}, err
	}
	safeError, err := canonicalErrorFromTool(state.Error)
	if err != nil {
		return canonicalToolV1{}, err
	}
	return canonicalToolV1{
		Present:          true,
		CallID:           state.CallID,
		Name:             state.Name,
		ArgumentsJSON:    state.ArgumentsJSON.Text(),
		State:            string(state.State),
		Status:           string(state.Status),
		Summary:          state.Summary.Text(),
		Result:           state.Result.Text(),
		Truncated:        state.Truncated,
		TruncationReason: state.TruncationReason.Text(),
		Artifact:         artifactRef,
		Error:            safeError,
	}, nil
}

func canonicalArtifactFromRef(ref *artifact.Ref) (canonicalArtifactRefV1, error) {
	if ref == nil {
		return canonicalArtifactRefV1{}, nil
	}
	if !validCanonicalString(ref.ID) {
		return canonicalArtifactRefV1{}, errStateDigestEncoding
	}
	return canonicalArtifactRefV1{
		Present:   true,
		ID:        ref.ID,
		Bytes:     ref.Bytes,
		CreatedAt: canonicalTime(ref.CreatedAt),
		Available: ref.Available,
		Complete:  ref.Complete,
	}, nil
}

func canonicalErrorFromTool(safeError *tool.SafeError) (canonicalToolSafeErrorV1, error) {
	if safeError == nil {
		return canonicalToolSafeErrorV1{}, nil
	}
	if !validCanonicalStrings(safeError.Code, safeError.Message.Text()) {
		return canonicalToolSafeErrorV1{}, errStateDigestEncoding
	}
	return canonicalToolSafeErrorV1{
		Present:     true,
		Code:        safeError.Code,
		Message:     safeError.Message.Text(),
		Recoverable: safeError.Recoverable,
	}, nil
}

func canonicalContextFromV2(context *ContextMetadata) (canonicalContextV1, error) {
	if context == nil {
		return canonicalContextV1{}, nil
	}
	if !validCanonicalStrings(context.Summary.Text(), context.LastBoundary.Text()) {
		return canonicalContextV1{}, errStateDigestEncoding
	}
	return canonicalContextV1{
		Summary:                 context.Summary.Text(),
		LastBoundary:            context.LastBoundary.Text(),
		LastCompressionAt:       canonicalOptionalTime(context.LastCompressionAt),
		SummaryFailureCount:     int64(context.SummaryFailureCount),
		LastInputTokens:         context.LastInputTokens,
		LastOutputTokens:        context.LastOutputTokens,
		LastEstimatedTokens:     context.LastEstimatedTokens,
		LastEstimatedCharacters: int64(context.LastEstimatedCharacters),
	}, nil
}

func canonicalOptionalTime(value *time.Time) canonicalOptionalTimeV1 {
	if value == nil {
		return canonicalOptionalTimeV1{}
	}
	return canonicalOptionalTimeV1{Present: true, Value: canonicalTime(*value)}
}

func canonicalTime(value time.Time) canonicalTimeV1 {
	value = value.UTC()
	return canonicalTimeV1{UnixSeconds: value.Unix(), Nanoseconds: int32(value.Nanosecond())}
}

func digestCanonicalState(value canonicalStateV1) (StateDigest, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", errStateDigestEncoding
	}
	digest := sha256.Sum256(encoded)
	return StateDigest(hex.EncodeToString(digest[:])), nil
}

func digestCanonicalMessages(value canonicalMessagesV1) (StateDigest, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", errStateDigestEncoding
	}
	digest := sha256.Sum256(encoded)
	return StateDigest(hex.EncodeToString(digest[:])), nil
}

func validCanonicalStrings(values ...string) bool {
	for _, value := range values {
		if !validCanonicalString(value) {
			return false
		}
	}
	return true
}

func validCanonicalString(value string) bool {
	return utf8.ValidString(value)
}
