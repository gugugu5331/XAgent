package worktree

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	CodeLockOrder           ErrorCode = "worktree_lock_order"
	CodeLockTimeout         ErrorCode = "worktree_lock_timeout"
	CodeLockFailed          ErrorCode = "worktree_lock_failed"
	CodePlatformLockMissing ErrorCode = "worktree_platform_lock_unsupported"
)

var (
	ErrLockOrder               = errors.New("worktree lock order violation")
	ErrLockTimeout             = errors.New("worktree lock timeout")
	ErrPlatformLockUnsupported = errors.New("platform file lock unsupported")
)

type Unlock func() error

type LockManager interface {
	LockRepository(context.Context, RepositoryIdentity) (Unlock, error)
	LockWorkspace(context.Context, string) (Unlock, error)
	AcquireActiveLease(context.Context, string, string) (Unlock, error)
	AcquireDeleteLease(context.Context, string, string) (Unlock, error)
}

type FileLockManager struct {
	root    string
	timeout time.Duration
}

type lockContextKey struct{}

type lockContextState struct {
	mu          sync.Mutex
	repository  bool
	workspace   bool
	lease       bool
	workspaceID string
}

// NewLockContext 为一次生命周期事务创建独立的锁顺序状态。
func NewLockContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, lockContextKey{}, &lockContextState{})
}

func NewFileLockManager(controlRoot string, timeout time.Duration) (*FileLockManager, error) {
	if !platformLockSupported() {
		return nil, NewSafeError(CodePlatformLockMissing, "reliable platform file locking is unavailable", ErrPlatformLockUnsupported)
	}
	if timeout <= 0 {
		return nil, NewSafeError(CodeLockFailed, "lock timeout must be positive", ErrInvalidConfig)
	}
	canonicalRoot, err := canonicalStoreRoot(controlRoot)
	if err != nil {
		return nil, NewSafeError(CodeLockFailed, "lock control root is unsafe", err)
	}
	lockRoot := filepath.Join(canonicalRoot, "locks")
	if err := ensurePrivateLockDirectories(lockRoot, filepath.Join(lockRoot, "workspaces"), filepath.Join(lockRoot, "leases")); err != nil {
		return nil, err
	}
	return &FileLockManager{root: lockRoot, timeout: timeout}, nil
}

func ensurePrivateLockDirectories(directories ...string) error {
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return NewSafeError(CodeLockFailed, "cannot create lock directory", err)
		}
		info, err := os.Lstat(directory)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return NewSafeError(CodeLockFailed, "lock directory is unsafe", ErrUnsafePath)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return NewSafeError(CodeLockFailed, "cannot protect lock directory", err)
		}
	}
	return nil
}

func (m *FileLockManager) LockRepository(ctx context.Context, identity RepositoryIdentity) (Unlock, error) {
	state, err := requireLockContext(ctx)
	if err != nil {
		return nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.repository || state.workspace || state.lease {
		return nil, lockOrderError()
	}
	lockPath, err := repositoryLockPath(identity)
	if err != nil {
		return nil, NewSafeError(CodeLockFailed, "repository identity is invalid", err)
	}
	held, err := m.acquire(ctx, lockPath, false)
	if err != nil {
		return nil, err
	}
	state.repository = true
	return scopedUnlock(state, held, func() { state.repository = false }), nil
}

func repositoryLockPath(identity RepositoryIdentity) (string, error) {
	if !filepath.IsAbs(identity.Root) || !filepath.IsAbs(identity.CommonDir) || !validDigest(identity.Digest) {
		return "", ErrIdentityMismatch
	}
	current, err := NewRepositoryIdentity(identity.Root, identity.CommonDir)
	if err != nil || !current.Equal(identity) {
		return "", ErrIdentityMismatch
	}
	mainRoot, err := repositoryMainRoot(current)
	if err != nil {
		return "", err
	}
	sharedControl, err := canonicalStoreRoot(filepath.Join(mainRoot, ".xagent", "worktrees", ".control"))
	if err != nil {
		return "", ErrUnsafePath
	}
	repositoryLocks := filepath.Join(sharedControl, "locks", "repositories")
	if err := ensurePrivateLockDirectories(repositoryLocks); err != nil {
		return "", err
	}
	return filepath.Join(repositoryLocks, current.Digest+".lock"), nil
}

func repositoryMainRoot(identity RepositoryIdentity) (string, error) {
	mainRoot := filepath.Dir(identity.CommonDir)
	canonicalDotGit, err := canonicalDirectory(filepath.Join(mainRoot, ".git"))
	if err != nil {
		return "", ErrIdentityMismatch
	}
	canonicalIdentity, err := NewRepositoryIdentity(mainRoot, canonicalDotGit)
	if err != nil || !canonicalIdentity.Equal(identity) {
		return "", ErrIdentityMismatch
	}
	if sameRepositoryDirectoryObject(mainRoot, identity.Root) {
		return mainRoot, nil
	}
	workspaceID := strings.ToLower(filepath.Base(identity.Root))
	if !ValidWorkspaceID(workspaceID) {
		return "", ErrIdentityMismatch
	}
	expectedManagedRoot := filepath.Join(mainRoot, ".xagent", "worktrees", "tasks", workspaceID[:2], workspaceID)
	if !sameRepositoryDirectoryObject(expectedManagedRoot, identity.Root) {
		return "", ErrIdentityMismatch
	}
	return mainRoot, nil
}

func sameRepositoryDirectoryObject(first, second string) bool {
	canonicalFirst, err := canonicalDirectory(first)
	if err != nil {
		return false
	}
	canonicalSecond, err := canonicalDirectory(second)
	if err != nil {
		return false
	}
	firstIdentity, err := repositoryDirectoryObjectIdentity(canonicalFirst)
	if err != nil {
		return false
	}
	secondIdentity, err := repositoryDirectoryObjectIdentity(canonicalSecond)
	return err == nil && bytes.Equal(firstIdentity, secondIdentity)
}

func (m *FileLockManager) LockWorkspace(ctx context.Context, workspaceID string) (Unlock, error) {
	state, err := requireLockContext(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidWorkspaceID(workspaceID) {
		return nil, NewSafeError(CodeLockFailed, "workspace identity is invalid", ErrIdentityMismatch)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.repository || state.workspace || state.lease {
		return nil, lockOrderError()
	}
	held, err := m.acquire(ctx, filepath.Join(m.root, "workspaces", workspaceID+".lock"), false)
	if err != nil {
		return nil, err
	}
	state.workspace = true
	state.workspaceID = workspaceID
	return scopedUnlock(state, held, func() {
		state.workspace = false
		if !state.lease {
			state.workspaceID = ""
		}
	}), nil
}

func (m *FileLockManager) AcquireActiveLease(ctx context.Context, workspaceID, ownerID string) (Unlock, error) {
	return m.acquireLease(ctx, workspaceID, ownerID, false)
}

func (m *FileLockManager) AcquireDeleteLease(ctx context.Context, workspaceID, ownerID string) (Unlock, error) {
	return m.acquireLease(ctx, workspaceID, ownerID, false)
}

func (m *FileLockManager) acquireLease(ctx context.Context, workspaceID, ownerID string, shared bool) (Unlock, error) {
	state, err := requireLockContext(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidWorkspaceID(workspaceID) || !ValidWorkspaceID(ownerID) {
		return nil, NewSafeError(CodeLockFailed, "lease identity is invalid", ErrIdentityMismatch)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.repository || !state.workspace || state.lease || state.workspaceID != workspaceID {
		return nil, lockOrderError()
	}
	held, err := m.acquire(ctx, filepath.Join(m.root, "leases", workspaceID+".lock"), shared)
	if err != nil {
		return nil, err
	}
	state.lease = true
	return scopedUnlock(state, held, func() {
		state.lease = false
		if !state.workspace {
			state.workspaceID = ""
		}
	}), nil
}

func (m *FileLockManager) acquire(ctx context.Context, path string, shared bool) (*platformHeldLock, error) {
	if ctx == nil {
		return nil, lockOrderError()
	}
	deadline := time.Now().Add(m.timeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		held, acquired, err := tryPlatformFileLock(path, shared)
		if err != nil {
			if errors.Is(err, ErrPlatformLockUnsupported) {
				return nil, NewSafeError(CodePlatformLockMissing, "reliable platform file locking is unavailable", err)
			}
			return nil, NewSafeError(CodeLockFailed, "cannot acquire file lock", err)
		}
		if acquired {
			return held, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, NewSafeError(CodeLockTimeout, "timed out waiting for worktree lock", ErrLockTimeout)
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
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func requireLockContext(ctx context.Context) (*lockContextState, error) {
	if ctx == nil {
		return nil, lockOrderError()
	}
	state, ok := ctx.Value(lockContextKey{}).(*lockContextState)
	if !ok || state == nil {
		return nil, lockOrderError()
	}
	return state, nil
}

func lockOrderError() error {
	return NewSafeError(CodeLockOrder, "worktree locks must be acquired in repository, workspace, lease order", ErrLockOrder)
}

func scopedUnlock(state *lockContextState, held *platformHeldLock, update func()) Unlock {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			result = releasePlatformFileLock(held)
			state.mu.Lock()
			update()
			state.mu.Unlock()
		})
		return result
	}
}

var _ LockManager = (*FileLockManager)(nil)
