package conversation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"xagent/internal/artifact"
	"xagent/internal/budget"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

const JSONLVersion = 1

const JSONLVersionV2 = 2

type RecordKind string

const (
	RecordBatch    RecordKind = "batch"
	RecordSnapshot RecordKind = "snapshot"
)

// MessageBatch contains the only state changes allowed in an append record.
// BaseMessageCount binds the append position and UpdatedAt makes the final
// T3.2 state digest exactly replayable.
type MessageBatch struct {
	BaseMessageCount int       `json:"base_message_count"`
	Messages         []Message `json:"messages"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// JSONLRecordV2 is kept separate from the legacy JSONLRecord until the Store
// API is atomically switched in T3.18.
type JSONLRecordV2 struct {
	Version        int           `json:"version"`
	Kind           RecordKind    `json:"kind"`
	SessionID      string        `json:"session_id"`
	Revision       uint64        `json:"revision"`
	PreviousDigest StateDigest   `json:"previous_digest"`
	Digest         StateDigest   `json:"digest"`
	Batch          *MessageBatch `json:"batch"`
	Snapshot       *Conversation `json:"snapshot"`
}

// RecordValidationContext binds a record to its file identity and the last
// verified logical state. A nil Previous denotes a snapshot chain anchor,
// including a compacted checkpoint whose Revision may be greater than one.
type RecordValidationContext struct {
	ExpectedSessionID string
	Previous          *PersistedState
}

type v2RecordValidationCode string

const (
	v2RecordInvalidVersion       v2RecordValidationCode = "v2_record_invalid_version"
	v2RecordInvalidSession       v2RecordValidationCode = "v2_record_invalid_session"
	v2RecordInvalidRevision      v2RecordValidationCode = "v2_record_invalid_revision"
	v2RecordInvalidPreviousState v2RecordValidationCode = "v2_record_invalid_previous_state"
	v2RecordInvalidPreviousLink  v2RecordValidationCode = "v2_record_invalid_previous_link"
	v2RecordInvalidDigest        v2RecordValidationCode = "v2_record_invalid_digest"
	v2RecordInvalidKind          v2RecordValidationCode = "v2_record_invalid_kind"
	v2RecordInvalidPayload       v2RecordValidationCode = "v2_record_invalid_payload"
	v2RecordInvalidBatch         v2RecordValidationCode = "v2_record_invalid_batch"
	v2RecordInvalidSnapshot      v2RecordValidationCode = "v2_record_invalid_snapshot"
)

type v2RecordValidationError struct {
	code v2RecordValidationCode
}

func (e *v2RecordValidationError) Error() string {
	if e == nil {
		return ""
	}
	return string(e.code)
}

func invalidV2Record(code v2RecordValidationCode) error {
	return &v2RecordValidationError{code: code}
}

// Validate checks the closed v2 envelope and its link to the last verified
// state. Applying a batch and recomputing its final Digest belongs to T3.9.
func (r JSONLRecordV2) Validate(context RecordValidationContext) error {
	if r.Version != JSONLVersionV2 {
		return invalidV2Record(v2RecordInvalidVersion)
	}
	if !validRecordIdentifier(context.ExpectedSessionID) || !validRecordIdentifier(r.SessionID) || r.SessionID != context.ExpectedSessionID {
		return invalidV2Record(v2RecordInvalidSession)
	}
	if r.Revision == 0 {
		return invalidV2Record(v2RecordInvalidRevision)
	}
	if err := validateV2RecordLink(r, context.Previous); err != nil {
		return err
	}
	if !validStateDigestSyntax(r.Digest) {
		return invalidV2Record(v2RecordInvalidDigest)
	}
	if r.Kind != RecordBatch && r.Kind != RecordSnapshot {
		return invalidV2Record(v2RecordInvalidKind)
	}
	if (r.Batch == nil) == (r.Snapshot == nil) {
		return invalidV2Record(v2RecordInvalidPayload)
	}

	switch r.Kind {
	case RecordBatch:
		if r.Batch == nil || r.Snapshot != nil || context.Previous == nil {
			return invalidV2Record(v2RecordInvalidPayload)
		}
		if err := validateV2MessageBatch(r.Batch, *context.Previous); err != nil {
			return err
		}
	case RecordSnapshot:
		if r.Snapshot == nil || r.Batch != nil {
			return invalidV2Record(v2RecordInvalidPayload)
		}
		if err := validateV2Snapshot(r.Snapshot, r.SessionID); err != nil {
			return err
		}
	}
	return nil
}

func validateV2RecordLink(record JSONLRecordV2, previous *PersistedState) error {
	if previous == nil {
		if record.PreviousDigest != "" {
			return invalidV2Record(v2RecordInvalidPreviousLink)
		}
		return nil
	}
	if previous.Revision == 0 || previous.Revision == ^uint64(0) || previous.MessageCount < 0 || !validStateDigestSyntax(previous.Digest) || !validStateDigestSyntax(previous.MessagesDigest) {
		return invalidV2Record(v2RecordInvalidPreviousState)
	}
	if record.Revision != previous.Revision+1 {
		return invalidV2Record(v2RecordInvalidRevision)
	}
	if record.PreviousDigest != previous.Digest {
		return invalidV2Record(v2RecordInvalidPreviousLink)
	}
	return nil
}

func validateV2MessageBatch(batch *MessageBatch, previous PersistedState) error {
	if batch == nil || batch.BaseMessageCount < 0 || batch.BaseMessageCount != previous.MessageCount || len(batch.Messages) == 0 || batch.UpdatedAt.IsZero() {
		return invalidV2Record(v2RecordInvalidBatch)
	}
	if _, err := computeMessagesDigest(batch.Messages); err != nil {
		return invalidV2Record(v2RecordInvalidBatch)
	}
	if !validV2Messages(batch.Messages) {
		return invalidV2Record(v2RecordInvalidBatch)
	}
	return nil
}

func validateV2Snapshot(snapshot *Conversation, sessionID string) error {
	if snapshot == nil || snapshot.ID != sessionID || snapshot.CreatedAt.IsZero() || snapshot.UpdatedAt.IsZero() {
		return invalidV2Record(v2RecordInvalidSnapshot)
	}
	if _, err := computePersistedState(snapshot, 0); err != nil || !validV2Messages(snapshot.Messages) || !validV2Context(snapshot.Context) {
		return invalidV2Record(v2RecordInvalidSnapshot)
	}
	return nil
}

func validV2Messages(messages []Message) bool {
	for index := range messages {
		message := messages[index]
		if !validV2MessageRole(message.Role) || message.CreatedAt.IsZero() {
			return false
		}
		if message.Tool == nil {
			if message.Role == RoleToolCall || message.Role == RoleToolResult {
				return false
			}
			continue
		}
		if message.Role != RoleToolCall && message.Role != RoleToolResult || !validV2ToolState(message.Tool, message.Role) {
			return false
		}
	}
	return true
}

func validV2ToolState(state *ToolState, role MessageRole) bool {
	if state == nil || !validRecordIdentifier(state.CallID) || !validRecordIdentifier(state.Name) || !state.State.Valid() || !validV2ResultStatus(state.Status, role) {
		return false
	}
	if role == RoleToolCall {
		return state.Artifact == nil && !state.Truncated && state.TruncationReason.Text() == "" && state.Error == nil
	}
	if !validV2ToolResultArtifact(state) {
		return false
	}
	if state.Error != nil && !validRecordIdentifier(state.Error.Code) {
		return false
	}
	return true
}

func validV2ToolResultArtifact(state *ToolState) bool {
	if state == nil {
		return false
	}
	reason := state.TruncationReason.Text()
	if state.Artifact == nil {
		if !state.Truncated {
			return reason == ""
		}
		return reason == "legacy_output_externalized" || reason == "legacy_output_truncated"
	}
	reference := state.Artifact
	if !validOpaqueV2ArtifactID(reference.ID) || reference.Bytes <= 0 || reference.CreatedAt.IsZero() || !reference.Available || !state.Truncated {
		return false
	}
	if reference.Complete {
		return reason == string(tool.CaptureTruncatedInline) || reason == "legacy_output_externalized"
	}
	switch tool.CaptureTruncationReason(reason) {
	case tool.CaptureTruncatedHardLimit, tool.CaptureTruncatedArtifact, tool.CaptureTruncatedWriteFailure, tool.CaptureTruncatedCanceled:
		return true
	default:
		return false
	}
}

func validOpaqueV2ArtifactID(id string) bool {
	if len(id) != sha256HexLength {
		return false
	}
	for index := 0; index < len(id); index++ {
		character := id[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validV2MessageRole(role MessageRole) bool {
	switch role {
	case RoleUser, RoleAssistant, RoleThinking, RoleToolCall, RoleToolResult, RoleContextSummary, RoleContextBoundary:
		return true
	default:
		return false
	}
}

func validV2ResultStatus(status tool.ResultStatus, role MessageRole) bool {
	if role == RoleToolCall && status == "" {
		return true
	}
	switch status {
	case tool.StatusSuccess, tool.StatusError, tool.StatusDenied, tool.StatusTimeout:
		return true
	default:
		return false
	}
}

func validV2Context(context *ContextMetadata) bool {
	if context == nil {
		return true
	}
	return (context.LastCompressionAt == nil || !context.LastCompressionAt.IsZero()) &&
		context.SummaryFailureCount >= 0 &&
		context.LastInputTokens >= 0 &&
		context.LastOutputTokens >= 0 &&
		context.LastEstimatedTokens >= 0 &&
		context.LastEstimatedCharacters >= 0
}

func validStateDigestSyntax(value StateDigest) bool {
	if len(value) != sha256HexLength {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

const sha256HexLength = 64

func validRecordIdentifier(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

const v2RecordReadBufferBytes = 32 * 1024

var (
	ErrV2RecordCodecConfig = errors.New("v2_record_codec_invalid_config")
	ErrV2RecordEncode      = errors.New("v2_record_encode_failed")
	ErrV2RecordMalformed   = errors.New("v2_record_malformed")
	ErrV2RecordTruncated   = errors.New("v2_record_truncated")
	ErrV2RecordRead        = errors.New("v2_record_read_failed")
	errV2RecordBufferLimit = errors.New("v2_record_buffer_limit")
)

// RecordBudgetError deliberately retains only stable safe metadata. In
// particular it never keeps the observed bytes or the record that exceeded a
// limit.
type RecordBudgetError struct {
	SessionID string
	Scope     budget.Scope
	Limit     int64
}

func (e *RecordBudgetError) Error() string {
	if e == nil {
		return "v2_record_budget_exceeded"
	}
	return fmt.Sprintf("v2_record_budget_exceeded session_id=%s scope=%s limit=%d", e.SessionID, e.Scope, e.Limit)
}

// V2RecordCodecOptions uses the resolved C8 values. Zero selects the approved
// default for callers that have not yet been switched to resolved config.
type V2RecordCodecOptions struct {
	MaxRecordBytes  int64
	MaxSessionBytes int64
	Redactor        *redact.RuntimeRedactor
}

// V2RecordCodec owns the immutable record and session byte ceilings shared by
// v2 writers and readers.
type V2RecordCodec struct {
	maxRecordBytes  int64
	maxSessionBytes int64
	redactor        *redact.RuntimeRedactor
}

func NewV2RecordCodec(options V2RecordCodecOptions) (*V2RecordCodec, error) {
	maxRecordBytes, err := resolveV2RecordLimit(budget.SessionMaxRecordBytes, options.MaxRecordBytes)
	if err != nil {
		return nil, err
	}
	maxSessionBytes, err := resolveV2RecordLimit(budget.SessionMaxSessionBytes, options.MaxSessionBytes)
	if err != nil {
		return nil, err
	}
	if maxRecordBytes > maxSessionBytes {
		return nil, ErrV2RecordCodecConfig
	}
	redactor := options.Redactor
	if redactor == nil {
		redactor = redact.NewRuntimeRedactor()
	}
	return &V2RecordCodec{
		maxRecordBytes:  maxRecordBytes,
		maxSessionBytes: maxSessionBytes,
		redactor:        redactor,
	}, nil
}

func resolveV2RecordLimit(scope budget.Scope, configured int64) (int64, error) {
	var selected *budget.Spec
	for _, candidate := range budget.AllSpecs() {
		if candidate.Scope == scope {
			copy := candidate
			selected = &copy
			break
		}
	}
	if selected == nil || selected.Dimension != budget.Bytes {
		return 0, ErrV2RecordCodecConfig
	}
	var candidate *int64
	if configured != 0 {
		candidate = &configured
	}
	return selected.Resolve(candidate)
}

// Encode forms one complete JSONL line in memory and publishes no bytes when
// its payload exceeds session.max_record_bytes.
func (c *V2RecordCodec) Encode(sessionID string, record JSONLRecordV2) ([]byte, error) {
	if c == nil || c.redactor == nil || c.maxRecordBytes <= 0 {
		return nil, ErrV2RecordCodecConfig
	}
	buffer := &boundedV2RecordBuffer{limit: c.maxRecordBytes + 1}
	encoder := json.NewEncoder(buffer)
	err := encoder.Encode(toV2RecordWire(record))
	if err != nil {
		if errors.Is(err, errV2RecordBufferLimit) {
			return nil, newRecordBudgetError(sessionID, budget.SessionMaxRecordBytes, c.maxRecordBytes)
		}
		return nil, ErrV2RecordEncode
	}
	line := buffer.Bytes()
	if len(line) == 0 || line[len(line)-1] != '\n' || int64(len(line)-1) > c.maxRecordBytes {
		return nil, newRecordBudgetError(sessionID, budget.SessionMaxRecordBytes, c.maxRecordBytes)
	}
	return append([]byte(nil), line...), nil
}

type boundedV2RecordBuffer struct {
	buffer bytes.Buffer
	limit  int64
}

func (b *boundedV2RecordBuffer) Write(data []byte) (int, error) {
	if b == nil || b.limit <= 0 || int64(len(data)) > b.limit-int64(b.buffer.Len()) {
		return 0, errV2RecordBufferLimit
	}
	return b.buffer.Write(data)
}

func (b *boundedV2RecordBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buffer.Bytes()
}

// NewDecoder creates one cumulative session reader. All records decoded from
// it share the same session.max_session_bytes consumption.
func (c *V2RecordCodec) NewDecoder(sessionID string, reader io.Reader) (*V2RecordDecoder, error) {
	if c == nil || c.redactor == nil || c.maxRecordBytes <= 0 || c.maxSessionBytes <= 0 || reader == nil {
		return nil, ErrV2RecordCodecConfig
	}
	return &V2RecordDecoder{
		codec:     c,
		sessionID: safeRecordBudgetSessionID(sessionID),
		reader:    bufio.NewReaderSize(reader, v2RecordReadBufferBytes),
	}, nil
}

// V2RecordDecoder reads one LF-delimited record at a time. A fatal framing,
// budget, or schema error poisons it so callers cannot resume after unverified
// bytes.
type V2RecordDecoder struct {
	codec       *V2RecordCodec
	sessionID   string
	reader      *bufio.Reader
	sessionUsed int64
	fatal       error
}

// BytesRead returns raw JSONL bytes consumed so far, including LF delimiters
// and bytes from a failing record. Loaders should publish this value only
// after the corresponding record has been fully verified.
func (d *V2RecordDecoder) BytesRead() int64 {
	if d == nil {
		return 0
	}
	return d.sessionUsed
}

func (d *V2RecordDecoder) Decode() (JSONLRecordV2, error) {
	if d == nil || d.codec == nil || d.reader == nil {
		return JSONLRecordV2{}, ErrV2RecordCodecConfig
	}
	if d.fatal != nil {
		return JSONLRecordV2{}, d.fatal
	}
	payload, err := d.readPayload()
	if err != nil {
		if !errors.Is(err, io.EOF) {
			d.fatal = err
		}
		return JSONLRecordV2{}, err
	}
	if !utf8.Valid(payload) {
		d.fatal = ErrV2RecordMalformed
		return JSONLRecordV2{}, d.fatal
	}
	record, err := decodeV2RecordWire(payload, d.codec.redactor)
	if err != nil {
		d.fatal = err
		return JSONLRecordV2{}, err
	}
	return record, nil
}

func (d *V2RecordDecoder) readPayload() ([]byte, error) {
	var payload []byte
	var recordBytes int64
	for {
		fragment, readErr := d.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if int64(len(fragment)) > d.codec.maxSessionBytes-d.sessionUsed {
				return nil, newRecordBudgetError(d.sessionID, budget.SessionMaxSessionBytes, d.codec.maxSessionBytes)
			}
			d.sessionUsed += int64(len(fragment))

			payloadFragment := fragment
			complete := fragment[len(fragment)-1] == '\n'
			if complete {
				payloadFragment = fragment[:len(fragment)-1]
			}
			if int64(len(payloadFragment)) > d.codec.maxRecordBytes-recordBytes {
				return nil, newRecordBudgetError(d.sessionID, budget.SessionMaxRecordBytes, d.codec.maxRecordBytes)
			}
			recordBytes += int64(len(payloadFragment))
			payload = append(payload, payloadFragment...)
			if complete {
				return payload, nil
			}
		}

		switch {
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF) && len(payload) == 0:
			return nil, io.EOF
		case errors.Is(readErr, io.EOF):
			return nil, ErrV2RecordTruncated
		case readErr != nil:
			return nil, ErrV2RecordRead
		default:
			return nil, ErrV2RecordMalformed
		}
	}
}

func newRecordBudgetError(sessionID string, scope budget.Scope, limit int64) error {
	return &RecordBudgetError{SessionID: safeRecordBudgetSessionID(sessionID), Scope: scope, Limit: limit}
}

func safeRecordBudgetSessionID(sessionID string) string {
	if !validRecordIdentifier(sessionID) {
		return "invalid-session"
	}
	return sessionID
}

type jsonlRecordV2Wire struct {
	Version        int                 `json:"version"`
	Kind           RecordKind          `json:"kind"`
	SessionID      string              `json:"session_id"`
	Revision       uint64              `json:"revision"`
	PreviousDigest StateDigest         `json:"previous_digest"`
	Digest         StateDigest         `json:"digest"`
	Batch          *messageBatchWire   `json:"batch"`
	Snapshot       *conversationV2Wire `json:"snapshot"`
}

type messageBatchWire struct {
	BaseMessageCount int             `json:"base_message_count"`
	Messages         []messageV2Wire `json:"messages"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type conversationV2Wire struct {
	ID        string                 `json:"id"`
	Title     string                 `json:"title"`
	Messages  []messageV2Wire        `json:"messages"`
	Context   *contextMetadataV2Wire `json:"context"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

type messageV2Wire struct {
	Role      MessageRole      `json:"role"`
	Content   string           `json:"content"`
	CreatedAt time.Time        `json:"created_at"`
	Tool      *toolStateV2Wire `json:"tool"`
}

type toolStateV2Wire struct {
	CallID           string              `json:"call_id"`
	Name             string              `json:"name"`
	ArgumentsJSON    string              `json:"arguments_json"`
	State            tool.ExecutionState `json:"state"`
	Status           tool.ResultStatus   `json:"status"`
	Summary          string              `json:"summary"`
	Result           string              `json:"result"`
	Truncated        bool                `json:"truncated"`
	TruncationReason string              `json:"truncation_reason"`
	Artifact         *artifact.Ref       `json:"artifact"`
	Error            *safeToolErrorWire  `json:"error"`
}

type safeToolErrorWire struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable"`
}

type contextMetadataV2Wire struct {
	Summary                 string     `json:"summary"`
	LastBoundary            string     `json:"last_boundary"`
	LastCompressionAt       *time.Time `json:"last_compression_at"`
	SummaryFailureCount     int        `json:"summary_failure_count"`
	LastInputTokens         int64      `json:"last_input_tokens"`
	LastOutputTokens        int64      `json:"last_output_tokens"`
	LastEstimatedTokens     int64      `json:"last_estimated_tokens"`
	LastEstimatedCharacters int        `json:"last_estimated_characters"`
}

func toV2RecordWire(record JSONLRecordV2) jsonlRecordV2Wire {
	return jsonlRecordV2Wire{
		Version:        record.Version,
		Kind:           record.Kind,
		SessionID:      record.SessionID,
		Revision:       record.Revision,
		PreviousDigest: record.PreviousDigest,
		Digest:         record.Digest,
		Batch:          toMessageBatchWire(record.Batch),
		Snapshot:       toConversationV2Wire(record.Snapshot),
	}
}

func toMessageBatchWire(batch *MessageBatch) *messageBatchWire {
	if batch == nil {
		return nil
	}
	return &messageBatchWire{BaseMessageCount: batch.BaseMessageCount, Messages: toMessageV2Wires(batch.Messages), UpdatedAt: batch.UpdatedAt}
}

func toConversationV2Wire(conversation *Conversation) *conversationV2Wire {
	if conversation == nil {
		return nil
	}
	return &conversationV2Wire{
		ID:        conversation.ID,
		Title:     conversation.Title.Text(),
		Messages:  toMessageV2Wires(conversation.Messages),
		Context:   toContextMetadataV2Wire(conversation.Context),
		CreatedAt: conversation.CreatedAt,
		UpdatedAt: conversation.UpdatedAt,
	}
}

func toMessageV2Wires(messages []Message) []messageV2Wire {
	if messages == nil {
		return nil
	}
	result := make([]messageV2Wire, len(messages))
	for index, message := range messages {
		result[index] = messageV2Wire{Role: message.Role, Content: message.Content.Text(), CreatedAt: message.CreatedAt, Tool: toToolStateV2Wire(message.Tool)}
	}
	return result
}

func toToolStateV2Wire(state *ToolState) *toolStateV2Wire {
	if state == nil {
		return nil
	}
	result := &toolStateV2Wire{
		CallID: state.CallID, Name: state.Name, ArgumentsJSON: state.ArgumentsJSON.Text(), State: state.State, Status: state.Status,
		Summary: state.Summary.Text(), Result: state.Result.Text(), Truncated: state.Truncated, TruncationReason: state.TruncationReason.Text(),
		Artifact: cloneArtifactRef(state.Artifact),
	}
	if state.Error != nil {
		result.Error = &safeToolErrorWire{Code: state.Error.Code, Message: state.Error.Message.Text(), Recoverable: state.Error.Recoverable}
	}
	return result
}

func toContextMetadataV2Wire(context *ContextMetadata) *contextMetadataV2Wire {
	if context == nil {
		return nil
	}
	return &contextMetadataV2Wire{
		Summary: context.Summary.Text(), LastBoundary: context.LastBoundary.Text(), LastCompressionAt: cloneTimePointer(context.LastCompressionAt),
		SummaryFailureCount: context.SummaryFailureCount, LastInputTokens: context.LastInputTokens, LastOutputTokens: context.LastOutputTokens,
		LastEstimatedTokens: context.LastEstimatedTokens, LastEstimatedCharacters: context.LastEstimatedCharacters,
	}
}

func decodeV2RecordWire(payload []byte, redactor *redact.RuntimeRedactor) (JSONLRecordV2, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var wire jsonlRecordV2Wire
	if err := decoder.Decode(&wire); err != nil {
		return JSONLRecordV2{}, ErrV2RecordMalformed
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return JSONLRecordV2{}, ErrV2RecordMalformed
	}
	return fromV2RecordWire(wire, redactor), nil
}

func fromV2RecordWire(wire jsonlRecordV2Wire, redactor *redact.RuntimeRedactor) JSONLRecordV2 {
	return JSONLRecordV2{
		Version: wire.Version, Kind: wire.Kind, SessionID: wire.SessionID, Revision: wire.Revision,
		PreviousDigest: wire.PreviousDigest, Digest: wire.Digest,
		Batch: fromMessageBatchWire(wire.Batch, redactor), Snapshot: fromConversationV2Wire(wire.Snapshot, redactor),
	}
}

func fromMessageBatchWire(batch *messageBatchWire, redactor *redact.RuntimeRedactor) *MessageBatch {
	if batch == nil {
		return nil
	}
	return &MessageBatch{BaseMessageCount: batch.BaseMessageCount, Messages: fromMessageV2Wires(batch.Messages, redactor), UpdatedAt: batch.UpdatedAt}
}

func fromConversationV2Wire(conversation *conversationV2Wire, redactor *redact.RuntimeRedactor) *Conversation {
	if conversation == nil {
		return nil
	}
	return &Conversation{
		ID: conversation.ID, Title: redactor.Redact(conversation.Title), Messages: fromMessageV2Wires(conversation.Messages, redactor),
		Context: fromContextMetadataV2Wire(conversation.Context, redactor), CreatedAt: conversation.CreatedAt, UpdatedAt: conversation.UpdatedAt,
	}
}

func fromMessageV2Wires(messages []messageV2Wire, redactor *redact.RuntimeRedactor) []Message {
	if messages == nil {
		return nil
	}
	result := make([]Message, len(messages))
	for index, message := range messages {
		result[index] = Message{Role: message.Role, Content: redactor.Redact(message.Content), CreatedAt: message.CreatedAt, Tool: fromToolStateV2Wire(message.Tool, redactor)}
	}
	return result
}

func fromToolStateV2Wire(state *toolStateV2Wire, redactor *redact.RuntimeRedactor) *ToolState {
	if state == nil {
		return nil
	}
	result := &ToolState{
		CallID: state.CallID, Name: state.Name, ArgumentsJSON: redactor.Redact(state.ArgumentsJSON), State: state.State, Status: state.Status,
		Summary: redactor.Redact(state.Summary), Result: redactor.Redact(state.Result), Truncated: state.Truncated,
		TruncationReason: redactor.Redact(state.TruncationReason), Artifact: cloneArtifactRef(state.Artifact),
	}
	if state.Error != nil {
		result.Error = &tool.SafeError{Code: state.Error.Code, Message: redactor.Redact(state.Error.Message), Recoverable: state.Error.Recoverable}
	}
	return result
}

func fromContextMetadataV2Wire(context *contextMetadataV2Wire, redactor *redact.RuntimeRedactor) *ContextMetadata {
	if context == nil {
		return nil
	}
	return &ContextMetadata{
		Summary: redactor.Redact(context.Summary), LastBoundary: redactor.Redact(context.LastBoundary), LastCompressionAt: cloneTimePointer(context.LastCompressionAt),
		SummaryFailureCount: context.SummaryFailureCount, LastInputTokens: context.LastInputTokens, LastOutputTokens: context.LastOutputTokens,
		LastEstimatedTokens: context.LastEstimatedTokens, LastEstimatedCharacters: context.LastEstimatedCharacters,
	}
}

func cloneArtifactRef(reference *artifact.Ref) *artifact.Ref {
	if reference == nil {
		return nil
	}
	cloned := *reference
	return &cloned
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type RecordType string

const (
	RecordTypeMessage  RecordType = "message"
	RecordTypeSnapshot RecordType = "snapshot"
)

type JSONLDiagnostic = diagnostics.Diagnostic

var (
	JSONLSeverityInfo    = diagnostics.SeverityInfo
	JSONLSeverityWarning = diagnostics.SeverityWarning
	JSONLSeverityError   = diagnostics.SeverityError
)

type JSONLRecord struct {
	Version               int                 `json:"version"`
	Type                  RecordType          `json:"type"`
	SessionID             string              `json:"session_id,omitempty"`
	MessageIndex          int                 `json:"message_index"`
	CreatedAt             time.Time           `json:"created_at"`
	ConversationTitle     string              `json:"conversation_title,omitempty"`
	ConversationCreatedAt time.Time           `json:"conversation_created_at,omitempty"`
	ConversationUpdatedAt time.Time           `json:"conversation_updated_at,omitempty"`
	Message               *legacyMessage      `json:"message,omitempty"`
	Snapshot              *legacyConversation `json:"snapshot,omitempty"`
	Diagnostics           []JSONLDiagnostic   `json:"diagnostics,omitempty"`
	Error                 string              `json:"error,omitempty"`
}

func (r JSONLRecord) Validate() []JSONLDiagnostic {
	var items []JSONLDiagnostic
	if r.Version != JSONLVersion {
		items = append(items, newJSONLDiagnostic("jsonl_unknown_version", fmt.Sprintf("未知 JSONL 版本 %d", r.Version), JSONLSeverityError))
	}
	switch r.Type {
	case RecordTypeMessage:
		if r.Message == nil {
			items = append(items, newJSONLDiagnostic("jsonl_missing_message", "message 记录缺少 message 字段", JSONLSeverityError))
		}
	case RecordTypeSnapshot:
		if r.Snapshot == nil {
			items = append(items, newJSONLDiagnostic("jsonl_missing_snapshot", "snapshot 记录缺少 snapshot 字段", JSONLSeverityError))
		}
	default:
		items = append(items, newJSONLDiagnostic("jsonl_invalid_record_type", fmt.Sprintf("非法 JSONL 记录类型 %q", r.Type), JSONLSeverityError))
	}
	return items
}

func (r JSONLRecord) Sanitized() JSONLRecord {
	r.Error = redact.Text(r.Error)
	for i := range r.Diagnostics {
		r.Diagnostics[i] = r.Diagnostics[i].Safe(redact.Text)
	}
	return r
}

func (r JSONLRecord) MarshalJSON() ([]byte, error) {
	type alias JSONLRecord
	sanitized := r.Sanitized()
	return json.Marshal(alias(sanitized))
}

func newJSONLDiagnostic(code string, message string, severity diagnostics.Severity) JSONLDiagnostic {
	return diagnostics.New(code, severity, message).Safe(redact.Text)
}
