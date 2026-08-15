package conversation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"xagent/internal/budget"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

var (
	ErrV2ListConfig    = errors.New("v2_list_invalid_config")
	ErrV2ListDirectory = errors.New("v2_list_directory_failed")
	ErrV2StoreConfig   = errors.New("v2_store_invalid_config")
	ErrV2StoreConflict = errors.New("v2_store_state_conflict")
	ErrV2StoreNotFound = errors.New("v2_store_not_found")
	ErrV2StoreRead     = errors.New("v2_store_read_failed")
)

const (
	v2ListTruncatedCode           = "conversation_v2_list_truncated"
	v2ListOpenFailedCode          = "conversation_v2_list_open_failed"
	v2ListUnsupportedEntryCode    = "conversation_v2_list_unsupported_entry"
	v2ListInvalidNameCode         = "conversation_v2_list_invalid_name"
	v2ListTruncatedMessage        = "The conversation list reached its scan budget."
	v2ListOpenFailedMessage       = "The conversation file could not be opened for a safe read."
	v2ListUnsupportedEntryMessage = "The conversation entry is not a regular file."
	v2ListInvalidNameMessage      = "The conversation entry has an invalid session identifier."
	v2ListPlaceholderTitle        = "Unavailable conversation"
)

type v2ListOptions struct {
	MaxScanFiles int64
	MaxScanBytes int64
}

type JSONLStoreOptions struct {
	DataDir                string
	Redactor               *redact.RuntimeRedactor
	MaxRecordBytes         int64
	MaxSessionBytes        int64
	MaxScanFiles           int64
	MaxScanBytes           int64
	RetentionDays          int64
	GapReminderDays        int64
	LegacyArtifactImporter LegacyArtifactImporter
	Now                    func() time.Time
}

type JSONLStore struct {
	legacyRoot  string
	v2Root      string
	now         func() time.Time
	codec       *V2RecordCodec
	committer   *v2SaveCommitter
	migration   legacyMigration
	list        v2ListOptions
	recovery    v2RecoveryMetadataOptions
	maintenance v2MaintenanceOptions
	lease       chan struct{}
	tracked     map[*Conversation]trackedConversation
}

type trackedConversation struct {
	id            string
	previous      *Conversation
	persisted     PersistedState
	recovery      RecoveryStatus
	verifiedBytes int64
}

var _ Store = (*JSONLStore)(nil)

func NewJSONLStore(options JSONLStoreOptions) (*JSONLStore, error) {
	dataDir := filepath.Clean(options.DataDir)
	if options.Redactor == nil || dataDir == "." || !filepath.IsAbs(dataDir) || dataDir != options.DataDir ||
		options.MaxScanFiles <= 0 || options.MaxScanBytes <= 0 || options.RetentionDays <= 0 || options.GapReminderDays <= 0 {
		return nil, ErrV2StoreConfig
	}
	codec, err := NewV2RecordCodec(V2RecordCodecOptions{MaxRecordBytes: options.MaxRecordBytes, MaxSessionBytes: options.MaxSessionBytes, Redactor: options.Redactor})
	if err != nil {
		return nil, ErrV2StoreConfig
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, ErrV2StoreConfig
	}
	v2Root := filepath.Join(dataDir, "v2")
	if err := os.Mkdir(v2Root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, ErrV2StoreConfig
	}
	info, err := os.Lstat(v2Root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, ErrV2StoreConfig
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	store := &JSONLStore{
		legacyRoot: dataDir, v2Root: v2Root, now: now, codec: codec,
		committer:   newV2SaveCommitter(codec),
		list:        v2ListOptions{MaxScanFiles: options.MaxScanFiles, MaxScanBytes: options.MaxScanBytes},
		recovery:    v2RecoveryMetadataOptions{GapReminderDays: options.GapReminderDays, Now: now},
		maintenance: v2MaintenanceOptions{RetentionDays: options.RetentionDays, MaxScanFiles: options.MaxScanFiles, MaxScanBytes: options.MaxScanBytes, Now: now},
		lease:       make(chan struct{}, 1), tracked: make(map[*Conversation]trackedConversation),
	}
	store.lease <- struct{}{}
	store.migration = legacyMigration{codec: codec, artifactImporter: options.LegacyArtifactImporter}
	return store, nil
}

func (s *JSONLStore) Create(ctx context.Context) (*Conversation, error) {
	if err := s.acquire(ctx); err != nil {
		return nil, err
	}
	defer s.release()
	now := s.now()
	if now.IsZero() {
		return nil, ErrV2StoreConfig
	}
	for attempts := 0; attempts < 32; attempts++ {
		id, err := newV2SessionID(now)
		if err != nil {
			return nil, err
		}
		if s.anySessionPathExists(id) {
			continue
		}
		conversation := &Conversation{ID: id, Title: s.codec.redactor.Redact("新会话"), Messages: []Message{}, CreatedAt: now, UpdatedAt: now}
		s.tracked[conversation] = trackedConversation{id: id, recovery: RecoveryClean}
		return conversation, nil
	}
	return nil, ErrV2StoreConflict
}

func (s *JSONLStore) Save(ctx context.Context, conversation *Conversation) (SaveResult, error) {
	if err := s.acquire(ctx); err != nil {
		return SaveResult{}, err
	}
	defer s.release()
	tracked, ok := s.tracked[conversation]
	if !ok || conversation == nil || conversation.ID != tracked.id || tracked.recovery == RecoveryPlaceholder {
		return SaveResult{}, ErrV2StoreConflict
	}
	desired := cloneV2Conversation(conversation)
	plan, err := classifyV2Save(v2SaveClassificationInput{Conversation: desired, Previous: cloneV2Conversation(tracked.previous), Persisted: tracked.persisted, Recovery: tracked.recovery})
	if err != nil {
		return SaveResult{}, err
	}
	nextBytes, err := plannedV2VerifiedBytes(s.codec, tracked.verifiedBytes, desired, plan)
	if err != nil {
		return SaveResult{}, err
	}
	published := tracked.persisted
	result, err := s.committer.commit(ctx, s.v2Path(tracked.id), tracked.verifiedBytes, desired, plan, &published)
	if err != nil {
		return SaveResult{}, err
	}
	if result.Persisted != published {
		return SaveResult{}, ErrV2StoreConflict
	}
	tracked.previous = cloneV2Conversation(desired)
	tracked.persisted = published
	tracked.recovery = RecoveryClean
	tracked.verifiedBytes = nextBytes
	s.tracked[conversation] = tracked
	*conversation = *cloneV2Conversation(desired)
	return result, nil
}

func (s *JSONLStore) Load(ctx context.Context, id string) (LoadResult, error) {
	if err := s.acquire(ctx); err != nil {
		return LoadResult{}, err
	}
	defer s.release()
	if !validV2ListSessionID(id) {
		return LoadResult{}, ErrV2StoreConfig
	}
	result, verifiedBytes, err := s.loadSession(ctx, id)
	if err != nil {
		return LoadResult{}, err
	}
	result = cloneLoadResult(result)
	if result.Available && result.Conversation != nil {
		conversation := result.Conversation
		var previous *Conversation
		if result.Persisted.Revision > 0 {
			previous = cloneV2Conversation(conversation)
		}
		s.tracked[conversation] = trackedConversation{id: id, previous: previous, persisted: result.Persisted, recovery: result.Recovery.Status, verifiedBytes: verifiedBytes}
	}
	return result, nil
}

func (s *JSONLStore) List(ctx context.Context) (ListResult, error) {
	if err := s.acquire(ctx); err != nil {
		return ListResult{}, err
	}
	defer s.release()
	result, err := listV2Sessions(ctx, s.codec, s.v2Root, s.list)
	if err != nil {
		return ListResult{}, err
	}
	return s.appendLegacyList(ctx, result)
}

func (s *JSONLStore) Maintain(ctx context.Context) (MaintenanceResult, error) {
	if err := s.acquire(ctx); err != nil {
		return MaintenanceResult{}, err
	}
	defer s.release()
	return maintainV2Sessions(ctx, s.codec, s.v2Root, s.maintenance)
}

// listV2Sessions is the bounded implementation behind the single Store.List
// entry point.
func listV2Sessions(ctx context.Context, codec *V2RecordCodec, dataRoot string, options v2ListOptions) (ListResult, error) {
	if ctx == nil || codec == nil || codec.redactor == nil || codec.maxRecordBytes <= 0 || codec.maxSessionBytes <= 0 ||
		dataRoot == "" || !filepath.IsAbs(dataRoot) || filepath.Clean(dataRoot) != dataRoot {
		return ListResult{}, ErrV2ListConfig
	}
	if err := ctx.Err(); err != nil {
		return ListResult{}, err
	}
	maxFiles, err := resolveV2ListLimit(budget.SessionMaxScanFiles, budget.Files, options.MaxScanFiles)
	if err != nil {
		return ListResult{}, err
	}
	maxBytes, err := resolveV2ListLimit(budget.SessionMaxScanBytes, budget.Bytes, options.MaxScanBytes)
	if err != nil {
		return ListResult{}, err
	}

	observedRoot, err := os.Lstat(dataRoot)
	if err != nil || observedRoot.Mode()&os.ModeSymlink != 0 || !observedRoot.IsDir() {
		if contextErr := ctx.Err(); contextErr != nil {
			return ListResult{}, contextErr
		}
		return ListResult{}, ErrV2ListDirectory
	}
	root, err := os.OpenRoot(dataRoot)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return ListResult{}, contextErr
		}
		return ListResult{}, ErrV2ListDirectory
	}
	defer root.Close()
	openedRoot, err := root.Stat(".")
	if err != nil || !openedRoot.IsDir() || !os.SameFile(observedRoot, openedRoot) {
		if contextErr := ctx.Err(); contextErr != nil {
			return ListResult{}, contextErr
		}
		return ListResult{}, ErrV2ListDirectory
	}
	directory, err := root.Open(".")
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return ListResult{}, contextErr
		}
		return ListResult{}, ErrV2ListDirectory
	}
	defer directory.Close()

	result := ListResult{Entries: make([]ListEntry, 0)}
	for {
		if err := ctx.Err(); err != nil {
			return ListResult{}, err
		}
		batch, readErr := directory.ReadDir(1)
		if err := ctx.Err(); err != nil {
			return ListResult{}, err
		}
		if errors.Is(readErr, io.EOF) && len(batch) == 0 {
			break
		}
		if (readErr != nil && !errors.Is(readErr, io.EOF)) || len(batch) != 1 {
			return ListResult{}, ErrV2ListDirectory
		}
		if int64(result.ScannedFiles) >= maxFiles {
			markV2ListTruncated(&result, codec)
			break
		}
		result.ScannedFiles++
		directoryEntry := batch[0]
		if directoryEntry.IsDir() || !strings.HasSuffix(directoryEntry.Name(), ".jsonl") {
			continue
		}

		name := directoryEntry.Name()
		sessionID := strings.TrimSuffix(name, ".jsonl")
		if !validV2ListSessionID(sessionID) {
			result.Entries = append(result.Entries, newV2ListPlaceholder(codec, opaqueV2ListID(name), time.Time{}, v2ListInvalidNameCode, v2ListInvalidNameMessage))
			continue
		}

		observed, statErr := root.Lstat(name)
		if statErr != nil {
			result.Entries = append(result.Entries, newV2ListPlaceholder(codec, sessionID, time.Time{}, v2ListOpenFailedCode, v2ListOpenFailedMessage))
			continue
		}
		if observed.Mode()&os.ModeSymlink != 0 || !observed.Mode().IsRegular() || observed.Size() < 0 {
			result.Entries = append(result.Entries, newV2ListPlaceholder(codec, sessionID, observed.ModTime(), v2ListUnsupportedEntryCode, v2ListUnsupportedEntryMessage))
			continue
		}
		if observed.Size() > maxBytes-result.ScannedBytes {
			markV2ListTruncated(&result, codec)
			break
		}
		if err := ctx.Err(); err != nil {
			return ListResult{}, err
		}

		file, openErr := root.Open(name)
		if openErr != nil {
			result.Entries = append(result.Entries, newV2ListPlaceholder(codec, sessionID, observed.ModTime(), v2ListOpenFailedCode, v2ListOpenFailedMessage))
			continue
		}
		opened, openedErr := file.Stat()
		if openedErr != nil || !opened.Mode().IsRegular() || !os.SameFile(observed, opened) || opened.Size() < 0 {
			_ = file.Close()
			result.Entries = append(result.Entries, newV2ListPlaceholder(codec, sessionID, observed.ModTime(), v2ListUnsupportedEntryCode, v2ListUnsupportedEntryMessage))
			continue
		}
		if opened.Size() > maxBytes-result.ScannedBytes {
			_ = file.Close()
			markV2ListTruncated(&result, codec)
			break
		}
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return ListResult{}, err
		}

		result.ScannedBytes += opened.Size()
		state, report, recoverErr := recoverV2Records(ctx, codec, sessionID, io.LimitReader(file, opened.Size()))
		closeErr := file.Close()
		if recoverErr != nil {
			if errors.Is(recoverErr, context.Canceled) || errors.Is(recoverErr, context.DeadlineExceeded) {
				return ListResult{}, recoverErr
			}
			result.Entries = append(result.Entries, newV2ListPlaceholder(codec, sessionID, opened.ModTime(), v2ListOpenFailedCode, v2ListOpenFailedMessage))
			continue
		}
		if closeErr != nil {
			result.Entries = append(result.Entries, newV2ListPlaceholder(codec, sessionID, opened.ModTime(), v2ListOpenFailedCode, v2ListOpenFailedMessage))
			continue
		}
		if err := ctx.Err(); err != nil {
			return ListResult{}, err
		}
		result.Entries = append(result.Entries, v2ListEntryFromRecovery(codec, sessionID, opened.ModTime(), state, report))
	}

	if err := ctx.Err(); err != nil {
		return ListResult{}, err
	}
	sort.SliceStable(result.Entries, func(left int, right int) bool {
		leftTime := result.Entries[left].Summary.UpdatedAt
		rightTime := result.Entries[right].Summary.UpdatedAt
		if leftTime.Equal(rightTime) {
			return result.Entries[left].Summary.ID < result.Entries[right].Summary.ID
		}
		return leftTime.After(rightTime)
	})
	if err := ctx.Err(); err != nil {
		return ListResult{}, err
	}
	return result, nil
}

func resolveV2ListLimit(scope budget.Scope, dimension budget.Dimension, configured int64) (int64, error) {
	for _, specification := range budget.AllSpecs() {
		if specification.Scope != scope {
			continue
		}
		if specification.Dimension != dimension {
			return 0, ErrV2ListConfig
		}
		var candidate *int64
		if configured != 0 {
			candidate = &configured
		}
		resolved, err := specification.Resolve(candidate)
		if err != nil {
			return 0, ErrV2ListConfig
		}
		return resolved, nil
	}
	return 0, ErrV2ListConfig
}

func validV2ListSessionID(sessionID string) bool {
	return validRecordIdentifier(sessionID) && sessionID != "." && sessionID != ".." &&
		!strings.ContainsAny(sessionID, `/\\`)
}

func opaqueV2ListID(name string) string {
	digest := sha256.Sum256([]byte(name))
	return "unavailable-" + hex.EncodeToString(digest[:])
}

func v2ListEntryFromRecovery(codec *V2RecordCodec, sessionID string, modifiedAt time.Time, state v2LoadState, report RecoveryReport) ListEntry {
	if report.Status == RecoveryPlaceholder || state.Conversation == nil {
		return newV2ListPlaceholderWithReport(codec, sessionID, modifiedAt, report)
	}
	conversation := state.Conversation
	return ListEntry{
		Summary: ConversationSummary{
			ID:           conversation.ID,
			Title:        conversation.Title,
			UpdatedAt:    conversation.UpdatedAt,
			MessageCount: len(conversation.Messages),
		},
		Available: true,
		Recovery:  report,
	}
}

func newV2ListPlaceholder(codec *V2RecordCodec, sessionID string, modifiedAt time.Time, code string, message string) ListEntry {
	report := newV2RecoveryReport(codec, RecoveryPlaceholder, 0, 0, code, message)
	return newV2ListPlaceholderWithReport(codec, sessionID, modifiedAt, report)
}

func newV2ListPlaceholderWithReport(codec *V2RecordCodec, sessionID string, modifiedAt time.Time, report RecoveryReport) ListEntry {
	return ListEntry{
		Summary: ConversationSummary{
			ID:        sessionID,
			Title:     codec.redactor.Redact(v2ListPlaceholderTitle),
			UpdatedAt: modifiedAt,
		},
		Available: false,
		Recovery:  report,
	}
}

func markV2ListTruncated(result *ListResult, codec *V2RecordCodec) {
	if result == nil || result.Truncated {
		return
	}
	result.Truncated = true
	diagnostic := diagnostics.New(v2ListTruncatedCode, diagnostics.SeverityWarning, v2ListTruncatedMessage).WithSource(v2RecoverySource)
	result.Diagnostics = append(result.Diagnostics, diagnostic.Safe(codec.redactor.Text))
}

func (s *JSONLStore) v2Path(id string) string         { return filepath.Join(s.v2Root, id+".jsonl") }
func (s *JSONLStore) legacyJSONPath(id string) string { return filepath.Join(s.legacyRoot, id+".json") }
func (s *JSONLStore) legacyJSONLPath(id string) string {
	return filepath.Join(s.legacyRoot, id+".jsonl")
}

func (s *JSONLStore) acquire(ctx context.Context) error {
	if s == nil || ctx == nil || s.lease == nil {
		return ErrV2StoreConfig
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.lease:
		return nil
	}
}
func (s *JSONLStore) release() { s.lease <- struct{}{} }

func newV2SessionID(now time.Time) (string, error) {
	var suffix [16]byte
	if _, err := io.ReadFull(rand.Reader, suffix[:]); err != nil {
		return "", ErrV2StoreConfig
	}
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(suffix[:]), nil
}

func (s *JSONLStore) anySessionPathExists(id string) bool {
	for _, path := range []string{s.v2Path(id), s.legacyJSONPath(id), s.legacyJSONLPath(id)} {
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func plannedV2VerifiedBytes(codec *V2RecordCodec, verified int64, conversation *Conversation, plan v2SavePlan) (int64, error) {
	if plan.Result.Kind == SaveNoop {
		return verified, nil
	}
	if codec == nil || plan.Record == nil {
		return 0, ErrV2StoreConflict
	}
	line, err := codec.Encode(plan.Record.SessionID, *plan.Record)
	if err != nil {
		return 0, err
	}
	if verified <= codec.maxSessionBytes && int64(len(line)) <= codec.maxSessionBytes-verified {
		return verified + int64(len(line)), nil
	}
	checkpoint, err := makeV2CheckpointPlan(conversation, plan)
	if err != nil {
		return 0, err
	}
	line, err = codec.Encode(checkpoint.Record.SessionID, *checkpoint.Record)
	if err != nil {
		return 0, err
	}
	return int64(len(line)), nil
}

func (s *JSONLStore) loadSession(ctx context.Context, id string) (LoadResult, int64, error) {
	if result, bytes, exists, err := s.loadV2Path(ctx, id); exists || err != nil {
		return result, bytes, err
	}
	if result, exists, err := s.loadLegacyPath(ctx, id, legacyFormatJSONLV1, s.legacyJSONLPath(id)); exists || err != nil {
		return result, 0, err
	}
	if result, exists, err := s.loadLegacyPath(ctx, id, legacyFormatJSON, s.legacyJSONPath(id)); exists || err != nil {
		return result, 0, err
	}
	return LoadResult{}, 0, ErrV2StoreNotFound
}

func openVerifiedRegular(path string) (*os.File, os.FileInfo, bool, error) {
	observed, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil || observed.Mode()&os.ModeSymlink != 0 || !observed.Mode().IsRegular() || observed.Size() < 0 {
		return nil, nil, true, ErrV2StoreRead
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, true, ErrV2StoreRead
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(observed, opened) || opened.Size() != observed.Size() {
		_ = file.Close()
		return nil, nil, true, ErrV2StoreRead
	}
	return file, opened, true, nil
}

func (s *JSONLStore) loadV2Path(ctx context.Context, id string) (LoadResult, int64, bool, error) {
	file, info, exists, err := openVerifiedRegular(s.v2Path(id))
	if !exists || err != nil {
		return LoadResult{}, 0, exists, err
	}
	state, report, recoverErr := recoverV2RecordsForLoad(ctx, s.codec, id, io.LimitReader(file, info.Size()), s.recovery)
	closeErr := file.Close()
	if recoverErr != nil {
		return LoadResult{}, 0, true, recoverErr
	}
	if closeErr != nil {
		return LoadResult{}, 0, true, ErrV2StoreRead
	}
	result := LoadResult{Conversation: state.Conversation, Available: state.Conversation != nil && report.Status != RecoveryPlaceholder, Persisted: state.Persisted, Recovery: report}
	return result, state.VerifiedBytes, true, nil
}

func (s *JSONLStore) loadLegacyPath(ctx context.Context, id string, format legacyConversationFormat, path string) (LoadResult, bool, error) {
	file, info, exists, err := openVerifiedRegular(path)
	if !exists || err != nil {
		return LoadResult{}, exists, err
	}
	result, loadErr := s.migration.loadResult(ctx, io.LimitReader(file, info.Size()), legacyLoadOptions{Format: format, ExpectedSessionID: id})
	closeErr := file.Close()
	if loadErr != nil {
		return LoadResult{}, true, loadErr
	}
	if closeErr != nil {
		return LoadResult{}, true, ErrV2StoreRead
	}
	return result, true, nil
}

func cloneLoadResult(result LoadResult) LoadResult {
	result.Conversation = cloneV2Conversation(result.Conversation)
	result.Recovery.Diagnostics = append([]diagnostics.Diagnostic(nil), result.Recovery.Diagnostics...)
	return result
}

type legacyListCandidate struct {
	id     string
	path   string
	format legacyConversationFormat
}

func (s *JSONLStore) appendLegacyList(ctx context.Context, result ListResult) (ListResult, error) {
	entries, err := os.ReadDir(s.legacyRoot)
	if err != nil {
		return ListResult{}, ErrV2ListDirectory
	}
	seen := make(map[string]struct{}, len(result.Entries))
	for _, entry := range result.Entries {
		seen[entry.Summary.ID] = struct{}{}
	}
	candidates := make(map[string]legacyListCandidate)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return ListResult{}, err
		}
		if int64(result.ScannedFiles) >= s.list.MaxScanFiles {
			markV2ListTruncated(&result, s.codec)
			break
		}
		result.ScannedFiles++
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		var format legacyConversationFormat
		var id string
		switch {
		case strings.HasSuffix(name, ".jsonl"):
			format, id = legacyFormatJSONLV1, strings.TrimSuffix(name, ".jsonl")
		case strings.HasSuffix(name, ".json"):
			format, id = legacyFormatJSON, strings.TrimSuffix(name, ".json")
		default:
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		if !validV2ListSessionID(id) {
			result.Entries = append(result.Entries, newV2ListPlaceholder(s.codec, opaqueV2ListID(name), time.Time{}, v2ListInvalidNameCode, v2ListInvalidNameMessage))
			continue
		}
		candidate := legacyListCandidate{id: id, path: filepath.Join(s.legacyRoot, name), format: format}
		if current, ok := candidates[id]; !ok || current.format == legacyFormatJSON {
			candidates[id] = candidate
		}
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	listMigration := legacyMigration{codec: s.codec}
	for _, id := range ids {
		candidate := candidates[id]
		file, info, exists, openErr := openVerifiedRegular(candidate.path)
		if openErr != nil || !exists {
			result.Entries = append(result.Entries, newV2ListPlaceholder(s.codec, id, time.Time{}, v2ListOpenFailedCode, v2ListOpenFailedMessage))
			continue
		}
		if info.Size() > s.list.MaxScanBytes-result.ScannedBytes {
			_ = file.Close()
			markV2ListTruncated(&result, s.codec)
			break
		}
		result.ScannedBytes += info.Size()
		loaded, loadErr := listMigration.loadResult(ctx, io.LimitReader(file, info.Size()), legacyLoadOptions{Format: candidate.format, ExpectedSessionID: id})
		closeErr := file.Close()
		if loadErr != nil || closeErr != nil || !loaded.Available || loaded.Conversation == nil {
			report := loaded.Recovery
			if report.Status != RecoveryPlaceholder {
				report = newV2RecoveryReport(s.codec, RecoveryPlaceholder, 0, 1, v2ListOpenFailedCode, v2ListOpenFailedMessage)
			}
			result.Entries = append(result.Entries, newV2ListPlaceholderWithReport(s.codec, id, info.ModTime(), report))
			continue
		}
		conversation := loaded.Conversation
		result.Entries = append(result.Entries, ListEntry{Summary: ConversationSummary{ID: id, Title: conversation.Title, UpdatedAt: conversation.UpdatedAt, MessageCount: len(conversation.Messages)}, Available: true, Recovery: loaded.Recovery})
	}
	sort.SliceStable(result.Entries, func(i, j int) bool {
		if result.Entries[i].Summary.UpdatedAt.Equal(result.Entries[j].Summary.UpdatedAt) {
			return result.Entries[i].Summary.ID < result.Entries[j].Summary.ID
		}
		return result.Entries[i].Summary.UpdatedAt.After(result.Entries[j].Summary.UpdatedAt)
	})
	return result, nil
}

// Transitional test helpers expose only the final v2 location.
func (s *JSONLStore) path(id string) string       { return s.v2Path(id) }
func (s *JSONLStore) legacyPath(id string) string { return s.legacyJSONPath(id) }

func writeJSONLRecord(writer io.Writer, record JSONLRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = writer.Write(append(data, '\n'))
	return err
}

func ctxErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func hasJSONLErrors(items []JSONLDiagnostic) bool {
	for _, item := range items {
		if item.Severity == diagnostics.SeverityError {
			return true
		}
	}
	return false
}

func hasDiagnosticCode(items []JSONLDiagnostic, code string) bool {
	for _, item := range items {
		if item.Code == code {
			return true
		}
	}
	return false
}

func hasDuplicatePrefix(diskMessages []Message, memoryMessages []Message, persisted int) bool {
	limit := persisted
	if len(diskMessages) < limit {
		limit = len(diskMessages)
	}
	if len(memoryMessages) < limit {
		limit = len(memoryMessages)
	}
	for i := 0; i < limit; i++ {
		if messagesEqual(diskMessages[i], memoryMessages[i]) {
			continue
		}
		return false
	}
	for i := persisted; i < len(diskMessages); i++ {
		for _, message := range memoryMessages {
			if messagesEqual(diskMessages[i], message) {
				return true
			}
		}
	}
	return false
}

func messagesEqual(a Message, b Message) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func cloneConversation(conversation *Conversation) Conversation {
	if conversation == nil {
		return Conversation{Messages: []Message{}}
	}
	clone := *conversation
	clone.Messages = cloneMessages(conversation.Messages)
	return clone
}

func cloneMessages(messages []Message) []Message {
	clone := make([]Message, len(messages))
	copy(clone, messages)
	return clone
}
