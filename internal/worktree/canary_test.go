package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecretCanaryManagerErrorsAreFixedAndPreserveCause(t *testing.T) {
	canary := t68WorktreeCanary()
	t.Setenv("XAGENT_T68_TEST_ENV", canary)

	t.Run("Acquire initializing fault", func(t *testing.T) {
		fixture := newT68ManagerFixture(t, canary)
		injected := errors.New("initialization failure " + canary + " repository=" + fixture.repo)
		manager := fixture.newManagerWithFault(func(stage string) error {
			if stage == FaultInitializing {
				return injected
			}
			return nil
		})

		_, err := manager.Acquire(context.Background(), fixture.acquireRequest())
		assertT68SafeManagerError(t, err, ErrorCode("worktree_lifecycle_failed"), "worktree lifecycle operation failed", canary)
		if !errors.Is(err, injected) {
			t.Fatal("sanitized Acquire error lost its internal cause")
		}
		record := mustLoadT68Record(t, fixture)
		assertT68NoCanary(t, record.Settlement.ReasonCode+" "+record.LogicalName, canary)
	})

	t.Run("Settle Git failure", func(t *testing.T) {
		fixture := newT68ManagerFixture(t, canary)
		privateCause := errors.New(strings.Join([]string{
			"remote=" + canary,
			"url=https://user:token@" + canary + ".invalid/repo.git",
			"repository=" + fixture.repo,
			"worktree=" + fixture.layout.WorkspaceRoot,
			"env=" + os.Getenv("XAGENT_T68_TEST_ENV"),
		}, " "))
		gitFailure := &GitCommandError{ExitCode: 128, cause: privateCause}
		git := &t68ManagerGit{fakeManagerGit: fixture.git, removeErr: gitFailure}
		manager, err := NewManager(ManagerOptions{
			ControlRoot: fixture.layout.Control,
			Config: Config{
				Lifecycle: LifecycleConfig{InitTimeout: time.Second, RecoveryTimeout: time.Second, SettleTimeout: time.Second},
				Limits:    Limits{MaxActive: 8, MaxRetained: 8},
			},
			Store: fixture.store, Locks: fixture.locks, GitReader: git, GitMutator: git, Initializer: fixture.init,
			Clock: func() time.Time { return time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC) },
		})
		if err != nil {
			t.Fatal("canary manager setup failed")
		}
		lease, err := manager.Acquire(context.Background(), fixture.acquireRequest())
		if err != nil {
			t.Fatal("canary manager acquire failed")
		}
		branchRef := "refs/heads/" + fixture.layout.Branch
		fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
		fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}

		settlement, err := manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
		if settlement.State != SettlementPartial || settlement.ReasonCode != "worktree_remove_failed" {
			t.Fatal("Git failure did not retain a stable partial settlement")
		}
		assertT68SafeManagerError(t, err, CodeGitCommandFailed, "git command failed", canary)
		var commandError *GitCommandError
		if !errors.As(err, &commandError) || commandError.ExitStatus() != "128" {
			t.Fatal("sanitized Settle error lost its typed internal cause")
		}
	})
}

func TestInitSecretCanaryContentAndPathsStayOutOfManifest(t *testing.T) {
	canary := t68WorktreeCanary()
	root := t.TempDir()
	repositoryRoot := filepath.Join(root, canary, "repository")
	managedRoot := filepath.Join(repositoryRoot, ".xagent", "worktrees")
	worktreeRoot := filepath.Join(managedRoot, "tasks", initializerWorkspaceID[:2], initializerWorkspaceID)
	sharedRoot := filepath.Join(root, canary, "shared-dependency")
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatal("canary worktree setup failed")
	}
	writeInitFixture(t, filepath.Join(repositoryRoot, "local.json"), canary, 0o600)
	writeInitFixture(t, filepath.Join(sharedRoot, "cache.bin"), canary, 0o600)
	links := &readonlyLinkFixture{}
	initializer := NewFilesystemInitializer(InitializerOptions{ReadonlyLinks: links})
	manifest, err := initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot,
		ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
		Config: InitConfig{
			Copy: []CopyRule{{Source: "local.json", Target: "runtime/local.json"}},
			Link: []LinkRule{{Source: sharedRoot, Target: "deps/cache"}},
		},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatal("canary initialization failed")
	}
	encoded, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal("canary manifest encoding failed")
	}
	assertT68NoCanary(t, string(encoded), canary)
	assertT68NoCanary(t, fmt.Sprintf("%#v", manifest), canary)
	if copied, readErr := os.ReadFile(filepath.Join(worktreeRoot, "runtime", "local.json")); readErr != nil || string(copied) != canary {
		t.Fatal("copy canary did not reach only the declared target")
	}
	linked, readErr := os.ReadFile(filepath.Join(worktreeRoot, "deps", "cache", "cache.bin"))
	if readErr != nil || string(linked) != canary || len(links.requests) != 1 {
		t.Fatal("link canary did not reach only the protected dependency")
	}
}

func TestJanitorCanaryErrorsAndPathsCollapseToBoundedMetrics(t *testing.T) {
	canary := t68WorktreeCanary()
	fixture := newT68ManagerFixture(t, canary)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal("canary fixture acquire failed")
	}
	fixture.git.status = StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "?", Path: "private.txt"}}}
	if _, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); err != nil {
		t.Fatal("canary fixture settlement failed")
	}
	record := mustLoadT68Record(t, fixture)
	collector := &fakeJanitorCollector{err: errors.New("collector failure " + canary + " path=" + fixture.layout.WorkspaceRoot)}
	result := newTestJanitor(t, fixture, collector, record.Settlement.CompletedAt.Add(2*time.Hour)).ScanOnce(context.Background())
	if result.CheckFailed != 1 || result.Candidates == 0 {
		t.Fatal("Janitor did not aggregate the canary failure")
	}
	assertT68NoCanary(t, fmt.Sprintf("%#v", result), canary)
}

type t68ManagerGit struct {
	*fakeManagerGit
	removeErr error
}

func (git *t68ManagerGit) RemoveWorktree(context.Context, string, string) error {
	return git.removeErr
}

func newT68ManagerFixture(t *testing.T, canary string) *managerFixture {
	t.Helper()
	repo := filepath.Join(t.TempDir(), canary, "repository")
	common := filepath.Join(repo, ".git")
	if err := os.MkdirAll(common, 0o700); err != nil {
		t.Fatal("canary repository setup failed")
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("/.xagent/worktrees/\n"), 0o600); err != nil {
		t.Fatal("canary ignore setup failed")
	}
	identity, err := NewRepositoryIdentity(repo, common)
	if err != nil {
		t.Fatal("canary identity setup failed")
	}
	layout, err := ResolveManagedLayout(repo, managerTestWorkspaceID)
	if err != nil {
		t.Fatal("canary layout setup failed")
	}
	if err := os.MkdirAll(layout.Control, 0o700); err != nil {
		t.Fatal("canary control setup failed")
	}
	events := &managerEvents{}
	store := newMemoryManagerStore(events)
	git := &fakeManagerGit{events: events, head: managerTestOID, ignored: true}
	initializer := &fakeManagerInitializer{events: events}
	locks := &fakeManagerLocks{events: events}
	manager, err := NewManager(ManagerOptions{
		ControlRoot: layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{
				RetentionTTL: time.Hour, JanitorInterval: time.Minute, InitTimeout: time.Second,
				RecoveryTimeout: time.Second, SettleTimeout: time.Second, JanitorTimeout: time.Second,
			},
			Limits: Limits{MaxActive: 8, MaxRetained: 8, MaxJanitorCandidates: 16, MaxJanitorConcurrency: 2},
		},
		Store: store, Locks: locks, GitReader: git, GitMutator: git, Initializer: initializer,
		Clock: func() time.Time { return time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal("canary manager setup failed")
	}
	return &managerFixture{
		t: t, repo: repo, common: common, control: layout.Control, layout: layout, identity: identity,
		events: events, store: store, git: git, init: initializer, locks: locks, manager: manager,
	}
}

func mustLoadT68Record(t *testing.T, fixture *managerFixture) Record {
	t.Helper()
	record, err := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	if err != nil {
		t.Fatal("canary record was unavailable")
	}
	return record
}

func assertT68SafeManagerError(t *testing.T, err error, code ErrorCode, message, canary string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a sanitized manager error")
	}
	assertT68NoCanary(t, err.Error(), canary)
	var safe *SafeError
	if !errors.As(err, &safe) || safe.Code != code || safe.Message != message || len(safe.Message) > 128 {
		t.Fatal("manager error did not use the fixed safe classification")
	}
}

func assertT68NoCanary(t *testing.T, visible, canary string) {
	t.Helper()
	if strings.Contains(visible, canary) {
		t.Fatal("observable output retained the secret canary")
	}
}

func t68WorktreeCanary() string {
	return strings.Join([]string{"t68", "opaque", "canary", "9f31"}, "-")
}
