package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIntegrationManagerCreateAndSafeDelete(t *testing.T) {
	repositoryRoot := t.TempDir()
	runManagerGit(t, repositoryRoot, "init")
	runManagerGit(t, repositoryRoot, "config", "user.name", "XAgent Test")
	runManagerGit(t, repositoryRoot, "config", "user.email", "xagent@example.invalid")
	if err := os.WriteFile(filepath.Join(repositoryRoot, ".gitignore"), []byte("/.xagent/worktrees/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryRoot, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runManagerGit(t, repositoryRoot, "add", ".gitignore", "tracked.txt")
	runManagerGit(t, repositoryRoot, "commit", "-m", "base")

	identity, err := NewRepositoryIdentity(repositoryRoot, filepath.Join(repositoryRoot, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	layout, err := ResolveManagedLayout(repositoryRoot, managerTestWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(layout.Control)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := NewFileLockManager(layout.Control, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	git := NewGitClient("git", nil, 5*time.Second, defaultGitOutputBytes)
	initializer := NewFilesystemInitializer(InitializerOptions{Git: git})
	manager, err := NewManager(ManagerOptions{
		ControlRoot: layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{InitTimeout: time.Second, RecoveryTimeout: time.Second, SettleTimeout: time.Second},
			Limits:    Limits{MaxActive: 4, MaxRetained: 4},
		},
		Store: store, Locks: locks, GitReader: git, GitMutator: git, Initializer: initializer,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := AcquireRequest{
		RepositoryRoot: repositoryRoot, RepositoryIdentity: identity, LogicalName: "integration/task",
		WorkspaceID: managerTestWorkspaceID, OwnerID: managerTestOwnerID,
	}
	lease, err := manager.Acquire(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(lease.Root, "tracked.txt")); err != nil || string(data) != "base\n" {
		t.Fatalf("worktree did not use immutable base: data=%q err=%v", data, err)
	}
	settlement, err := manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if settlement.State != SettlementDeleted {
		t.Fatalf("clean worktree not deleted: %#v", settlement)
	}
	if _, err := os.Lstat(layout.WorkspaceRoot); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
	refs := runManagerGit(t, repositoryRoot, "for-each-ref", "--format=%(refname)", "refs/heads/xagent/worktree/*")
	if strings.TrimSpace(refs) != "" {
		t.Fatalf("temporary branch still exists: %q", refs)
	}
	if status := strings.TrimSpace(runManagerGit(t, repositoryRoot, "status", "--porcelain")); status != "" {
		t.Fatalf("main worktree changed: %q", status)
	}
}

func TestIntegrationManagerRejectsTasksSymlinkBeforeAnyAcquireSideEffect(t *testing.T) {
	repositoryRoot := t.TempDir()
	runManagerGit(t, repositoryRoot, "init")
	runManagerGit(t, repositoryRoot, "config", "user.name", "XAgent Test")
	runManagerGit(t, repositoryRoot, "config", "user.email", "xagent@example.invalid")
	if err := os.WriteFile(filepath.Join(repositoryRoot, ".gitignore"), []byte("/.xagent/worktrees/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryRoot, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runManagerGit(t, repositoryRoot, "add", ".gitignore", "tracked.txt")
	runManagerGit(t, repositoryRoot, "commit", "-m", "base")

	identity, err := NewRepositoryIdentity(repositoryRoot, filepath.Join(repositoryRoot, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	layout, err := ResolveManagedLayout(repositoryRoot, managerTestWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.Symlink(external, layout.Tasks); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(layout.Control)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := NewFileLockManager(layout.Control, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	git := NewGitClient("git", nil, 5*time.Second, defaultGitOutputBytes)
	manager, err := NewManager(ManagerOptions{
		ControlRoot: layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{InitTimeout: time.Second, RecoveryTimeout: time.Second, SettleTimeout: time.Second},
			Limits:    Limits{MaxActive: 4, MaxRetained: 4},
		},
		Store: store, Locks: locks, GitReader: git, GitMutator: git,
		Initializer: NewFilesystemInitializer(InitializerOptions{Git: git}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	request := AcquireRequest{
		RepositoryRoot: repositoryRoot, RepositoryIdentity: identity, LogicalName: "integration/tasks-symlink",
		WorkspaceID: managerTestWorkspaceID, OwnerID: managerTestOwnerID,
	}
	if lease, acquireErr := manager.Acquire(context.Background(), request); acquireErr == nil {
		t.Fatalf("Acquire accepted tasks symlink: %#v", lease)
	}
	if entries, readErr := os.ReadDir(external); readErr != nil || len(entries) != 0 {
		t.Fatalf("Acquire wrote outside managed root: entries=%v err=%v", entries, readErr)
	}
	refs := strings.TrimSpace(runManagerGit(t, repositoryRoot, "for-each-ref", "--format=%(refname)", "refs/heads/"+layout.Branch))
	if refs != "" {
		t.Fatalf("Acquire created temporary branch before rejecting tasks symlink: %q", refs)
	}
	if record, loadErr := store.Load(context.Background(), managerTestWorkspaceID); !errors.Is(loadErr, ErrNotFound) {
		t.Fatalf("Acquire created record before rejecting tasks symlink: record=%#v err=%v", record, loadErr)
	}
}

func TestIntegrationManagerRejectsBroadIgnoreBeforeAcquireSideEffects(t *testing.T) {
	repositoryRoot := t.TempDir()
	runManagerGit(t, repositoryRoot, "init")
	runManagerGit(t, repositoryRoot, "config", "user.name", "XAgent Test")
	runManagerGit(t, repositoryRoot, "config", "user.email", "xagent@example.invalid")
	if err := os.WriteFile(filepath.Join(repositoryRoot, ".gitignore"), []byte("/.xagent/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryRoot, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runManagerGit(t, repositoryRoot, "add", ".gitignore", "tracked.txt")
	runManagerGit(t, repositoryRoot, "commit", "-m", "base")

	identity, err := NewRepositoryIdentity(repositoryRoot, filepath.Join(repositoryRoot, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	layout, err := ResolveManagedLayout(repositoryRoot, managerTestWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(layout.Control)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := NewFileLockManager(layout.Control, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	git := NewGitClient("git", nil, 5*time.Second, defaultGitOutputBytes)
	manager, err := NewManager(ManagerOptions{
		ControlRoot: layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{InitTimeout: time.Second, RecoveryTimeout: time.Second, SettleTimeout: time.Second},
			Limits:    Limits{MaxActive: 4, MaxRetained: 4},
		},
		Store: store, Locks: locks, GitReader: git, GitMutator: git,
		Initializer: NewFilesystemInitializer(InitializerOptions{Git: git}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	request := AcquireRequest{
		RepositoryRoot: repositoryRoot, RepositoryIdentity: identity, LogicalName: "integration/broad-ignore",
		WorkspaceID: managerTestWorkspaceID, OwnerID: managerTestOwnerID,
	}
	if lease, acquireErr := manager.Acquire(context.Background(), request); acquireErr == nil {
		t.Fatalf("Acquire accepted broad ignore rule: %#v", lease)
	}
	refs := strings.TrimSpace(runManagerGit(t, repositoryRoot, "for-each-ref", "--format=%(refname)", "refs/heads/"+layout.Branch))
	if refs != "" {
		t.Fatalf("Acquire created temporary branch before rejecting broad ignore: %q", refs)
	}
	if record, loadErr := store.Load(context.Background(), managerTestWorkspaceID); !errors.Is(loadErr, ErrNotFound) {
		t.Fatalf("Acquire created record before rejecting broad ignore: record=%#v err=%v", record, loadErr)
	}
}

func TestIntegrationManagerRejectsTrackedManagedPathBeforeAcquireSideEffects(t *testing.T) {
	repositoryRoot := t.TempDir()
	runManagerGit(t, repositoryRoot, "init")
	runManagerGit(t, repositoryRoot, "config", "user.name", "XAgent Test")
	runManagerGit(t, repositoryRoot, "config", "user.email", "xagent@example.invalid")
	if err := os.WriteFile(filepath.Join(repositoryRoot, ".gitignore"), []byte("/.xagent/worktrees/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryRoot, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	trackedManaged := filepath.Join(repositoryRoot, ".xagent", "worktrees", "tracked-by-git.txt")
	if err := os.MkdirAll(filepath.Dir(trackedManaged), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trackedManaged, []byte("must reject\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runManagerGit(t, repositoryRoot, "add", ".gitignore", "tracked.txt")
	runManagerGit(t, repositoryRoot, "add", "-f", ".xagent/worktrees/tracked-by-git.txt")
	runManagerGit(t, repositoryRoot, "commit", "-m", "base")

	identity, err := NewRepositoryIdentity(repositoryRoot, filepath.Join(repositoryRoot, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	layout, err := ResolveManagedLayout(repositoryRoot, managerTestWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(layout.Control)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := NewFileLockManager(layout.Control, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	git := NewGitClient("git", nil, 5*time.Second, defaultGitOutputBytes)
	manager, err := NewManager(ManagerOptions{
		ControlRoot: layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{InitTimeout: time.Second, RecoveryTimeout: time.Second, SettleTimeout: time.Second},
			Limits:    Limits{MaxActive: 4, MaxRetained: 4},
		},
		Store: store, Locks: locks, GitReader: git, GitMutator: git,
		Initializer: NewFilesystemInitializer(InitializerOptions{Git: git}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	request := AcquireRequest{
		RepositoryRoot: repositoryRoot, RepositoryIdentity: identity, LogicalName: "integration/tracked-managed",
		WorkspaceID: managerTestWorkspaceID, OwnerID: managerTestOwnerID,
	}
	if lease, acquireErr := manager.Acquire(context.Background(), request); acquireErr == nil {
		t.Fatalf("Acquire accepted tracked managed path: %#v", lease)
	}
	refs := strings.TrimSpace(runManagerGit(t, repositoryRoot, "for-each-ref", "--format=%(refname)", "refs/heads/"+layout.Branch))
	if refs != "" {
		t.Fatalf("Acquire created temporary branch before rejecting tracked managed path: %q", refs)
	}
	if record, loadErr := store.Load(context.Background(), managerTestWorkspaceID); !errors.Is(loadErr, ErrNotFound) {
		t.Fatalf("Acquire created record before rejecting tracked managed path: record=%#v err=%v", record, loadErr)
	}
}

func TestIntegrationManagerRejectsMissingIgnoreBeforeAcquireSideEffects(t *testing.T) {
	repositoryRoot := t.TempDir()
	runManagerGit(t, repositoryRoot, "init")
	runManagerGit(t, repositoryRoot, "config", "user.name", "XAgent Test")
	runManagerGit(t, repositoryRoot, "config", "user.email", "xagent@example.invalid")
	if err := os.WriteFile(filepath.Join(repositoryRoot, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runManagerGit(t, repositoryRoot, "add", "tracked.txt")
	runManagerGit(t, repositoryRoot, "commit", "-m", "base")

	identity, err := NewRepositoryIdentity(repositoryRoot, filepath.Join(repositoryRoot, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	layout, err := ResolveManagedLayout(repositoryRoot, managerTestWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(layout.Control)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := NewFileLockManager(layout.Control, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	git := NewGitClient("git", nil, 5*time.Second, defaultGitOutputBytes)
	manager, err := NewManager(ManagerOptions{
		ControlRoot: layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{InitTimeout: time.Second, RecoveryTimeout: time.Second, SettleTimeout: time.Second},
			Limits:    Limits{MaxActive: 4, MaxRetained: 4},
		},
		Store: store, Locks: locks, GitReader: git, GitMutator: git,
		Initializer: NewFilesystemInitializer(InitializerOptions{Git: git}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	request := AcquireRequest{
		RepositoryRoot: repositoryRoot, RepositoryIdentity: identity, LogicalName: "integration/missing-ignore",
		WorkspaceID: managerTestWorkspaceID, OwnerID: managerTestOwnerID,
	}
	if lease, acquireErr := manager.Acquire(context.Background(), request); acquireErr == nil {
		t.Fatalf("Acquire accepted missing ignore rule: %#v", lease)
	}
	refs := strings.TrimSpace(runManagerGit(t, repositoryRoot, "for-each-ref", "--format=%(refname)", "refs/heads/"+layout.Branch))
	if refs != "" {
		t.Fatalf("Acquire created temporary branch before rejecting missing ignore: %q", refs)
	}
	if record, loadErr := store.Load(context.Background(), managerTestWorkspaceID); !errors.Is(loadErr, ErrNotFound) {
		t.Fatalf("Acquire created record before rejecting missing ignore: record=%#v err=%v", record, loadErr)
	}
}

func runManagerGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, output)
	}
	return string(output)
}
