package conversation

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"

	"xagent/internal/budget"
)

var (
	ErrV2SaveClassification = errors.New("v2_save_classification_failed")
	ErrV2SaveCommit         = errors.New("v2_save_commit_failed")
	ErrV2BatchCommit        = errors.New("v2_batch_commit_failed")
	ErrV2SnapshotCommit     = errors.New("v2_snapshot_commit_failed")
)

// v2PersistenceMutationGate serializes the transitional v2 Save and Maintain
// mutation paths until T3.18 moves the same invariant under the Store owner.
// A channel rather than sync.Mutex keeps waiting cancellable.
var v2PersistenceMutationGate = func() chan struct{} {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return gate
}()

func acquireV2PersistenceMutation(ctx context.Context) error {
	if ctx == nil {
		return ErrV2SaveCommit
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-v2PersistenceMutationGate:
		return nil
	}
}

func releaseV2PersistenceMutation() {
	v2PersistenceMutationGate <- struct{}{}
}

// v2SaveClassificationInput is the verified state known by the Store and the
// caller's desired final state. Previous must be an independent snapshot of
// Persisted whenever Persisted.Revision is non-zero.
type v2SaveClassificationInput struct {
	Conversation *Conversation
	Previous     *Conversation
	Persisted    PersistedState
	Recovery     RecoveryStatus
}

// v2SavePlan contains exactly one visible record for a mutating save. Noop
// deliberately has a nil Record and republishes the existing Persisted value.
type v2SavePlan struct {
	Result                SaveResult
	Record                *JSONLRecordV2
	ReplaceUnverifiedTail bool
}

func classifyV2Save(input v2SaveClassificationInput) (v2SavePlan, error) {
	if input.Conversation == nil || (input.Recovery != RecoveryClean && input.Recovery != RecoveryPartial) {
		return v2SavePlan{}, ErrV2SaveClassification
	}
	if err := validateV2SaveBaseline(input); err != nil {
		return v2SavePlan{}, err
	}

	candidate, err := computePersistedState(input.Conversation, input.Persisted.Revision)
	if err != nil {
		return v2SavePlan{}, ErrV2SaveClassification
	}
	if input.Recovery == RecoveryClean && input.Persisted.Revision > 0 && candidate.Digest == input.Persisted.Digest {
		return v2SavePlan{Result: SaveResult{Kind: SaveNoop, Persisted: input.Persisted}}, nil
	}

	nextRevision, err := nextV2SaveRevision(input.Persisted.Revision)
	if err != nil {
		return v2SavePlan{}, err
	}
	candidate.Revision = nextRevision

	if input.Recovery == RecoveryClean && isV2TailAppend(input, candidate) {
		record := &JSONLRecordV2{
			Version:        JSONLVersionV2,
			Kind:           RecordBatch,
			SessionID:      input.Conversation.ID,
			Revision:       nextRevision,
			PreviousDigest: input.Persisted.Digest,
			Digest:         candidate.Digest,
			Batch: &MessageBatch{
				BaseMessageCount: input.Persisted.MessageCount,
				Messages:         cloneV2MessageSlice(input.Conversation.Messages[input.Persisted.MessageCount:]),
				UpdatedAt:        input.Conversation.UpdatedAt,
			},
		}
		if err := record.Validate(RecordValidationContext{ExpectedSessionID: input.Conversation.ID, Previous: &input.Persisted}); err != nil {
			return v2SavePlan{}, ErrV2SaveClassification
		}
		return v2SavePlan{Result: SaveResult{Kind: SaveBatch, Persisted: candidate}, Record: record}, nil
	}

	record := &JSONLRecordV2{
		Version:   JSONLVersionV2,
		Kind:      RecordSnapshot,
		SessionID: input.Conversation.ID,
		Revision:  nextRevision,
		Digest:    candidate.Digest,
		Snapshot:  cloneV2Conversation(input.Conversation),
	}
	if input.Persisted.Revision > 0 {
		record.PreviousDigest = input.Persisted.Digest
	}
	context := RecordValidationContext{ExpectedSessionID: input.Conversation.ID}
	if input.Persisted.Revision > 0 {
		context.Previous = &input.Persisted
	}
	if err := record.Validate(context); err != nil {
		return v2SavePlan{}, ErrV2SaveClassification
	}
	return v2SavePlan{
		Result:                SaveResult{Kind: SaveSnapshot, Persisted: candidate},
		Record:                record,
		ReplaceUnverifiedTail: input.Recovery == RecoveryPartial,
	}, nil
}

func validateV2SaveBaseline(input v2SaveClassificationInput) error {
	previous := input.Persisted
	if previous.Revision == 0 {
		if input.Previous != nil || previous.MessageCount != 0 || previous.Digest != "" || previous.MessagesDigest != "" {
			return ErrV2SaveClassification
		}
		return nil
	}
	if input.Previous == nil || previous.Revision == math.MaxUint64 || previous.MessageCount < 0 ||
		!validStateDigestSyntax(previous.Digest) || !validStateDigestSyntax(previous.MessagesDigest) {
		return ErrV2SaveClassification
	}
	if !validRecordIdentifier(input.Previous.ID) || validateV2Snapshot(input.Previous, input.Previous.ID) != nil {
		return ErrV2SaveClassification
	}
	verified, err := computePersistedState(input.Previous, previous.Revision)
	if err != nil || verified != previous {
		return ErrV2SaveClassification
	}
	return nil
}

func nextV2SaveRevision(previous uint64) (uint64, error) {
	if previous == math.MaxUint64 {
		return 0, ErrV2SaveClassification
	}
	return previous + 1, nil
}

func isV2TailAppend(input v2SaveClassificationInput, candidate PersistedState) bool {
	if input.Persisted.Revision == 0 || input.Previous == nil ||
		candidate.MessageCount <= input.Persisted.MessageCount ||
		len(input.Conversation.Messages) < input.Persisted.MessageCount {
		return false
	}
	prefixDigest, err := computeMessagesDigest(input.Conversation.Messages[:input.Persisted.MessageCount])
	if err != nil || prefixDigest != input.Persisted.MessagesDigest {
		return false
	}

	// Compare the caller's non-message state with the verified baseline while
	// normalizing the two fields that an append is allowed to change: the
	// message tail and final UpdatedAt.
	projected := cloneV2Conversation(input.Conversation)
	projected.Messages = cloneV2MessageSlice(input.Previous.Messages)
	projected.UpdatedAt = input.Previous.UpdatedAt
	projectedState, err := computePersistedState(projected, input.Persisted.Revision)
	return err == nil && projectedState == input.Persisted
}

func cloneV2Conversation(source *Conversation) *Conversation {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Messages = cloneV2MessageSlice(source.Messages)
	if source.Context != nil {
		context := *source.Context
		context.LastCompressionAt = cloneTimePointer(source.Context.LastCompressionAt)
		cloned.Context = &context
	}
	return &cloned
}

func cloneV2MessageSlice(source []Message) []Message {
	if source == nil {
		return nil
	}
	cloned := make([]Message, len(source))
	copy(cloned, source)
	for index := range source {
		if source[index].Tool == nil {
			continue
		}
		state := *source[index].Tool
		state.Artifact = cloneArtifactRef(source[index].Tool.Artifact)
		if source[index].Tool.Error != nil {
			safeError := *source[index].Tool.Error
			state.Error = &safeError
		}
		cloned[index].Tool = &state
	}
	return cloned
}

// v2SaveCommitter measures the already-classified next record against the
// verified on-disk prefix before choosing append, prefix-preserving snapshot
// replacement, or a single-record checkpoint replacement.
type v2SaveCommitter struct {
	codec    *V2RecordCodec
	batch    *v2BatchCommitter
	snapshot *v2SnapshotCommitter
}

func newV2SaveCommitter(codec *V2RecordCodec) *v2SaveCommitter {
	return &v2SaveCommitter{
		codec:    codec,
		batch:    newV2BatchCommitter(codec),
		snapshot: newV2SnapshotCommitter(codec),
	}
}

func (c *v2SaveCommitter) commit(
	ctx context.Context,
	path string,
	verifiedBytes int64,
	conversation *Conversation,
	plan v2SavePlan,
	published *PersistedState,
) (SaveResult, error) {
	if ctx == nil || c == nil || c.codec == nil || c.batch == nil || c.snapshot == nil ||
		c.batch.codec != c.codec || c.snapshot.codec != c.codec ||
		c.codec.maxRecordBytes <= 0 || c.codec.maxSessionBytes <= 0 || path == "" ||
		verifiedBytes < 0 || conversation == nil || published == nil ||
		(published.Revision == 0) != (verifiedBytes == 0) {
		return SaveResult{}, ErrV2SaveCommit
	}
	select {
	case <-ctx.Done():
		return SaveResult{}, ctx.Err()
	default:
	}
	if err := acquireV2PersistenceMutation(ctx); err != nil {
		return SaveResult{}, err
	}
	defer releaseV2PersistenceMutation()
	if err := ctx.Err(); err != nil {
		return SaveResult{}, err
	}

	if plan.Result.Kind == SaveNoop {
		if plan.Record != nil || plan.Result.Persisted != *published ||
			plan.ReplaceUnverifiedTail || !v2ConversationMatchesPersisted(conversation, plan.Result.Persisted) {
			return SaveResult{}, ErrV2SaveCommit
		}
		if err := validateV2SaveDiskBaseline(ctx, c.codec, conversation.ID, path, verifiedBytes, *published, false); err != nil {
			return SaveResult{}, err
		}
		return plan.Result, nil
	}
	if !validV2MutatingSavePlan(plan, *published) ||
		!v2ConversationMatchesPersisted(conversation, plan.Result.Persisted) {
		return SaveResult{}, ErrV2SaveCommit
	}
	if err := validateV2SaveDiskBaseline(ctx, c.codec, conversation.ID, path, verifiedBytes, *published, plan.ReplaceUnverifiedTail); err != nil {
		return SaveResult{}, err
	}

	line, err := c.codec.Encode(plan.Record.SessionID, *plan.Record)
	if err != nil {
		return SaveResult{}, err
	}
	lineBytes := int64(len(line))
	needsCheckpoint := verifiedBytes > c.codec.maxSessionBytes ||
		lineBytes > c.codec.maxSessionBytes-verifiedBytes
	if !needsCheckpoint {
		switch plan.Result.Kind {
		case SaveBatch:
			return c.batch.commitBatch(ctx, path, plan, published)
		case SaveSnapshot:
			return c.snapshot.commitSnapshot(ctx, path, verifiedBytes, plan, published)
		default:
			return SaveResult{}, ErrV2SaveCommit
		}
	}

	checkpoint, err := makeV2CheckpointPlan(conversation, plan)
	if err != nil {
		return SaveResult{}, err
	}
	return c.snapshot.commitCheckpoint(ctx, path, checkpoint, published)
}

func validV2MutatingSavePlan(plan v2SavePlan, published PersistedState) bool {
	switch plan.Result.Kind {
	case SaveBatch:
		return !plan.ReplaceUnverifiedTail && validV2BatchSavePlan(plan, published)
	case SaveSnapshot:
		return validV2SnapshotSavePlan(plan, published)
	default:
		return false
	}
}

func validateV2SaveDiskBaseline(
	ctx context.Context,
	codec *V2RecordCodec,
	sessionID string,
	path string,
	verifiedBytes int64,
	published PersistedState,
	allowUnverifiedTail bool,
) error {
	if ctx == nil || codec == nil || !validRecordIdentifier(sessionID) || path == "" || verifiedBytes < 0 {
		return ErrV2SaveCommit
	}
	if published.Revision == 0 {
		if published != (PersistedState{}) || verifiedBytes != 0 || allowUnverifiedTail {
			return ErrV2SaveCommit
		}
		_, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		return ErrV2SaveCommit
	}
	if verifiedBytes == 0 {
		return ErrV2SaveCommit
	}
	observed, err := os.Lstat(path)
	if err != nil || observed.Mode()&os.ModeSymlink != 0 || !observed.Mode().IsRegular() || observed.Size() < verifiedBytes ||
		(!allowUnverifiedTail && observed.Size() != verifiedBytes) {
		return ErrV2SaveCommit
	}
	file, err := os.Open(path)
	if err != nil {
		return ErrV2SaveCommit
	}
	opened, statErr := file.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(observed, opened) || opened.Size() != observed.Size() ||
		!opened.ModTime().Equal(observed.ModTime()) {
		_ = file.Close()
		return ErrV2SaveCommit
	}
	state, loadErr := loadV2Records(ctx, codec, sessionID, io.LimitReader(file, verifiedBytes))
	closeErr := file.Close()
	if loadErr != nil {
		if errors.Is(loadErr, context.Canceled) || errors.Is(loadErr, context.DeadlineExceeded) {
			return loadErr
		}
		return ErrV2SaveCommit
	}
	if closeErr != nil || state.VerifiedBytes != verifiedBytes || state.Persisted != published {
		return ErrV2SaveCommit
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(opened, current) ||
		current.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) {
		return ErrV2SaveCommit
	}
	return nil
}

func v2ConversationMatchesPersisted(conversation *Conversation, persisted PersistedState) bool {
	if conversation == nil || persisted.Revision == 0 {
		return false
	}
	computed, err := computePersistedState(conversation, persisted.Revision)
	return err == nil && computed == persisted
}

func makeV2CheckpointPlan(conversation *Conversation, source v2SavePlan) (v2SavePlan, error) {
	if conversation == nil || source.Record == nil ||
		(source.Result.Kind != SaveBatch && source.Result.Kind != SaveSnapshot) ||
		!v2ConversationMatchesPersisted(conversation, source.Result.Persisted) {
		return v2SavePlan{}, ErrV2SaveCommit
	}
	checkpoint := v2SavePlan{
		Result: SaveResult{Kind: SaveSnapshot, Persisted: source.Result.Persisted},
		Record: &JSONLRecordV2{
			Version:   JSONLVersionV2,
			Kind:      RecordSnapshot,
			SessionID: conversation.ID,
			Revision:  source.Result.Persisted.Revision,
			Digest:    source.Result.Persisted.Digest,
			Snapshot:  cloneV2Conversation(conversation),
		},
	}
	if !validV2CheckpointSavePlan(checkpoint, source.Result.Persisted.Revision-1) {
		return v2SavePlan{}, ErrV2SaveCommit
	}
	return checkpoint, nil
}

type v2AppendFile interface {
	io.Writer
	Sync() error
	Close() error
}

type v2FlushWriter interface {
	io.Writer
	Flush() error
}

type v2RollbackAppendFile interface {
	Stat() (os.FileInfo, error)
	Truncate(int64) error
}

type v2BatchCommitter struct {
	codec      *V2RecordCodec
	openAppend func(string) (v2AppendFile, error)
	newBuffer  func(io.Writer) v2FlushWriter
}

func newV2BatchCommitter(codec *V2RecordCodec) *v2BatchCommitter {
	return &v2BatchCommitter{
		codec: codec,
		openAppend: func(path string) (v2AppendFile, error) {
			return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		},
		newBuffer: func(writer io.Writer) v2FlushWriter {
			return bufio.NewWriterSize(writer, v2RecordReadBufferBytes)
		},
	}
}

// commitBatch appends one already-classified batch. The caller-owned state is
// published only after the complete line has been flushed and fsynced. Once
// writing starts, the commit is not interrupted by a late context
// cancellation because doing so would create an avoidable ambiguous tail.
func (c *v2BatchCommitter) commitBatch(ctx context.Context, path string, plan v2SavePlan, published *PersistedState) (SaveResult, error) {
	if ctx == nil || c == nil || c.codec == nil || c.openAppend == nil || c.newBuffer == nil ||
		path == "" || published == nil || !validV2BatchSavePlan(plan, *published) {
		return SaveResult{}, ErrV2BatchCommit
	}
	select {
	case <-ctx.Done():
		return SaveResult{}, ctx.Err()
	default:
	}

	line, err := c.codec.Encode(plan.Record.SessionID, *plan.Record)
	if err != nil {
		return SaveResult{}, err
	}
	file, err := c.openAppend(path)
	if err != nil || file == nil {
		return SaveResult{}, ErrV2BatchCommit
	}
	defer func() { _ = file.Close() }()
	rollbackFile, rollbackAvailable := file.(v2RollbackAppendFile)
	var originalSize int64
	if rollbackAvailable {
		info, statErr := rollbackFile.Stat()
		if statErr != nil {
			return SaveResult{}, ErrV2BatchCommit
		}
		originalSize = info.Size()
	}

	buffer := c.newBuffer(file)
	if buffer == nil {
		return SaveResult{}, ErrV2BatchCommit
	}
	if err := writeFullV2Record(buffer, line); err != nil {
		rollbackV2Append(file, rollbackFile, rollbackAvailable, originalSize)
		return SaveResult{}, ErrV2BatchCommit
	}
	if err := buffer.Flush(); err != nil {
		rollbackV2Append(file, rollbackFile, rollbackAvailable, originalSize)
		return SaveResult{}, ErrV2BatchCommit
	}
	if err := file.Sync(); err != nil {
		rollbackV2Append(file, rollbackFile, rollbackAvailable, originalSize)
		return SaveResult{}, ErrV2BatchCommit
	}

	*published = plan.Result.Persisted
	return plan.Result, nil
}

func rollbackV2Append(file v2AppendFile, rollback v2RollbackAppendFile, available bool, size int64) {
	if !available || rollback == nil || size < 0 {
		return
	}
	if rollback.Truncate(size) == nil {
		_ = file.Sync()
	}
}

func validV2BatchSavePlan(plan v2SavePlan, published PersistedState) bool {
	if plan.Result.Kind != SaveBatch || plan.Record == nil || plan.Record.Kind != RecordBatch ||
		plan.Record.Batch == nil || plan.Record.Snapshot != nil || plan.Result.Persisted.Revision != plan.Record.Revision ||
		plan.Result.Persisted.Digest != plan.Record.Digest || plan.Result.Persisted.MessageCount != plan.Record.Batch.BaseMessageCount+len(plan.Record.Batch.Messages) ||
		!validStateDigestSyntax(plan.Result.Persisted.MessagesDigest) {
		return false
	}
	if err := plan.Record.Validate(RecordValidationContext{ExpectedSessionID: plan.Record.SessionID, Previous: &published}); err != nil {
		return false
	}
	return true
}

func writeFullV2Record(writer io.Writer, line []byte) error {
	if writer == nil || len(line) == 0 || line[len(line)-1] != '\n' {
		return ErrV2BatchCommit
	}
	for written := 0; written < len(line); {
		count, err := writer.Write(line[written:])
		if count < 0 || count > len(line)-written {
			return ErrV2BatchCommit
		}
		written += count
		if err != nil || count == 0 {
			return ErrV2BatchCommit
		}
	}
	return nil
}

type v2StageFile interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
}

type v2SnapshotCommitter struct {
	codec        *V2RecordCodec
	createStage  func(string, string) (v2StageFile, error)
	openVerified func(string) (io.ReadCloser, error)
	newBuffer    func(io.Writer) v2FlushWriter
	replace      func(string, string) error
	remove       func(string) error
}

func newV2SnapshotCommitter(codec *V2RecordCodec) *v2SnapshotCommitter {
	return &v2SnapshotCommitter{
		codec: codec,
		createStage: func(directory string, pattern string) (v2StageFile, error) {
			file, err := os.CreateTemp(directory, pattern)
			if err != nil {
				return nil, err
			}
			if err := file.Chmod(0o600); err != nil {
				_ = file.Close()
				_ = os.Remove(file.Name())
				return nil, err
			}
			return file, nil
		},
		openVerified: func(path string) (io.ReadCloser, error) {
			return os.Open(path)
		},
		newBuffer: func(writer io.Writer) v2FlushWriter {
			return bufio.NewWriterSize(writer, v2RecordReadBufferBytes)
		},
		replace: replaceV2FileAtomic,
		remove:  os.Remove,
	}
}

// commitSnapshot builds a complete replacement beside the target. Only the
// caller-provided verified prefix is copied; unverified bytes after that
// boundary can never be carried into the replacement. Rename is the commit
// point, and the caller-owned state is published only after it succeeds.
func (c *v2SnapshotCommitter) commitSnapshot(
	ctx context.Context,
	path string,
	verifiedBytes int64,
	plan v2SavePlan,
	published *PersistedState,
) (SaveResult, error) {
	if ctx == nil || c == nil || c.codec == nil || c.createStage == nil || c.openVerified == nil ||
		c.newBuffer == nil || c.replace == nil || c.remove == nil || path == "" || verifiedBytes < 0 ||
		published == nil || (published.Revision == 0) != (verifiedBytes == 0) || !validV2SnapshotSavePlan(plan, *published) {
		return SaveResult{}, ErrV2SnapshotCommit
	}
	select {
	case <-ctx.Done():
		return SaveResult{}, ctx.Err()
	default:
	}

	line, err := c.codec.Encode(plan.Record.SessionID, *plan.Record)
	if err != nil {
		return SaveResult{}, err
	}
	return c.commitReplacement(path, verifiedBytes, line, plan.Result, published)
}

// commitCheckpoint atomically replaces all history with one self-contained
// snapshot anchor. Its revision remains the classified next revision; only
// the physical predecessor link is intentionally removed.
func (c *v2SnapshotCommitter) commitCheckpoint(
	ctx context.Context,
	path string,
	plan v2SavePlan,
	published *PersistedState,
) (SaveResult, error) {
	if ctx == nil || c == nil || c.codec == nil || c.createStage == nil || c.openVerified == nil ||
		c.newBuffer == nil || c.replace == nil || c.remove == nil || path == "" || published == nil ||
		!validV2CheckpointSavePlan(plan, published.Revision) {
		return SaveResult{}, ErrV2SnapshotCommit
	}
	select {
	case <-ctx.Done():
		return SaveResult{}, ctx.Err()
	default:
	}

	line, err := c.codec.Encode(plan.Record.SessionID, *plan.Record)
	if err != nil {
		return SaveResult{}, err
	}
	if int64(len(line)) > c.codec.maxSessionBytes {
		return SaveResult{}, newRecordBudgetError(plan.Record.SessionID, budget.SessionMaxSessionBytes, c.codec.maxSessionBytes)
	}
	return c.commitReplacement(path, 0, line, plan.Result, published)
}

func (c *v2SnapshotCommitter) commitReplacement(
	path string,
	verifiedBytes int64,
	line []byte,
	result SaveResult,
	published *PersistedState,
) (SaveResult, error) {
	directory := filepath.Dir(path)
	stage, err := c.createStage(directory, ".xagent-snapshot-*")
	if err != nil || stage == nil || filepath.Dir(stage.Name()) != directory {
		if stage != nil {
			_ = stage.Close()
			_ = c.remove(stage.Name())
		}
		return SaveResult{}, ErrV2SnapshotCommit
	}
	stageName := stage.Name()
	stageOpen := true
	replaced := false
	defer func() {
		if stageOpen {
			_ = stage.Close()
		}
		if !replaced {
			_ = c.remove(stageName)
		}
	}()

	if verifiedBytes > 0 {
		if err := c.copyVerifiedPrefix(path, stage, verifiedBytes); err != nil {
			return SaveResult{}, err
		}
	}
	buffer := c.newBuffer(stage)
	if buffer == nil || writeFullV2Record(buffer, line) != nil || buffer.Flush() != nil || stage.Sync() != nil {
		return SaveResult{}, ErrV2SnapshotCommit
	}
	if err := stage.Close(); err != nil {
		return SaveResult{}, ErrV2SnapshotCommit
	}
	stageOpen = false
	if err := c.replace(stageName, path); err != nil {
		return SaveResult{}, ErrV2SnapshotCommit
	}
	replaced = true

	*published = result.Persisted
	return result, nil
}

func (c *v2SnapshotCommitter) copyVerifiedPrefix(path string, stage io.Writer, verifiedBytes int64) error {
	source, err := c.openVerified(path)
	if err != nil || source == nil {
		return ErrV2SnapshotCommit
	}
	defer source.Close()
	tracker := &v2VerifiedPrefixWriter{writer: stage}
	written, err := io.CopyN(tracker, source, verifiedBytes)
	if err != nil || written != verifiedBytes || tracker.written != verifiedBytes || tracker.last != '\n' {
		return ErrV2SnapshotCommit
	}
	return nil
}

type v2VerifiedPrefixWriter struct {
	writer  io.Writer
	written int64
	last    byte
}

func (w *v2VerifiedPrefixWriter) Write(data []byte) (int, error) {
	if w == nil || w.writer == nil {
		return 0, ErrV2SnapshotCommit
	}
	count, err := w.writer.Write(data)
	if count < 0 || count > len(data) {
		return 0, ErrV2SnapshotCommit
	}
	if count > 0 {
		w.last = data[count-1]
		w.written += int64(count)
	}
	if err == nil && count != len(data) {
		return count, io.ErrShortWrite
	}
	return count, err
}

func validV2SnapshotSavePlan(plan v2SavePlan, published PersistedState) bool {
	if published.Revision == 0 && published != (PersistedState{}) {
		return false
	}
	if plan.Result.Kind != SaveSnapshot || plan.Record == nil || plan.Record.Kind != RecordSnapshot ||
		plan.Record.Snapshot == nil || plan.Record.Batch != nil || plan.Result.Persisted.Revision != plan.Record.Revision ||
		plan.Result.Persisted.Digest != plan.Record.Digest || plan.Result.Persisted.MessageCount != len(plan.Record.Snapshot.Messages) ||
		!validStateDigestSyntax(plan.Result.Persisted.MessagesDigest) {
		return false
	}
	recomputed, err := computePersistedState(plan.Record.Snapshot, plan.Record.Revision)
	if err != nil || recomputed != plan.Result.Persisted {
		return false
	}
	context := RecordValidationContext{ExpectedSessionID: plan.Record.SessionID}
	if published.Revision > 0 {
		context.Previous = &published
	}
	return plan.Record.Validate(context) == nil
}

func validV2CheckpointSavePlan(plan v2SavePlan, previousRevision uint64) bool {
	if previousRevision == math.MaxUint64 || plan.Result.Kind != SaveSnapshot || plan.Record == nil ||
		plan.Record.Kind != RecordSnapshot || plan.Record.Snapshot == nil || plan.Record.Batch != nil ||
		plan.Record.PreviousDigest != "" || plan.Record.Revision != previousRevision+1 ||
		plan.Result.Persisted.Revision != plan.Record.Revision ||
		plan.Result.Persisted.Digest != plan.Record.Digest ||
		plan.Result.Persisted.MessageCount != len(plan.Record.Snapshot.Messages) ||
		!validStateDigestSyntax(plan.Result.Persisted.MessagesDigest) {
		return false
	}
	recomputed, err := computePersistedState(plan.Record.Snapshot, plan.Record.Revision)
	if err != nil || recomputed != plan.Result.Persisted {
		return false
	}
	return plan.Record.Validate(RecordValidationContext{ExpectedSessionID: plan.Record.SessionID}) == nil
}
