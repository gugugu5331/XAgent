package conversation

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func TestConversationSnapshotDeepCopiesVisibleStateAndResetsRuntimeMetadata(t *testing.T) {
	createdAt := time.Date(2026, time.August, 15, 8, 0, 0, 0, time.UTC)
	compressedAt := createdAt.Add(time.Minute)
	redactor := redact.NewRuntimeRedactor()
	conversation := &Conversation{
		ID:    "parent-conversation",
		Title: redactor.Redact("parent title"),
		Messages: []Message{
			{Role: RoleUser, Content: redactor.Redact("parent user"), CreatedAt: createdAt},
			{Role: RoleThinking, Content: redactor.Redact("private thinking"), CreatedAt: createdAt.Add(time.Second)},
			{
				Role:      RoleToolResult,
				Content:   redactor.Redact("safe tool result"),
				CreatedAt: createdAt.Add(2 * time.Second),
				Tool: &ToolState{
					CallID: "call-1", Name: "Read", State: tool.Completed, Status: tool.StatusSuccess,
					Summary: redactor.Redact("safe summary"), Result: redactor.Redact("safe tool result"),
					Truncated: true, TruncationReason: redactor.Redact(string(tool.CaptureTruncatedInline)),
					Artifact: &artifact.Ref{ID: strings.Repeat("a", 64), Bytes: 12, CreatedAt: createdAt, Available: true, Complete: true},
					Error:    &tool.SafeError{Code: "safe_error", Message: redactor.Redact("safe error"), Recoverable: true},
				},
			},
			{Role: RoleContextSummary, Content: redactor.Redact("context summary"), CreatedAt: createdAt.Add(3 * time.Second)},
			{Role: RoleContextBoundary, Content: redactor.Redact("context boundary"), CreatedAt: createdAt.Add(4 * time.Second)},
		},
		Context: &ContextMetadata{
			Summary: redactor.Redact("context summary"), LastBoundary: redactor.Redact("context boundary"),
			LastCompressionAt: &compressedAt, SummaryFailureCount: 2, LastInputTokens: 101,
			LastOutputTokens: 202, LastEstimatedTokens: 303, LastEstimatedCharacters: 404,
		},
		CreatedAt: createdAt,
		UpdatedAt: createdAt.Add(5 * time.Second),
	}

	snapshot, err := TakeSnapshot(conversation)
	if err != nil {
		t.Fatalf("take snapshot: %v", err)
	}

	// Mutating the parent after the ordered capture boundary must not change the
	// task's immutable audit snapshot.
	conversation.Title = redactor.Redact("mutated title")
	conversation.Messages[0].Content = redactor.Redact("mutated user")
	conversation.Messages[2].Tool.Artifact.ID = strings.Repeat("b", 64)
	conversation.Messages[2].Tool.Error.Code = "mutated_error"
	conversation.Context.Summary = redactor.Redact("mutated context")

	childNow := createdAt.Add(time.Hour)
	child := snapshot.MaterializeEphemeral("child-conversation", childNow)
	if child == nil {
		t.Fatal("materialized conversation is nil")
	}
	if child.ID != "child-conversation" || child.Title.Text() != "parent title" || child.CreatedAt != childNow || child.UpdatedAt != childNow {
		t.Fatalf("materialized identity/times = %#v", child)
	}
	if len(child.Messages) != 4 {
		t.Fatalf("materialized messages = %#v, want visible user/tool/context messages only", child.Messages)
	}
	for _, message := range child.Messages {
		if message.Role == RoleThinking || message.Role == RoleSubagentNotification {
			t.Fatalf("transient message leaked into snapshot: %#v", message)
		}
	}
	if child.Messages[0].Content.Text() != "parent user" || child.Messages[1].Tool == nil ||
		child.Messages[1].Tool.Artifact == nil || child.Messages[1].Tool.Artifact.ID != strings.Repeat("a", 64) ||
		child.Messages[1].Tool.Error == nil || child.Messages[1].Tool.Error.Code != "safe_error" {
		t.Fatalf("snapshot did not deep-copy parent state: %#v", child.Messages)
	}
	if child.Context == nil || child.Context.Summary.Text() != "context summary" || child.Context.LastBoundary.Text() != "context boundary" {
		t.Fatalf("context semantics were not copied: %#v", child.Context)
	}
	if child.Context.LastCompressionAt != nil || child.Context.SummaryFailureCount != 0 ||
		child.Context.LastInputTokens != 0 || child.Context.LastOutputTokens != 0 ||
		child.Context.LastEstimatedTokens != 0 || child.Context.LastEstimatedCharacters != 0 {
		t.Fatalf("runtime context counters were not reset: %#v", child.Context)
	}

	// Materialization itself also returns a detached graph on every call.
	child.Messages[1].Tool.Artifact.ID = strings.Repeat("c", 64)
	child.Context.Summary = redactor.Redact("child mutation")
	second := snapshot.MaterializeEphemeral("second-child", childNow.Add(time.Second))
	if second == nil || second.Messages[1].Tool.Artifact.ID != strings.Repeat("a", 64) || second.Context.Summary.Text() != "context summary" {
		t.Fatalf("materialization aliased prior child: %#v", second)
	}
}

func TestConversationSnapshotRejectsUnavailableInputAndInvalidMaterialization(t *testing.T) {
	if _, err := TakeSnapshot(nil); err == nil {
		t.Fatal("nil conversation snapshot succeeded")
	}
	conversation := NewConversation("parent", time.Now())
	snapshot, err := TakeSnapshot(conversation)
	if err != nil {
		t.Fatal(err)
	}
	if child := snapshot.MaterializeEphemeral("", time.Now()); child != nil {
		t.Fatalf("empty child ID materialized: %#v", child)
	}
	if child := snapshot.MaterializeEphemeral("child", time.Time{}); child != nil {
		t.Fatalf("zero child time materialized: %#v", child)
	}
}

func TestConversationSnapshotByteBudgetFailsClosed(t *testing.T) {
	if _, ok := boundedSnapshotStrings(5, "abc", "def"); ok {
		t.Fatal("snapshot byte counter accepted fields above its limit")
	}
	if got, ok := boundedSnapshotStrings(6, "abc", "def"); !ok || got != 6 {
		t.Fatalf("snapshot byte counter = (%d,%v), want (6,true)", got, ok)
	}
	if _, ok := boundedSnapshotStrings(10, string([]byte{0xff})); ok {
		t.Fatal("snapshot byte counter accepted invalid UTF-8")
	}
}

func TestAppendSubagentNotificationValidatesAndDeduplicates(t *testing.T) {
	now := time.Date(2026, time.August, 15, 9, 0, 0, 0, time.UTC)
	redactor := redact.NewRuntimeRedactor()
	conversation := NewConversation("parent", now.Add(-time.Minute))
	notification := SubagentNotificationMessage{
		NotificationID: "notification-1",
		TaskID:         "task-1",
		Status:         "completed",
		Summary:        redactor.Redact("safe bounded summary"),
		StopReason:     "completed",
		CreatedAt:      now,
	}
	if err := AppendSubagentNotification(conversation, notification); err != nil {
		t.Fatalf("append notification: %v", err)
	}
	if err := AppendSubagentNotification(conversation, notification); err != nil {
		t.Fatalf("idempotent append: %v", err)
	}
	if len(conversation.Messages) != 1 {
		t.Fatalf("duplicate notification appended %d messages", len(conversation.Messages))
	}
	message := conversation.Messages[0]
	if message.Role != RoleSubagentNotification || message.Content.Text() != notification.Summary.Text() || message.Subagent == nil || *message.Subagent != notification {
		t.Fatalf("persisted notification = %#v", message)
	}
	if context := ContextMessages(conversation); len(context) != 0 {
		t.Fatalf("notification leaked into Provider context: %#v", context)
	}
	newerParentUpdate := now.Add(time.Minute)
	staleClockConversation := NewConversation("stale-clock", now.Add(-time.Hour))
	staleClockConversation.UpdatedAt = newerParentUpdate
	if err := AppendSubagentNotification(staleClockConversation, SubagentNotificationMessage{
		NotificationID: "notification-stale-clock", TaskID: "task-stale-clock", Status: "completed",
		Summary: redactor.Redact("safe"), StopReason: "completed", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if staleClockConversation.UpdatedAt != newerParentUpdate {
		t.Fatalf("notification regressed conversation UpdatedAt to %v", staleClockConversation.UpdatedAt)
	}

	invalid := []SubagentNotificationMessage{
		{},
		{NotificationID: strings.Repeat("n", SubagentNotificationMaxIDBytes+1), TaskID: "task", Status: "completed", Summary: redactor.Redact("ok"), StopReason: "completed", CreatedAt: now},
		{NotificationID: "notification", TaskID: "task\n", Status: "completed", Summary: redactor.Redact("ok"), StopReason: "completed", CreatedAt: now},
		{NotificationID: "notification", TaskID: "task", Status: "running", Summary: redactor.Redact("ok"), StopReason: "completed", CreatedAt: now},
		{NotificationID: "notification", TaskID: "task", Status: "completed", Summary: redactor.Redact(strings.Repeat("s", SubagentNotificationMaxSummaryBytes+1)), StopReason: "completed", CreatedAt: now},
		{NotificationID: "notification", TaskID: "task", Status: "completed", Summary: redactor.Redact("ok"), SummaryTruncated: false, TruncationReason: redactor.Redact("result_limit"), StopReason: "completed", CreatedAt: now},
		{NotificationID: "notification", TaskID: "task", Status: "completed", Summary: redactor.Redact("ok"), StopReason: "provider_error", CreatedAt: now},
	}
	for index, candidate := range invalid {
		copy := NewConversation("invalid", now)
		if err := AppendSubagentNotification(copy, candidate); err == nil || len(copy.Messages) != 0 {
			t.Fatalf("invalid notification %d appended: err=%v messages=%#v", index, err, copy.Messages)
		}
	}
}

func TestJSONLSubagentNotificationRoundTripAndValidation(t *testing.T) {
	now := time.Date(2026, time.August, 15, 10, 0, 0, 0, time.UTC)
	redactor := redact.NewRuntimeRedactor()
	conversation := NewConversation("notification-session", now)
	notification := SubagentNotificationMessage{
		NotificationID:   "notification-jsonl",
		TaskID:           "task-jsonl",
		Status:           "failed",
		Summary:          redactor.Redact("safe failure summary"),
		SummaryTruncated: true,
		TruncationReason: redactor.Redact("result_limit"),
		StopReason:       "provider_error",
		CreatedAt:        now.Add(time.Second),
	}
	if err := AppendSubagentNotification(conversation, notification); err != nil {
		t.Fatal(err)
	}
	state, err := computePersistedState(conversation, 1)
	if err != nil {
		t.Fatal(err)
	}
	record := JSONLRecordV2{Version: JSONLVersionV2, Kind: RecordSnapshot, SessionID: conversation.ID, Revision: 1, Digest: state.Digest, Snapshot: conversation}
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	line, err := codec.Encode(conversation.ID, record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(line, []byte(`"subagent_notification"`)) || bytes.Contains(line, []byte("parent_ref")) || bytes.Contains(line, []byte("tool_call_id")) {
		t.Fatalf("notification JSONL schema is unsafe: %s", line)
	}
	decoder, err := codec.NewDecoder(conversation.ID, bytes.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decoder.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(RecordValidationContext{ExpectedSessionID: conversation.ID}); err != nil {
		t.Fatalf("decoded notification invalid: %v", err)
	}
	if decoded.Snapshot == nil || len(decoded.Snapshot.Messages) != 1 || decoded.Snapshot.Messages[0].Subagent == nil ||
		*decoded.Snapshot.Messages[0].Subagent != notification {
		t.Fatalf("notification round trip = %#v", decoded.Snapshot)
	}

	invalid := cloneV2Conversation(decoded.Snapshot)
	invalid.Messages[0].Subagent.Status = "running"
	if validateV2Snapshot(invalid, invalid.ID) == nil {
		t.Fatal("JSONL accepted non-terminal notification status")
	}
	invalid = cloneV2Conversation(decoded.Snapshot)
	invalid.Messages[0].Subagent = nil
	if validateV2Snapshot(invalid, invalid.ID) == nil {
		t.Fatal("JSONL accepted notification role without payload")
	}
	invalid = cloneV2Conversation(decoded.Snapshot)
	invalid.Messages[0].Role = RoleAssistant
	if validateV2Snapshot(invalid, invalid.ID) == nil {
		t.Fatal("JSONL accepted notification payload on assistant role")
	}
}

func TestLegacyJSONLMigrationPersistsAndDeduplicatesSubagentNotification(t *testing.T) {
	const (
		sessionID = "notification-migrated-session"
		secret    = "notification-migration-secret"
	)
	now := time.Date(2026, time.August, 15, 11, 0, 0, 0, time.UTC)
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(secret)
	legacy := legacyConversation{
		ID: sessionID, Title: "legacy session",
		Messages:  []legacyMessage{{Role: RoleUser, Content: "legacy user message", CreatedAt: now}},
		CreatedAt: now, UpdatedAt: now,
	}
	v1 := encodeT315V1(t, JSONLRecord{
		Version: JSONLVersion, Type: RecordTypeSnapshot, SessionID: sessionID,
		MessageIndex: 0, CreatedAt: now, Snapshot: &legacy,
	})
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := loadLegacyConversationV2(
		context.Background(), codec, bytes.NewReader(v1),
		legacyLoadOptions{Format: legacyFormatJSONLV1, ExpectedSessionID: sessionID},
	)
	if err != nil {
		t.Fatal(err)
	}
	notification := SubagentNotificationMessage{
		NotificationID: "notification-after-migration", TaskID: "task-after-migration",
		Status: "completed", Summary: redactor.Redact("safe result " + secret),
		StopReason: "completed", CreatedAt: now.Add(time.Second),
	}
	if err := AppendSubagentNotification(migrated, notification); err != nil {
		t.Fatal(err)
	}
	state, err := computePersistedState(migrated, 1)
	if err != nil {
		t.Fatal(err)
	}
	line, err := codec.Encode(sessionID, JSONLRecordV2{
		Version: JSONLVersionV2, Kind: RecordSnapshot, SessionID: sessionID,
		Revision: 1, Digest: state.Digest, Snapshot: migrated,
	})
	if err != nil {
		t.Fatal(err)
	}
	decoder, err := codec.NewDecoder(sessionID, bytes.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := decoder.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Validate(RecordValidationContext{ExpectedSessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	if err := AppendSubagentNotification(reloaded.Snapshot, notification); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Snapshot.Messages) != 2 || reloaded.Snapshot.Messages[1].Subagent == nil ||
		reloaded.Snapshot.Messages[1].Subagent.NotificationID != notification.NotificationID {
		t.Fatalf("notification was not stable across migration/reload dedup: %#v", reloaded.Snapshot)
	}
	providerContext := ContextMessages(reloaded.Snapshot)
	if len(providerContext) != 1 || providerContext[0].Role != RoleUser || providerContext[0].Content.Text() != "legacy user message" {
		t.Fatalf("migrated notification entered Provider context: %#v", providerContext)
	}
	if bytes.Contains(line, []byte(secret)) || bytes.Contains(line, []byte("parent_ref")) || bytes.Contains(line, []byte("tool_call_id")) {
		t.Fatalf("migrated notification JSONL crossed the sensitive boundary: %s", line)
	}
}
