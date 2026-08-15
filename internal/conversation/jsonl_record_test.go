package conversation

import (
	"bytes"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func TestJSONLPersistsOnlyPersistedContentAndOpaqueRef(t *testing.T) {
	const modelPreviewCanary = "model-preview-must-not-survive-restart"
	createdAt := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	reference := &artifact.Ref{
		ID:        strings.Repeat("a", 64),
		Bytes:     128,
		CreatedAt: createdAt,
		Available: true,
		Complete:  true,
	}
	redactor := redact.NewRuntimeRedactor()
	factory, err := tool.NewResultFactory(redactor)
	if err != nil {
		t.Fatal(err)
	}
	result, err := factory.Build(tool.ResultFactoryInput{
		CallID:           "call-1",
		Name:             "Read",
		State:            tool.Completed,
		Status:           tool.StatusSuccess,
		Summary:          "safe persisted summary",
		Preview:          modelPreviewCanary,
		Artifact:         reference,
		CapturedBytes:    reference.Bytes,
		Truncated:        true,
		TruncationReason: string(tool.CaptureTruncatedInline),
	})
	if err != nil {
		t.Fatal(err)
	}
	modelContent := result.ModelContent()
	userView := result.UserView()
	persistedContent := result.PersistedContent()
	outputMeta := result.OutputMeta()
	if !strings.Contains(modelContent.Text(), modelPreviewCanary) || strings.Contains(persistedContent.Text(), modelPreviewCanary) {
		t.Fatalf("factory fixture did not separate model and persistence views: model=%q persisted=%q", modelContent.Text(), persistedContent.Text())
	}

	conversation := NewConversation("conversation-1", createdAt)
	messageIndex, err := AppendProjectedToolResultMessage(conversation, ToolResultMessageInput{
		CallID:           "call-1",
		Name:             "Read",
		PersistedContent: persistedContent,
		UserView:         userView,
		OutputMeta:       outputMeta,
	})
	if err != nil {
		t.Fatal(err)
	}
	if messageIndex != 0 || len(conversation.Messages) != 1 {
		t.Fatalf("unexpected append result: index=%d messages=%d", messageIndex, len(conversation.Messages))
	}
	message := conversation.Messages[0]
	if message.Tool == nil || message.Content.Text() != persistedContent.Text() || message.Tool.Result.Text() != persistedContent.Text() ||
		strings.Contains(message.Content.Text(), modelPreviewCanary) || strings.Contains(message.Tool.Result.Text(), modelPreviewCanary) {
		t.Fatalf("conversation retained a non-persisted result view: %#v", message)
	}
	if message.Tool.Artifact == nil || *message.Tool.Artifact != *reference || message.Tool.Artifact == reference {
		t.Fatalf("conversation did not retain an independent opaque ref: %#v", message.Tool.Artifact)
	}

	state, err := computePersistedState(conversation, 1)
	if err != nil {
		t.Fatal(err)
	}
	record := JSONLRecordV2{
		Version:   JSONLVersionV2,
		Kind:      RecordSnapshot,
		SessionID: conversation.ID,
		Revision:  1,
		Digest:    state.Digest,
		Snapshot:  conversation,
	}
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	line, err := codec.Encode(conversation.ID, record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(line, []byte(modelPreviewCanary)) {
		t.Fatalf("JSONL retained ModelContent preview: %s", line)
	}
	if !bytes.Contains(line, []byte(reference.ID)) {
		t.Fatalf("JSONL omitted opaque artifact ref: %s", line)
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
		t.Fatal(err)
	}
	restarted := decoded.Snapshot
	if restarted == nil || len(restarted.Messages) != 1 || restarted.Messages[0].Tool == nil {
		t.Fatalf("restart lost tool result state: %#v", restarted)
	}
	restartedMessage := restarted.Messages[0]
	if strings.Contains(restartedMessage.Content.Text(), modelPreviewCanary) || strings.Contains(restartedMessage.Tool.Result.Text(), modelPreviewCanary) ||
		restartedMessage.Content.Text() != persistedContent.Text() || restartedMessage.Tool.Result.Text() != persistedContent.Text() ||
		restartedMessage.Tool.Artifact == nil || *restartedMessage.Tool.Artifact != *reference {
		t.Fatalf("restart restored a non-persisted projection: %#v", restartedMessage)
	}

	validateSnapshot := func(snapshot *Conversation) error {
		state, stateErr := computePersistedState(snapshot, 1)
		if stateErr != nil {
			return stateErr
		}
		candidate := JSONLRecordV2{
			Version:   JSONLVersionV2,
			Kind:      RecordSnapshot,
			SessionID: snapshot.ID,
			Revision:  1,
			Digest:    state.Digest,
			Snapshot:  snapshot,
		}
		encoded, encodeErr := codec.Encode(snapshot.ID, candidate)
		if encodeErr != nil {
			return encodeErr
		}
		candidateDecoder, decoderErr := codec.NewDecoder(snapshot.ID, bytes.NewReader(encoded))
		if decoderErr != nil {
			return decoderErr
		}
		untrusted, decodeErr := candidateDecoder.Decode()
		if decodeErr != nil {
			return decodeErr
		}
		return untrusted.Validate(RecordValidationContext{ExpectedSessionID: snapshot.ID})
	}

	negativeMutations := []struct {
		name   string
		mutate func(*ToolState)
	}{
		{name: "short artifact id", mutate: func(state *ToolState) { state.Artifact.ID = "abcd" }},
		{name: "uppercase artifact id", mutate: func(state *ToolState) { state.Artifact.ID = strings.Repeat("A", 64) }},
		{name: "path-like artifact id", mutate: func(state *ToolState) { state.Artifact.ID = strings.Repeat("a", 63) + "/" }},
		{name: "unavailable artifact", mutate: func(state *ToolState) { state.Artifact.Available = false }},
		{name: "zero captured bytes", mutate: func(state *ToolState) { state.Artifact.Bytes = 0 }},
		{name: "negative captured bytes", mutate: func(state *ToolState) { state.Artifact.Bytes = -1 }},
		{name: "ref without truncation", mutate: func(state *ToolState) { state.Truncated = false }},
		{name: "complete ref with incomplete reason", mutate: func(state *ToolState) {
			state.TruncationReason = redactor.Redact(string(tool.CaptureTruncatedHardLimit))
		}},
		{name: "complete ref with other legacy reason", mutate: func(state *ToolState) {
			state.TruncationReason = redactor.Redact("legacy_output_truncated")
		}},
		{name: "complete ref with unknown legacy reason", mutate: func(state *ToolState) {
			state.TruncationReason = redactor.Redact("legacy_output_future")
		}},
		{name: "incomplete ref with inline reason", mutate: func(state *ToolState) { state.Artifact.Complete = false }},
		{name: "incomplete ref with unknown reason", mutate: func(state *ToolState) {
			state.Artifact.Complete = false
			state.TruncationReason = redactor.Redact("unknown_reason")
		}},
		{name: "missing ref with truncation", mutate: func(state *ToolState) { state.Artifact = nil }},
		{name: "missing ref with unknown legacy reason", mutate: func(state *ToolState) {
			state.Artifact = nil
			state.TruncationReason = redactor.Redact("legacy_output_future")
		}},
		{name: "missing ref with reason", mutate: func(state *ToolState) {
			state.Artifact = nil
			state.Truncated = false
		}},
	}
	for _, mutation := range negativeMutations {
		candidate := cloneConversationV2(conversation)
		state := candidate.Messages[0].Tool
		mutation.mutate(state)
		if mutation.name == "missing ref with reason" {
			state.TruncationReason = redactor.Redact(string(tool.CaptureTruncatedInline))
		}
		if err := validateSnapshot(candidate); err == nil {
			t.Fatalf("untrusted JSONL accepted %s", mutation.name)
		}
	}

	for _, reason := range []tool.CaptureTruncationReason{
		tool.CaptureTruncatedHardLimit,
		tool.CaptureTruncatedArtifact,
		tool.CaptureTruncatedWriteFailure,
		tool.CaptureTruncatedCanceled,
	} {
		candidate := cloneConversationV2(conversation)
		state := candidate.Messages[0].Tool
		state.Artifact.Complete = false
		state.TruncationReason = redactor.Redact(string(reason))
		if err := validateSnapshot(candidate); err != nil {
			t.Fatalf("valid incomplete artifact reason %q rejected: %v", reason, err)
		}
	}

	legacyCompatibility := []struct {
		name   string
		mutate func(*ToolState)
	}{
		{name: "complete imported ref", mutate: func(state *ToolState) {
			state.TruncationReason = redactor.Redact("legacy_output_externalized")
		}},
		{name: "externalized without ref", mutate: func(state *ToolState) {
			state.Artifact = nil
			state.TruncationReason = redactor.Redact("legacy_output_externalized")
		}},
		{name: "truncated without ref", mutate: func(state *ToolState) {
			state.Artifact = nil
			state.TruncationReason = redactor.Redact("legacy_output_truncated")
		}},
	}
	for _, compatibility := range legacyCompatibility {
		candidate := cloneConversationV2(conversation)
		compatibility.mutate(candidate.Messages[0].Tool)
		if err := validateSnapshot(candidate); err != nil {
			t.Fatalf("valid legacy compatibility state %q rejected: %v", compatibility.name, err)
		}
	}
}

func TestV2RecordValidationMatrix(t *testing.T) {
	t.Run("valid records", func(t *testing.T) {
		cases := []struct {
			name    string
			record  JSONLRecordV2
			context RecordValidationContext
		}{
			{
				name:    "revision one snapshot anchor",
				record:  validV2SnapshotRecord(t, 1),
				context: RecordValidationContext{ExpectedSessionID: "conversation-1"},
			},
			{
				name:    "higher revision checkpoint anchor",
				record:  validV2SnapshotRecord(t, 27),
				context: RecordValidationContext{ExpectedSessionID: "conversation-1"},
			},
			{
				name:    "successor batch",
				record:  validV2BatchRecord(t),
				context: validV2SuccessorContext(t),
			},
			{
				name:    "successor snapshot",
				record:  validV2SuccessorSnapshotRecord(t),
				context: validV2SuccessorContext(t),
			},
		}

		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				assertV2Validation(t, testCase.record, testCase.context, "")
			})
		}
	})

	t.Run("invalid records", func(t *testing.T) {
		type fixture func(*testing.T) (JSONLRecordV2, RecordValidationContext)
		anchor := func(t *testing.T) (JSONLRecordV2, RecordValidationContext) {
			return validV2SnapshotRecord(t, 1), RecordValidationContext{ExpectedSessionID: "conversation-1"}
		}
		batch := func(t *testing.T) (JSONLRecordV2, RecordValidationContext) {
			return validV2BatchRecord(t), validV2SuccessorContext(t)
		}
		successorSnapshot := func(t *testing.T) (JSONLRecordV2, RecordValidationContext) {
			return validV2SuccessorSnapshotRecord(t), validV2SuccessorContext(t)
		}

		cases := []struct {
			name    string
			fixture fixture
			mutate  func(*JSONLRecordV2, *RecordValidationContext)
			want    v2RecordValidationCode
		}{
			{name: "zero version", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Version = 0 }, want: v2RecordInvalidVersion},
			{name: "legacy version", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Version = JSONLVersion }, want: v2RecordInvalidVersion},
			{name: "future version", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Version = JSONLVersionV2 + 1 }, want: v2RecordInvalidVersion},
			{name: "empty kind", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Kind = "" }, want: v2RecordInvalidKind},
			{name: "legacy message kind", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Kind = "message" }, want: v2RecordInvalidKind},
			{name: "unknown kind", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Kind = "delta" }, want: v2RecordInvalidKind},
			{name: "empty record session", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.SessionID = "" }, want: v2RecordInvalidSession},
			{name: "blank record session", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.SessionID = "  " }, want: v2RecordInvalidSession},
			{name: "mismatched record session", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.SessionID = "another-session" }, want: v2RecordInvalidSession},
			{name: "control character in record session", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.SessionID = "conversation-1\n" }, want: v2RecordInvalidSession},
			{name: "invalid UTF-8 record session", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.SessionID = string([]byte{0xff}) }, want: v2RecordInvalidSession},
			{name: "empty expected session", fixture: anchor, mutate: func(_ *JSONLRecordV2, c *RecordValidationContext) { c.ExpectedSessionID = "" }, want: v2RecordInvalidSession},
			{name: "blank expected session", fixture: anchor, mutate: func(_ *JSONLRecordV2, c *RecordValidationContext) { c.ExpectedSessionID = " conversation-1" }, want: v2RecordInvalidSession},
			{name: "zero revision", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Revision = 0 }, want: v2RecordInvalidRevision},
			{name: "duplicate revision", fixture: batch, mutate: func(r *JSONLRecordV2, c *RecordValidationContext) { r.Revision = c.Previous.Revision }, want: v2RecordInvalidRevision},
			{name: "backward revision", fixture: batch, mutate: func(r *JSONLRecordV2, c *RecordValidationContext) { r.Revision = c.Previous.Revision - 1 }, want: v2RecordInvalidRevision},
			{name: "skipped revision", fixture: batch, mutate: func(r *JSONLRecordV2, c *RecordValidationContext) { r.Revision = c.Previous.Revision + 2 }, want: v2RecordInvalidRevision},
			{name: "previous revision overflow", fixture: batch, mutate: func(_ *JSONLRecordV2, c *RecordValidationContext) { c.Previous.Revision = math.MaxUint64 }, want: v2RecordInvalidPreviousState},
			{name: "anchor has previous digest", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) {
				r.PreviousDigest = StateDigest(strings.Repeat("a", sha256HexLength))
			}, want: v2RecordInvalidPreviousLink},
			{name: "successor missing previous digest", fixture: batch, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.PreviousDigest = "" }, want: v2RecordInvalidPreviousLink},
			{name: "successor wrong previous digest", fixture: batch, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) {
				r.PreviousDigest = StateDigest(strings.Repeat("a", sha256HexLength))
			}, want: v2RecordInvalidPreviousLink},
			{name: "previous zero revision", fixture: batch, mutate: func(_ *JSONLRecordV2, c *RecordValidationContext) { c.Previous.Revision = 0 }, want: v2RecordInvalidPreviousState},
			{name: "previous negative message count", fixture: batch, mutate: func(_ *JSONLRecordV2, c *RecordValidationContext) { c.Previous.MessageCount = -1 }, want: v2RecordInvalidPreviousState},
			{name: "previous malformed digest", fixture: batch, mutate: func(_ *JSONLRecordV2, c *RecordValidationContext) { c.Previous.Digest = "bad" }, want: v2RecordInvalidPreviousState},
			{name: "previous malformed messages digest", fixture: batch, mutate: func(_ *JSONLRecordV2, c *RecordValidationContext) { c.Previous.MessagesDigest = "bad" }, want: v2RecordInvalidPreviousState},
			{name: "empty digest", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Digest = "" }, want: v2RecordInvalidDigest},
			{name: "short digest", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Digest = "abcd" }, want: v2RecordInvalidDigest},
			{name: "non hexadecimal digest", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) {
				r.Digest = StateDigest(strings.Repeat("g", sha256HexLength))
			}, want: v2RecordInvalidDigest},
			{name: "uppercase digest", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) {
				r.Digest = StateDigest(strings.Repeat("A", sha256HexLength))
			}, want: v2RecordInvalidDigest},
			{name: "both payloads absent", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot = nil }, want: v2RecordInvalidPayload},
			{name: "both payloads present", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Batch = validV2BatchPayload() }, want: v2RecordInvalidPayload},
			{name: "batch kind with snapshot payload", fixture: successorSnapshot, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Kind = RecordBatch }, want: v2RecordInvalidPayload},
			{name: "snapshot kind with batch payload", fixture: batch, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Kind = RecordSnapshot }, want: v2RecordInvalidPayload},
			{name: "anchor cannot be batch", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) {
				r.Kind, r.Batch, r.Snapshot = RecordBatch, validV2BatchPayload(), nil
			}, want: v2RecordInvalidPayload},
			{name: "nil batch messages", fixture: batch, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Batch.Messages = nil }, want: v2RecordInvalidBatch},
			{name: "empty batch messages", fixture: batch, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Batch.Messages = []Message{} }, want: v2RecordInvalidBatch},
			{name: "wrong batch base count", fixture: batch, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Batch.BaseMessageCount++ }, want: v2RecordInvalidBatch},
			{name: "negative batch base count", fixture: batch, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Batch.BaseMessageCount = -1 }, want: v2RecordInvalidBatch},
			{name: "zero batch updated time", fixture: batch, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Batch.UpdatedAt = time.Time{} }, want: v2RecordInvalidBatch},
			{name: "empty snapshot ID", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.ID = "" }, want: v2RecordInvalidSnapshot},
			{name: "mismatched snapshot ID", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.ID = "another-session" }, want: v2RecordInvalidSnapshot},
			{name: "zero snapshot created time", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.CreatedAt = time.Time{} }, want: v2RecordInvalidSnapshot},
			{name: "zero snapshot updated time", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.UpdatedAt = time.Time{} }, want: v2RecordInvalidSnapshot},
			{name: "unknown message role", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.Messages[0].Role = "operator" }, want: v2RecordInvalidSnapshot},
			{name: "tool role without tool state", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.Messages[0].Role = RoleToolCall }, want: v2RecordInvalidSnapshot},
			{name: "non-tool role with tool state", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.Messages[1].Role = RoleAssistant }, want: v2RecordInvalidSnapshot},
			{name: "unknown execution state", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.Messages[1].Tool.State = "queued" }, want: v2RecordInvalidSnapshot},
			{name: "unknown result status", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.Messages[1].Tool.Status = "partial" }, want: v2RecordInvalidSnapshot},
			{name: "zero message created time", fixture: batch, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Batch.Messages[0].CreatedAt = time.Time{} }, want: v2RecordInvalidBatch},
			{name: "negative context counter", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.Context.LastInputTokens = -1 }, want: v2RecordInvalidSnapshot},
			{name: "empty artifact ID", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.Messages[1].Tool.Artifact.ID = "" }, want: v2RecordInvalidSnapshot},
			{name: "negative artifact bytes", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.Messages[1].Tool.Artifact.Bytes = -1 }, want: v2RecordInvalidSnapshot},
			{name: "empty safe error code", fixture: anchor, mutate: func(r *JSONLRecordV2, _ *RecordValidationContext) { r.Snapshot.Messages[1].Tool.Error.Code = "" }, want: v2RecordInvalidSnapshot},
		}

		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				record, context := testCase.fixture(t)
				testCase.mutate(&record, &context)
				assertV2Validation(t, record, context, testCase.want)
			})
		}
	})

	t.Run("errors contain codes only", func(t *testing.T) {
		const canarySession = "session-secret-canary"
		const canaryBody = "message-body-secret-canary"
		const canaryDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		record := validV2SnapshotRecord(t, 1)
		record.SessionID = canarySession
		record.Digest = canaryDigest
		record.Snapshot.Messages[0].Content = digestSafeText(canaryBody)

		err := record.Validate(RecordValidationContext{ExpectedSessionID: "conversation-1"})
		if err == nil {
			t.Fatal("canary record unexpectedly passed validation")
		}
		message := err.Error()
		for _, canary := range []string{canarySession, canaryBody, canaryDigest} {
			if strings.Contains(message, canary) {
				t.Fatalf("validation error leaked canary %q: %q", canary, message)
			}
		}
		if message != string(v2RecordInvalidSession) {
			t.Fatalf("validation error was not code-only: %q", message)
		}
	})
}

func assertV2Validation(t *testing.T, record JSONLRecordV2, context RecordValidationContext, want v2RecordValidationCode) {
	t.Helper()
	beforeRecord := cloneJSONLRecordV2(record)
	beforeContext := cloneRecordValidationContext(context)

	err := record.Validate(context)
	if want == "" {
		if err != nil {
			t.Fatalf("valid record rejected: %v", err)
		}
	} else if err == nil || err.Error() != string(want) {
		t.Fatalf("validation error = %v, want %s", err, want)
	}
	if !reflect.DeepEqual(record, beforeRecord) {
		t.Fatal("Validate mutated its record input")
	}
	if !reflect.DeepEqual(context, beforeContext) {
		t.Fatal("Validate mutated its context input")
	}
}

func validV2SnapshotRecord(t *testing.T, revision uint64) JSONLRecordV2 {
	t.Helper()
	snapshot := stateDigestFixture()
	state := mustComputePersistedState(t, snapshot, revision)
	return JSONLRecordV2{
		Version:   JSONLVersionV2,
		Kind:      RecordSnapshot,
		SessionID: snapshot.ID,
		Revision:  revision,
		Digest:    state.Digest,
		Snapshot:  snapshot,
	}
}

func validV2SuccessorContext(t *testing.T) RecordValidationContext {
	t.Helper()
	previous := mustComputePersistedState(t, stateDigestFixture(), 1)
	return RecordValidationContext{ExpectedSessionID: "conversation-1", Previous: &previous}
}

func validV2BatchRecord(t *testing.T) JSONLRecordV2 {
	t.Helper()
	context := validV2SuccessorContext(t)
	return JSONLRecordV2{
		Version:        JSONLVersionV2,
		Kind:           RecordBatch,
		SessionID:      context.ExpectedSessionID,
		Revision:       context.Previous.Revision + 1,
		PreviousDigest: context.Previous.Digest,
		Digest:         StateDigest(strings.Repeat("b", sha256HexLength)),
		Batch:          validV2BatchPayload(),
	}
}

func validV2BatchPayload() *MessageBatch {
	return &MessageBatch{
		BaseMessageCount: 2,
		Messages: []Message{{
			Role:      RoleAssistant,
			Content:   digestSafeText("bounded append"),
			CreatedAt: time.Date(2026, time.August, 2, 14, 6, 0, 0, time.UTC),
		}},
		UpdatedAt: time.Date(2026, time.August, 2, 14, 6, 1, 0, time.UTC),
	}
}

func validV2SuccessorSnapshotRecord(t *testing.T) JSONLRecordV2 {
	t.Helper()
	context := validV2SuccessorContext(t)
	snapshot := stateDigestFixture()
	snapshot.UpdatedAt = snapshot.UpdatedAt.Add(time.Minute)
	state := mustComputePersistedState(t, snapshot, context.Previous.Revision+1)
	return JSONLRecordV2{
		Version:        JSONLVersionV2,
		Kind:           RecordSnapshot,
		SessionID:      context.ExpectedSessionID,
		Revision:       context.Previous.Revision + 1,
		PreviousDigest: context.Previous.Digest,
		Digest:         state.Digest,
		Snapshot:       snapshot,
	}
}

func cloneJSONLRecordV2(source JSONLRecordV2) JSONLRecordV2 {
	cloned := source
	cloned.Snapshot = cloneConversationV2(source.Snapshot)
	if source.Batch != nil {
		batch := *source.Batch
		batch.Messages = cloneV2Messages(source.Batch.Messages)
		cloned.Batch = &batch
	}
	return cloned
}

func cloneV2Messages(source []Message) []Message {
	clonedConversation := cloneConversationV2(&Conversation{Messages: source})
	if source != nil && clonedConversation.Messages == nil {
		return []Message{}
	}
	return clonedConversation.Messages
}

func cloneRecordValidationContext(source RecordValidationContext) RecordValidationContext {
	cloned := source
	if source.Previous != nil {
		previous := *source.Previous
		cloned.Previous = &previous
	}
	return cloned
}
