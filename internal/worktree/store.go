package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const storeOperationLockTimeout = 5 * time.Second

type FileStore struct {
	root         string
	lockRoot     string
	beforeRename func(temporary, target string) error
}

func NewFileStore(controlRoot string) (*FileStore, error) {
	if !platformLockSupported() {
		return nil, NewSafeError(CodePlatformLockMissing, "reliable Store locking is unavailable", ErrPlatformLockUnsupported)
	}
	abs, err := canonicalStoreRoot(controlRoot)
	if err != nil {
		return nil, ErrUnsafePath
	}
	lockRoot := filepath.Join(abs, "record-locks")
	for _, directory := range []string{abs, filepath.Join(abs, "records"), filepath.Join(abs, "tombstones"), lockRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create worktree metadata directory: %w", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("protect worktree metadata directory: %w", err)
		}
		info, err := os.Lstat(directory)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, ErrUnsafePath
		}
	}
	return &FileStore{root: filepath.Clean(abs), lockRoot: lockRoot}, nil
}

func canonicalStoreRoot(controlRoot string) (string, error) {
	abs, err := filepath.Abs(controlRoot)
	if err != nil {
		return "", ErrUnsafePath
	}
	abs = filepath.Clean(abs)
	current := abs
	missing := make([]string, 0, 4)
	for {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", ErrUnsafePath
			}
			real, evalErr := filepath.EvalSymlinks(current)
			if evalErr != nil {
				return "", ErrUnsafePath
			}
			for index := len(missing) - 1; index >= 0; index-- {
				real = filepath.Join(real, missing[index])
			}
			return filepath.Clean(real), nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", ErrUnsafePath
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", ErrUnsafePath
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func (s *FileStore) Root() string { return s.root }

func (s *FileStore) withWorkspaceLock(ctx context.Context, workspaceID string, operation func() error) (returnErr error) {
	if ctx == nil || operation == nil || !ValidWorkspaceID(workspaceID) {
		return ErrInvalidMetadata
	}
	deadline := time.Now().Add(storeOperationLockTimeout)
	lockPath := filepath.Join(s.lockRoot, workspaceID+".lock")
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		held, acquired, err := tryPlatformFileLock(lockPath, false)
		if err != nil {
			if errors.Is(err, ErrPlatformLockUnsupported) {
				return NewSafeError(CodePlatformLockMissing, "reliable Store locking is unavailable", err)
			}
			return NewSafeError(CodeLockFailed, "cannot acquire Store record lock", err)
		}
		if acquired {
			defer func() {
				if err := releasePlatformFileLock(held); err != nil {
					returnErr = errors.Join(returnErr, NewSafeError(CodeLockFailed, "cannot release Store record lock", err))
				}
			}()
			if err := ctx.Err(); err != nil {
				return err
			}
			return operation()
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return NewSafeError(CodeLockTimeout, "timed out waiting for Store record lock", ErrLockTimeout)
		}
		wait := 5 * time.Millisecond
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *FileStore) Create(ctx context.Context, record Record) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if record.Revision != 1 {
		return ErrInvalidMetadata
	}
	encoded, err := EncodeRecord(record)
	if err != nil {
		return err
	}
	return s.withWorkspaceLock(ctx, record.WorkspaceID, func() error {
		target, err := s.recordPath(record.WorkspaceID)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(target); err == nil {
			return ErrAlreadyExists
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect worktree record: %w", err)
		}
		return s.atomicWrite(target, encoded)
	})
}

func (s *FileStore) Load(ctx context.Context, workspaceID string) (Record, error) {
	if err := contextError(ctx); err != nil {
		return Record{}, err
	}
	return s.loadUnlocked(workspaceID)
}

func (s *FileStore) CompareAndSwap(ctx context.Context, next Record, expectedRevision uint64) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	return s.withWorkspaceLock(ctx, next.WorkspaceID, func() error {
		current, err := s.loadUnlocked(next.WorkspaceID)
		if err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return ErrRevisionConflict
		}
		if current.WorkspaceID != next.WorkspaceID || !current.RepositoryIdentity.Equal(next.RepositoryIdentity) || !CanTransition(current.State, next.State) {
			return ErrIdentityMismatch
		}
		next.Revision = expectedRevision + 1
		encoded, err := EncodeRecord(next)
		if err != nil {
			return err
		}
		target, err := s.recordPath(next.WorkspaceID)
		if err != nil {
			return err
		}
		return s.atomicWrite(target, encoded)
	})
}

func (s *FileStore) WriteTombstone(ctx context.Context, tombstone Tombstone) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if tombstone.SchemaVersion == 0 {
		tombstone.SchemaVersion = TombstoneSchemaVersion
	}
	if tombstone.SchemaVersion != TombstoneSchemaVersion || !ValidWorkspaceID(tombstone.WorkspaceID) || tombstone.DeletedAt.IsZero() || tombstone.RecordRevision == 0 {
		return ErrInvalidMetadata
	}
	digest, err := tombstoneDigest(tombstone)
	if err != nil {
		return err
	}
	tombstone.Digest = digest
	encoded, err := json.Marshal(tombstone)
	if err != nil {
		return ErrInvalidMetadata
	}
	encoded = append(encoded, '\n')
	return s.withWorkspaceLock(ctx, tombstone.WorkspaceID, func() error {
		return s.atomicWrite(filepath.Join(s.root, "tombstones", tombstone.WorkspaceID+".json"), encoded)
	})
}

func (s *FileStore) List(ctx context.Context, query ListQuery) ([]Record, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "records"))
	if err != nil {
		return nil, fmt.Errorf("list worktree records: %w", err)
	}
	stateFilter := make(map[State]bool, len(query.States))
	for _, state := range query.States {
		stateFilter[state] = true
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		workspaceID := strings.TrimSuffix(entry.Name(), ".json")
		if !ValidWorkspaceID(workspaceID) {
			continue
		}
		record, loadErr := s.loadUnlocked(workspaceID)
		if loadErr != nil {
			return nil, loadErr
		}
		if len(stateFilter) != 0 && !stateFilter[record.State] {
			continue
		}
		records = append(records, record)
		if query.Limit > 0 && len(records) >= query.Limit {
			break
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].WorkspaceID < records[j].WorkspaceID })
	return records, nil
}

func EncodeRecord(record Record) ([]byte, error) {
	if record.SchemaVersion == 0 {
		record.SchemaVersion = RecordSchemaVersion
	}
	if err := validateRecord(record, false); err != nil {
		return nil, err
	}
	digest, err := recordDigest(record)
	if err != nil {
		return nil, err
	}
	record.IntegrityDigest = digest
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, ErrInvalidMetadata
	}
	if len(encoded) > maxMetadataBytes {
		return nil, ErrMetadataTooLarge
	}
	return append(encoded, '\n'), nil
}

func DecodeRecord(data []byte) (Record, error) {
	if len(data) > maxMetadataBytes {
		return Record{}, ErrMetadataTooLarge
	}
	var record Record
	if err := strictJSON(data, &record); err != nil {
		return Record{}, fmt.Errorf("decode worktree record: %w", ErrInvalidMetadata)
	}
	if record.SchemaVersion != RecordSchemaVersion {
		return Record{}, ErrUnknownSchema
	}
	if err := validateRecord(record, true); err != nil {
		return Record{}, err
	}
	digest, err := recordDigest(record)
	if err != nil {
		return Record{}, err
	}
	if !equalDigest(record.IntegrityDigest, digest) {
		return Record{}, ErrIntegrityMismatch
	}
	return record, nil
}

func validateRecord(record Record, requireDigest bool) error {
	if record.SchemaVersion != RecordSchemaVersion || record.Revision == 0 || !ValidWorkspaceID(record.WorkspaceID) || !ValidWorkspaceID(record.OwnerID) ||
		!record.State.Valid() || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || !filepath.IsAbs(record.Directory) ||
		record.Branch != "xagent/worktree/"+record.WorkspaceID || !validOID(record.BaseOID) || (record.HeadOID != "" && !validOID(record.HeadOID)) {
		return ErrInvalidMetadata
	}
	if record.UpdatedAt.Before(record.CreatedAt) || (!record.ExpiresAt.IsZero() && record.ExpiresAt.Before(record.CreatedAt)) {
		return ErrInvalidMetadata
	}
	// Config-specific limits are enforced before the record reaches Store.
	// Persistence validates against the closed domain hard bounds so a valid
	// explicit configuration above the default is not rejected on encoding.
	if err := ValidateLogicalName(record.LogicalName, Limits{
		MaxNameBytes: hardMaxNameBytes, MaxSegmentBytes: hardMaxSegmentBytes, MaxDepth: hardMaxDepth,
	}); err != nil {
		return ErrInvalidMetadata
	}
	identity := record.RepositoryIdentity
	if !filepath.IsAbs(identity.Root) || filepath.Clean(identity.Root) != identity.Root || !filepath.IsAbs(identity.CommonDir) || filepath.Clean(identity.CommonDir) != identity.CommonDir || !validDigest(identity.Digest) {
		return ErrInvalidMetadata
	}
	expectedDirectory := filepath.Join(identity.Root, ".xagent", "worktrees", "tasks", record.WorkspaceID[:2], record.WorkspaceID)
	if filepath.Clean(record.Directory) != expectedDirectory {
		return ErrInvalidMetadata
	}
	if record.Manifest.Path == "" {
		if record.Manifest.Digest != "" {
			return ErrInvalidMetadata
		}
	} else if !validRelativeMetadataPath(record.Manifest.Path) || !validDigest(record.Manifest.Digest) {
		return ErrInvalidMetadata
	}
	leasePresent := record.Lease.OwnerID != "" || record.Lease.Mode != "" || !record.Lease.AcquiredAt.IsZero() || !record.Lease.Heartbeat.IsZero()
	if leasePresent && (!ValidWorkspaceID(record.Lease.OwnerID) || record.Lease.Mode == "" || len(record.Lease.Mode) > 64 ||
		record.Lease.AcquiredAt.IsZero() || record.Lease.Heartbeat.Before(record.Lease.AcquiredAt)) {
		return ErrInvalidMetadata
	}
	if record.Settlement.State != "" {
		switch record.Settlement.State {
		case SettlementDeleted, SettlementRetained, SettlementPartial, SettlementManualAttention:
		default:
			return ErrInvalidMetadata
		}
	}
	if len(record.Settlement.ReasonCode) > 128 {
		return ErrMetadataTooLarge
	}
	if requireDigest && !validDigest(record.IntegrityDigest) {
		return ErrInvalidMetadata
	}
	if len(record.LogicalName) > maxMetadataPath || len(record.Directory) > maxMetadataPath || len(record.Branch) > maxMetadataPath {
		return ErrMetadataTooLarge
	}
	return nil
}

func recordDigest(record Record) (string, error) {
	record.IntegrityDigest = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", ErrInvalidMetadata
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func tombstoneDigest(tombstone Tombstone) (string, error) {
	tombstone.Digest = ""
	encoded, err := json.Marshal(tombstone)
	if err != nil {
		return "", ErrInvalidMetadata
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (s *FileStore) recordPath(workspaceID string) (string, error) {
	if !ValidWorkspaceID(workspaceID) {
		return "", ErrIdentityMismatch
	}
	return filepath.Join(s.root, "records", workspaceID+".json"), nil
}

func (s *FileStore) loadUnlocked(workspaceID string) (Record, error) {
	target, err := s.recordPath(workspaceID)
	if err != nil {
		return Record{}, err
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrNotFound
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Record{}, ErrInvalidMetadata
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return Record{}, fmt.Errorf("read worktree record: %w", err)
	}
	return DecodeRecord(data)
}

func (s *FileStore) atomicWrite(target string, data []byte) (returnErr error) {
	if len(data) > maxMetadataBytes {
		return ErrMetadataTooLarge
	}
	directory := filepath.Dir(target)
	temporary, err := os.CreateTemp(directory, ".xagent-write-*")
	if err != nil {
		return fmt.Errorf("create worktree metadata temp file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if s.beforeRename != nil {
		if err := s.beforeRename(temporaryPath, target); err != nil {
			return err
		}
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("publish worktree metadata: %w", err)
	}
	dirHandle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open worktree metadata directory: %w", err)
	}
	defer dirHandle.Close()
	if err := dirHandle.Sync(); err != nil {
		return fmt.Errorf("sync worktree metadata directory: %w", err)
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("nil context")
	}
	return ctx.Err()
}
