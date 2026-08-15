package conversation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"xagent/internal/artifact"
	"xagent/internal/budget"
	"xagent/internal/safefs"
	"xagent/internal/tool"
)

var (
	ErrLegacyMigrationConfig          = errors.New("legacy_migration_invalid_config")
	ErrLegacyMigrationFormat          = errors.New("legacy_migration_invalid_format")
	ErrLegacyExternalArtifactRequired = errors.New("legacy_migration_external_artifact_required")
	errLegacyArtifactImportFailed     = errors.New("legacy_migration_artifact_import_failed")
)

const (
	legacyArtifactMediaType            = "application/json"
	legacyArtifactImportDiagnosticCode = "conversation_legacy_artifact_import_failed"
	legacyArtifactImportMessage        = "A legacy external artifact could not be imported safely."
	legacyArtifactReadBufferBytes      = 32 * 1024
	legacyArtifactIDBytes              = 32
)

type legacyConversationFormat uint8

const (
	legacyFormatJSON legacyConversationFormat = iota + 1
	legacyFormatJSONLV1
)

type legacyLoadOptions struct {
	Format            legacyConversationFormat
	ExpectedSessionID string
}

// Legacy DTOs are private to Migration. They preserve the historical wire
// shape without making raw strings or filesystem paths reachable from Store.
type legacyConversation struct {
	ID        string                 `json:"id"`
	Title     string                 `json:"title"`
	Messages  []legacyMessage        `json:"messages"`
	Context   *legacyContextMetadata `json:"context,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

type legacyContextMetadata struct {
	Summary                 string            `json:"summary,omitempty"`
	LastBoundary            string            `json:"last_boundary,omitempty"`
	LastCompressionAt       *time.Time        `json:"last_compression_at,omitempty"`
	SummaryFailureCount     int               `json:"summary_failure_count,omitempty"`
	LastInputTokens         int64             `json:"last_input_tokens,omitempty"`
	LastOutputTokens        int64             `json:"last_output_tokens,omitempty"`
	LastEstimatedTokens     int64             `json:"last_estimated_tokens,omitempty"`
	LastEstimatedCharacters int               `json:"last_estimated_characters,omitempty"`
	RecoveryDiagnostics     []JSONLDiagnostic `json:"recovery_diagnostics,omitempty"`
}

// legacyMessage is the only legacy DTO allowed to contain ExternalPath.
type legacyMessage struct {
	Role                MessageRole     `json:"role"`
	Content             string          `json:"content"`
	CreatedAt           time.Time       `json:"created_at"`
	ToolCallID          string          `json:"tool_call_id,omitempty"`
	ToolName            string          `json:"tool_name,omitempty"`
	RawToolArguments    string          `json:"raw_tool_arguments,omitempty"`
	ToolResultContent   string          `json:"tool_result_content,omitempty"`
	ToolResultStatus    string          `json:"tool_result_status,omitempty"`
	ToolResultSummary   string          `json:"tool_result_summary,omitempty"`
	ToolResultTruncated bool            `json:"tool_result_truncated,omitempty"`
	ToolResultData      json.RawMessage `json:"tool_result_data,omitempty"`
	ToolResultError     json.RawMessage `json:"tool_result_error,omitempty"`
	ToolErrorCode       string          `json:"tool_error_code,omitempty"`
	Externalized        bool            `json:"externalized,omitempty"`
	ExternalPath        string          `json:"external_path,omitempty"`
	ExternalBytes       int64           `json:"external_bytes,omitempty"`
	ExternalPreview     string          `json:"external_preview,omitempty"`
}

// LegacyArtifactImporter is the sole capability migration may use to turn a
// legacy external path into an opaque artifact identity.
type LegacyArtifactImporter interface {
	Import(ctx context.Context, legacyPath string, metadata artifact.Metadata) (artifact.Ref, error)
}

// legacyMigration deliberately owns only the narrow import capability. It
// must not grow an artifact.Store (or any other path-opening capability).
type legacyMigration struct {
	codec            *V2RecordCodec
	artifactImporter LegacyArtifactImporter
}

// LegacyArtifactSink is the Begin-only capability used by the concrete
// importer. It excludes user reads, cleanup, and Store lifecycle ownership.
type LegacyArtifactSink interface {
	Begin(ctx context.Context, metadata artifact.Metadata) (artifact.Writer, error)
}

// safefsLegacyArtifactImporter is the capability-bearing adapter owned by the
// assembly layer. Migration itself sees only LegacyArtifactImporter.
type safefsLegacyArtifactImporter struct {
	legacyRoot     *safefs.Root
	legacyRootPath string
	artifacts      LegacyArtifactSink
}

// NewLegacyArtifactImporter binds the path-bearing adapter to the already-open
// legacy Root and a Begin-only artifact sink. The returned migration capability
// exposes neither dependency.
func NewLegacyArtifactImporter(
	legacyRoot *safefs.Root,
	legacyRootPath string,
	artifacts LegacyArtifactSink,
) (LegacyArtifactImporter, error) {
	if legacyRoot == nil || artifacts == nil || !validLegacyAbsolutePath(legacyRootPath) {
		return nil, ErrLegacyMigrationConfig
	}
	return &safefsLegacyArtifactImporter{
		legacyRoot:     legacyRoot,
		legacyRootPath: legacyRootPath,
		artifacts:      artifacts,
	}, nil
}

func (i *safefsLegacyArtifactImporter) Import(
	ctx context.Context,
	legacyPath string,
	metadata artifact.Metadata,
) (artifact.Ref, error) {
	if ctx == nil || i == nil || i.legacyRoot == nil || i.artifacts == nil {
		return artifact.Ref{}, errLegacyArtifactImportFailed
	}
	if err := ctx.Err(); err != nil {
		return artifact.Ref{}, err
	}
	relative, ok := legacyRootRelativePath(i.legacyRootPath, legacyPath)
	if !ok {
		return artifact.Ref{}, errLegacyArtifactImportFailed
	}
	source, err := i.legacyRoot.OpenRead(ctx, relative)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return artifact.Ref{}, contextErr
		}
		return artifact.Ref{}, errLegacyArtifactImportFailed
	}
	sourceOpen := true
	defer func() {
		if sourceOpen {
			_ = source.Close()
		}
	}()

	writer, err := i.artifacts.Begin(ctx, metadata)
	if err != nil || writer == nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return artifact.Ref{}, contextErr
		}
		return artifact.Ref{}, errLegacyArtifactImportFailed
	}
	writerFinalized := false
	defer func() {
		if !writerFinalized {
			_ = writer.Abort()
		}
	}()

	reader := legacyContextReader{ctx: ctx, reader: source}
	if _, err := io.CopyBuffer(writer, reader, make([]byte, legacyArtifactReadBufferBytes)); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return artifact.Ref{}, contextErr
		}
		return artifact.Ref{}, errLegacyArtifactImportFailed
	}
	if err := source.Close(); err != nil {
		sourceOpen = false
		return artifact.Ref{}, errLegacyArtifactImportFailed
	}
	sourceOpen = false
	if err := ctx.Err(); err != nil {
		return artifact.Ref{}, err
	}
	ref, err := writer.Commit(ctx)
	writerFinalized = true
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return artifact.Ref{}, contextErr
		}
		return artifact.Ref{}, errLegacyArtifactImportFailed
	}
	return ref, nil
}

type legacyContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r legacyContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := r.reader.Read(buffer)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return count, contextErr
	}
	return count, err
}

func validLegacyAbsolutePath(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.IndexByte(value, 0) < 0 &&
		filepath.IsAbs(value) && filepath.Clean(value) == value
}

func legacyRootRelativePath(rootPath string, legacyPath string) (string, bool) {
	if !validLegacyAbsolutePath(rootPath) || !validLegacyAbsolutePath(legacyPath) {
		return "", false
	}
	relative, err := filepath.Rel(rootPath, legacyPath)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

// loadLegacyConversationV2 borrows reader and converts one explicitly chosen
// legacy format directly into the safe v2 graph. It never closes, seeks, or
// writes the source. Persistence state deliberately remains the Store's
// responsibility so T3.18 can classify the first successful save as a v2
// snapshot without making Load itself destructive.
func loadLegacyConversationV2(
	ctx context.Context,
	codec *V2RecordCodec,
	reader io.Reader,
	options legacyLoadOptions,
) (*Conversation, error) {
	return legacyMigration{codec: codec}.load(ctx, reader, options)
}

func (m legacyMigration) loadResult(
	ctx context.Context,
	reader io.Reader,
	options legacyLoadOptions,
) (LoadResult, error) {
	conversation, err := m.load(ctx, reader, options)
	if err == nil {
		return LoadResult{
			Conversation: conversation,
			Available:    true,
			Persisted:    PersistedState{},
			Recovery:     RecoveryReport{Status: RecoveryClean},
		}, nil
	}
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return LoadResult{}, contextErr
		}
	}
	if err == ErrLegacyExternalArtifactRequired || err == errLegacyArtifactImportFailed {
		return LoadResult{
			Recovery: newV2RecoveryReport(
				m.codec,
				RecoveryPlaceholder,
				0,
				1,
				legacyArtifactImportDiagnosticCode,
				legacyArtifactImportMessage,
			),
		}, nil
	}
	return LoadResult{}, err
}

func (m legacyMigration) load(
	ctx context.Context,
	reader io.Reader,
	options legacyLoadOptions,
) (*Conversation, error) {
	codec := m.codec
	if ctx == nil || codec == nil || codec.redactor == nil || codec.maxRecordBytes <= 0 || codec.maxSessionBytes <= 0 ||
		reader == nil || !validV2ListSessionID(options.ExpectedSessionID) ||
		(options.Format != legacyFormatJSON && options.Format != legacyFormatJSONLV1) {
		return nil, ErrLegacyMigrationConfig
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	var (
		conversation *Conversation
		err          error
	)
	switch options.Format {
	case legacyFormatJSON:
		conversation, err = m.loadLegacyJSONV2(ctx, options.ExpectedSessionID, reader)
	case legacyFormatJSONLV1:
		conversation, err = m.loadLegacyJSONLV1V2(ctx, options.ExpectedSessionID, reader)
	}
	if err != nil {
		return nil, err
	}
	if conversation == nil || validateV2Snapshot(conversation, options.ExpectedSessionID) != nil {
		return nil, ErrLegacyMigrationFormat
	}
	if _, err := computePersistedState(conversation, 0); err != nil {
		return nil, ErrLegacyMigrationFormat
	}
	return conversation, nil
}

func (m legacyMigration) loadLegacyJSONV2(ctx context.Context, expectedID string, reader io.Reader) (*Conversation, error) {
	payload, err := readLegacyJSONDocument(ctx, m.codec, expectedID, reader)
	if err != nil {
		return nil, err
	}
	var legacy legacyConversation
	if err := decodeLegacyJSON(payload, &legacy); err != nil {
		return nil, err
	}
	return m.convertLegacyConversation(ctx, expectedID, &legacy)
}

func (m legacyMigration) loadLegacyJSONLV1V2(ctx context.Context, expectedID string, reader io.Reader) (*Conversation, error) {
	lines := &legacyJSONLReader{
		reader:    bufio.NewReaderSize(reader, v2RecordReadBufferBytes),
		sessionID: expectedID,
		codec:     m.codec,
	}
	var legacy *legacyConversation
	recordCount := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		payload, ok, err := lines.next(ctx)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if len(bytes.TrimSpace(payload)) == 0 {
			continue
		}

		var record JSONLRecord
		if err := decodeLegacyJSON(payload, &record); err != nil {
			return nil, err
		}
		messageCount := 0
		if legacy != nil {
			messageCount = len(legacy.Messages)
		}
		if err := validateLegacyV1Record(record, expectedID, messageCount); err != nil {
			return nil, err
		}
		switch record.Type {
		case RecordTypeSnapshot:
			legacy = record.Snapshot
		case RecordTypeMessage:
			legacy, err = applyLegacyV1MessageDTO(expectedID, legacy, record)
		default:
			err = ErrLegacyMigrationFormat
		}
		if err != nil {
			return nil, err
		}
		recordCount++
	}
	if recordCount == 0 || legacy == nil {
		return nil, ErrLegacyMigrationFormat
	}
	return m.convertLegacyConversation(ctx, expectedID, legacy)
}

func readLegacyJSONDocument(ctx context.Context, codec *V2RecordCodec, sessionID string, reader io.Reader) ([]byte, error) {
	buffer := bytes.NewBuffer(make([]byte, 0, minInt64(codec.maxSessionBytes, v2RecordReadBufferBytes)))
	chunk := make([]byte, v2RecordReadBufferBytes)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, readErr := reader.Read(chunk)
		if count < 0 || count > len(chunk) {
			return nil, ErrLegacyMigrationFormat
		}
		if int64(count) > codec.maxSessionBytes-int64(buffer.Len()) {
			return nil, newRecordBudgetError(sessionID, budget.SessionMaxSessionBytes, codec.maxSessionBytes)
		}
		if count > 0 {
			_, _ = buffer.Write(chunk[:count])
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch {
		case errors.Is(readErr, io.EOF):
			if len(bytes.TrimSpace(buffer.Bytes())) == 0 {
				return nil, ErrLegacyMigrationFormat
			}
			return append([]byte(nil), buffer.Bytes()...), nil
		case readErr != nil:
			return nil, ErrLegacyMigrationFormat
		case count == 0:
			return nil, ErrLegacyMigrationFormat
		}
	}
}

type legacyJSONLReader struct {
	reader      *bufio.Reader
	sessionID   string
	codec       *V2RecordCodec
	sessionUsed int64
}

func (r *legacyJSONLReader) next(ctx context.Context) ([]byte, bool, error) {
	if ctx == nil || r == nil || r.reader == nil || r.codec == nil {
		return nil, false, ErrLegacyMigrationConfig
	}
	payload := make([]byte, 0, minInt64(r.codec.maxRecordBytes, v2RecordReadBufferBytes))
	var recordBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		fragment, readErr := r.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if int64(len(fragment)) > r.codec.maxSessionBytes-r.sessionUsed {
				return nil, false, newRecordBudgetError(r.sessionID, budget.SessionMaxSessionBytes, r.codec.maxSessionBytes)
			}
			r.sessionUsed += int64(len(fragment))
			complete := fragment[len(fragment)-1] == '\n'
			content := fragment
			if complete {
				content = fragment[:len(fragment)-1]
			}
			if int64(len(content)) > r.codec.maxRecordBytes-recordBytes {
				return nil, false, newRecordBudgetError(r.sessionID, budget.SessionMaxRecordBytes, r.codec.maxRecordBytes)
			}
			recordBytes += int64(len(content))
			payload = append(payload, content...)
			if complete {
				if err := ctx.Err(); err != nil {
					return nil, false, err
				}
				return payload, true, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}

		switch {
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF) && len(payload) == 0:
			return nil, false, nil
		case errors.Is(readErr, io.EOF):
			return payload, true, nil
		case readErr != nil:
			return nil, false, ErrLegacyMigrationFormat
		default:
			return nil, false, ErrLegacyMigrationFormat
		}
	}
}

func decodeLegacyJSON(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrLegacyMigrationFormat
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrLegacyMigrationFormat
	}
	return nil
}

func validateLegacyV1Record(record JSONLRecord, expectedID string, messageCount int) error {
	if record.Version != JSONLVersion || messageCount < 0 || record.MessageIndex != messageCount || record.CreatedAt.IsZero() {
		return ErrLegacyMigrationFormat
	}
	if record.SessionID != "" && (record.SessionID != expectedID || !validV2ListSessionID(record.SessionID)) {
		return ErrLegacyMigrationFormat
	}
	switch record.Type {
	case RecordTypeMessage:
		if record.Message == nil || record.Snapshot != nil {
			return ErrLegacyMigrationFormat
		}
	case RecordTypeSnapshot:
		if record.Snapshot == nil || record.Message != nil {
			return ErrLegacyMigrationFormat
		}
	default:
		return ErrLegacyMigrationFormat
	}
	return nil
}

func applyLegacyV1MessageDTO(expectedID string, current *legacyConversation, record JSONLRecord) (*legacyConversation, error) {
	if current == nil {
		current = &legacyConversation{ID: expectedID, Title: "新会话", Messages: []legacyMessage{}}
	}
	if record.SessionID != "" && record.SessionID != expectedID {
		return nil, ErrLegacyMigrationFormat
	}
	if record.ConversationTitle != "" {
		current.Title = record.ConversationTitle
	}
	if !record.ConversationCreatedAt.IsZero() {
		current.CreatedAt = record.ConversationCreatedAt
	}
	if !record.ConversationUpdatedAt.IsZero() {
		current.UpdatedAt = record.ConversationUpdatedAt
	}
	message := *record.Message
	message.ToolResultData = append(json.RawMessage(nil), record.Message.ToolResultData...)
	message.ToolResultError = append(json.RawMessage(nil), record.Message.ToolResultError...)
	current.Messages = append(current.Messages, message)
	if current.CreatedAt.IsZero() {
		current.CreatedAt = message.CreatedAt
	}
	if current.UpdatedAt.IsZero() || message.CreatedAt.After(current.UpdatedAt) {
		current.UpdatedAt = message.CreatedAt
	}
	return current, nil
}

func (m legacyMigration) convertLegacyConversation(ctx context.Context, expectedID string, legacy *legacyConversation) (*Conversation, error) {
	containsExternal := legacyConversationContainsExternal(legacy)
	if containsExternal && m.artifactImporter == nil {
		return nil, ErrLegacyExternalArtifactRequired
	}
	validated, err := m.convertLegacyConversationPass(ctx, expectedID, legacy, false)
	if err != nil || !containsExternal {
		return validated, err
	}
	return m.convertLegacyConversationPass(ctx, expectedID, legacy, true)
}

func (m legacyMigration) convertLegacyConversationPass(
	ctx context.Context,
	expectedID string,
	legacy *legacyConversation,
	importExternal bool,
) (*Conversation, error) {
	codec := m.codec
	if legacy == nil || (legacy.ID != "" && (legacy.ID != expectedID || !validV2ListSessionID(legacy.ID))) {
		return nil, ErrLegacyMigrationFormat
	}
	result := &Conversation{
		ID:        expectedID,
		Title:     codec.redactor.Redact(legacy.Title),
		Messages:  make([]Message, 0, len(legacy.Messages)),
		CreatedAt: normalizeLegacyTime(legacy.CreatedAt),
		UpdatedAt: normalizeLegacyTime(legacy.UpdatedAt),
	}
	if strings.TrimSpace(legacy.Title) == "" {
		result.Title = codec.redactor.Redact("新会话")
	}
	toolBindings := make(map[string]legacyToolBinding)
	for index := range legacy.Messages {
		message, err := m.convertLegacyMessage(ctx, &legacy.Messages[index], toolBindings, importExternal)
		if err != nil {
			return nil, err
		}
		result.Messages = append(result.Messages, message)
	}
	contextV2, err := convertLegacyContext(codec, legacy.Context)
	if err != nil {
		return nil, err
	}
	result.Context = contextV2
	return finalizeLegacyConversation(codec, expectedID, result)
}

func legacyConversationContainsExternal(legacy *legacyConversation) bool {
	if legacy == nil {
		return false
	}
	for index := range legacy.Messages {
		message := &legacy.Messages[index]
		if message.Externalized || message.ExternalPath != "" || message.ExternalBytes != 0 || message.ExternalPreview != "" {
			return true
		}
	}
	return false
}

func finalizeLegacyConversation(codec *V2RecordCodec, expectedID string, conversation *Conversation) (*Conversation, error) {
	if conversation == nil || conversation.ID != expectedID {
		return nil, ErrLegacyMigrationFormat
	}
	if strings.TrimSpace(conversation.Title.Text()) == "" {
		conversation.Title = codec.redactor.Redact("新会话")
	}
	if len(conversation.Messages) > 0 {
		if conversation.CreatedAt.IsZero() {
			conversation.CreatedAt = conversation.Messages[0].CreatedAt
		}
		if conversation.UpdatedAt.IsZero() {
			conversation.UpdatedAt = conversation.Messages[len(conversation.Messages)-1].CreatedAt
		}
	}
	conversation.CreatedAt = normalizeLegacyTime(conversation.CreatedAt)
	conversation.UpdatedAt = normalizeLegacyTime(conversation.UpdatedAt)
	if conversation.CreatedAt.IsZero() || conversation.UpdatedAt.IsZero() || conversation.UpdatedAt.Before(conversation.CreatedAt) {
		return nil, ErrLegacyMigrationFormat
	}
	for index := range conversation.Messages {
		createdAt := conversation.Messages[index].CreatedAt
		if createdAt.Before(conversation.CreatedAt) || createdAt.After(conversation.UpdatedAt) {
			return nil, ErrLegacyMigrationFormat
		}
	}
	if validateV2Snapshot(conversation, expectedID) != nil {
		return nil, ErrLegacyMigrationFormat
	}
	return conversation, nil
}

func convertLegacyContext(codec *V2RecordCodec, legacy *legacyContextMetadata) (*ContextMetadata, error) {
	if legacy == nil {
		return nil, nil
	}
	if legacy.SummaryFailureCount < 0 || legacy.LastInputTokens < 0 || legacy.LastOutputTokens < 0 ||
		legacy.LastEstimatedTokens < 0 || legacy.LastEstimatedCharacters < 0 ||
		(legacy.LastCompressionAt != nil && legacy.LastCompressionAt.IsZero()) {
		return nil, ErrLegacyMigrationFormat
	}
	return &ContextMetadata{
		Summary:                 codec.redactor.Redact(legacy.Summary),
		LastBoundary:            codec.redactor.Redact(legacy.LastBoundary),
		LastCompressionAt:       normalizeLegacyTimePointer(legacy.LastCompressionAt),
		SummaryFailureCount:     legacy.SummaryFailureCount,
		LastInputTokens:         legacy.LastInputTokens,
		LastOutputTokens:        legacy.LastOutputTokens,
		LastEstimatedTokens:     legacy.LastEstimatedTokens,
		LastEstimatedCharacters: legacy.LastEstimatedCharacters,
	}, nil
}

type legacyToolBinding struct {
	name       string
	resultSeen bool
}

func (m legacyMigration) convertLegacyMessage(
	ctx context.Context,
	legacy *legacyMessage,
	toolBindings map[string]legacyToolBinding,
	importExternal bool,
) (Message, error) {
	codec := m.codec
	if legacy == nil || legacy.CreatedAt.IsZero() || !validV2MessageRole(legacy.Role) {
		return Message{}, ErrLegacyMigrationFormat
	}
	external := legacy.Externalized || legacy.ExternalPath != "" || legacy.ExternalBytes != 0 || legacy.ExternalPreview != ""
	if external && (legacy.Role != RoleToolResult || !legacy.Externalized || legacy.ExternalPath == "" || legacy.ExternalBytes < 0) {
		return Message{}, ErrLegacyMigrationFormat
	}
	result := Message{
		Role:      legacy.Role,
		Content:   codec.redactor.Redact(legacy.Content),
		CreatedAt: normalizeLegacyTime(legacy.CreatedAt),
	}
	switch legacy.Role {
	case RoleToolCall:
		if !validRecordIdentifier(legacy.ToolCallID) || !validRecordIdentifier(legacy.ToolName) ||
			legacy.ToolResultContent != "" || legacy.ToolResultStatus != "" || legacy.ToolResultSummary != "" ||
			legacy.ToolResultTruncated || len(legacy.ToolResultData) != 0 || len(legacy.ToolResultError) != 0 || legacy.ToolErrorCode != "" {
			return Message{}, ErrLegacyMigrationFormat
		}
		if _, exists := toolBindings[legacy.ToolCallID]; exists {
			return Message{}, ErrLegacyMigrationFormat
		}
		result.Tool = &ToolState{
			CallID:        legacy.ToolCallID,
			Name:          legacy.ToolName,
			ArgumentsJSON: codec.redactor.Redact(legacy.RawToolArguments),
			State:         tool.Prepared,
		}
		toolBindings[legacy.ToolCallID] = legacyToolBinding{name: legacy.ToolName}
	case RoleToolResult:
		converted, content, err := convertLegacyToolResult(codec, legacy, toolBindings)
		if err != nil {
			return Message{}, err
		}
		if external && importExternal {
			ref, importErr := m.artifactImporter.Import(ctx, legacy.ExternalPath, artifact.Metadata{MediaType: legacyArtifactMediaType})
			if contextErr := ctx.Err(); contextErr != nil {
				return Message{}, contextErr
			}
			if importErr != nil || !validLegacyImportedArtifact(ref) {
				return Message{}, errLegacyArtifactImportFailed
			}
			converted.Artifact = &ref
			converted.Truncated = true
			converted.TruncationReason = codec.redactor.Redact("legacy_output_externalized")
		} else if external {
			converted.Truncated = true
			converted.TruncationReason = codec.redactor.Redact("legacy_output_externalized")
		}
		result.Content = codec.redactor.Redact(content)
		result.Tool = converted
	default:
		if legacy.ToolCallID != "" || legacy.ToolName != "" || legacy.RawToolArguments != "" ||
			legacy.ToolResultContent != "" || legacy.ToolResultStatus != "" || legacy.ToolResultSummary != "" ||
			legacy.ToolResultTruncated || len(legacy.ToolResultData) != 0 || len(legacy.ToolResultError) != 0 || legacy.ToolErrorCode != "" {
			return Message{}, ErrLegacyMigrationFormat
		}
	}
	return result, nil
}

func validLegacyImportedArtifact(ref artifact.Ref) bool {
	if ref.Bytes < 0 || ref.CreatedAt.IsZero() || !ref.Available || !ref.Complete ||
		len(ref.ID) != legacyArtifactIDBytes*2 || ref.ID != strings.ToLower(ref.ID) {
		return false
	}
	decoded, err := hex.DecodeString(ref.ID)
	return err == nil && len(decoded) == legacyArtifactIDBytes
}

func convertLegacyToolResult(codec *V2RecordCodec, legacy *legacyMessage, toolBindings map[string]legacyToolBinding) (*ToolState, string, error) {
	if !validRecordIdentifier(legacy.ToolCallID) || legacy.RawToolArguments != "" {
		return nil, "", ErrLegacyMigrationFormat
	}
	binding, exists := toolBindings[legacy.ToolCallID]
	if !exists || binding.resultSeen {
		return nil, "", ErrLegacyMigrationFormat
	}
	name := legacy.ToolName
	if name == "" {
		name = binding.name
	}
	if !validRecordIdentifier(name) || name != binding.name {
		return nil, "", ErrLegacyMigrationFormat
	}
	content := legacy.ToolResultContent
	if content == "" {
		content = legacy.Content
	} else if legacy.Content != "" && legacy.Content != content {
		return nil, "", ErrLegacyMigrationFormat
	}
	started, err := legacyToolStarted(legacy.ToolResultData)
	if err != nil {
		return nil, "", err
	}
	parsedError, err := decodeLegacyToolError(legacy.ToolResultError)
	if err != nil {
		return nil, "", err
	}
	effectiveErrorCode := strings.TrimSpace(legacy.ToolErrorCode)
	if parsedError != nil && parsedError.Code != "" {
		if effectiveErrorCode != "" && parsedError.Code != effectiveErrorCode {
			return nil, "", ErrLegacyMigrationFormat
		}
		if effectiveErrorCode == "" {
			effectiveErrorCode = parsedError.Code
		}
	}
	state, status, defaultCode, err := mapLegacyToolStatus(legacy.ToolResultStatus, effectiveErrorCode, started)
	if err != nil {
		return nil, "", err
	}
	if !state.CanProduceResult() {
		return nil, "", ErrLegacyMigrationFormat
	}
	if status == tool.StatusSuccess && (legacy.ToolErrorCode != "" || parsedError != nil) {
		return nil, "", ErrLegacyMigrationFormat
	}

	result := &ToolState{
		CallID:    legacy.ToolCallID,
		Name:      name,
		State:     state,
		Status:    status,
		Summary:   codec.redactor.Redact(legacy.ToolResultSummary),
		Result:    codec.redactor.Redact(content),
		Truncated: legacy.ToolResultTruncated,
	}
	if legacy.ToolResultTruncated {
		result.TruncationReason = codec.redactor.Redact("legacy_output_truncated")
	}
	if status != tool.StatusSuccess {
		code := effectiveErrorCode
		message := legacy.ToolResultSummary
		recoverable := false
		if parsedError != nil {
			if parsedError.Message != "" {
				message = parsedError.Message
			}
			recoverable = parsedError.Recoverable
		}
		if code == "" {
			code = defaultCode
		}
		if !validRecordIdentifier(code) {
			return nil, "", ErrLegacyMigrationFormat
		}
		result.Error = &tool.SafeError{Code: code, Message: codec.redactor.Redact(message), Recoverable: recoverable}
	}
	binding.resultSeen = true
	toolBindings[legacy.ToolCallID] = binding
	return result, content, nil
}

type legacyToolError struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable"`
}

func decodeLegacyToolError(payload json.RawMessage) (*legacyToolError, error) {
	if len(bytes.TrimSpace(payload)) == 0 || bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
		return nil, nil
	}
	var result legacyToolError
	if err := decodeLegacyJSON(payload, &result); err != nil {
		return nil, err
	}
	if result.Code != "" && !validRecordIdentifier(result.Code) {
		return nil, ErrLegacyMigrationFormat
	}
	return &result, nil
}

func mapLegacyToolStatus(status string, errorCode string, started *bool) (tool.ExecutionState, tool.ResultStatus, string, error) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success":
		if started != nil && !*started {
			return "", "", "", ErrLegacyMigrationFormat
		}
		return tool.Completed, tool.StatusSuccess, "", nil
	case "error", "failed":
		preStartCode := legacyPreStartErrorCode(errorCode)
		if started != nil && *started && preStartCode {
			return "", "", "", ErrLegacyMigrationFormat
		}
		if preStartCode || started != nil && !*started {
			return tool.Rejected, tool.StatusError, "legacy_tool_error", nil
		}
		return tool.Completed, tool.StatusError, "legacy_tool_error", nil
	case "denied":
		if started != nil && *started {
			return "", "", "", ErrLegacyMigrationFormat
		}
		return tool.Rejected, tool.StatusDenied, tool.ErrPermissionDenied, nil
	case "timeout":
		if started != nil && !*started {
			return tool.Rejected, tool.StatusTimeout, tool.ErrTimeout, nil
		}
		return tool.CancelledAfterStart, tool.StatusTimeout, tool.ErrTimeout, nil
	case "cancelled":
		if errorCode == tool.ErrPermissionDenied {
			if started != nil && *started {
				return "", "", "", ErrLegacyMigrationFormat
			}
			return tool.Rejected, tool.StatusDenied, tool.ErrPermissionDenied, nil
		}
		if started != nil && !*started {
			return tool.Rejected, tool.StatusError, "legacy_cancelled", nil
		}
		return tool.CancelledAfterStart, tool.StatusError, "legacy_cancelled", nil
	default:
		return "", "", "", ErrLegacyMigrationFormat
	}
}

func legacyPreStartErrorCode(code string) bool {
	switch code {
	case tool.ErrInvalidArguments, tool.ErrToolNotFound, tool.ErrMultipleToolCallsUnsupported,
		tool.ErrPermissionDenied, tool.ErrHookDenied, tool.ErrInternalRoutingRequired:
		return true
	default:
		return false
	}
}

func legacyToolStarted(payload json.RawMessage) (*bool, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	var values map[string]json.RawMessage
	if err := decoder.Decode(&values); err != nil || values == nil {
		return nil, ErrLegacyMigrationFormat
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrLegacyMigrationFormat
	}
	raw, exists := values["started"]
	if !exists {
		return nil, nil
	}
	var started bool
	if err := json.Unmarshal(raw, &started); err != nil {
		return nil, ErrLegacyMigrationFormat
	}
	return &started, nil
}

func legacyToolBindingsFromMessages(messages []Message) (map[string]legacyToolBinding, error) {
	result := make(map[string]legacyToolBinding)
	for index := range messages {
		message := messages[index]
		if message.Tool == nil {
			continue
		}
		switch message.Role {
		case RoleToolCall:
			if _, exists := result[message.Tool.CallID]; exists {
				return nil, ErrLegacyMigrationFormat
			}
			result[message.Tool.CallID] = legacyToolBinding{name: message.Tool.Name}
		case RoleToolResult:
			binding, exists := result[message.Tool.CallID]
			if !exists || binding.resultSeen || binding.name != message.Tool.Name {
				return nil, ErrLegacyMigrationFormat
			}
			binding.resultSeen = true
			result[message.Tool.CallID] = binding
		}
	}
	return result, nil
}

func normalizeLegacyTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.Round(0).UTC()
}

func normalizeLegacyTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := normalizeLegacyTime(*value)
	return &normalized
}

func minInt64(left int64, right int) int {
	if left <= 0 {
		return 0
	}
	if left < int64(right) {
		return int(left)
	}
	return right
}
