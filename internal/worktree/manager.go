package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	ErrManagerInvalid        = errors.New("worktree manager configuration is invalid")
	ErrRecoveryRejected      = errors.New("worktree recovery rejected")
	ErrSettlementUnavailable = errors.New("worktree settlement unavailable")
)

const (
	FaultCreatingRecord = "creating_record"
	FaultWorktreeAdd    = "worktree_add"
	FaultInitializing   = "initializing"
	FaultManifest       = "manifest"
	FaultReady          = "ready"
	FaultActiveLease    = "active_lease"
	FaultSettling       = "settling"
	FaultWorktreeRemove = "worktree_remove"
	FaultBranchCAS      = "branch_cas"
	FaultTombstone      = "tombstone"
)

type AcquireRequest struct {
	RepositoryRoot     string
	RepositoryIdentity RepositoryIdentity
	LogicalName        string
	WorkspaceID        string
	OwnerID            string
}

type SettleRequest struct {
	RuntimeStopped bool
}

type ManagerOptions struct {
	ControlRoot          string
	Config               Config
	Store                Store
	Locks                LockManager
	GitReader            GitReader
	GitMutator           GitMutator
	Initializer          Initializer
	Clock                func() time.Time
	Fault                func(string) error
	BeforeManifestWrite  func()
	BeforeManagementRead func()
}

type Manager interface {
	Acquire(context.Context, AcquireRequest) (Lease, error)
	Settle(context.Context, Lease, SettleRequest) (Settlement, error)
	// Resume converges a previously persisted settlement crash checkpoint. It
	// does not recover or authorize Agent execution.
	Resume(context.Context, string) (Settlement, error)
	// Collect safely re-evaluates an expired retained or interrupted settlement.
	// It never authorizes Agent execution and never bypasses deletion protection.
	Collect(context.Context, string) (Settlement, error)
	Release(context.Context, Lease) error
	Snapshot(context.Context, string) (Record, error)
	List(context.Context, ListQuery) ([]Record, error)
	Shutdown(context.Context) error
}

type lifecycleManager struct {
	options ManagerOptions
	control string

	admission sync.RWMutex
	mu        sync.Mutex
	leases    map[string]heldManagerLease
	closed    bool
}

type heldManagerLease struct {
	lease  Lease
	unlock Unlock
}

func NewManager(options ManagerOptions) (Manager, error) {
	if options.Store == nil || options.Locks == nil || options.GitReader == nil || options.ControlRoot == "" || options.Config.Validate() != nil {
		return nil, ErrManagerInvalid
	}
	control, err := canonicalDirectory(options.ControlRoot)
	if err != nil {
		return nil, ErrManagerInvalid
	}
	if err := ensureManagerManifestRoot(control); err != nil {
		return nil, ErrManagerInvalid
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &lifecycleManager{options: options, control: control, leases: make(map[string]heldManagerLease)}, nil
}

func (m *lifecycleManager) Acquire(parent context.Context, request AcquireRequest) (lease Lease, returnErr error) {
	defer func() { returnErr = sanitizeManagerBoundaryError(returnErr) }()
	m.admission.RLock()
	defer m.admission.RUnlock()
	if parent == nil || m.isClosed() {
		return Lease{}, ErrManagerInvalid
	}
	if err := m.validateAcquireRequest(request); err != nil {
		return Lease{}, err
	}
	layout, _ := ResolveManagedLayout(request.RepositoryRoot, request.WorkspaceID)
	if filepath.Clean(layout.Control) != m.control {
		return Lease{}, ErrIdentityMismatch
	}
	ignoreAuthority, err := openExactManagedIgnoreRule(request.RepositoryRoot)
	if err != nil {
		return Lease{}, err
	}
	defer ignoreAuthority.close()
	ignored, err := m.options.GitReader.CheckIgnore(parent, request.RepositoryRoot, layout.Root)
	if err != nil || !ignored {
		return Lease{}, ErrUnsafePath
	}
	workspacePath, err := ValidateManagedPath(layout.Root, layout.WorkspaceRoot, true)
	if err != nil {
		return Lease{}, ErrUnsafePath
	}
	if workspacePath.Exists() {
		return m.recover(parent, request, layout, ignoreAuthority)
	}
	if m.options.GitMutator == nil || m.options.Initializer == nil {
		return Lease{}, ErrManagerInvalid
	}
	ctx := NewLockContext(parent)
	repositoryUnlock, err := m.options.Locks.LockRepository(ctx, request.RepositoryIdentity)
	if err != nil {
		return Lease{}, err
	}
	defer repositoryUnlock()
	if err := m.checkQuota(ctx); err != nil {
		return Lease{}, err
	}
	workspaceUnlock, err := m.options.Locks.LockWorkspace(ctx, request.WorkspaceID)
	if err != nil {
		return Lease{}, err
	}
	defer workspaceUnlock()
	workspaceParent, err := prepareManagedWorkspaceParent(layout)
	if err != nil {
		return Lease{}, err
	}
	defer workspaceParent.close()

	baseOID, err := m.options.GitReader.ResolveHEAD(ctx, request.RepositoryRoot)
	if err != nil || !validOID(baseOID) {
		return Lease{}, errors.Join(ErrManagerInvalid, err)
	}
	now := m.now()
	record := Record{
		SchemaVersion: RecordSchemaVersion, Revision: 1,
		WorkspaceID: request.WorkspaceID, OwnerID: request.OwnerID,
		RepositoryIdentity: request.RepositoryIdentity, LogicalName: request.LogicalName,
		Directory: layout.WorkspaceRoot, Branch: layout.Branch, BaseOID: baseOID, HeadOID: baseOID,
		State: StateCreating, CreatedAt: now, UpdatedAt: now,
	}
	if err := workspaceParent.revalidate(); err != nil {
		return Lease{}, err
	}
	if err := m.revalidateManagedIgnore(ctx, ignoreAuthority, request.RepositoryRoot, layout.Root); err != nil {
		return Lease{}, err
	}
	if err := m.options.Store.Create(ctx, record); err != nil {
		return Lease{}, err
	}
	if err := m.inject(FaultCreatingRecord); err != nil {
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	if err := workspaceParent.revalidate(); err != nil {
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	if err := m.revalidateManagedIgnore(ctx, ignoreAuthority, request.RepositoryRoot, layout.Root); err != nil {
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	// Git CLI cannot consume the open directory/.gitignore handles. These are
	// therefore the final identity/content checks before argv execution, but not
	// an identity-conditional filesystem CAS against a same-user rename attacker.
	if err := m.options.GitMutator.AddWorktree(ctx, AddWorktreeRequest{
		RepositoryRoot: request.RepositoryRoot, Directory: layout.WorkspaceRoot, Branch: layout.Branch, BaseOID: baseOID,
	}); err != nil {
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	if err := m.inject(FaultWorktreeAdd); err != nil {
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	if _, err := ValidateManagedPath(layout.Root, layout.WorkspaceRoot, false); err != nil {
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	if err := m.transition(ctx, &record, StateInitializing, nil); err != nil {
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	if err := m.inject(FaultInitializing); err != nil {
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	manifest, err := m.options.Initializer.Prepare(ctx, InitRequest{
		WorkspaceID: request.WorkspaceID, RepositoryRoot: request.RepositoryRoot,
		WorktreeRoot: layout.WorkspaceRoot, ManagedRoot: layout.Root,
		Config: m.options.Config.Init, Limits: m.options.Config.Limits, Timeout: m.options.Config.Lifecycle.InitTimeout,
		RecordManifest: func(recordCtx context.Context, manifest Manifest) error {
			return m.publishManifest(recordCtx, &record, manifest)
		},
	})
	if err != nil {
		if !errors.Is(err, ErrInitializationRetained) {
			if rollbackErr := m.rollbackFailedAcquire(ctx, &record, manifest, request.OwnerID); rollbackErr == nil {
				return Lease{}, err
			} else {
				err = errors.Join(err, rollbackErr)
			}
		}
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	if record.Manifest.Digest == "" {
		if err := m.publishManifest(ctx, &record, manifest); err != nil {
			return Lease{}, m.classifyFailure(ctx, &record, err)
		}
	}
	if err := m.inject(FaultManifest); err != nil {
		if rollbackErr := m.rollbackFailedAcquire(ctx, &record, manifest, request.OwnerID); rollbackErr == nil {
			return Lease{}, err
		} else {
			err = errors.Join(err, rollbackErr)
		}
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	if err := m.transition(ctx, &record, StateReady, nil); err != nil {
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	if err := m.inject(FaultReady); err != nil {
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	activeUnlock, err := m.options.Locks.AcquireActiveLease(ctx, request.WorkspaceID, request.OwnerID)
	if err != nil {
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	if err := m.inject(FaultActiveLease); err != nil {
		_ = activeUnlock()
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	record.OwnerID = request.OwnerID
	record.Lease = LeaseRecord{OwnerID: request.OwnerID, Mode: "active", AcquiredAt: now, Heartbeat: now}
	if err := m.transition(ctx, &record, StateActive, nil); err != nil {
		_ = activeUnlock()
		return Lease{}, m.classifyFailure(ctx, &record, err)
	}
	lease = Lease{WorkspaceID: request.WorkspaceID, OwnerID: request.OwnerID, Root: layout.WorkspaceRoot, Branch: layout.Branch, BaseOID: baseOID, AcquiredAt: now}
	m.mu.Lock()
	m.leases[leaseKey(lease)] = heldManagerLease{lease: lease, unlock: activeUnlock}
	m.mu.Unlock()
	return lease, nil
}

func (m *lifecycleManager) revalidateManagedIgnore(ctx context.Context, authority *managedIgnoreAuthority, repositoryRoot, managedRoot string) error {
	if err := authority.revalidate(); err != nil {
		return err
	}
	ignored, err := m.options.GitReader.CheckIgnore(ctx, repositoryRoot, managedRoot)
	if err != nil || !ignored {
		return errors.Join(ErrUnsafePath, err)
	}
	return authority.revalidate()
}

func (m *lifecycleManager) rollbackFailedAcquire(ctx context.Context, record *Record, manifest Manifest, ownerID string) error {
	deleteUnlock, err := m.options.Locks.AcquireDeleteLease(ctx, record.WorkspaceID, ownerID)
	if err != nil {
		return err
	}
	defer deleteUnlock()
	layout, err := ResolveManagedLayout(record.RepositoryIdentity.Root, record.WorkspaceID)
	if err != nil || layout.WorkspaceRoot != record.Directory || filepath.Clean(layout.Control) != m.control {
		return ErrIdentityMismatch
	}
	pathIdentity, err := ValidateManagedPath(layout.Root, record.Directory, false)
	if err != nil {
		return err
	}
	if err := validateGitManagementFile(layout.Root, record.Directory, record.RepositoryIdentity.CommonDir, m.options.BeforeManagementRead); err != nil {
		return err
	}
	if err := m.options.Initializer.Rollback(ctx, RollbackRequest{
		RepositoryRoot: record.RepositoryIdentity.Root, ManagedRoot: layout.Root, WorktreeRoot: record.Directory, Manifest: manifest,
	}); err != nil {
		return err
	}
	if err := pathIdentity.Revalidate(); err != nil {
		return err
	}
	if err := m.transition(ctx, record, StateDeleting, nil); err != nil {
		return err
	}
	if err := m.options.GitMutator.RemoveWorktree(ctx, record.RepositoryIdentity.Root, record.Directory); err != nil {
		return err
	}
	branchRef := "refs/heads/" + record.Branch
	if err := m.options.GitMutator.DeleteRefCAS(ctx, record.RepositoryIdentity.Root, branchRef, record.BaseOID); err != nil {
		return err
	}
	deletedAt := m.now()
	if err := m.options.Store.WriteTombstone(ctx, Tombstone{
		SchemaVersion: TombstoneSchemaVersion, WorkspaceID: record.WorkspaceID, DeletedAt: deletedAt, RecordRevision: record.Revision,
	}); err != nil {
		return err
	}
	return m.transition(ctx, record, StateDeleted, func(next *Record) {
		next.Settlement = SettlementRecord{State: SettlementDeleted, ReasonCode: "initialization_rolled_back", CompletedAt: deletedAt}
	})
}

func (m *lifecycleManager) validateAcquireRequest(request AcquireRequest) error {
	if !ValidWorkspaceID(request.WorkspaceID) || !ValidWorkspaceID(request.OwnerID) ||
		ValidateLogicalName(request.LogicalName, m.options.Config.Limits) != nil || !filepath.IsAbs(request.RepositoryRoot) {
		return ErrManagerInvalid
	}
	current, err := NewRepositoryIdentity(request.RepositoryRoot, request.RepositoryIdentity.CommonDir)
	if err != nil || !current.Equal(request.RepositoryIdentity) || !sameRepositoryDirectoryObject(current.Root, request.RepositoryIdentity.Root) {
		return ErrIdentityMismatch
	}
	return nil
}

func (m *lifecycleManager) checkQuota(ctx context.Context) error {
	if limit := m.options.Config.Limits.MaxActive; limit > 0 {
		records, err := m.options.Store.List(ctx, ListQuery{States: []State{StateCreating, StateInitializing, StateReady, StateActive, StateSettling, StateDeleting}, Limit: limit + 1})
		if err != nil || len(records) >= limit {
			return errors.Join(ErrManagerInvalid, err)
		}
	}
	return nil
}

func (m *lifecycleManager) publishManifest(ctx context.Context, record *Record, manifest Manifest) error {
	encoded, err := EncodeManifest(manifest)
	if err != nil {
		return err
	}
	decoded, err := DecodeManifest(encoded)
	if err != nil {
		return err
	}
	nonce, err := GenerateWorkspaceID(nil)
	if err != nil {
		return err
	}
	relative := filepath.ToSlash(filepath.Join("manifests", record.WorkspaceID+"-"+decoded.IntegrityDigest+"-"+nonce+".json"))
	if err := atomicManagerWrite(m.control, relative, encoded, m.options.BeforeManifestWrite); err != nil {
		return err
	}
	previous := record.Manifest
	if err := m.transition(ctx, record, record.State, func(next *Record) {
		next.Manifest = ManifestRef{Path: relative, Digest: decoded.IntegrityDigest}
	}); err != nil {
		return err
	}
	if previous.Path != "" && previous.Path != relative {
		_ = m.removeSupersededManifest(record.WorkspaceID, previous)
	}
	return nil
}

func (m *lifecycleManager) removeSupersededManifest(workspaceID string, ref ManifestRef) error {
	path := filepath.ToSlash(ref.Path)
	base := filepath.Base(path)
	if filepath.Dir(path) != "manifests" || !strings.HasPrefix(base, workspaceID+"-") || !strings.HasSuffix(base, ".json") || !validDigest(ref.Digest) {
		return ErrInvalidMetadata
	}
	// Supported platforms do not expose identity-conditional unlink. Even an
	// fd-relative no-follow check leaves a rename-swap window before unlinkat,
	// so superseded immutable checkpoints are conservatively retained.
	return nil
}

func ensureManagerManifestRoot(control string) error {
	root, err := newInitializerRoot(control)
	if err != nil {
		return ErrUnsafePath
	}
	defer root.Close()
	info, err := root.Stat("manifests")
	if errors.Is(err, os.ErrNotExist) {
		if err := root.Mkdir("manifests", 0o700); err != nil {
			return err
		}
		info, err = root.Stat("manifests")
	}
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return ErrUnsafePath
	}
	return nil
}

func atomicManagerWrite(control, relative string, data []byte, beforeWrite func()) (returnErr error) {
	if len(data) > maxMetadataBytes {
		return ErrMetadataTooLarge
	}
	if !validRelativeMetadataPath(relative) || filepath.ToSlash(filepath.Dir(relative)) != "manifests" {
		return ErrUnsafePath
	}
	directory := filepath.Join(control, "manifests")
	before, err := ValidateManagedPath(control, directory, false)
	if err != nil || !before.Exists() {
		return ErrUnsafePath
	}
	if beforeWrite != nil {
		beforeWrite()
	}
	if err := before.Revalidate(); err != nil {
		return ErrUnsafePath
	}
	root, err := newInitializerRoot(control)
	if err != nil {
		return ErrUnsafePath
	}
	defer root.Close()
	parentIdentity, err := root.Identity("manifests")
	if err != nil {
		return ErrUnsafePath
	}
	file, err := root.CreateFile(relative, 0o600)
	if err != nil {
		return err
	}
	created := true
	defer func() {
		_ = file.Close()
		if returnErr != nil && created {
			_ = root.Remove(relative, false)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	afterIdentity, err := root.Identity("manifests")
	if err != nil || afterIdentity != parentIdentity || before.Revalidate() != nil {
		return ErrUnsafePath
	}
	created = false
	return nil
}

func (m *lifecycleManager) transition(ctx context.Context, record *Record, state State, mutate func(*Record)) error {
	next := record.Clone()
	next.State = state
	next.UpdatedAt = m.now()
	if mutate != nil {
		mutate(&next)
	}
	if err := m.options.Store.CompareAndSwap(ctx, next, record.Revision); err != nil {
		return err
	}
	next.Revision = record.Revision + 1
	*record = next
	return nil
}

func (m *lifecycleManager) classifyFailure(ctx context.Context, record *Record, cause error) error {
	state := StatePartial
	settlement := SettlementPartial
	if errors.Is(cause, ErrIdentityMismatch) || errors.Is(cause, ErrUnsafePath) || errors.Is(cause, ErrIntegrityMismatch) || errors.Is(cause, ErrInvalidMetadata) {
		state = StateManualAttention
		settlement = SettlementManualAttention
	}
	_ = m.transition(context.WithoutCancel(ctx), record, state, func(next *Record) {
		next.Settlement = SettlementRecord{State: settlement, ReasonCode: "acquire_failed", CompletedAt: m.now()}
	})
	return cause
}

// sanitizeManagerBoundaryError keeps dependency, path, Git, and fault details
// available through Unwrap while exposing only a stable bounded message at the
// public lifecycle boundary.
func sanitizeManagerBoundaryError(err error) error {
	if err == nil {
		return nil
	}
	code, message, recoverable := classifyManagerBoundaryError(err)
	return &SafeError{Code: code, Message: message, Recoverable: recoverable, cause: err}
}

func classifyManagerBoundaryError(err error) (ErrorCode, string, bool) {
	var safe *SafeError
	if errors.As(err, &safe) {
		if message, ok := canonicalManagerSafeMessage(safe.Code); ok {
			return safe.Code, message, safe.Recoverable
		}
	}
	var gitError *GitCommandError
	if errors.As(err, &gitError) {
		return CodeGitCommandFailed, "git command failed", false
	}
	switch {
	case errors.Is(err, ErrManagerInvalid):
		return CodeInvalidConfig, "worktree manager configuration is invalid", false
	case errors.Is(err, ErrRecoveryRejected):
		return CodeLifecycleFailed, "worktree recovery rejected", false
	case errors.Is(err, ErrSettlementUnavailable):
		return CodeLifecycleFailed, "worktree settlement unavailable", true
	case errors.Is(err, ErrInitializationRetained):
		return CodeLifecycleFailed, "worktree initialization artifacts retained", false
	case errors.Is(err, ErrInitializationFailed):
		return CodeLifecycleFailed, "worktree initialization failed", false
	case errors.Is(err, ErrUnsupportedPlatform):
		return CodeUnsupportedPlatform, "worktree initialization unsupported on this platform", false
	case errors.Is(err, ErrUnsafePath):
		return CodeUnsafePath, "unsafe managed path", false
	case errors.Is(err, ErrIdentityMismatch):
		return CodeIdentityMismatch, "worktree identity mismatch", false
	case errors.Is(err, ErrUnknownSchema):
		return CodeUnknownSchema, "unknown worktree metadata schema", false
	case errors.Is(err, ErrIntegrityMismatch):
		return CodeIntegrityMismatch, "worktree metadata integrity mismatch", false
	case errors.Is(err, ErrInvalidMetadata), errors.Is(err, ErrMetadataTooLarge):
		return CodeInvalidMetadata, "invalid worktree metadata", false
	case errors.Is(err, ErrRevisionConflict), errors.Is(err, ErrAlreadyExists):
		return CodeRevisionConflict, "worktree record changed", true
	case errors.Is(err, ErrForbiddenGitCommand):
		return CodeGitCommandRejected, "git command rejected", false
	case errors.Is(err, ErrGitOutputLimit):
		return CodeGitOutputLimit, "git output exceeded limit", false
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return CodeLifecycleFailed, "worktree lifecycle operation timed out", true
	default:
		return CodeLifecycleFailed, "worktree lifecycle operation failed", false
	}
}

func canonicalManagerSafeMessage(code ErrorCode) (string, bool) {
	switch code {
	case CodeInvalidConfig:
		return "invalid worktree configuration", true
	case CodeInvalidLogicalName:
		return "invalid worktree logical name", true
	case CodeUnsafePath:
		return "unsafe managed path", true
	case CodeIdentityMismatch:
		return "worktree identity mismatch", true
	case CodeInvalidMetadata:
		return "invalid worktree metadata", true
	case CodeUnknownSchema:
		return "unknown worktree metadata schema", true
	case CodeIntegrityMismatch:
		return "worktree metadata integrity mismatch", true
	case CodeRevisionConflict:
		return "worktree record changed", true
	case CodeGitCommandRejected:
		return "git command rejected", true
	case CodeGitCommandFailed:
		return "git command failed", true
	case CodeGitOutputLimit:
		return "git output exceeded limit", true
	case CodeUnsupportedPlatform:
		return "worktree initialization unsupported on this platform", true
	case CodeLifecycleFailed:
		return "worktree lifecycle operation failed", true
	case CodeLockOrder:
		return "worktree lock order is invalid", true
	case CodeLockTimeout:
		return "timed out waiting for worktree lock", true
	case CodeLockFailed:
		return "worktree lock operation failed", true
	case CodePlatformLockMissing:
		return "reliable platform worktree locking is unavailable", true
	default:
		return "", false
	}
}

func (m *lifecycleManager) recover(parent context.Context, request AcquireRequest, layout ManagedLayout, ignoreAuthority *managedIgnoreAuthority) (Lease, error) {
	ctx := NewLockContext(parent)
	if timeout := m.options.Config.Lifecycle.RecoveryTimeout; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	repositoryUnlock, err := m.options.Locks.LockRepository(ctx, request.RepositoryIdentity)
	if err != nil {
		return Lease{}, err
	}
	defer repositoryUnlock()
	workspaceUnlock, err := m.options.Locks.LockWorkspace(ctx, request.WorkspaceID)
	if err != nil {
		return Lease{}, err
	}
	defer workspaceUnlock()
	if err := m.revalidateManagedIgnore(ctx, ignoreAuthority, request.RepositoryRoot, layout.Root); err != nil {
		return Lease{}, errors.Join(ErrRecoveryRejected, err)
	}

	record, err := m.options.Store.Load(ctx, request.WorkspaceID)
	if err != nil || !record.RepositoryIdentity.Equal(request.RepositoryIdentity) || record.Directory != layout.WorkspaceRoot ||
		record.Branch != layout.Branch || record.LogicalName != request.LogicalName ||
		(record.State != StateReady && record.State != StateActive && record.State != StateRetained) {
		return Lease{}, errors.Join(ErrRecoveryRejected, fmt.Errorf("record: %w", err))
	}
	if _, err := ValidateManagedPath(layout.Root, layout.WorkspaceRoot, false); err != nil {
		return Lease{}, errors.Join(ErrRecoveryRejected, fmt.Errorf("path: %w", err))
	}
	manifest, err := m.loadManifest(record)
	if err != nil {
		return Lease{}, errors.Join(ErrRecoveryRejected, fmt.Errorf("manifest: %w", err))
	}
	worktrees, err := m.options.GitReader.WorktreeList(ctx, request.RepositoryRoot)
	if err != nil || !matchingRegisteredWorktree(worktrees, record) {
		return Lease{}, errors.Join(ErrRecoveryRejected, fmt.Errorf("registration: %w", err))
	}
	head, err := m.options.GitReader.ResolveHEAD(ctx, record.Directory)
	if err != nil || head != record.HeadOID {
		return Lease{}, errors.Join(ErrRecoveryRejected, fmt.Errorf("head: %w", err))
	}
	symbolic, err := m.options.GitReader.SymbolicRef(ctx, record.Directory)
	if err != nil || symbolic != "refs/heads/"+record.Branch {
		return Lease{}, errors.Join(ErrRecoveryRejected, fmt.Errorf("symbolic ref: %w", err))
	}
	refs, err := m.options.GitReader.ForEachRef(ctx, request.RepositoryRoot, "refs/heads/"+record.Branch)
	if err != nil || len(refs) != 1 || refs[0].Name != "refs/heads/"+record.Branch || refs[0].ObjectName != record.HeadOID {
		return Lease{}, errors.Join(ErrRecoveryRejected, fmt.Errorf("branch ref: %w", err))
	}
	if err := validateGitManagementFile(layout.Root, record.Directory, record.RepositoryIdentity.CommonDir, m.options.BeforeManagementRead); err != nil {
		return Lease{}, errors.Join(ErrRecoveryRejected, fmt.Errorf("git management: %w", err))
	}
	if m.options.Initializer == nil || m.options.Initializer.Verify(ctx, VerifyRequest{
		RepositoryRoot: request.RepositoryRoot, ManagedRoot: layout.Root, WorktreeRoot: record.Directory, Manifest: manifest,
	}) != nil {
		return Lease{}, ErrRecoveryRejected
	}
	activeUnlock, err := m.options.Locks.AcquireActiveLease(ctx, record.WorkspaceID, request.OwnerID)
	if err != nil {
		return Lease{}, err
	}
	now := m.now()
	if err := m.revalidateManagedIgnore(ctx, ignoreAuthority, request.RepositoryRoot, layout.Root); err != nil {
		_ = activeUnlock()
		return Lease{}, errors.Join(ErrRecoveryRejected, err)
	}
	record.OwnerID = request.OwnerID
	record.Lease = LeaseRecord{OwnerID: request.OwnerID, Mode: "active", AcquiredAt: now, Heartbeat: now}
	if err := m.transition(ctx, &record, StateActive, nil); err != nil {
		_ = activeUnlock()
		return Lease{}, err
	}
	lease := Lease{WorkspaceID: record.WorkspaceID, OwnerID: request.OwnerID, Root: record.Directory, Branch: record.Branch, BaseOID: record.BaseOID, AcquiredAt: now}
	m.mu.Lock()
	m.leases[leaseKey(lease)] = heldManagerLease{lease: lease, unlock: activeUnlock}
	m.mu.Unlock()
	return lease, nil
}

func (m *lifecycleManager) loadManifest(record Record) (Manifest, error) {
	path := filepath.ToSlash(record.Manifest.Path)
	base := filepath.Base(path)
	if filepath.Dir(path) != "manifests" || !strings.HasPrefix(base, record.WorkspaceID+"-") || !strings.HasSuffix(base, ".json") || !validDigest(record.Manifest.Digest) {
		return Manifest{}, ErrInvalidMetadata
	}
	root, err := newInitializerRoot(m.control)
	if err != nil {
		return Manifest{}, ErrUnsafePath
	}
	defer root.Close()
	file, info, err := root.OpenRead(record.Manifest.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxMetadataBytes {
		return Manifest{}, ErrInvalidMetadata
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
	if err != nil {
		return Manifest{}, err
	}
	if len(data) > maxMetadataBytes {
		return Manifest{}, ErrMetadataTooLarge
	}
	manifest, err := DecodeManifest(data)
	if err != nil || manifest.WorkspaceID != record.WorkspaceID || manifest.IntegrityDigest != record.Manifest.Digest {
		return Manifest{}, errors.Join(ErrIntegrityMismatch, err)
	}
	return manifest, nil
}

func matchingRegisteredWorktree(worktrees []WorktreeInfo, record Record) bool {
	wantedBranch := "refs/heads/" + record.Branch
	matches := 0
	for _, worktree := range worktrees {
		if worktree.Branch == wantedBranch && worktree.Path != record.Directory {
			return false
		}
		if worktree.Path == record.Directory {
			if worktree.HEAD != record.HeadOID || worktree.Branch != wantedBranch || worktree.Bare || worktree.Detached || worktree.Prunable {
				return false
			}
			matches++
		}
	}
	return matches == 1
}

func validateGitManagementFile(managedRoot, worktreeRoot, commonDir string, beforeRead func()) error {
	pathIdentity, err := ValidateManagedPath(managedRoot, worktreeRoot, false)
	if err != nil {
		return ErrIdentityMismatch
	}
	data, err := readGitManagementFile(worktreeRoot, beforeRead)
	if err != nil || len(data) == 0 || len(data) > maxMetadataPath || pathIdentity.Revalidate() != nil {
		return ErrIdentityMismatch
	}
	line := string(data)
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	if strings.ContainsAny(line, "\r\n") {
		return ErrIdentityMismatch
	}
	const prefix = "gitdir: "
	if len(line) <= len(prefix) || line[:len(prefix)] != prefix {
		return ErrIdentityMismatch
	}
	gitDir := filepath.Clean(line[len(prefix):])
	if !filepath.IsAbs(gitDir) {
		return ErrIdentityMismatch
	}
	canonicalCommon, err := canonicalDirectory(commonDir)
	if err != nil {
		return ErrIdentityMismatch
	}
	canonicalGitDir, err := canonicalDirectory(gitDir)
	if err != nil || !sameOrDescendant(canonicalCommon, canonicalGitDir) {
		return ErrIdentityMismatch
	}
	relative, err := filepath.Rel(canonicalCommon, canonicalGitDir)
	if err != nil || relative == "." || filepath.Dir(relative) != "worktrees" {
		return ErrIdentityMismatch
	}
	return nil
}

func (m *lifecycleManager) Settle(parent context.Context, lease Lease, request SettleRequest) (settlement Settlement, returnErr error) {
	defer func() { returnErr = sanitizeManagerBoundaryError(returnErr) }()
	if parent == nil || !validManagerLease(lease) {
		return Settlement{}, ErrManagerInvalid
	}
	ctx := parent
	if timeout := m.options.Config.Lifecycle.SettleTimeout; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, timeout)
		defer cancel()
	}
	record, err := m.options.Store.Load(ctx, lease.WorkspaceID)
	if err != nil {
		return Settlement{}, err
	}
	if !recordMatchesLease(record, lease) {
		return Settlement{}, ErrIdentityMismatch
	}
	if terminalSettlement(record) {
		return settlementFromRecord(record), nil
	}
	if !request.RuntimeStopped {
		return Settlement{State: SettlementRetained, ReasonCode: "runtime_active"}, ErrSettlementUnavailable
	}
	held, err := m.takeHeldLease(lease)
	if err != nil {
		return Settlement{}, err
	}
	if err := m.transition(ctx, &record, StateSettling, nil); err != nil {
		m.restoreHeldLease(held)
		return Settlement{}, err
	}
	if err := m.inject(FaultSettling); err != nil {
		_ = held.unlock()
		return Settlement{}, err
	}
	if err := held.unlock(); err != nil {
		return m.retain(ctx, &record, changeInspection{dirty: true, reason: "lease_release_failed", head: record.HeadOID})
	}
	inspection := m.inspectProtectedChanges(ctx, record)
	if inspection.dirty || inspection.unpushed || inspection.reason != "" {
		return m.retain(ctx, &record, inspection)
	}
	if inspection.head != record.HeadOID {
		if err := m.transition(ctx, &record, StateSettling, func(next *Record) { next.HeadOID = inspection.head }); err != nil {
			return Settlement{}, err
		}
	}
	return m.safeDelete(ctx, &record, lease)
}

type changeInspection struct {
	dirty    bool
	unpushed bool
	reason   string
	head     string
}

func (m *lifecycleManager) inspectProtectedChanges(ctx context.Context, record Record) changeInspection {
	result := changeInspection{head: record.HeadOID}
	layout, err := ResolveManagedLayout(record.RepositoryIdentity.Root, record.WorkspaceID)
	if err != nil || layout.WorkspaceRoot != record.Directory || filepath.Clean(layout.Control) != m.control {
		result.dirty, result.reason = true, "identity_unknown"
		return result
	}
	manifest, err := m.loadManifest(record)
	if err != nil {
		result.dirty, result.reason = true, "manifest_unknown"
		return result
	}
	if m.options.Initializer == nil || m.options.Initializer.Verify(ctx, VerifyRequest{
		RepositoryRoot: record.RepositoryIdentity.Root, ManagedRoot: layout.Root,
		WorktreeRoot: record.Directory, Manifest: manifest,
	}) != nil {
		result.dirty, result.reason = true, "initialization_changed"
		return result
	}
	status, err := m.options.GitReader.Status(ctx, record.Directory)
	if err != nil {
		result.dirty, result.reason = true, "inspection_unknown"
		return result
	}
	coveredIgnored := make(map[string]bool, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		if entry.State == ManifestEntryCreated {
			coveredIgnored[entry.Path] = true
		}
	}
	for _, entry := range status.Entries {
		if entry.Code == "!" && coveredIgnored[filepath.ToSlash(entry.Path)] {
			continue
		}
		result.dirty, result.reason = true, "protected_changes"
		return result
	}
	if status.Dirty && len(status.Entries) == 0 {
		result.dirty, result.reason = true, "protected_changes"
		return result
	}
	head, err := m.options.GitReader.ResolveHEAD(ctx, record.Directory)
	if err != nil || !validOID(head) {
		result.reason = "inspection_unknown"
		return result
	}
	result.head = head
	branchRef := "refs/heads/" + record.Branch
	refs, err := m.options.GitReader.ForEachRef(ctx, record.RepositoryIdentity.Root, branchRef)
	if err != nil || len(refs) != 1 || refs[0].Name != branchRef || refs[0].ObjectName != head {
		result.reason = "inspection_unknown"
		return result
	}
	if head == record.BaseOID {
		return result
	}
	upstream := refs[0].Upstream
	if !validRemoteTrackingRef(upstream) {
		result.unpushed, result.reason = true, "unpushed_commits"
		return result
	}
	upstreamRefs, err := m.options.GitReader.ForEachRef(ctx, record.RepositoryIdentity.Root, upstream)
	if err != nil || len(upstreamRefs) != 1 || upstreamRefs[0].Name != upstream {
		result.unpushed, result.reason = true, "unpushed_commits"
		return result
	}
	reachable, err := m.options.GitReader.MergeBaseIsAncestor(ctx, record.RepositoryIdentity.Root, head, upstreamRefs[0].ObjectName)
	if err != nil || !reachable {
		result.unpushed, result.reason = true, "unpushed_commits"
	}
	return result
}

func validRemoteTrackingRef(ref string) bool {
	const prefix = "refs/remotes/"
	if !strings.HasPrefix(ref, prefix) || !validRef(ref) || strings.HasSuffix(ref, "/HEAD") {
		return false
	}
	remainder := strings.TrimPrefix(ref, prefix)
	return strings.Contains(remainder, "/") && !strings.HasPrefix(remainder, "/")
}

func (m *lifecycleManager) retain(ctx context.Context, record *Record, inspection changeInspection) (Settlement, error) {
	reason := inspection.reason
	if reason == "" {
		reason = "protected_changes"
	}
	completed := record.Settlement.CompletedAt
	if completed.IsZero() {
		completed = m.now()
	}
	err := m.transition(context.WithoutCancel(ctx), record, StateRetained, func(next *Record) {
		if validOID(inspection.head) {
			next.HeadOID = inspection.head
		}
		next.Lease = LeaseRecord{}
		next.Settlement = SettlementRecord{State: SettlementRetained, Dirty: inspection.dirty, Unpushed: inspection.unpushed, ReasonCode: reason, CompletedAt: completed}
		if ttl := m.options.Config.Lifecycle.RetentionTTL; ttl > 0 {
			next.ExpiresAt = completed.Add(ttl)
		}
	})
	settlement := Settlement{State: SettlementRetained, Dirty: inspection.dirty, Unpushed: inspection.unpushed, ReasonCode: reason}
	return settlement, err
}

func (m *lifecycleManager) safeDelete(parent context.Context, record *Record, lease Lease) (Settlement, error) {
	if m.options.GitMutator == nil || m.options.Initializer == nil {
		return m.retain(parent, record, changeInspection{reason: "delete_unavailable", head: record.HeadOID})
	}
	layout, err := ResolveManagedLayout(record.RepositoryIdentity.Root, record.WorkspaceID)
	if err != nil || layout.WorkspaceRoot != record.Directory || filepath.Clean(layout.Control) != m.control {
		return m.manual(parent, record, "identity_mismatch")
	}
	ctx := NewLockContext(parent)
	repositoryUnlock, err := m.options.Locks.LockRepository(ctx, record.RepositoryIdentity)
	if err != nil {
		return m.retain(parent, record, changeInspection{reason: "delete_lock_failed", head: record.HeadOID})
	}
	defer repositoryUnlock()
	workspaceUnlock, err := m.options.Locks.LockWorkspace(ctx, record.WorkspaceID)
	if err != nil {
		return m.retain(parent, record, changeInspection{reason: "delete_lock_failed", head: record.HeadOID})
	}
	defer workspaceUnlock()
	deleteUnlock, err := m.options.Locks.AcquireDeleteLease(ctx, record.WorkspaceID, lease.OwnerID)
	if err != nil {
		return m.retain(parent, record, changeInspection{reason: "delete_lease_failed", head: record.HeadOID})
	}
	defer deleteUnlock()
	return m.safeDeleteLocked(ctx, record, lease)
}

func (m *lifecycleManager) safeDeleteLocked(ctx context.Context, record *Record, lease Lease) (Settlement, error) {
	layout, err := ResolveManagedLayout(record.RepositoryIdentity.Root, record.WorkspaceID)
	if err != nil || layout.WorkspaceRoot != record.Directory || filepath.Clean(layout.Control) != m.control {
		return m.manual(ctx, record, "identity_mismatch")
	}
	pathIdentity, err := ValidateManagedPath(layout.Root, record.Directory, false)
	if err != nil {
		return m.manual(ctx, record, "directory_identity_mismatch")
	}
	current, err := m.options.Store.Load(ctx, record.WorkspaceID)
	if err != nil || current.Revision != record.Revision || current.State != StateSettling || !recordMatchesLease(current, lease) {
		return m.manual(ctx, record, "record_changed")
	}
	*record = current
	inspection := m.inspectProtectedChanges(ctx, current)
	if inspection.dirty || inspection.unpushed || inspection.reason != "" {
		return m.retain(ctx, record, inspection)
	}
	if inspection.head != record.HeadOID {
		if err := m.transition(ctx, record, StateSettling, func(next *Record) { next.HeadOID = inspection.head }); err != nil {
			return Settlement{}, err
		}
	}
	worktrees, err := m.options.GitReader.WorktreeList(ctx, record.RepositoryIdentity.Root)
	if err != nil || !matchingRegisteredWorktree(worktrees, *record) {
		return m.retain(ctx, record, changeInspection{reason: "worktree_in_use", head: record.HeadOID})
	}
	if err := validateGitManagementFile(layout.Root, record.Directory, record.RepositoryIdentity.CommonDir, m.options.BeforeManagementRead); err != nil {
		return m.manual(ctx, record, "git_management_mismatch")
	}
	branchRef := "refs/heads/" + record.Branch
	refs, err := m.options.GitReader.ForEachRef(ctx, record.RepositoryIdentity.Root, branchRef)
	if err != nil || len(refs) != 1 || refs[0].Name != branchRef || refs[0].ObjectName != record.HeadOID {
		return m.manual(ctx, record, "branch_moved")
	}
	manifest, err := m.loadManifest(*record)
	if err != nil {
		return m.manual(ctx, record, "manifest_mismatch")
	}
	if err := m.options.Initializer.Rollback(ctx, RollbackRequest{
		RepositoryRoot: record.RepositoryIdentity.Root, ManagedRoot: layout.Root, WorktreeRoot: record.Directory, Manifest: manifest,
	}); err != nil {
		return m.retain(ctx, record, changeInspection{dirty: true, reason: "initialization_changed", head: record.HeadOID})
	}
	if err := pathIdentity.Revalidate(); err != nil {
		return m.manual(ctx, record, "directory_identity_mismatch")
	}
	if err := m.transition(ctx, record, StateDeleting, nil); err != nil {
		return Settlement{}, err
	}
	if err := m.options.GitMutator.RemoveWorktree(ctx, record.RepositoryIdentity.Root, record.Directory); err != nil {
		return m.partial(ctx, record, "worktree_remove_failed", err)
	}
	if err := m.inject(FaultWorktreeRemove); err != nil {
		return m.partial(ctx, record, "worktree_remove_interrupted", err)
	}
	if err := m.options.GitMutator.DeleteRefCAS(ctx, record.RepositoryIdentity.Root, branchRef, record.HeadOID); err != nil {
		return m.partial(ctx, record, "branch_cas_failed", err)
	}
	if err := m.inject(FaultBranchCAS); err != nil {
		return m.partial(ctx, record, "branch_cas_interrupted", err)
	}
	deletedAt := m.now()
	if err := m.options.Store.WriteTombstone(ctx, Tombstone{
		SchemaVersion: TombstoneSchemaVersion, WorkspaceID: record.WorkspaceID, DeletedAt: deletedAt, RecordRevision: record.Revision,
	}); err != nil {
		return m.partial(ctx, record, "tombstone_failed", err)
	}
	if err := m.inject(FaultTombstone); err != nil {
		return m.partial(ctx, record, "tombstone_interrupted", err)
	}
	err = m.transition(ctx, record, StateDeleted, func(next *Record) {
		next.Lease = LeaseRecord{}
		next.Settlement = SettlementRecord{State: SettlementDeleted, ReasonCode: "clean", CompletedAt: deletedAt}
	})
	return Settlement{State: SettlementDeleted, ReasonCode: "clean"}, err
}

// Resume completes only persisted settlement checkpoints whose deletion
// authority can be reconstructed from the validated record and an exclusive OS
// delete lease. It never recreates an active Agent lease.
func (m *lifecycleManager) Resume(parent context.Context, workspaceID string) (settlement Settlement, returnErr error) {
	defer func() { returnErr = sanitizeManagerBoundaryError(returnErr) }()
	m.admission.RLock()
	defer m.admission.RUnlock()
	if parent == nil || m.isClosed() || !ValidWorkspaceID(workspaceID) {
		return Settlement{}, ErrManagerInvalid
	}
	record, err := m.options.Store.Load(parent, workspaceID)
	if err != nil {
		return Settlement{}, err
	}
	return m.resumeLoaded(parent, record, false)
}

func (m *lifecycleManager) resumeLoaded(parent context.Context, record Record, requireExpired bool) (Settlement, error) {
	if record.State == StateRetained || record.State == StateDeleted || record.State == StateManualAttention {
		return settlementFromRecord(record), nil
	}
	if record.State != StateSettling && (record.State != StatePartial || !resumableDeleteReason(record.Settlement.ReasonCode)) {
		return Settlement{}, ErrSettlementUnavailable
	}
	if m.options.GitMutator == nil || m.options.Initializer == nil {
		return Settlement{}, ErrSettlementUnavailable
	}
	layout, err := m.validateResumeRecord(record)
	if err != nil {
		return Settlement{}, err
	}
	ctx := NewLockContext(parent)
	timeout := m.options.Config.Lifecycle.SettleTimeout
	if requireExpired {
		timeout = m.options.Config.Lifecycle.JanitorTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	repositoryUnlock, err := m.options.Locks.LockRepository(ctx, record.RepositoryIdentity)
	if err != nil {
		return Settlement{}, err
	}
	defer repositoryUnlock()
	workspaceUnlock, err := m.options.Locks.LockWorkspace(ctx, record.WorkspaceID)
	if err != nil {
		return Settlement{}, err
	}
	defer workspaceUnlock()
	if !ValidWorkspaceID(record.OwnerID) {
		return Settlement{}, ErrIdentityMismatch
	}
	deleteUnlock, err := m.options.Locks.AcquireDeleteLease(ctx, record.WorkspaceID, record.OwnerID)
	if err != nil {
		return Settlement{}, err
	}
	defer deleteUnlock()
	current, err := m.options.Store.Load(ctx, record.WorkspaceID)
	if err != nil {
		return Settlement{}, err
	}
	if current.Revision != record.Revision || current.State != record.State || !current.RepositoryIdentity.Equal(record.RepositoryIdentity) {
		if current.State == StateRetained || current.State == StateDeleted || current.State == StateManualAttention {
			return settlementFromRecord(current), nil
		}
		return Settlement{}, ErrRevisionConflict
	}
	if requireExpired && !recordExpired(current, m.now(), m.options.Config.Lifecycle.RetentionTTL) {
		return Settlement{}, ErrSettlementUnavailable
	}
	if current.State == StateSettling {
		lease := Lease{
			WorkspaceID: current.WorkspaceID, OwnerID: current.OwnerID, Root: current.Directory,
			Branch: current.Branch, BaseOID: current.BaseOID, AcquiredAt: current.Lease.AcquiredAt,
		}
		if !validManagerLease(lease) || !recordMatchesLease(current, lease) || current.Lease.OwnerID != current.OwnerID || current.Lease.Mode != "active" {
			return m.manual(ctx, &current, "lease_identity_mismatch")
		}
		return m.safeDeleteLocked(ctx, &current, lease)
	}
	return m.resumeDeletePartialLocked(ctx, &current, layout)
}

func (m *lifecycleManager) Collect(parent context.Context, workspaceID string) (settlement Settlement, returnErr error) {
	defer func() { returnErr = sanitizeManagerBoundaryError(returnErr) }()
	m.admission.RLock()
	defer m.admission.RUnlock()
	if parent == nil || m.isClosed() || !ValidWorkspaceID(workspaceID) || m.options.Config.Lifecycle.RetentionTTL <= 0 ||
		m.options.GitMutator == nil || m.options.Initializer == nil {
		return Settlement{}, ErrManagerInvalid
	}
	record, err := m.options.Store.Load(parent, workspaceID)
	if err != nil {
		return Settlement{}, err
	}
	if !collectibleState(record) || !recordExpired(record, m.now(), m.options.Config.Lifecycle.RetentionTTL) {
		return Settlement{}, ErrSettlementUnavailable
	}
	if record.State == StateSettling || record.State == StatePartial {
		return m.resumeLoaded(parent, record, true)
	}
	layout, err := m.validateResumeRecord(record)
	if err != nil {
		return Settlement{}, err
	}
	ctx := NewLockContext(parent)
	if timeout := m.options.Config.Lifecycle.JanitorTimeout; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	repositoryUnlock, err := m.options.Locks.LockRepository(ctx, record.RepositoryIdentity)
	if err != nil {
		return Settlement{}, err
	}
	defer repositoryUnlock()
	workspaceUnlock, err := m.options.Locks.LockWorkspace(ctx, workspaceID)
	if err != nil {
		return Settlement{}, err
	}
	defer workspaceUnlock()
	deleteUnlock, err := m.options.Locks.AcquireDeleteLease(ctx, workspaceID, record.OwnerID)
	if err != nil {
		return Settlement{}, err
	}
	defer deleteUnlock()
	current, err := m.options.Store.Load(ctx, workspaceID)
	if err != nil {
		return Settlement{}, err
	}
	if current.Revision != record.Revision || current.State != StateRetained || !current.RepositoryIdentity.Equal(record.RepositoryIdentity) ||
		!recordExpired(current, m.now(), m.options.Config.Lifecycle.RetentionTTL) {
		return Settlement{}, ErrRevisionConflict
	}
	lease := Lease{
		WorkspaceID: current.WorkspaceID, OwnerID: current.OwnerID, Root: current.Directory,
		Branch: current.Branch, BaseOID: current.BaseOID, AcquiredAt: current.CreatedAt,
	}
	if !validManagerLease(lease) || !recordMatchesLease(current, lease) {
		return m.manual(ctx, &current, "lease_identity_mismatch")
	}
	if err := m.transition(ctx, &current, StateSettling, nil); err != nil {
		return Settlement{}, err
	}
	_ = layout
	return m.safeDeleteLocked(ctx, &current, lease)
}

func collectibleState(record Record) bool {
	return record.State == StateRetained || record.State == StateSettling ||
		(record.State == StatePartial && resumableDeleteReason(record.Settlement.ReasonCode))
}

func recordExpired(record Record, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 || now.IsZero() {
		return false
	}
	reference := record.Settlement.CompletedAt
	if reference.IsZero() {
		reference = record.Lease.Heartbeat
	}
	if reference.IsZero() {
		reference = record.UpdatedAt
	}
	return !reference.IsZero() && !now.Before(reference) && now.Sub(reference) >= ttl
}

func (m *lifecycleManager) validateResumeRecord(record Record) (ManagedLayout, error) {
	identity, err := NewRepositoryIdentity(record.RepositoryIdentity.Root, record.RepositoryIdentity.CommonDir)
	if err != nil || !identity.Equal(record.RepositoryIdentity) || !sameRepositoryDirectoryObject(identity.Root, record.RepositoryIdentity.Root) {
		return ManagedLayout{}, ErrIdentityMismatch
	}
	layout, err := ResolveManagedLayout(identity.Root, record.WorkspaceID)
	if err != nil || layout.WorkspaceRoot != record.Directory || layout.Branch != record.Branch || filepath.Clean(layout.Control) != m.control {
		return ManagedLayout{}, ErrIdentityMismatch
	}
	return layout, nil
}

func resumableDeleteReason(reason string) bool {
	switch reason {
	case "worktree_remove_interrupted", "branch_cas_failed", "branch_cas_interrupted", "tombstone_failed", "tombstone_interrupted":
		return true
	default:
		return false
	}
}

func (m *lifecycleManager) resumeDeletePartialLocked(ctx context.Context, record *Record, layout ManagedLayout) (Settlement, error) {
	pathIdentity, err := ValidateManagedPath(layout.Root, record.Directory, true)
	if err != nil || pathIdentity.Exists() {
		return m.manual(ctx, record, "partial_directory_unknown")
	}
	worktrees, err := m.options.GitReader.WorktreeList(ctx, record.RepositoryIdentity.Root)
	if err != nil {
		return m.manual(ctx, record, "partial_registration_unknown")
	}
	branchRef := "refs/heads/" + record.Branch
	for _, worktree := range worktrees {
		if worktree.Path == record.Directory || worktree.Branch == branchRef {
			return m.manual(ctx, record, "partial_registration_present")
		}
	}
	refs, err := m.options.GitReader.ForEachRef(ctx, record.RepositoryIdentity.Root, branchRef)
	if err != nil || len(refs) > 1 {
		return m.manual(ctx, record, "partial_branch_unknown")
	}
	branchPresent := len(refs) == 1
	if branchPresent && (refs[0].Name != branchRef || refs[0].ObjectName != record.HeadOID) {
		return m.manual(ctx, record, "partial_branch_moved")
	}
	reason := record.Settlement.ReasonCode
	if (reason == "tombstone_failed" || reason == "tombstone_interrupted") && branchPresent {
		return m.manual(ctx, record, "partial_branch_present")
	}
	if err := m.transition(ctx, record, StateSettling, nil); err != nil {
		return Settlement{}, err
	}
	if branchPresent {
		if err := m.options.GitMutator.DeleteRefCAS(ctx, record.RepositoryIdentity.Root, branchRef, record.HeadOID); err != nil {
			return m.partial(ctx, record, "branch_cas_failed", err)
		}
	}
	deletedAt := m.now()
	if err := m.options.Store.WriteTombstone(ctx, Tombstone{
		SchemaVersion: TombstoneSchemaVersion, WorkspaceID: record.WorkspaceID, DeletedAt: deletedAt, RecordRevision: record.Revision,
	}); err != nil {
		return m.partial(ctx, record, "tombstone_failed", err)
	}
	err = m.transition(ctx, record, StateDeleted, func(next *Record) {
		next.Lease = LeaseRecord{}
		next.Settlement = SettlementRecord{State: SettlementDeleted, ReasonCode: "clean", CompletedAt: deletedAt}
	})
	return Settlement{State: SettlementDeleted, ReasonCode: "clean"}, err
}

func (m *lifecycleManager) partial(ctx context.Context, record *Record, reason string, cause error) (Settlement, error) {
	completed := m.now()
	err := m.transition(context.WithoutCancel(ctx), record, StatePartial, func(next *Record) {
		next.Lease = LeaseRecord{}
		next.Settlement = SettlementRecord{State: SettlementPartial, ReasonCode: reason, CompletedAt: completed}
	})
	return Settlement{State: SettlementPartial, ReasonCode: reason}, errors.Join(cause, err)
}

func (m *lifecycleManager) manual(ctx context.Context, record *Record, reason string) (Settlement, error) {
	completed := m.now()
	err := m.transition(context.WithoutCancel(ctx), record, StateManualAttention, func(next *Record) {
		next.Lease = LeaseRecord{}
		next.Settlement = SettlementRecord{State: SettlementManualAttention, ReasonCode: reason, CompletedAt: completed}
	})
	return Settlement{State: SettlementManualAttention, ReasonCode: reason}, err
}

func (m *lifecycleManager) Release(_ context.Context, lease Lease) error {
	held, err := m.takeHeldLease(lease)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if held.unlock != nil {
		return held.unlock()
	}
	return nil
}

func (m *lifecycleManager) takeHeldLease(lease Lease) (heldManagerLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	held, exists := m.leases[leaseKey(lease)]
	if !exists {
		return heldManagerLease{}, ErrNotFound
	}
	if !sameManagerLease(held.lease, lease) {
		return heldManagerLease{}, ErrIdentityMismatch
	}
	delete(m.leases, leaseKey(lease))
	return held, nil
}

func (m *lifecycleManager) restoreHeldLease(held heldManagerLease) {
	m.mu.Lock()
	m.leases[leaseKey(held.lease)] = held
	m.mu.Unlock()
}

func (m *lifecycleManager) Snapshot(ctx context.Context, workspaceID string) (Record, error) {
	return m.options.Store.Load(ctx, workspaceID)
}

func (m *lifecycleManager) List(ctx context.Context, query ListQuery) ([]Record, error) {
	return m.options.Store.List(ctx, query)
}

func (m *lifecycleManager) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return ErrManagerInvalid
	}
	m.admission.Lock()
	defer m.admission.Unlock()
	m.mu.Lock()
	m.closed = true
	unlocks := make([]Unlock, 0, len(m.leases))
	for key, held := range m.leases {
		unlocks = append(unlocks, held.unlock)
		delete(m.leases, key)
	}
	m.mu.Unlock()
	var result error
	for _, unlock := range unlocks {
		result = errors.Join(result, unlock())
	}
	return result
}

func (m *lifecycleManager) now() time.Time { return m.options.Clock().UTC() }
func (m *lifecycleManager) inject(stage string) error {
	if m.options.Fault == nil {
		return nil
	}
	return m.options.Fault(stage)
}
func (m *lifecycleManager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}
func leaseKey(lease Lease) string { return lease.WorkspaceID + ":" + lease.OwnerID }

func validManagerLease(lease Lease) bool {
	return ValidWorkspaceID(lease.WorkspaceID) && ValidWorkspaceID(lease.OwnerID) && filepath.IsAbs(lease.Root) && filepath.Clean(lease.Root) == lease.Root &&
		lease.Branch == "xagent/worktree/"+lease.WorkspaceID && validOID(lease.BaseOID) && !lease.AcquiredAt.IsZero()
}

func sameManagerLease(first, second Lease) bool {
	return first.WorkspaceID == second.WorkspaceID && first.OwnerID == second.OwnerID && first.Root == second.Root && first.Branch == second.Branch &&
		first.BaseOID == second.BaseOID && first.AcquiredAt.Equal(second.AcquiredAt)
}

func recordMatchesLease(record Record, lease Lease) bool {
	return record.WorkspaceID == lease.WorkspaceID && record.OwnerID == lease.OwnerID && record.Directory == lease.Root && record.Branch == lease.Branch && record.BaseOID == lease.BaseOID
}

func terminalSettlement(record Record) bool {
	return record.State == StateRetained || record.State == StateDeleted || record.State == StatePartial || record.State == StateManualAttention
}

func settlementFromRecord(record Record) Settlement {
	return Settlement{State: record.Settlement.State, Dirty: record.Settlement.Dirty, Unpushed: record.Settlement.Unpushed, ReasonCode: record.Settlement.ReasonCode}
}

var _ Manager = (*lifecycleManager)(nil)
var _ = fmt.Sprintf
