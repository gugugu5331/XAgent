package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIntegrationRecoveryIsReadOnlyAndDoesNotReinitialize(t *testing.T) {
	fixture := newCreationIntegrationRepository(t)
	writeIntegrationFile(t, filepath.Join(fixture.repository, "recovery-source.txt"), "frozen recovery data\n", 0o600)
	initConfig := InitConfig{Copy: []CopyRule{{Source: "recovery-source.txt", Target: ".runtime/recovery.txt"}}}
	creator, _ := fixture.lifecycleManager(t, initConfig, nil, true, nil)
	created, err := creator.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerOne))
	if err != nil {
		t.Fatal(err)
	}
	if err := creator.Release(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	initializedPath := filepath.Join(created.Root, ".runtime", "recovery.txt")
	beforeInfo, err := os.Lstat(initializedPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeStatus := fixture.git(t, created.Root, "status", "--porcelain=v2", "--untracked-files=all", "--ignored=matching")
	beforeConfig, beforeConfigExit := fixture.gitExit(fixture.repository, "config", "--get", "extensions.worktreeConfig")

	recovery, recorder := fixture.lifecycleManager(t, initConfig, nil, true, nil)
	request := fixture.request(t, integrationWorkspaceOne, integrationOwnerTwo)
	recovered, err := recovery.Acquire(context.Background(), request)
	if err != nil {
		t.Fatalf("read-only recovery: %v", err)
	}
	if recovered.Root != created.Root || recovered.Branch != created.Branch || recovered.BaseOID != created.BaseOID {
		t.Fatalf("recovered lease changed workspace identity: created=%#v recovered=%#v", created, recovered)
	}
	afterInfo, err := os.Lstat(initializedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeInfo, afterInfo) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf("recovery recreated initialized output: before=%#v after=%#v", beforeInfo, afterInfo)
	}
	assertIntegrationFile(t, initializedPath, "frozen recovery data\n")
	if afterStatus := fixture.git(t, created.Root, "status", "--porcelain=v2", "--untracked-files=all", "--ignored=matching"); afterStatus != beforeStatus {
		t.Fatalf("recovery changed worktree status:\nbefore=%q\nafter=%q", beforeStatus, afterStatus)
	}
	if afterConfig, afterConfigExit := fixture.gitExit(fixture.repository, "config", "--get", "extensions.worktreeConfig"); afterConfig != beforeConfig || afterConfigExit != beforeConfigExit {
		t.Fatalf("recovery changed repository config: before=(%q,%d) after=(%q,%d)", beforeConfig, beforeConfigExit, afterConfig, afterConfigExit)
	}
	if commands := recorder.Commands(); len(commands) == 0 {
		t.Fatal("successful recovery performed no authoritative Git checks")
	} else {
		assertRecoveryGitCommandsReadOnly(t, commands)
	}
}

func TestIntegrationRecoveryRejectsChangedAuthorityWithoutRepair(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *creationIntegrationRepository, Manager, Lease) string
	}{
		{
			name: "initialization output",
			mutate: func(t *testing.T, _ *creationIntegrationRepository, _ Manager, lease Lease) string {
				path := filepath.Join(lease.Root, ".runtime", "recovery.txt")
				writeIntegrationFile(t, path, "operator changed output\n", 0o600)
				return path
			},
		},
		{
			name: "manifest",
			mutate: func(t *testing.T, fixture *creationIntegrationRepository, manager Manager, lease Lease) string {
				record, err := manager.Snapshot(context.Background(), lease.WorkspaceID)
				if err != nil {
					t.Fatal(err)
				}
				layout, err := ResolveManagedLayout(fixture.repository, lease.WorkspaceID)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(layout.Control, filepath.FromSlash(record.Manifest.Path))
				writeIntegrationFile(t, path, "{corrupt manifest", 0o600)
				return path
			},
		},
		{
			name: "git management file",
			mutate: func(t *testing.T, _ *creationIntegrationRepository, _ Manager, lease Lease) string {
				path := filepath.Join(lease.Root, ".git")
				writeIntegrationFile(t, path, "gitdir: /outside/xagent-test\n", 0o600)
				return path
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCreationIntegrationRepository(t)
			writeIntegrationFile(t, filepath.Join(fixture.repository, "recovery-source.txt"), "frozen recovery data\n", 0o600)
			initConfig := InitConfig{Copy: []CopyRule{{Source: "recovery-source.txt", Target: ".runtime/recovery.txt"}}}
			creator, _ := fixture.lifecycleManager(t, initConfig, nil, true, nil)
			created, err := creator.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerOne))
			if err != nil {
				t.Fatal(err)
			}
			if err := creator.Release(context.Background(), created); err != nil {
				t.Fatal(err)
			}
			changedPath := test.mutate(t, fixture, creator, created)
			before, err := os.ReadFile(changedPath)
			if err != nil {
				t.Fatal(err)
			}
			recovery, recorder := fixture.lifecycleManager(t, initConfig, nil, true, nil)
			_, err = recovery.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerTwo))
			if !errors.Is(err, ErrRecoveryRejected) {
				t.Fatalf("changed authority was not rejected: %v", err)
			}
			after, readErr := os.ReadFile(changedPath)
			if readErr != nil || string(after) != string(before) {
				t.Fatalf("recovery repaired or overwrote changed authority: before=%q after=%q err=%v", before, after, readErr)
			}
			assertRecoveryGitCommandsReadOnly(t, recorder.Commands())
		})
	}
}

func TestIntegrationSettlementRetainsProtectedGitStates(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *creationIntegrationRepository, Lease)
	}{
		{
			name: "staged",
			prepare: func(t *testing.T, fixture *creationIntegrationRepository, lease Lease) {
				writeIntegrationFile(t, filepath.Join(lease.Root, "tracked.txt"), "staged change\n", 0o600)
				fixture.git(t, lease.Root, "add", "tracked.txt")
			},
		},
		{
			name: "unstaged",
			prepare: func(t *testing.T, _ *creationIntegrationRepository, lease Lease) {
				writeIntegrationFile(t, filepath.Join(lease.Root, "tracked.txt"), "unstaged change\n", 0o600)
			},
		},
		{
			name: "untracked",
			prepare: func(t *testing.T, _ *creationIntegrationRepository, lease Lease) {
				writeIntegrationFile(t, filepath.Join(lease.Root, "untracked.txt"), "untracked\n", 0o600)
			},
		},
		{
			name: "ignored not covered by manifest",
			prepare: func(t *testing.T, _ *creationIntegrationRepository, lease Lease) {
				writeIntegrationFile(t, filepath.Join(lease.Root, ".env"), "unknown ignored\n", 0o600)
			},
		},
		{
			name: "intent to add",
			prepare: func(t *testing.T, fixture *creationIntegrationRepository, lease Lease) {
				writeIntegrationFile(t, filepath.Join(lease.Root, "intent.txt"), "intent\n", 0o600)
				fixture.git(t, lease.Root, "add", "-N", "intent.txt")
			},
		},
		{
			name:    "conflict",
			prepare: prepareIntegrationConflict,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCreationIntegrationRepository(t)
			manager, recorder := fixture.lifecycleManager(t, InitConfig{}, nil, true, nil)
			lease, err := manager.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerOne))
			if err != nil {
				t.Fatal(err)
			}
			test.prepare(t, fixture, lease)
			recorder.Reset()
			settlement, err := manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
			if err != nil {
				t.Fatal(err)
			}
			assertIntegrationSettlementRetained(t, fixture, lease, settlement, "protected_changes", true, false)
			assertNoDestructiveSettlementCommands(t, recorder.Commands())
		})
	}
}

func TestIntegrationSettlementRetainsDirtySubmodule(t *testing.T) {
	fixture := newCreationIntegrationRepository(t)
	submodule := filepath.Join(filepath.Dir(fixture.repository), "submodule-source")
	if err := os.Mkdir(submodule, 0o700); err != nil {
		t.Fatal(err)
	}
	if output, exit := fixture.gitExit(submodule, "init"); exit != 0 {
		t.Skipf("git submodule repository capability unavailable: exit=%d output=%s", exit, output)
	}
	fixture.git(t, submodule, "config", "user.name", "XAgent Integration")
	fixture.git(t, submodule, "config", "user.email", "xagent-integration@example.invalid")
	writeIntegrationFile(t, filepath.Join(submodule, "module.txt"), "module base\n", 0o600)
	fixture.git(t, submodule, "add", "module.txt")
	fixture.git(t, submodule, "commit", "-m", "module base")
	if output, exit := fixture.gitExit(fixture.repository, "-c", "protocol.file.allow=always", "submodule", "add", submodule, "modules/sub"); exit != 0 {
		t.Skipf("local file submodule capability unavailable: exit=%d output=%s", exit, output)
	}
	fixture.git(t, fixture.repository, "commit", "-m", "add submodule")

	manager, recorder := fixture.lifecycleManager(t, InitConfig{}, nil, true, nil)
	lease, err := manager.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerOne))
	if err != nil {
		t.Fatal(err)
	}
	if output, exit := fixture.gitExit(lease.Root, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "modules/sub"); exit != 0 {
		t.Skipf("submodule checkout capability unavailable: exit=%d output=%s", exit, output)
	}
	writeIntegrationFile(t, filepath.Join(lease.Root, "modules", "sub", "module.txt"), "dirty module\n", 0o600)
	recorder.Reset()
	settlement, err := manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	assertIntegrationSettlementRetained(t, fixture, lease, settlement, "protected_changes", true, false)
	assertNoDestructiveSettlementCommands(t, recorder.Commands())
}

func TestIntegrationSettlementRetainsUnpushedCommits(t *testing.T) {
	tests := []struct {
		name        string
		setUpstream bool
	}{
		{name: "no upstream"},
		{name: "local branch ahead of remote tracking", setUpstream: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCreationIntegrationRepository(t)
			manager, recorder := fixture.lifecycleManager(t, InitConfig{}, nil, true, nil)
			lease, err := manager.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerOne))
			if err != nil {
				t.Fatal(err)
			}
			writeIntegrationFile(t, filepath.Join(lease.Root, "local-commit.txt"), "local\n", 0o600)
			fixture.git(t, lease.Root, "add", "local-commit.txt")
			fixture.git(t, lease.Root, "commit", "-m", "local commit")
			if test.setUpstream {
				fixture.git(t, lease.Root, "branch", "--set-upstream-to=origin/main", lease.Branch)
			}
			recorder.Reset()
			settlement, err := manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
			if err != nil {
				t.Fatal(err)
			}
			assertIntegrationSettlementRetained(t, fixture, lease, settlement, "unpushed_commits", false, true)
			assertNoDestructiveSettlementCommands(t, recorder.Commands())
		})
	}
}

func TestIntegrationSettlementDeletesOnlySafeReachableStates(t *testing.T) {
	t.Run("clean base", func(t *testing.T) {
		fixture := newCreationIntegrationRepository(t)
		originBefore := fixture.git(t, fixture.bareRemote, "show-ref", "--verify", "refs/heads/main")
		manager, recorder := fixture.lifecycleManager(t, InitConfig{}, nil, true, nil)
		lease, err := manager.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerOne))
		if err != nil {
			t.Fatal(err)
		}
		recorder.Reset()
		settlement, err := manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
		if err != nil || settlement.State != SettlementDeleted || settlement.ReasonCode != "clean" {
			t.Fatalf("clean base settlement = %#v err=%v", settlement, err)
		}
		assertIntegrationDeleted(t, fixture, lease)
		if originAfter := fixture.git(t, fixture.bareRemote, "show-ref", "--verify", "refs/heads/main"); originAfter != originBefore {
			t.Fatalf("safe delete changed remote main: before=%q after=%q", originBefore, originAfter)
		}
		assertSafeLocalDeleteCommands(t, recorder.Commands())
	})

	t.Run("pushed reachable commit", func(t *testing.T) {
		fixture := newCreationIntegrationRepository(t)
		manager, recorder := fixture.lifecycleManager(t, InitConfig{}, nil, true, nil)
		lease, err := manager.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerOne))
		if err != nil {
			t.Fatal(err)
		}
		writeIntegrationFile(t, filepath.Join(lease.Root, "pushed.txt"), "pushed\n", 0o600)
		fixture.git(t, lease.Root, "add", "pushed.txt")
		fixture.git(t, lease.Root, "commit", "-m", "pushed commit")
		fixture.git(t, lease.Root, "push", "-u", "origin", "HEAD:refs/heads/integration-pushed")
		pushedOID := strings.TrimSpace(fixture.git(t, lease.Root, "rev-parse", "HEAD"))
		recorder.Reset()
		settlement, err := manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
		if err != nil || settlement.State != SettlementDeleted || settlement.ReasonCode != "clean" {
			t.Fatalf("pushed settlement = %#v err=%v", settlement, err)
		}
		assertIntegrationDeleted(t, fixture, lease)
		remote := strings.Fields(fixture.git(t, fixture.bareRemote, "show-ref", "--verify", "refs/heads/integration-pushed"))
		if len(remote) != 2 || remote[0] != pushedOID {
			t.Fatalf("safe delete removed or changed remote branch: %v want oid %q", remote, pushedOID)
		}
		assertSafeLocalDeleteCommands(t, recorder.Commands())
	})
}

func TestIntegrationSettlementFailsClosedOnDeletePhaseDrift(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		mutate func(*testing.T, *creationIntegrationRepository, Manager, Lease)
	}{
		{
			name: "branch ref", reason: "branch_moved",
			mutate: func(t *testing.T, fixture *creationIntegrationRepository, _ Manager, lease Lease) {
				writeIntegrationFile(t, filepath.Join(fixture.repository, "drift.txt"), "drift\n", 0o600)
				fixture.git(t, fixture.repository, "add", "drift.txt")
				fixture.git(t, fixture.repository, "commit", "-m", "drift oid")
				drift := strings.TrimSpace(fixture.git(t, fixture.repository, "rev-parse", "HEAD"))
				fixture.git(t, fixture.repository, "update-ref", "refs/heads/"+lease.Branch, drift, lease.BaseOID)
			},
		},
		{
			name: "git management file", reason: "git_management_mismatch",
			mutate: func(t *testing.T, _ *creationIntegrationRepository, _ Manager, lease Lease) {
				writeIntegrationFile(t, filepath.Join(lease.Root, ".git"), "gitdir: /outside/xagent-test\n", 0o600)
			},
		},
		{
			name: "manifest", reason: "manifest_mismatch",
			mutate: func(t *testing.T, fixture *creationIntegrationRepository, manager Manager, lease Lease) {
				record, err := manager.Snapshot(context.Background(), lease.WorkspaceID)
				if err != nil {
					t.Fatal(err)
				}
				layout, err := ResolveManagedLayout(fixture.repository, lease.WorkspaceID)
				if err != nil {
					t.Fatal(err)
				}
				writeIntegrationFile(t, filepath.Join(layout.Control, filepath.FromSlash(record.Manifest.Path)), "{corrupt manifest", 0o600)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCreationIntegrationRepository(t)
			var manager Manager
			mutated := false
			callback := func() {
				if mutated {
					return
				}
				mutated = true
				record, err := manager.Snapshot(context.Background(), integrationWorkspaceOne)
				if err != nil {
					t.Fatalf("Snapshot in drift callback: %v", err)
				}
				lease := Lease{WorkspaceID: record.WorkspaceID, OwnerID: record.OwnerID, Root: record.Directory, Branch: record.Branch, BaseOID: record.BaseOID, AcquiredAt: record.Lease.AcquiredAt}
				test.mutate(t, fixture, manager, lease)
			}
			var recorder *recordingIntegrationGitRunner
			manager, recorder = fixture.lifecycleManager(t, InitConfig{}, nil, true, callback)
			lease, err := manager.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerOne))
			if err != nil {
				t.Fatal(err)
			}
			recorder.Reset()
			settlement, err := manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
			if err != nil {
				t.Fatal(err)
			}
			if !mutated || settlement.State != SettlementManualAttention || settlement.ReasonCode != test.reason {
				t.Fatalf("delete-phase drift settlement = %#v mutated=%t, want manual %q", settlement, mutated, test.reason)
			}
			if _, err := os.Lstat(lease.Root); err != nil {
				t.Fatalf("manual-attention workspace was removed: %v", err)
			}
			assertNoRemoteOrForceCommands(t, recorder.Commands())
		})
	}
}

func TestIntegrationSettlementRetainsChangedInitializationOutput(t *testing.T) {
	fixture := newCreationIntegrationRepository(t)
	writeIntegrationFile(t, filepath.Join(fixture.repository, "init-source.txt"), "initial\n", 0o600)
	manager, recorder := fixture.lifecycleManager(t, InitConfig{Copy: []CopyRule{{Source: "init-source.txt", Target: ".runtime/init.txt"}}}, nil, true, nil)
	lease, err := manager.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerOne))
	if err != nil {
		t.Fatal(err)
	}
	writeIntegrationFile(t, filepath.Join(lease.Root, ".runtime", "init.txt"), "operator changed\n", 0o600)
	recorder.Reset()
	settlement, err := manager.Settle(context.Background(), lease, SettleRequest{RuntimeStopped: true})
	if err != nil {
		t.Fatal(err)
	}
	assertIntegrationSettlementRetained(t, fixture, lease, settlement, "initialization_changed", true, false)
	assertNoDestructiveSettlementCommands(t, recorder.Commands())
}

func (f *creationIntegrationRepository) lifecycleManager(
	t *testing.T,
	initConfig InitConfig,
	runner *recordingIntegrationGitRunner,
	withMutator bool,
	beforeManagementRead func(),
) (Manager, *recordingIntegrationGitRunner) {
	t.Helper()
	if runner == nil {
		runner = &recordingIntegrationGitRunner{delegate: integrationGitRunner{env: f.env}}
	}
	layout, err := ResolveManagedLayout(f.repository, integrationWorkspaceOne)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(layout.Control)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := NewFileLockManager(layout.Control, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	git := NewGitClient(f.gitPath, runner, 10*time.Second, defaultGitOutputBytes)
	initializer := NewFilesystemInitializer(InitializerOptions{Git: git, ReadonlyLinks: integrationReadonlyLinks{}})
	options := ManagerOptions{
		ControlRoot: layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{InitTimeout: 10 * time.Second, RecoveryTimeout: 10 * time.Second, SettleTimeout: 10 * time.Second},
			Limits:    Limits{MaxActive: 8, MaxRetained: 8}, Init: initConfig,
		},
		Store: store, Locks: locks, GitReader: git, Initializer: initializer, BeforeManagementRead: beforeManagementRead,
	}
	if withMutator {
		options.GitMutator = git
	}
	manager, err := NewManager(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return manager, runner
}

type recordingIntegrationGitRunner struct {
	mu       sync.Mutex
	delegate GitCommandRunner
	commands []GitCommand
}

func (r *recordingIntegrationGitRunner) Run(ctx context.Context, command GitCommand) (GitOutput, error) {
	r.mu.Lock()
	r.commands = append(r.commands, command.Clone())
	r.mu.Unlock()
	return r.delegate.Run(ctx, command)
}

func (r *recordingIntegrationGitRunner) Reset() {
	r.mu.Lock()
	r.commands = nil
	r.mu.Unlock()
}

func (r *recordingIntegrationGitRunner) Commands() []GitCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	commands := make([]GitCommand, len(r.commands))
	for index := range r.commands {
		commands[index] = r.commands[index].Clone()
	}
	return commands
}

func assertRecoveryGitCommandsReadOnly(t *testing.T, commands []GitCommand) {
	t.Helper()
	for _, command := range commands {
		if integrationGitCommandMutates(command.Args) {
			t.Fatalf("read-only recovery executed mutating Git argv: %v", command.Args)
		}
	}
}

func integrationGitCommandMutates(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "add", "am", "apply", "branch", "checkout", "clean", "commit", "config", "fetch", "merge", "mv", "pull", "push", "rebase", "reset", "restore", "rm", "switch", "update-ref":
		return true
	case "worktree":
		return len(args) < 2 || args[1] != "list"
	default:
		return false
	}
}

func prepareIntegrationConflict(t *testing.T, fixture *creationIntegrationRepository, lease Lease) {
	t.Helper()
	writeIntegrationFile(t, filepath.Join(lease.Root, "tracked.txt"), "worktree side\n", 0o600)
	fixture.git(t, lease.Root, "add", "tracked.txt")
	fixture.git(t, lease.Root, "commit", "-m", "worktree conflict side")
	writeIntegrationFile(t, filepath.Join(fixture.repository, "tracked.txt"), "main side\n", 0o600)
	fixture.git(t, fixture.repository, "add", "tracked.txt")
	fixture.git(t, fixture.repository, "commit", "-m", "main conflict side")
	if output, exit := fixture.gitExit(lease.Root, "merge", "main"); exit == 0 {
		t.Fatalf("merge unexpectedly avoided conflict: %s", output)
	}
}

func assertIntegrationSettlementRetained(
	t *testing.T,
	fixture *creationIntegrationRepository,
	lease Lease,
	settlement Settlement,
	reason string,
	dirty, unpushed bool,
) {
	t.Helper()
	if settlement.State != SettlementRetained || settlement.ReasonCode != reason || settlement.Dirty != dirty || settlement.Unpushed != unpushed {
		t.Fatalf("settlement = %#v, want retained reason=%q dirty=%t unpushed=%t", settlement, reason, dirty, unpushed)
	}
	if _, err := os.Lstat(lease.Root); err != nil {
		t.Fatalf("retained worktree missing: %v", err)
	}
	if output, exit := fixture.gitExit(fixture.repository, "show-ref", "--verify", "--quiet", "refs/heads/"+lease.Branch); exit != 0 || strings.TrimSpace(output) != "" {
		t.Fatalf("retained local branch missing: exit=%d output=%q", exit, output)
	}
}

func assertIntegrationDeleted(t *testing.T, fixture *creationIntegrationRepository, lease Lease) {
	t.Helper()
	if _, err := os.Lstat(lease.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted worktree remains: %v", err)
	}
	if output, exit := fixture.gitExit(fixture.repository, "show-ref", "--verify", "--quiet", "refs/heads/"+lease.Branch); exit != 1 || strings.TrimSpace(output) != "" {
		t.Fatalf("deleted local branch remains: exit=%d output=%q", exit, output)
	}
}

func assertNoDestructiveSettlementCommands(t *testing.T, commands []GitCommand) {
	t.Helper()
	for _, command := range commands {
		if len(command.Args) > 1 && command.Args[0] == "worktree" && command.Args[1] == "remove" {
			t.Fatalf("retained settlement removed worktree: %v", command.Args)
		}
		if len(command.Args) > 0 && command.Args[0] == "update-ref" {
			t.Fatalf("retained settlement deleted or moved ref: %v", command.Args)
		}
	}
	assertNoRemoteOrForceCommands(t, commands)
}

func assertSafeLocalDeleteCommands(t *testing.T, commands []GitCommand) {
	t.Helper()
	removeSeen := false
	deleteRefSeen := false
	for _, command := range commands {
		if len(command.Args) > 1 && command.Args[0] == "worktree" && command.Args[1] == "remove" {
			removeSeen = true
			for _, arg := range command.Args[2:] {
				if arg == "--force" || arg == "-f" {
					t.Fatalf("safe delete forced worktree removal: %v", command.Args)
				}
			}
		}
		if len(command.Args) == 4 && command.Args[0] == "update-ref" && command.Args[1] == "-d" && strings.HasPrefix(command.Args[2], "refs/heads/xagent/worktree/") && validOID(command.Args[3]) {
			deleteRefSeen = true
		}
	}
	if !removeSeen || !deleteRefSeen {
		t.Fatalf("safe local delete commands missing: remove=%t delete-ref-cas=%t commands=%v", removeSeen, deleteRefSeen, commandArgs(commands))
	}
	assertNoRemoteOrForceCommands(t, commands)
}

func assertNoRemoteOrForceCommands(t *testing.T, commands []GitCommand) {
	t.Helper()
	for _, command := range commands {
		for _, arg := range command.Args {
			if arg == "--force" || arg == "-f" {
				t.Fatalf("settlement used force argv: %v", command.Args)
			}
		}
		if len(command.Args) > 0 {
			switch command.Args[0] {
			case "fetch", "push", "pull":
				t.Fatalf("settlement accessed remote: %v", command.Args)
			}
		}
	}
}

func commandArgs(commands []GitCommand) [][]string {
	result := make([][]string, len(commands))
	for index := range commands {
		result[index] = append([]string(nil), commands[index].Args...)
	}
	return result
}
