package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	managerTestWorkspaceID = "0123456789abcdef0123456789abcdef"
	managerTestOwnerID     = "abcdefabcdefabcdefabcdefabcdefab"
	managerTestOID         = "1111111111111111111111111111111111111111"
)

func TestAcquireCreatePersistsIntentBeforeGitAndBecomesActive(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	if lease.WorkspaceID != managerTestWorkspaceID || lease.OwnerID != managerTestOwnerID || lease.Root != fixture.layout.WorkspaceRoot ||
		lease.Branch != fixture.layout.Branch || lease.BaseOID != managerTestOID || lease.AcquiredAt.IsZero() {
		t.Fatalf("unexpected lease: %#v", lease)
	}
	wantEvents := []string{
		"lock:repository", "store:list", "lock:workspace", "git:head", "store:create:creating", "git:add",
		"store:cas:initializing", "init:prepare", "store:cas:initializing", "store:cas:ready", "lock:active", "store:cas:active",
	}
	if !slices.Equal(fixture.events.snapshot(), wantEvents) {
		t.Fatalf("acquire ordering mismatch\n got: %v\nwant: %v", fixture.events.snapshot(), wantEvents)
	}
	record, err := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != StateActive || record.BaseOID != managerTestOID || record.HeadOID != managerTestOID || record.Manifest.Path == "" || !validDigest(record.Manifest.Digest) {
		t.Fatalf("active record incomplete: %#v", record)
	}
	if err := fixture.manager.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
}

func TestManagerAcquireRejectsUnsafeManagedAncestorsBeforeSideEffects(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *managerFixture)
	}{
		{
			name: "tasks regular file",
			setup: func(t *testing.T, fixture *managerFixture) {
				if err := os.WriteFile(fixture.layout.Tasks, []byte("unsafe"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "prefix symlink",
			setup: func(t *testing.T, fixture *managerFixture) {
				if err := os.Mkdir(fixture.layout.Tasks, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Dir(fixture.layout.WorkspaceRoot)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "prefix regular file",
			setup: func(t *testing.T, fixture *managerFixture) {
				if err := os.Mkdir(fixture.layout.Tasks, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Dir(fixture.layout.WorkspaceRoot), []byte("unsafe"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			test.setup(t, fixture)
			if lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest()); err == nil {
				t.Fatalf("Acquire accepted unsafe managed ancestor: %#v", lease)
			}
			if fixture.git.mutatorCalls != 0 {
				t.Fatalf("Git mutator calls = %d, want 0", fixture.git.mutatorCalls)
			}
			if record, err := fixture.store.Load(context.Background(), managerTestWorkspaceID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("unsafe ancestor created record: %#v err=%v", record, err)
			}
		})
	}
}

func TestManagerAcquireRevalidatesExactIgnoreAfterCheckBeforeSideEffects(t *testing.T) {
	fixture := newManagerFixture(t)
	fixture.git.afterCheckIgnoreCall = 1
	fixture.git.afterCheckIgnore = func() {
		ignorePath := filepath.Join(fixture.repo, ".gitignore")
		if err := os.Rename(ignorePath, ignorePath+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ignorePath, []byte("/.xagent/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest()); err == nil {
		t.Fatalf("Acquire accepted replaced .gitignore: %#v", lease)
	}
	if fixture.git.mutatorCalls != 0 {
		t.Fatalf("Git mutator calls = %d, want 0", fixture.git.mutatorCalls)
	}
	if record, err := fixture.store.Load(context.Background(), managerTestWorkspaceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replaced .gitignore created record: %#v err=%v", record, err)
	}
}

func TestManagerAcquireRevalidatesInPlaceIgnoreRewriteBeforeSideEffects(t *testing.T) {
	fixture := newManagerFixture(t)
	fixture.git.afterCheckIgnoreCall = 1
	fixture.git.afterCheckIgnore = func() {
		if err := os.WriteFile(filepath.Join(fixture.repo, ".gitignore"), []byte("/.xagent/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest()); err == nil {
		t.Fatalf("Acquire accepted rewritten .gitignore: %#v", lease)
	}
	if fixture.git.mutatorCalls != 0 {
		t.Fatalf("Git mutator calls = %d, want 0", fixture.git.mutatorCalls)
	}
	if record, err := fixture.store.Load(context.Background(), managerTestWorkspaceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rewritten .gitignore created record: %#v err=%v", record, err)
	}
}

func TestManagerAcquireRevalidatesExactIgnoreImmediatelyBeforeAddWorktree(t *testing.T) {
	fixture := newManagerFixture(t)
	fixture.git.afterCheckIgnoreCall = 3
	fixture.git.afterCheckIgnore = func() {
		ignorePath := filepath.Join(fixture.repo, ".gitignore")
		if err := os.Rename(ignorePath, ignorePath+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ignorePath, []byte("/.xagent/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest()); err == nil {
		t.Fatalf("Acquire accepted .gitignore replacement before AddWorktree: %#v", lease)
	}
	if fixture.git.mutatorCalls != 0 {
		t.Fatalf("Git mutator calls = %d, want 0", fixture.git.mutatorCalls)
	}
	if fixture.git.checkIgnoreCalls < 3 {
		t.Fatalf("CheckIgnore calls = %d, want at least 3", fixture.git.checkIgnoreCalls)
	}
	record, err := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	if err != nil {
		t.Fatalf("creating checkpoint was not retained: %v", err)
	}
	if record.State == StateActive || record.State == StateDeleted {
		t.Fatalf("unsafe ignore replacement reached terminal execution state: %#v", record)
	}
}

func TestRecoveryReadOnlyCrossValidatesAndAcquiresLease(t *testing.T) {
	fixture := newManagerFixture(t)
	created, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Release(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: "refs/heads/" + fixture.layout.Branch}}
	fixture.git.refs = []RefInfo{{Name: "refs/heads/" + fixture.layout.Branch, ObjectName: managerTestOID}}
	if err := validateGitManagementFile(fixture.layout.Root, fixture.layout.WorkspaceRoot, fixture.common, nil); err != nil {
		t.Fatalf("fixture git management file invalid: %v", err)
	}
	fixture.events = &managerEvents{}
	fixture.store.events = fixture.events
	fixture.git.events = fixture.events
	fixture.init.events = fixture.events
	fixture.locks.events = fixture.events
	mutationsBefore := fixture.git.mutatorCalls
	prepareBefore := fixture.init.prepareCall

	recovery, err := NewManager(ManagerOptions{
		ControlRoot: fixture.layout.Control,
		Config:      Config{Lifecycle: LifecycleConfig{RecoveryTimeout: time.Second}, Limits: Limits{MaxActive: 8, MaxRetained: 8}},
		Store:       fixture.store, Locks: fixture.locks, GitReader: fixture.git, Initializer: fixture.init,
		Clock: func() time.Time { return time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := fixture.acquireRequest()
	request.OwnerID = "22222222222222222222222222222222"
	lease, err := recovery.Acquire(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if lease.OwnerID != request.OwnerID || lease.Root != fixture.layout.WorkspaceRoot {
		t.Fatalf("unexpected recovered lease: %#v", lease)
	}
	if fixture.git.mutatorCalls != mutationsBefore || fixture.init.prepareCall != prepareBefore || fixture.init.verifyCall != 1 {
		t.Fatalf("recovery mutated workspace: git=%d/%d prepare=%d/%d verify=%d", fixture.git.mutatorCalls, mutationsBefore, fixture.init.prepareCall, prepareBefore, fixture.init.verifyCall)
	}
	want := []string{"lock:repository", "lock:workspace", "git:worktrees", "git:head", "git:symbolic-ref", "git:refs", "init:verify", "lock:active", "store:cas:active"}
	if !slices.Equal(fixture.events.snapshot(), want) {
		t.Fatalf("read-only recovery calls mismatch\n got: %v\nwant: %v", fixture.events.snapshot(), want)
	}
}

func TestRecoveryRejectsChangedExactIgnoreBeforeRecordUpdate(t *testing.T) {
	fixture := newManagerFixture(t)
	created, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Release(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: "refs/heads/" + fixture.layout.Branch}}
	fixture.git.refs = []RefInfo{{Name: "refs/heads/" + fixture.layout.Branch, ObjectName: managerTestOID}}
	before, err := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.repo, ".gitignore"), []byte("/.xagent/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovery := fixture.newManagerWithFault(nil)
	request := fixture.acquireRequest()
	request.OwnerID = "22222222222222222222222222222222"
	if lease, acquireErr := recovery.Acquire(context.Background(), request); acquireErr == nil {
		t.Fatalf("recovery accepted changed exact ignore: %#v", lease)
	}
	after, err := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.OwnerID != before.OwnerID || after.Lease != before.Lease {
		t.Fatalf("rejected recovery updated record: before=%#v after=%#v", before, after)
	}
}

func TestProtectedChangesRetainWorkspace(t *testing.T) {
	tests := []struct {
		name      string
		status    StatusSnapshot
		verifyErr error
		reason    string
	}{
		{name: "staged or unstaged", status: StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "1", Path: "changed.txt"}}}, reason: "protected_changes"},
		{name: "conflict", status: StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "u", Path: "conflict.txt"}}}, reason: "protected_changes"},
		{name: "intent to add", status: StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "1", Path: "intent.txt"}}}, reason: "protected_changes"},
		{name: "untracked nested repository", status: StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "?", Path: "nested"}}}, reason: "protected_changes"},
		{name: "extra ignored", status: StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "!", Path: "extra.log"}}}, reason: "protected_changes"},
		{name: "initialization fingerprint changed", verifyErr: ErrIntegrityMismatch, reason: "initialization_changed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
			if err != nil {
				t.Fatal(err)
			}
			fixture.git.status = test.status
			fixture.init.verifyErr = test.verifyErr
			settlement, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
			if err != nil {
				t.Fatal(err)
			}
			if settlement.State != SettlementRetained || !settlement.Dirty || settlement.ReasonCode != test.reason {
				t.Fatalf("unexpected settlement: %#v", settlement)
			}
			record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
			if record.State != StateRetained || fixture.git.removeCalls != 0 || fixture.git.deleteRefCalls != 0 {
				t.Fatalf("protected workspace was not retained: record=%#v remove=%d ref=%d", record, fixture.git.removeCalls, fixture.git.deleteRefCalls)
			}
		})
	}
}

func TestUnpushedRequiresProvableRemoteTrackingReachability(t *testing.T) {
	newHead := "2222222222222222222222222222222222222222"
	tests := []struct {
		name     string
		upstream string
		refs     []RefInfo
		ancestor bool
	}{
		{name: "no upstream"},
		{name: "local upstream", upstream: "refs/heads/main", refs: []RefInfo{{Name: "refs/heads/main", ObjectName: newHead}}, ancestor: true},
		{name: "missing remote tracking", upstream: "refs/remotes/origin/task"},
		{name: "tip not reachable", upstream: "refs/remotes/origin/task", refs: []RefInfo{{Name: "refs/remotes/origin/task", ObjectName: managerTestOID}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
			if err != nil {
				t.Fatal(err)
			}
			branchRef := "refs/heads/" + fixture.layout.Branch
			fixture.git.head = newHead
			fixture.git.refs = append([]RefInfo{{Name: branchRef, ObjectName: newHead, Upstream: test.upstream}}, test.refs...)
			fixture.git.ancestor = test.ancestor
			settlement, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
			if err != nil {
				t.Fatal(err)
			}
			if settlement.State != SettlementRetained || !settlement.Unpushed || settlement.ReasonCode != "unpushed_commits" {
				t.Fatalf("unsafe upstream was not retained: %#v", settlement)
			}
		})
	}
}

func TestSafeDeleteUsesNonForceRemoveAndExpectedOIDRefCAS(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	branchRef := "refs/heads/" + fixture.layout.Branch
	fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
	fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
	settlement, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if settlement.State != SettlementDeleted || settlement.Dirty || settlement.Unpushed || settlement.ReasonCode != "clean" {
		t.Fatalf("unexpected clean settlement: %#v", settlement)
	}
	if fixture.git.removeCalls != 1 || fixture.git.deleteRefCalls != 1 || fixture.git.deletedRef != branchRef || fixture.git.deletedExpectedOID != managerTestOID {
		t.Fatalf("safe delete calls mismatch: remove=%d ref=%d deleted=%q oid=%q", fixture.git.removeCalls, fixture.git.deleteRefCalls, fixture.git.deletedRef, fixture.git.deletedExpectedOID)
	}
	if _, err := os.Lstat(fixture.layout.WorkspaceRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree directory still exists: %v", err)
	}
	record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	if record.State != StateDeleted || record.Settlement.State != SettlementDeleted || fixture.store.tombstones != 1 {
		t.Fatalf("deleted record/tombstone missing: record=%#v tombstones=%d", record, fixture.store.tombstones)
	}
}

func TestCleanPushedCommitIsSafelyDeleted(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	newHead := "2222222222222222222222222222222222222222"
	branchRef := "refs/heads/" + fixture.layout.Branch
	upstream := "refs/remotes/origin/task"
	fixture.git.head = newHead
	fixture.git.ancestor = true
	fixture.git.refs = []RefInfo{
		{Name: branchRef, ObjectName: newHead, Upstream: upstream},
		{Name: upstream, ObjectName: newHead},
	}
	fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: newHead, Branch: branchRef}}
	settlement, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
	if err != nil || settlement.State != SettlementDeleted || fixture.git.deletedExpectedOID != newHead {
		t.Fatalf("provably pushed commit not deleted: settlement=%#v oid=%q err=%v", settlement, fixture.git.deletedExpectedOID, err)
	}
}

func TestTemporaryBranchUsedByAnotherWorktreeIsRetained(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	branchRef := "refs/heads/" + fixture.layout.Branch
	fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
	fixture.git.worktrees = []WorktreeInfo{
		{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef},
		{Path: filepath.Join(fixture.repo, "other"), HEAD: managerTestOID, Branch: branchRef},
	}
	settlement, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
	if err != nil || settlement.State != SettlementRetained || settlement.ReasonCode != "worktree_in_use" || fixture.git.removeCalls != 0 {
		t.Fatalf("branch in use was not retained: settlement=%#v remove=%d err=%v", settlement, fixture.git.removeCalls, err)
	}
}

func TestSettleTimeoutRetainsUnknownState(t *testing.T) {
	fixture := newManagerFixture(t)
	manager, err := NewManager(ManagerOptions{
		ControlRoot: fixture.layout.Control,
		Config:      Config{Lifecycle: LifecycleConfig{InitTimeout: time.Second, SettleTimeout: 25 * time.Millisecond}, Limits: Limits{MaxActive: 8, MaxRetained: 8}},
		Store:       fixture.store, Locks: fixture.locks, GitReader: fixture.git, GitMutator: fixture.git, Initializer: fixture.init,
		Clock: func() time.Time { return time.Date(2026, 8, 16, 6, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	fixture.git.blockStatus = true
	started := time.Now()
	parent, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	settlement, err := manager.Settle(parent, lease, SettleRequest{RuntimeStopped: true})
	if err != nil || settlement.State != SettlementRetained || settlement.ReasonCode != "inspection_unknown" {
		t.Fatalf("settle timeout did not retain unknown state: settlement=%#v err=%v", settlement, err)
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond || elapsed > 100*time.Millisecond {
		t.Fatalf("settle timeout not applied: %v", elapsed)
	}
}

func TestManifestWriteRejectsDirectoryReplacementWithoutExternalWrite(t *testing.T) {
	fixture := newManagerFixture(t)
	external := t.TempDir()
	var once sync.Once
	manager, err := NewManager(ManagerOptions{
		ControlRoot: fixture.layout.Control,
		Config:      Config{Lifecycle: LifecycleConfig{InitTimeout: time.Second}, Limits: Limits{MaxActive: 8, MaxRetained: 8}},
		Store:       fixture.store, Locks: fixture.locks, GitReader: fixture.git, GitMutator: fixture.git, Initializer: fixture.init,
		Clock: func() time.Time { return time.Date(2026, 8, 16, 4, 0, 0, 0, time.UTC) },
		BeforeManifestWrite: func() {
			once.Do(func() {
				manifests := filepath.Join(fixture.layout.Control, "manifests")
				if renameErr := os.Rename(manifests, manifests+"-old"); renameErr != nil {
					t.Fatal(renameErr)
				}
				if symlinkErr := os.Symlink(external, manifests); symlinkErr != nil {
					t.Fatal(symlinkErr)
				}
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Acquire(context.Background(), fixture.acquireRequest()); err == nil {
		t.Fatal("manifest parent replacement must fail closed")
	}
	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("manifest replacement wrote outside control root: %v", entries)
	}
}

func TestManifestCheckpointsRemainBoundedOnNormalInitialization(t *testing.T) {
	fixture := newManagerFixture(t)
	const checkpoints = 32
	fixture.init.checkpointCount = checkpoints
	if _, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(fixture.layout.Control, "manifests"))
	if err != nil {
		t.Fatal(err)
	}
	// Each callback snapshot is immutable and uniquely named (the Manager only
	// adds a final snapshot when the initializer emitted no checkpoint).
	// Automatic unlink is deliberately disabled because no supported platform
	// can atomically bind unlink to the inode that was verified.
	if len(entries) != checkpoints {
		t.Fatalf("manifest checkpoint count escaped the bounded initialization plan: %d files", len(entries))
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	if total > int64(len(entries))*maxMetadataBytes {
		t.Fatalf("manifest checkpoints exceeded bounded metadata budget: %d", total)
	}
}

func TestRecoveryRejectsWorktreeDirectoryReplacementDuringManagementRead(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	branchRef := "refs/heads/" + fixture.layout.Branch
	fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
	fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
	external := t.TempDir()
	externalGitDir := filepath.Join(fixture.common, "worktrees", "external")
	if err := os.MkdirAll(externalGitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, ".git"), []byte("gitdir: "+externalGitDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := fixture.layout.WorkspaceRoot + "-original"
	var once sync.Once
	restarted, err := NewManager(ManagerOptions{
		ControlRoot: fixture.layout.Control,
		Config:      Config{Lifecycle: LifecycleConfig{RecoveryTimeout: time.Second}, Limits: Limits{MaxActive: 8, MaxRetained: 8}},
		Store:       fixture.store, Locks: fixture.locks, GitReader: fixture.git, Initializer: fixture.init,
		BeforeManagementRead: func() {
			once.Do(func() {
				if renameErr := os.Rename(fixture.layout.WorkspaceRoot, backup); renameErr != nil {
					t.Fatal(renameErr)
				}
				if symlinkErr := os.Symlink(external, fixture.layout.WorkspaceRoot); symlinkErr != nil {
					t.Fatal(symlinkErr)
				}
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Acquire(context.Background(), fixture.acquireRequest()); !errors.Is(err, ErrRecoveryRejected) {
		t.Fatalf("worktree replacement during management read was accepted: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(external, ".git"))
	if err != nil || string(data) != "gitdir: "+externalGitDir+"\n" {
		t.Fatalf("external management file was modified: %q err=%v", data, err)
	}
}

func TestReleaseRequiresExactLease(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	forged := lease
	forged.Root = filepath.Join(fixture.repo, "forged")
	if err := fixture.manager.Release(context.Background(), forged); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("forged lease must be rejected: %v", err)
	}
	if fixture.locks.activeUnlocks != 0 {
		t.Fatal("forged lease released the real active lock")
	}
	if err := fixture.manager.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if fixture.locks.activeUnlocks != 1 {
		t.Fatalf("real lease did not release exactly once: %d", fixture.locks.activeUnlocks)
	}
}

func TestShutdownLinearizesWithAcquireAdmission(t *testing.T) {
	fixture := newManagerFixture(t)
	headEntered := make(chan struct{})
	fixture.git.headEntered = headEntered
	fixture.git.headRelease = make(chan struct{})
	acquireDone := make(chan error, 1)
	go func() {
		_, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
		acquireDone <- err
	}()
	<-headEntered
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- fixture.manager.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before in-flight Acquire linearized: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(fixture.git.headRelease)
	if err := <-acquireDone; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if fixture.locks.activeUnlocks != 1 {
		t.Fatalf("Shutdown did not release admitted lease: %d", fixture.locks.activeUnlocks)
	}
	if _, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest()); !errors.Is(err, ErrManagerInvalid) {
		t.Fatalf("Acquire admitted after Shutdown: %v", err)
	}
}

func TestSafeDeleteRevalidatesGitManagementFile(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	branchRef := "refs/heads/" + fixture.layout.Branch
	fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
	fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
	if err := os.WriteFile(filepath.Join(fixture.layout.WorkspaceRoot, ".git"), []byte("gitdir: /outside/repository\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settlement, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if settlement.State != SettlementManualAttention || fixture.git.removeCalls != 0 {
		t.Fatalf("unsafe git management pointer was deleted: settlement=%#v remove=%d", settlement, fixture.git.removeCalls)
	}
}

func TestSafeDeleteReinspectsProtectedChangesUnderDeleteLease(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	branchRef := "refs/heads/" + fixture.layout.Branch
	fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
	fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
	fixture.locks.afterDeleteLease = func() {
		fixture.git.status = StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "??", Path: "late.txt"}}}
	}
	settlement, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	if settlement.State != SettlementRetained || settlement.ReasonCode != "protected_changes" {
		t.Fatalf("late protected change was not retained: %#v", settlement)
	}
	if fixture.init.rollbackCalls != 0 || fixture.git.removeCalls != 0 || fixture.git.deleteRefCalls != 0 {
		t.Fatalf("late protected change reached destructive path: rollback=%d remove=%d delete-ref=%d", fixture.init.rollbackCalls, fixture.git.removeCalls, fixture.git.deleteRefCalls)
	}
}

func TestAcquireCrashCheckpointsPersistNonActiveClassification(t *testing.T) {
	injected := errors.New("injected manager crash")
	for _, stage := range []string{FaultCreatingRecord, FaultWorktreeAdd, FaultInitializing, FaultManifest, FaultReady, FaultActiveLease} {
		t.Run(stage, func(t *testing.T) {
			fixture := newManagerFixture(t)
			manager := fixture.newManagerWithFault(func(candidate string) error {
				if candidate == stage {
					return injected
				}
				return nil
			})
			if _, err := manager.Acquire(context.Background(), fixture.acquireRequest()); !errors.Is(err, injected) {
				t.Fatalf("checkpoint %s did not fail: %v", stage, err)
			}
			record, err := fixture.store.Load(context.Background(), managerTestWorkspaceID)
			wantState, wantSettlement := StatePartial, SettlementPartial
			if stage == FaultManifest {
				wantState, wantSettlement = StateDeleted, SettlementDeleted
			}
			if err != nil || record.State != wantState || record.Settlement.State != wantSettlement {
				t.Fatalf("checkpoint %s not classified: record=%#v err=%v", stage, record, err)
			}
		})
	}
}

func TestDeleteCrashCheckpointsPersistPartial(t *testing.T) {
	injected := errors.New("injected delete crash")
	for _, stage := range []string{FaultWorktreeRemove, FaultBranchCAS, FaultTombstone} {
		t.Run(stage, func(t *testing.T) {
			fixture := newManagerFixture(t)
			manager := fixture.newManagerWithFault(func(candidate string) error {
				if candidate == stage {
					return injected
				}
				return nil
			})
			lease, err := manager.Acquire(context.Background(), fixture.acquireRequest())
			if err != nil {
				t.Fatal(err)
			}
			branchRef := "refs/heads/" + fixture.layout.Branch
			fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
			fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
			settlement, err := manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
			if !errors.Is(err, injected) || settlement.State != SettlementPartial {
				t.Fatalf("delete checkpoint %s not partial: settlement=%#v err=%v", stage, settlement, err)
			}
			record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
			if record.State != StatePartial || record.Settlement.State != SettlementPartial {
				t.Fatalf("delete checkpoint %s record not partial: %#v", stage, record)
			}
		})
	}
}

func TestResumeConvergesSettlingRecordAfterManagerRestart(t *testing.T) {
	injected := errors.New("simulated process loss after settling")
	fixture := newManagerFixture(t)
	first := fixture.newManagerWithFault(func(stage string) error {
		if stage == FaultSettling {
			return injected
		}
		return nil
	})
	lease, err := first.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	branchRef := "refs/heads/" + fixture.layout.Branch
	fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
	fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
	if _, err := first.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); !errors.Is(err, injected) {
		t.Fatalf("settling checkpoint did not interrupt: %v", err)
	}
	record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	if record.State != StateSettling {
		t.Fatalf("crash checkpoint did not leave resumable settling record: %s", record.State)
	}
	restarted := fixture.newManagerWithFault(nil)
	settlement, err := restarted.Resume(context.Background(), managerTestWorkspaceID)
	if err != nil || settlement.State != SettlementDeleted {
		t.Fatalf("restarted manager did not converge settling record: settlement=%#v err=%v", settlement, err)
	}
}

func TestResumeConvergesDeletePartialAfterManagerRestart(t *testing.T) {
	injected := errors.New("simulated process loss after worktree removal")
	fixture := newManagerFixture(t)
	first := fixture.newManagerWithFault(func(stage string) error {
		if stage == FaultWorktreeRemove {
			return injected
		}
		return nil
	})
	lease, err := first.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	branchRef := "refs/heads/" + fixture.layout.Branch
	fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
	fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
	if _, err := first.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); !errors.Is(err, injected) {
		t.Fatalf("delete checkpoint did not interrupt: %v", err)
	}
	fixture.git.worktrees = nil // successful git worktree remove is now externally observable
	restarted := fixture.newManagerWithFault(nil)
	settlement, err := restarted.Resume(context.Background(), managerTestWorkspaceID)
	if err != nil || settlement.State != SettlementDeleted {
		t.Fatalf("restarted manager did not converge delete partial: settlement=%#v err=%v", settlement, err)
	}
	if fixture.git.deleteRefCalls != 1 || fixture.store.tombstones != 1 {
		t.Fatalf("resume did not complete expected ref/tombstone: ref=%d tombstones=%d", fixture.git.deleteRefCalls, fixture.store.tombstones)
	}
}

func TestResumeRejectsUnapprovedPartialAndIsIdempotentForTerminalRecord(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	record.State = StatePartial
	record.Lease = LeaseRecord{}
	record.Settlement = SettlementRecord{State: SettlementPartial, ReasonCode: "acquire_failed", CompletedAt: time.Now().UTC()}
	if err := fixture.store.CompareAndSwap(context.Background(), record, record.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.Resume(context.Background(), managerTestWorkspaceID); !errors.Is(err, ErrSettlementUnavailable) {
		t.Fatalf("unapproved creation partial was resumed: %v", err)
	}
	if fixture.git.removeCalls != 0 || fixture.git.deleteRefCalls != 0 {
		t.Fatalf("unapproved partial reached destructive path: remove=%d ref=%d", fixture.git.removeCalls, fixture.git.deleteRefCalls)
	}

	terminal := record.Clone()
	terminal.Revision++
	terminal.State = StateManualAttention
	terminal.Settlement = SettlementRecord{State: SettlementManualAttention, ReasonCode: "operator_required", CompletedAt: time.Now().UTC()}
	fixture.store.mu.Lock()
	fixture.store.records[managerTestWorkspaceID] = terminal
	fixture.store.mu.Unlock()
	settlement, err := fixture.manager.Resume(context.Background(), managerTestWorkspaceID)
	if err != nil || settlement.State != SettlementManualAttention {
		t.Fatalf("terminal Resume was not idempotent: settlement=%#v err=%v", settlement, err)
	}
	if _, err := fixture.manager.Resume(context.Background(), "../invalid"); !errors.Is(err, ErrManagerInvalid) {
		t.Fatalf("invalid workspace id accepted by Resume: %v", err)
	}
}

func TestResumeUnknownStateRetainsAndConcurrentResumeAdvancesOnce(t *testing.T) {
	t.Run("unknown protected state", func(t *testing.T) {
		injected := errors.New("settling crash")
		fixture := newManagerFixture(t)
		first := fixture.newManagerWithFault(func(stage string) error {
			if stage == FaultSettling {
				return injected
			}
			return nil
		})
		lease, err := first.Acquire(context.Background(), fixture.acquireRequest())
		if err != nil {
			t.Fatal(err)
		}
		branchRef := "refs/heads/" + fixture.layout.Branch
		fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
		fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
		if _, err := first.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); !errors.Is(err, injected) {
			t.Fatal(err)
		}
		fixture.git.status = StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "??", Path: "unknown.txt"}}}
		settlement, err := fixture.newManagerWithFault(nil).Resume(context.Background(), managerTestWorkspaceID)
		if err != nil || settlement.State != SettlementRetained || fixture.git.removeCalls != 0 {
			t.Fatalf("unknown restart state did not fail closed: settlement=%#v remove=%d err=%v", settlement, fixture.git.removeCalls, err)
		}
	})

	t.Run("concurrent convergence", func(t *testing.T) {
		injected := errors.New("settling crash")
		fixture := newManagerFixture(t)
		first := fixture.newManagerWithFault(func(stage string) error {
			if stage == FaultSettling {
				return injected
			}
			return nil
		})
		lease, err := first.Acquire(context.Background(), fixture.acquireRequest())
		if err != nil {
			t.Fatal(err)
		}
		branchRef := "refs/heads/" + fixture.layout.Branch
		fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
		fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
		if _, err := first.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); !errors.Is(err, injected) {
			t.Fatal(err)
		}
		managers := []Manager{fixture.newManagerWithFault(nil), fixture.newManagerWithFault(nil)}
		results := make(chan error, len(managers))
		for _, manager := range managers {
			go func(candidate Manager) {
				settlement, err := candidate.Resume(context.Background(), managerTestWorkspaceID)
				if err == nil && settlement.State != SettlementDeleted {
					err = fmt.Errorf("unexpected settlement: %s", settlement.State)
				}
				results <- err
			}(manager)
		}
		for range managers {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		if fixture.git.removeCalls != 1 || fixture.git.deleteRefCalls != 1 {
			t.Fatalf("concurrent Resume advanced destructive stages more than once: remove=%d ref=%d", fixture.git.removeCalls, fixture.git.deleteRefCalls)
		}
	})
}

func TestCollectRequiresTTLAndRechecksRetainedWorkspaceWithoutRefreshingTerminalTime(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	fixture.git.status = StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "??", Path: "kept.txt"}}}
	retained, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
	if err != nil || retained.State != SettlementRetained {
		t.Fatalf("fixture did not retain: settlement=%#v err=%v", retained, err)
	}
	record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	firstTerminal := record.Settlement.CompletedAt
	clock := firstTerminal.Add(2 * time.Hour)
	collector, err := NewManager(ManagerOptions{
		ControlRoot: fixture.layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{RetentionTTL: time.Hour, SettleTimeout: time.Second},
			Limits:    Limits{MaxActive: 8, MaxRetained: 8},
		},
		Store: fixture.store, Locks: fixture.locks, GitReader: fixture.git, GitMutator: fixture.git, Initializer: fixture.init,
		Clock: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	settlement, err := collector.Collect(context.Background(), managerTestWorkspaceID)
	if err != nil || settlement.State != SettlementRetained {
		t.Fatalf("dirty retained workspace was not preserved: settlement=%#v err=%v", settlement, err)
	}
	rechecked, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	if !rechecked.Settlement.CompletedAt.Equal(firstTerminal) {
		t.Fatalf("failed collection refreshed authoritative TTL: first=%v after=%v", firstTerminal, rechecked.Settlement.CompletedAt)
	}

	fixture.git.status = StatusSnapshot{}
	branchRef := "refs/heads/" + fixture.layout.Branch
	fixture.git.worktrees = []WorktreeInfo{{Path: fixture.layout.WorkspaceRoot, HEAD: managerTestOID, Branch: branchRef}}
	fixture.git.refs = []RefInfo{{Name: branchRef, ObjectName: managerTestOID}}
	settlement, err = collector.Collect(context.Background(), managerTestWorkspaceID)
	if err != nil || settlement.State != SettlementDeleted {
		t.Fatalf("expired clean retained workspace was not collected: settlement=%#v err=%v", settlement, err)
	}
}

func TestCollectRejectsFreshAndUnauthorizedStates(t *testing.T) {
	fixture := newManagerFixture(t)
	lease, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest())
	if err != nil {
		t.Fatal(err)
	}
	fixture.git.status = StatusSnapshot{Dirty: true, Entries: []StatusEntry{{Code: "??", Path: "kept.txt"}}}
	if _, err := fixture.manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true}); err != nil {
		t.Fatal(err)
	}
	collector, err := NewManager(ManagerOptions{
		ControlRoot: fixture.layout.Control,
		Config:      Config{Lifecycle: LifecycleConfig{RetentionTTL: 24 * time.Hour, SettleTimeout: time.Second}, Limits: Limits{MaxActive: 8, MaxRetained: 8}},
		Store:       fixture.store, Locks: fixture.locks, GitReader: fixture.git, GitMutator: fixture.git, Initializer: fixture.init,
		Clock: func() time.Time { return time.Date(2026, 8, 16, 2, 0, 0, 0, time.UTC).Add(time.Hour) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collector.Collect(context.Background(), managerTestWorkspaceID); !errors.Is(err, ErrSettlementUnavailable) {
		t.Fatalf("fresh retained workspace was collectible: %v", err)
	}
	record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
	record.State = StateManualAttention
	record.Settlement = SettlementRecord{State: SettlementManualAttention, ReasonCode: "ownership_unknown", CompletedAt: record.Settlement.CompletedAt}
	fixture.store.mu.Lock()
	fixture.store.records[record.WorkspaceID] = record
	fixture.store.mu.Unlock()
	if _, err := collector.Collect(context.Background(), managerTestWorkspaceID); !errors.Is(err, ErrSettlementUnavailable) {
		t.Fatalf("manual-attention workspace was collectible: %v", err)
	}
}

func TestInitializationFailureRollsBackOnlyProvableWorkspace(t *testing.T) {
	injected := errors.New("initializer failed")
	t.Run("provable artifacts", func(t *testing.T) {
		fixture := newManagerFixture(t)
		fixture.init.prepareErr = injected
		if _, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest()); !errors.Is(err, injected) {
			t.Fatalf("initializer error lost: %v", err)
		}
		record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
		if record.State != StateDeleted || fixture.git.removeCalls != 1 || fixture.git.deleteRefCalls != 1 {
			t.Fatalf("provable initialization failure not rolled back: record=%#v remove=%d ref=%d", record, fixture.git.removeCalls, fixture.git.deleteRefCalls)
		}
		if _, err := os.Lstat(fixture.layout.WorkspaceRoot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rolled back worktree still exists: %v", err)
		}
	})
	t.Run("unknown artifacts retained", func(t *testing.T) {
		fixture := newManagerFixture(t)
		fixture.init.prepareErr = errors.Join(injected, ErrInitializationRetained)
		if _, err := fixture.manager.Acquire(context.Background(), fixture.acquireRequest()); !errors.Is(err, injected) {
			t.Fatalf("initializer retained error lost: %v", err)
		}
		record, _ := fixture.store.Load(context.Background(), managerTestWorkspaceID)
		if record.State != StatePartial || fixture.git.removeCalls != 0 {
			t.Fatalf("unknown initialization artifacts were not preserved: record=%#v remove=%d", record, fixture.git.removeCalls)
		}
		if _, err := os.Lstat(fixture.layout.WorkspaceRoot); err != nil {
			t.Fatalf("retained worktree missing: %v", err)
		}
	})
}

type managerFixture struct {
	t        *testing.T
	repo     string
	common   string
	control  string
	layout   ManagedLayout
	identity RepositoryIdentity
	events   *managerEvents
	store    *memoryManagerStore
	git      *fakeManagerGit
	init     *fakeManagerInitializer
	locks    *fakeManagerLocks
	manager  Manager
}

func newManagerFixture(t *testing.T) *managerFixture {
	t.Helper()
	repo := t.TempDir()
	common := filepath.Join(repo, ".git")
	if err := os.Mkdir(common, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("/.xagent/worktrees/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := NewRepositoryIdentity(repo, common)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := ResolveManagedLayout(repo, managerTestWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.Control, 0o700); err != nil {
		t.Fatal(err)
	}
	events := &managerEvents{}
	store := newMemoryManagerStore(events)
	git := &fakeManagerGit{events: events, head: managerTestOID, ignored: true}
	initializer := &fakeManagerInitializer{events: events}
	locks := &fakeManagerLocks{events: events}
	manager, err := NewManager(ManagerOptions{
		ControlRoot: layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{InitTimeout: time.Second, RecoveryTimeout: time.Second, SettleTimeout: time.Second},
			Limits:    Limits{MaxActive: 8, MaxRetained: 8},
		},
		Store: store, Locks: locks, GitReader: git, GitMutator: git, Initializer: initializer,
		Clock: func() time.Time { return time.Date(2026, 8, 16, 2, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &managerFixture{t: t, repo: repo, common: common, control: layout.Control, layout: layout, identity: identity, events: events, store: store, git: git, init: initializer, locks: locks, manager: manager}
}

func (f *managerFixture) acquireRequest() AcquireRequest {
	return AcquireRequest{RepositoryRoot: f.repo, RepositoryIdentity: f.identity, LogicalName: "review/task", WorkspaceID: managerTestWorkspaceID, OwnerID: managerTestOwnerID}
}

func (f *managerFixture) newManagerWithFault(fault func(string) error) Manager {
	f.t.Helper()
	manager, err := NewManager(ManagerOptions{
		ControlRoot: f.layout.Control,
		Config:      Config{Lifecycle: LifecycleConfig{InitTimeout: time.Second, RecoveryTimeout: time.Second, SettleTimeout: time.Second}, Limits: Limits{MaxActive: 8, MaxRetained: 8}},
		Store:       f.store, Locks: f.locks, GitReader: f.git, GitMutator: f.git, Initializer: f.init, Fault: fault,
		Clock: func() time.Time { return time.Date(2026, 8, 16, 5, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return manager
}

type managerEvents struct {
	mu     sync.Mutex
	values []string
}

func (e *managerEvents) add(value string) {
	e.mu.Lock()
	e.values = append(e.values, value)
	e.mu.Unlock()
}

func (e *managerEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.values...)
}

type memoryManagerStore struct {
	mu         sync.Mutex
	events     *managerEvents
	records    map[string]Record
	tombstones int
}

func newMemoryManagerStore(events *managerEvents) *memoryManagerStore {
	return &memoryManagerStore{events: events, records: make(map[string]Record)}
}

func (s *memoryManagerStore) Create(_ context.Context, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[record.WorkspaceID]; exists {
		return ErrAlreadyExists
	}
	s.records[record.WorkspaceID] = record.Clone()
	s.events.add("store:create:" + string(record.State))
	return nil
}

func (s *memoryManagerStore) Load(_ context.Context, workspaceID string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[workspaceID]
	if !exists {
		return Record{}, ErrNotFound
	}
	return record.Clone(), nil
}

func (s *memoryManagerStore) CompareAndSwap(_ context.Context, next Record, expected uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.records[next.WorkspaceID]
	if !exists {
		return ErrNotFound
	}
	if current.Revision != expected {
		return ErrRevisionConflict
	}
	next.Revision = expected + 1
	s.records[next.WorkspaceID] = next.Clone()
	s.events.add("store:cas:" + string(next.State))
	return nil
}

func (s *memoryManagerStore) WriteTombstone(context.Context, Tombstone) error {
	s.mu.Lock()
	s.tombstones++
	s.mu.Unlock()
	s.events.add("store:tombstone")
	return nil
}

func (s *memoryManagerStore) List(_ context.Context, query ListQuery) ([]Record, error) {
	s.events.add("store:list")
	s.mu.Lock()
	defer s.mu.Unlock()
	var records []Record
	for _, record := range s.records {
		if len(query.States) == 0 || slices.Contains(query.States, record.State) {
			records = append(records, record.Clone())
		}
	}
	return records, nil
}

type fakeManagerLocks struct {
	events           *managerEvents
	activeUnlocks    int
	afterDeleteLease func()
	deleteMu         sync.Mutex
}

func (l *fakeManagerLocks) LockRepository(context.Context, RepositoryIdentity) (Unlock, error) {
	l.events.add("lock:repository")
	return func() error { return nil }, nil
}
func (l *fakeManagerLocks) LockWorkspace(context.Context, string) (Unlock, error) {
	l.events.add("lock:workspace")
	return func() error { return nil }, nil
}
func (l *fakeManagerLocks) AcquireActiveLease(context.Context, string, string) (Unlock, error) {
	l.events.add("lock:active")
	return func() error { l.activeUnlocks++; return nil }, nil
}
func (l *fakeManagerLocks) AcquireDeleteLease(context.Context, string, string) (Unlock, error) {
	l.deleteMu.Lock()
	l.events.add("lock:delete")
	if l.afterDeleteLease != nil {
		l.afterDeleteLease()
		l.afterDeleteLease = nil
	}
	return func() error { l.deleteMu.Unlock(); return nil }, nil
}

type fakeManagerGit struct {
	events               *managerEvents
	head                 string
	ignored              bool
	status               StatusSnapshot
	worktrees            []WorktreeInfo
	refs                 []RefInfo
	ancestor             bool
	mutatorCalls         int
	removeCalls          int
	deleteRefCalls       int
	deletedRef           string
	deletedExpectedOID   string
	headEntered          chan struct{}
	headRelease          chan struct{}
	blockStatus          bool
	afterCheckIgnore     func()
	afterCheckIgnoreCall int
	checkIgnoreCalls     int
}

func (g *fakeManagerGit) ResolveHEAD(context.Context, string) (string, error) {
	g.events.add("git:head")
	if g.headEntered != nil {
		close(g.headEntered)
		g.headEntered = nil
		<-g.headRelease
	}
	return g.head, nil
}
func (g *fakeManagerGit) WorktreeList(context.Context, string) ([]WorktreeInfo, error) {
	g.events.add("git:worktrees")
	return append([]WorktreeInfo(nil), g.worktrees...), nil
}
func (g *fakeManagerGit) Status(ctx context.Context, _ string) (StatusSnapshot, error) {
	if g.blockStatus {
		<-ctx.Done()
		return StatusSnapshot{}, ctx.Err()
	}
	return g.status, nil
}
func (g *fakeManagerGit) SymbolicRef(context.Context, string) (string, error) {
	g.events.add("git:symbolic-ref")
	return "refs/heads/xagent/worktree/" + managerTestWorkspaceID, nil
}
func (g *fakeManagerGit) ForEachRef(_ context.Context, _, pattern string) ([]RefInfo, error) {
	g.events.add("git:refs")
	var matched []RefInfo
	for _, ref := range g.refs {
		if ref.Name == pattern || (strings.HasSuffix(pattern, "/*") && strings.HasPrefix(ref.Name, strings.TrimSuffix(pattern, "*"))) {
			matched = append(matched, ref)
		}
	}
	return matched, nil
}
func (g *fakeManagerGit) MergeBaseIsAncestor(context.Context, string, string, string) (bool, error) {
	return g.ancestor, nil
}
func (g *fakeManagerGit) CheckIgnore(context.Context, string, string) (bool, error) {
	g.checkIgnoreCalls++
	ignored := g.ignored
	if g.afterCheckIgnore != nil && (g.afterCheckIgnoreCall == 0 || g.checkIgnoreCalls == g.afterCheckIgnoreCall) {
		g.afterCheckIgnore()
		g.afterCheckIgnore = nil
	}
	return ignored, nil
}
func (g *fakeManagerGit) ConfigGet(context.Context, string, string) (string, error) {
	return "", ErrNotFound
}
func (g *fakeManagerGit) WorktreeConfigGet(context.Context, string, string) (string, error) {
	return "", ErrNotFound
}
func (g *fakeManagerGit) AddWorktree(_ context.Context, request AddWorktreeRequest) error {
	g.mutatorCalls++
	g.events.add("git:add")
	if err := os.MkdirAll(request.Directory, 0o700); err != nil {
		return err
	}
	gitDir := filepath.Join(request.RepositoryRoot, ".git", "worktrees", filepath.Base(request.Directory))
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(request.Directory, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600)
}
func (g *fakeManagerGit) EnableWorktreeConfig(context.Context, string) error {
	g.mutatorCalls++
	return nil
}
func (g *fakeManagerGit) SetWorktreeConfig(context.Context, string, string, string) error {
	g.mutatorCalls++
	return nil
}
func (g *fakeManagerGit) RestoreWorktreeConfigExtension(context.Context, string, string, bool) error {
	g.mutatorCalls++
	return nil
}
func (g *fakeManagerGit) RestoreWorktreeHooksPath(context.Context, string, string, bool) error {
	g.mutatorCalls++
	return nil
}
func (g *fakeManagerGit) RemoveWorktree(_ context.Context, _, directory string) error {
	g.mutatorCalls++
	g.removeCalls++
	if err := os.Remove(filepath.Join(directory, ".git")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(directory)
}
func (g *fakeManagerGit) DeleteRefCAS(_ context.Context, _, ref, expectedOID string) error {
	g.mutatorCalls++
	g.deleteRefCalls++
	g.deletedRef = ref
	g.deletedExpectedOID = expectedOID
	return nil
}

type fakeManagerInitializer struct {
	events          *managerEvents
	prepareCall     int
	verifyCall      int
	verifyErr       error
	rollbackErr     error
	rollbackCalls   int
	prepareErr      error
	checkpointCount int
}

func (i *fakeManagerInitializer) Prepare(ctx context.Context, request InitRequest) (Manifest, error) {
	i.prepareCall++
	i.events.add("init:prepare")
	manifest := Manifest{SchemaVersion: ManifestSchemaVersion, WorkspaceID: request.WorkspaceID, CreatedAt: time.Date(2026, 8, 16, 2, 0, 0, 0, time.UTC)}
	if request.RecordManifest != nil {
		count := i.checkpointCount
		if count == 0 {
			count = 1
		}
		for checkpoint := 0; checkpoint < count; checkpoint++ {
			if err := request.RecordManifest(ctx, manifest); err != nil {
				return manifest, err
			}
		}
	}
	return manifest, i.prepareErr
}
func (i *fakeManagerInitializer) Verify(context.Context, VerifyRequest) error {
	i.verifyCall++
	i.events.add("init:verify")
	return i.verifyErr
}
func (i *fakeManagerInitializer) Rollback(context.Context, RollbackRequest) error {
	i.rollbackCalls++
	return i.rollbackErr
}

var (
	_ Store       = (*memoryManagerStore)(nil)
	_ LockManager = (*fakeManagerLocks)(nil)
	_ GitReader   = (*fakeManagerGit)(nil)
	_ GitMutator  = (*fakeManagerGit)(nil)
	_ Initializer = (*fakeManagerInitializer)(nil)
	_             = errors.Is
	_             = strings.Contains
)
