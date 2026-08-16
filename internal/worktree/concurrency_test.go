package worktree

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	concurrencyWorkspaceOne = "10101010101010101010101010101010"
	concurrencyWorkspaceTwo = "20202020202020202020202020202020"
	concurrencyOwnerOne     = "30303030303030303030303030303030"
	concurrencyOwnerTwo     = "40404040404040404040404040404040"
)

func TestConcurrencyDoubleAcquireSameWorkspaceAndQuotaAcrossProcesses(t *testing.T) {
	repository := newConcurrencyRepository(t)
	child := startConcurrencyChild(t, repository, "acquire_hold")
	child.expect(t, "acquired")

	manager := newConcurrencyManager(t, repository, 1, time.Now)
	identity := concurrencyIdentity(t, repository)
	_, err := manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: identity, LogicalName: "concurrency/child",
		WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerOne,
	})
	if !concurrencyLockContention(err) {
		t.Fatalf("same owner reacquired a process-owned active workspace: %v", err)
	}
	_, err = manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: identity, LogicalName: "concurrency/child",
		WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerTwo,
	})
	if !concurrencyLockContention(err) {
		t.Fatalf("second process acquired the same active workspace: %v", err)
	}
	child.send(t, "release")
	child.expect(t, "released")
	child.wait(t)
	recovered, err := manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: identity, LogicalName: "concurrency/child",
		WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerTwo,
	})
	if err != nil {
		t.Fatalf("workspace was not recoverable after OS owner released: %v", err)
	}
	if err := manager.Release(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrencySameProcessCannotAcquireActiveWorkspaceTwice(t *testing.T) {
	repository := newConcurrencyRepository(t)
	manager := newConcurrencyManager(t, repository, 4, time.Now)
	identity := concurrencyIdentity(t, repository)
	first, err := manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: identity, LogicalName: "concurrency/child",
		WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerOne,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: identity, LogicalName: "concurrency/child",
		WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerOne,
	})
	if !concurrencyLockContention(err) {
		t.Fatalf("same process acquired the active workspace twice: %v", err)
	}
	if err := manager.Release(context.Background(), first); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrencyCrashedProcessReleasesActiveOwnership(t *testing.T) {
	repository := newConcurrencyRepository(t)
	child := startConcurrencyChild(t, repository, "acquire_hold")
	child.expect(t, "acquired")
	child.kill(t)

	manager := newConcurrencyManager(t, repository, 4, time.Now)
	lease, err := manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: concurrencyIdentity(t, repository), LogicalName: "concurrency/child",
		WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerTwo,
	})
	if err != nil {
		t.Fatalf("workspace was not recoverable after process exit released its OS lease: %v", err)
	}
	if err := manager.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrencyQuotaCountsAnotherProcessActiveWorkspace(t *testing.T) {
	repository := newConcurrencyRepository(t)
	child := startConcurrencyChild(t, repository, "acquire_hold")
	child.expect(t, "acquired")
	manager := newConcurrencyManager(t, repository, 1, time.Now)
	identity := concurrencyIdentity(t, repository)
	_, err := manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: identity, LogicalName: "concurrency/quota",
		WorkspaceID: concurrencyWorkspaceTwo, OwnerID: concurrencyOwnerTwo,
	})
	if !errors.Is(err, ErrManagerInvalid) {
		t.Fatalf("cross-process active quota was exceeded: %v", err)
	}
	child.send(t, "release")
	child.expect(t, "released")
	child.wait(t)
}

func TestConcurrencyActiveLeaseAndExpiredHeartbeatBlockJanitorAcrossProcesses(t *testing.T) {
	repository := newConcurrencyRepository(t)
	child := startConcurrencyChild(t, repository, "acquire_hold")
	child.expect(t, "acquired")
	layout, err := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(layout.Control)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Load(context.Background(), concurrencyWorkspaceOne)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-2 * time.Hour)
	record.State = StateSettling
	record.Lease.AcquiredAt = old
	record.Lease.Heartbeat = old
	if err := store.CompareAndSwap(context.Background(), record, record.Revision); err != nil {
		t.Fatal(err)
	}
	manager := newConcurrencyManager(t, repository, 4, time.Now)
	janitor, err := NewJanitor(JanitorOptions{
		ControlRoot: layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{RetentionTTL: time.Minute, JanitorInterval: time.Minute, JanitorTimeout: 500 * time.Millisecond},
			Limits:    Limits{MaxJanitorCandidates: 8, MaxJanitorConcurrency: 1},
		},
		Store: store, Manager: manager, Clock: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := janitor.ScanOnce(context.Background())
	if result.SkippedBusy != 1 || result.Deleted != 0 {
		t.Fatalf("expired heartbeat bypassed active OS lease: %#v", result)
	}
	child.send(t, "release")
	child.expect(t, "released")
	child.wait(t)
}

func TestConcurrencyTwoJanitorsDeleteAtMostOnceAcrossProcesses(t *testing.T) {
	repository := newConcurrencyRepository(t)
	manager := newConcurrencyManager(t, repository, 4, time.Now)
	lease, err := manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: concurrencyIdentity(t, repository), LogicalName: "concurrency/child",
		WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerOne,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	layout, _ := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
	store, _ := NewFileStore(layout.Control)
	record, err := store.Load(context.Background(), concurrencyWorkspaceOne)
	if err != nil {
		t.Fatal(err)
	}
	record.State = StateSettling
	record.Lease.AcquiredAt = time.Now().UTC().Add(-2 * time.Hour)
	record.Lease.Heartbeat = record.Lease.AcquiredAt
	if err := store.CompareAndSwap(context.Background(), record, record.Revision); err != nil {
		t.Fatal(err)
	}

	first := startConcurrencyChild(t, repository, "janitor_once")
	second := startConcurrencyChild(t, repository, "janitor_once")
	first.expect(t, "ready")
	second.expect(t, "ready")
	first.send(t, "go")
	second.send(t, "go")
	results := []string{first.read(t), second.read(t)}
	first.wait(t)
	second.wait(t)
	deleted := 0
	for _, result := range results {
		if result == "deleted" {
			deleted++
		} else if result != "skipped" {
			t.Fatal("child janitor returned an invalid bounded result")
		}
	}
	if deleted > 1 {
		t.Fatalf("two Janitors advanced the Git deletion stage more than once: deleted=%d", deleted)
	}
	if _, err := store.Load(context.Background(), concurrencyWorkspaceOne); err != nil {
		t.Fatalf("concurrent Janitors corrupted the authoritative record: %v", err)
	}
}

func TestConcurrencyFileStoreCASAcrossProcesses(t *testing.T) {
	repository := newConcurrencyRepository(t)
	manager := newConcurrencyManager(t, repository, 4, time.Now)
	lease, err := manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: concurrencyIdentity(t, repository), LogicalName: "concurrency/child",
		WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerOne,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}

	first := startConcurrencyChild(t, repository, "store_cas")
	first.expect(t, "attempt")
	first.expect(t, "prepared")
	second := startConcurrencyChild(t, repository, "store_cas")
	second.expect(t, "attempt")
	first.send(t, "commit")
	results := []string{first.read(t), second.read(t)}
	first.wait(t)
	second.wait(t)
	successes, conflicts := 0, 0
	for _, result := range results {
		switch result {
		case "success":
			successes++
		case "conflict":
			conflicts++
		default:
			t.Fatal("child CAS returned an invalid bounded result")
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("cross-process CAS was not atomic: successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestConcurrencyFileStoreCreateAcrossProcesses(t *testing.T) {
	repository := newConcurrencyRepository(t)
	first := startConcurrencyChild(t, repository, "store_create")
	first.expect(t, "attempt")
	first.expect(t, "prepared")
	second := startConcurrencyChild(t, repository, "store_create")
	second.expect(t, "attempt")
	first.send(t, "commit")
	results := []string{first.read(t), second.read(t)}
	first.wait(t)
	second.wait(t)
	successes, exists := 0, 0
	for _, result := range results {
		switch result {
		case "success":
			successes++
		case "exists":
			exists++
		default:
			t.Fatal("child Create returned an invalid bounded result")
		}
	}
	if successes != 1 || exists != 1 {
		t.Fatalf("cross-process Create was not exclusive: successes=%d already-exists=%d", successes, exists)
	}
}

func TestConcurrencyFileStoreWaitHonorsContextAndDoesNotBlockOtherWorkspace(t *testing.T) {
	repository := newConcurrencyRepository(t)
	manager := newConcurrencyManager(t, repository, 4, time.Now)
	lease, err := manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: concurrencyIdentity(t, repository), LogicalName: "concurrency/child",
		WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerOne,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	layout, _ := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
	store, _ := NewFileStore(layout.Control)
	current, err := store.Load(context.Background(), concurrencyWorkspaceOne)
	if err != nil {
		t.Fatal(err)
	}
	next := current
	next.State = StateSettling

	child := startConcurrencyChild(t, repository, "store_cas")
	child.expect(t, "attempt")
	child.expect(t, "prepared")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := store.CompareAndSwap(ctx, next, current.Revision); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting Store operation ignored context cancellation: %v", err)
	}
	if err := store.Create(context.Background(), concurrencyStoreRecord(t, repository, concurrencyWorkspaceTwo, concurrencyOwnerTwo)); err != nil {
		t.Fatalf("lock for one workspace blocked a different workspace: %v", err)
	}
	child.send(t, "commit")
	if result := child.read(t); result != "success" {
		t.Fatal("lock-owning child did not commit")
	}
	child.wait(t)
}

func TestConcurrencyDeleteLeaseBlocksRecoveryAndResumeAcrossProcesses(t *testing.T) {
	repository := newConcurrencyRepository(t)
	manager := newConcurrencyManager(t, repository, 4, time.Now)
	lease, err := manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: concurrencyIdentity(t, repository), LogicalName: "concurrency/child",
		WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerOne,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	child := startConcurrencyChild(t, repository, "delete_lock_hold")
	child.expect(t, "held")
	_, err = manager.Acquire(context.Background(), AcquireRequest{
		RepositoryRoot: repository, RepositoryIdentity: concurrencyIdentity(t, repository), LogicalName: "concurrency/child",
		WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerTwo,
	})
	if !concurrencyLockContention(err) {
		t.Fatalf("recovery crossed process-held delete lock: %v", err)
	}
	layout, _ := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
	store, _ := NewFileStore(layout.Control)
	record, err := store.Load(context.Background(), concurrencyWorkspaceOne)
	if err != nil {
		t.Fatal(err)
	}
	record.State = StateSettling
	if err := store.CompareAndSwap(context.Background(), record, record.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resume(context.Background(), concurrencyWorkspaceOne); !concurrencyLockContention(err) {
		t.Fatalf("Resume crossed process-held delete lock: %v", err)
	}
	child.send(t, "release")
	child.expect(t, "released")
	child.wait(t)
	settlement, err := manager.Resume(context.Background(), concurrencyWorkspaceOne)
	if err != nil || settlement.State != SettlementDeleted {
		t.Fatalf("Resume did not converge after delete lock release: settlement=%#v err=%v", settlement, err)
	}
}

func TestConcurrencyRepositoryLockSharedAcrossMainAndManagedRootsAcrossProcesses(t *testing.T) {
	repository := newConcurrencyRepository(t)
	child := startConcurrencyChild(t, repository, "repository_lock_hold")
	child.expect(t, "held")
	managedLayout, err := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(managedLayout.WorkspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := NewRepositoryIdentity(managedLayout.WorkspaceRoot, filepath.Join(repository, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	locks, err := NewFileLockManager(filepath.Join(repository, "alternate-control"), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locks.LockRepository(NewLockContext(context.Background()), identity); !concurrencyLockContention(err) {
		t.Fatalf("managed-root identity bypassed main repository lock: %v", err)
	}
	child.send(t, "release")
	child.expect(t, "released")
	child.wait(t)
}

func concurrencyLockContention(err error) bool {
	return errors.Is(err, ErrLockTimeout) || errors.Is(err, context.DeadlineExceeded)
}

func TestConcurrencyProcessHelper(t *testing.T) {
	mode := os.Getenv("XAGENT_CONCURRENCY_HELPER")
	if mode == "" {
		return
	}
	repository, err := os.Getwd()
	if err != nil {
		concurrencyChildFailure(t, "cwd")
	}
	switch mode {
	case "acquire_hold":
		manager := newConcurrencyManager(t, repository, 1, time.Now)
		lease, err := manager.Acquire(context.Background(), AcquireRequest{
			RepositoryRoot: repository, RepositoryIdentity: concurrencyIdentity(t, repository), LogicalName: "concurrency/child",
			WorkspaceID: concurrencyWorkspaceOne, OwnerID: concurrencyOwnerOne,
		})
		if err != nil {
			concurrencyChildFailure(t, "acquire")
		}
		fmt.Println("acquired")
		if !concurrencyChildRead("release") {
			concurrencyChildFailure(t, "protocol")
		}
		if err := manager.Release(context.Background(), lease); err != nil {
			concurrencyChildFailure(t, "release")
		}
		fmt.Println("released")
	case "delete_lock_hold":
		layout, err := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
		if err != nil {
			concurrencyChildFailure(t, "layout")
		}
		locks, err := NewFileLockManager(layout.Control, 3*time.Second)
		if err != nil {
			concurrencyChildFailure(t, "locks")
		}
		ctx := NewLockContext(context.Background())
		repositoryUnlock, err := locks.LockRepository(ctx, concurrencyIdentity(t, repository))
		if err != nil {
			concurrencyChildFailure(t, "repository-lock")
		}
		workspaceUnlock, err := locks.LockWorkspace(ctx, concurrencyWorkspaceOne)
		if err != nil {
			_ = repositoryUnlock()
			concurrencyChildFailure(t, "workspace-lock")
		}
		deleteUnlock, err := locks.AcquireDeleteLease(ctx, concurrencyWorkspaceOne, concurrencyOwnerOne)
		if err != nil {
			_ = workspaceUnlock()
			_ = repositoryUnlock()
			concurrencyChildFailure(t, "delete-lock")
		}
		fmt.Println("held")
		if !concurrencyChildRead("release") {
			concurrencyChildFailure(t, "protocol")
		}
		_ = deleteUnlock()
		_ = workspaceUnlock()
		_ = repositoryUnlock()
		fmt.Println("released")
	case "repository_lock_hold":
		layout, err := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
		if err != nil {
			concurrencyChildFailure(t, "layout")
		}
		locks, err := NewFileLockManager(layout.Control, 3*time.Second)
		if err != nil {
			concurrencyChildFailure(t, "locks")
		}
		repositoryUnlock, err := locks.LockRepository(NewLockContext(context.Background()), concurrencyIdentity(t, repository))
		if err != nil {
			concurrencyChildFailure(t, "repository-lock")
		}
		fmt.Println("held")
		if !concurrencyChildRead("release") {
			concurrencyChildFailure(t, "protocol")
		}
		_ = repositoryUnlock()
		fmt.Println("released")
	case "janitor_once":
		layout, err := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
		if err != nil {
			concurrencyChildFailure(t, "layout")
		}
		store, err := NewFileStore(layout.Control)
		if err != nil {
			concurrencyChildFailure(t, "store")
		}
		didDelete := false
		manager := newConcurrencyManagerWithFault(t, repository, 4, time.Now, func(stage string) error {
			if stage == FaultWorktreeRemove {
				didDelete = true
			}
			return nil
		})
		janitor, err := NewJanitor(JanitorOptions{
			ControlRoot: layout.Control,
			Config: Config{
				Lifecycle: LifecycleConfig{RetentionTTL: time.Minute, JanitorInterval: time.Minute, JanitorTimeout: 500 * time.Millisecond},
				Limits:    Limits{MaxJanitorCandidates: 8, MaxJanitorConcurrency: 1},
			},
			Store: store, Manager: manager, Clock: time.Now,
		})
		if err != nil {
			concurrencyChildFailure(t, "janitor")
		}
		fmt.Println("ready")
		if !concurrencyChildRead("go") {
			concurrencyChildFailure(t, "protocol")
		}
		_ = janitor.ScanOnce(context.Background())
		if didDelete {
			fmt.Println("deleted")
		} else {
			fmt.Println("skipped")
		}
	case "store_cas":
		layout, err := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
		if err != nil {
			concurrencyChildFailure(t, "layout")
		}
		store, err := NewFileStore(layout.Control)
		if err != nil {
			concurrencyChildFailure(t, "store")
		}
		record, err := store.Load(context.Background(), concurrencyWorkspaceOne)
		if err != nil {
			concurrencyChildFailure(t, "load")
		}
		record.State = StateSettling
		fmt.Println("attempt")
		store.beforeRename = func(string, string) error {
			fmt.Println("prepared")
			if !concurrencyChildRead("commit") {
				return errors.New("invalid child protocol")
			}
			return nil
		}
		err = store.CompareAndSwap(context.Background(), record, record.Revision)
		switch {
		case err == nil:
			fmt.Println("success")
		case errors.Is(err, ErrRevisionConflict):
			fmt.Println("conflict")
		default:
			concurrencyChildFailure(t, "cas")
		}
	case "store_create":
		layout, err := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
		if err != nil {
			concurrencyChildFailure(t, "layout")
		}
		store, err := NewFileStore(layout.Control)
		if err != nil {
			concurrencyChildFailure(t, "store")
		}
		fmt.Println("attempt")
		store.beforeRename = func(string, string) error {
			fmt.Println("prepared")
			if !concurrencyChildRead("commit") {
				return errors.New("invalid child protocol")
			}
			return nil
		}
		err = store.Create(context.Background(), concurrencyStoreRecord(t, repository, concurrencyWorkspaceOne, concurrencyOwnerOne))
		switch {
		case err == nil:
			fmt.Println("success")
		case errors.Is(err, ErrAlreadyExists):
			fmt.Println("exists")
		default:
			concurrencyChildFailure(t, "create")
		}
	default:
		concurrencyChildFailure(t, "mode")
	}
}

func newConcurrencyRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	runManagerGit(t, repository, "init")
	runManagerGit(t, repository, "config", "user.name", "XAgent Concurrency")
	runManagerGit(t, repository, "config", "user.email", "xagent-concurrency@example.invalid")
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("/.xagent/worktrees/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runManagerGit(t, repository, "add", ".gitignore", "tracked.txt")
	runManagerGit(t, repository, "commit", "-m", "base")
	layout, err := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(layout.Control); err != nil {
		t.Fatal(err)
	}
	return repository
}

func concurrencyIdentity(t *testing.T, repository string) RepositoryIdentity {
	t.Helper()
	identity, err := NewRepositoryIdentity(repository, filepath.Join(repository, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func concurrencyStoreRecord(t *testing.T, repository, workspaceID, ownerID string) Record {
	t.Helper()
	layout, err := ResolveManagedLayout(repository, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	return Record{
		SchemaVersion: RecordSchemaVersion, Revision: 1, WorkspaceID: workspaceID, OwnerID: ownerID,
		RepositoryIdentity: concurrencyIdentity(t, repository), LogicalName: "concurrency/store",
		Directory: layout.WorkspaceRoot, Branch: layout.Branch,
		BaseOID: strings.Repeat("a", 40), HeadOID: strings.Repeat("a", 40), State: StateCreating,
		CreatedAt: now, UpdatedAt: now,
	}
}

func newConcurrencyManager(t *testing.T, repository string, maxActive int, clock func() time.Time) Manager {
	t.Helper()
	return newConcurrencyManagerWithFault(t, repository, maxActive, clock, nil)
}

func newConcurrencyManagerWithFault(t *testing.T, repository string, maxActive int, clock func() time.Time, fault func(string) error) Manager {
	t.Helper()
	layout, err := ResolveManagedLayout(repository, concurrencyWorkspaceOne)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(layout.Control)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := NewFileLockManager(layout.Control, 150*time.Millisecond)
	if errors.Is(err, ErrPlatformLockUnsupported) {
		t.Skip("reliable cross-process file locks are unsupported")
	}
	if err != nil {
		t.Fatal(err)
	}
	git := NewGitClient("git", nil, 3*time.Second, defaultGitOutputBytes)
	manager, err := NewManager(ManagerOptions{
		ControlRoot: layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{
				RetentionTTL: time.Minute, InitTimeout: 3 * time.Second, RecoveryTimeout: 500 * time.Millisecond,
				SettleTimeout: 3 * time.Second, JanitorTimeout: 500 * time.Millisecond,
			},
			Limits: Limits{MaxActive: maxActive, MaxRetained: 8, MaxJanitorCandidates: 16, MaxJanitorConcurrency: 2},
		},
		Store: store, Locks: locks, GitReader: git, GitMutator: git,
		Initializer: NewFilesystemInitializer(InitializerOptions{Git: git}), Clock: clock, Fault: fault,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

type concurrencyChild struct {
	command *exec.Cmd
	input   *bufio.Writer
	output  *bufio.Scanner
	waited  bool
}

func startConcurrencyChild(t *testing.T, repository, mode string) *concurrencyChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestConcurrencyProcessHelper$")
	command.Dir = repository
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=.", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "XAGENT_CONCURRENCY_HELPER=" + mode,
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	child := &concurrencyChild{command: command, input: bufio.NewWriter(stdin), output: bufio.NewScanner(stdout)}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !child.waited && child.command.Process != nil {
			_ = child.command.Process.Kill()
			_ = child.command.Wait()
		}
	})
	return child
}

func (c *concurrencyChild) expect(t *testing.T, expected string) {
	t.Helper()
	token := make(chan string, 1)
	go func() {
		if c.output.Scan() {
			token <- strings.TrimSpace(c.output.Text())
			return
		}
		token <- ""
	}()
	select {
	case actual := <-token:
		if actual != expected {
			t.Fatal("child protocol mismatch")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child protocol timeout")
	}
}

func (c *concurrencyChild) read(t *testing.T) string {
	t.Helper()
	token := make(chan string, 1)
	go func() {
		if c.output.Scan() {
			switch value := strings.TrimSpace(c.output.Text()); value {
			case "deleted", "skipped", "success", "conflict", "exists":
				token <- value
			default:
				token <- "invalid"
			}
			return
		}
		token <- "invalid"
	}()
	select {
	case value := <-token:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("child protocol timeout")
		return "invalid"
	}
}

func (c *concurrencyChild) send(t *testing.T, token string) {
	t.Helper()
	if _, err := fmt.Fprintln(c.input, token); err != nil {
		t.Fatal(err)
	}
	if err := c.input.Flush(); err != nil {
		t.Fatal(err)
	}
}

func (c *concurrencyChild) wait(t *testing.T) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.command.Wait() }()
	select {
	case err := <-done:
		c.waited = true
		if err != nil {
			t.Fatal("child process failed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child process wait timeout")
	}
}

func (c *concurrencyChild) kill(t *testing.T) {
	t.Helper()
	if err := c.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := c.command.Wait(); err == nil {
		t.Fatal("killed child unexpectedly exited successfully")
	}
	c.waited = true
}

func concurrencyChildRead(expected string) bool {
	scanner := bufio.NewScanner(os.Stdin)
	return scanner.Scan() && strings.TrimSpace(scanner.Text()) == expected
}

func concurrencyChildFailure(t *testing.T, code string) {
	t.Helper()
	fmt.Println("fail:" + code)
	t.FailNow()
}
