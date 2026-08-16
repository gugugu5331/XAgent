package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	integrationWorkspaceOne  = "11111111111111111111111111111111"
	integrationWorkspaceTwo  = "22222222222222222222222222222222"
	integrationWorkspaceFail = "33333333333333333333333333333333"
	integrationOwnerOne      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	integrationOwnerTwo      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestIntegrationCreateFreezesHEADAndInitializesExactAllowlist(t *testing.T) {
	fixture := newCreationIntegrationRepository(t)
	prepareCreationIntegrationSources(t, fixture)
	if _, exit := fixture.gitExit(fixture.repository, "check-ignore", "-q", "--", "ignored/runtime.env"); exit != 0 {
		t.Fatalf("ignored-copy source is not exactly ignored: exit=%d", exit)
	}
	if _, exit := fixture.gitExit(fixture.repository, "check-ignore", "-q", "--", ".local/config.json"); exit != 1 {
		t.Fatalf("ordinary copy source unexpectedly matched ignore rules: exit=%d", exit)
	}
	baseOID := strings.TrimSpace(fixture.git(t, fixture.repository, "rev-parse", "HEAD"))

	manager := fixture.manager(t, InitConfig{
		Copy: []CopyRule{
			{Source: ".local/config.json", Target: ".runtime/config.json"},
			{Source: ".local-hooks", Target: ".hooks"},
		},
		IgnoredCopy: []CopyRule{{Source: "ignored/runtime.env", Target: ".runtime/runtime.env"}},
		Link:        []LinkRule{{Source: fixture.readonlyCache, Target: "deps/cache"}},
		GitHooks:    GitHooksRule{Enabled: true, Path: ".hooks"},
	})
	lease, err := manager.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerOne))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.BaseOID != baseOID || strings.TrimSpace(fixture.git(t, lease.Root, "rev-parse", "HEAD")) != baseOID {
		t.Fatalf("worktree HEAD was not frozen: lease=%q base=%q live=%q", lease.BaseOID, baseOID, fixture.git(t, lease.Root, "rev-parse", "HEAD"))
	}
	assertIntegrationFile(t, filepath.Join(lease.Root, "tracked.txt"), "base\n")
	assertIntegrationFile(t, filepath.Join(lease.Root, ".runtime", "config.json"), "local config\n")
	assertIntegrationFile(t, filepath.Join(lease.Root, ".runtime", "runtime.env"), "runtime secret\n")
	assertIntegrationFile(t, filepath.Join(lease.Root, "deps", "cache", "artifact.bin"), "readonly cache\n")
	linkInfo, err := os.Lstat(filepath.Join(lease.Root, "deps", "cache"))
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("readonly dependency is not a symlink: mode=%v err=%v", linkInfo, err)
	}
	linked, err := os.Readlink(filepath.Join(lease.Root, "deps", "cache"))
	wantLinked, canonicalErr := filepath.EvalSymlinks(fixture.readonlyCache)
	if err != nil || canonicalErr != nil || filepath.Clean(linked) != filepath.Clean(wantLinked) {
		t.Fatalf("readonly dependency target = %q err=%v, want %q (canonical err=%v)", linked, err, wantLinked, canonicalErr)
	}

	for _, undeclared := range []string{
		".env", "local-only.txt", "node_modules", ".local", ".local-hooks", "ignored/runtime.env",
	} {
		if _, err := os.Lstat(filepath.Join(lease.Root, filepath.FromSlash(undeclared))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("undeclared main-worktree path %q was inherited: %v", undeclared, err)
		}
	}
	mainStatus := fixture.git(t, fixture.repository, "status", "--porcelain=v1", "--untracked-files=all")
	for _, want := range []string{"MM tracked.txt", "?? .local/config.json", "?? local-only.txt"} {
		if !strings.Contains(mainStatus, want) {
			t.Fatalf("main dirty state lost %q after Acquire: %q", want, mainStatus)
		}
	}

	if got := strings.TrimSpace(fixture.git(t, lease.Root, "config", "--worktree", "--get", "core.hooksPath")); got != ".hooks" {
		t.Fatalf("target hooksPath = %q, want .hooks", got)
	}
	if output, exit := fixture.gitExit(fixture.repository, "config", "--worktree", "--get", "core.hooksPath"); exit != 1 || strings.TrimSpace(output) != "" {
		t.Fatalf("main worktree inherited hooksPath: exit=%d output=%q", exit, output)
	}
	writeIntegrationFile(t, filepath.Join(fixture.repository, "main-only.txt"), "main\n", 0o600)
	fixture.git(t, fixture.repository, "add", "main-only.txt")
	fixture.git(t, fixture.repository, "commit", "--only", "main-only.txt", "-m", "main without target hook")
	if _, err := os.Lstat(filepath.Join(fixture.repository, "hook-fired")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target worktree hook ran in main worktree: %v", err)
	}
	if got := strings.TrimSpace(fixture.git(t, lease.Root, "rev-parse", "HEAD")); got != baseOID {
		t.Fatalf("main HEAD advance changed frozen worktree HEAD: got=%q want=%q", got, baseOID)
	}
	writeIntegrationFile(t, filepath.Join(lease.Root, "target-commit.txt"), "target\n", 0o600)
	fixture.git(t, lease.Root, "add", "target-commit.txt")
	fixture.git(t, lease.Root, "commit", "-m", "target hook")
	assertIntegrationFile(t, filepath.Join(lease.Root, "hook-fired"), "target hook\n")

	otherRoot := filepath.Join(filepath.Dir(fixture.repository), "plain-worktree")
	fixture.git(t, fixture.repository, "worktree", "add", "-b", "integration-plain", otherRoot, baseOID)
	if output, exit := fixture.gitExit(otherRoot, "config", "--worktree", "--get", "core.hooksPath"); exit != 1 || strings.TrimSpace(output) != "" {
		t.Fatalf("plain worktree inherited target hooksPath: exit=%d output=%q", exit, output)
	}
	writeIntegrationFile(t, filepath.Join(otherRoot, "plain-commit.txt"), "plain\n", 0o600)
	fixture.git(t, otherRoot, "add", "plain-commit.txt")
	fixture.git(t, otherRoot, "commit", "-m", "plain without target hook")
	if _, err := os.Lstat(filepath.Join(otherRoot, "hook-fired")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target worktree hook ran in another worktree: %v", err)
	}
	if data, err := os.ReadFile(fixture.globalConfig); err != nil || len(data) != 0 {
		t.Fatalf("test global Git config changed: data=%q err=%v", data, err)
	}
}

func TestIntegrationCreateUsesUniqueBranchAndManagedPath(t *testing.T) {
	fixture := newCreationIntegrationRepository(t)
	manager := fixture.manager(t, InitConfig{})
	first, err := manager.Acquire(context.Background(), fixture.request(t, integrationWorkspaceOne, integrationOwnerOne))
	if err != nil {
		t.Fatalf("Acquire first: %v", err)
	}
	second, err := manager.Acquire(context.Background(), fixture.request(t, integrationWorkspaceTwo, integrationOwnerTwo))
	if err != nil {
		t.Fatalf("Acquire second: %v", err)
	}
	if first.Root == second.Root || first.Branch == second.Branch {
		t.Fatalf("workspaces reused path or branch: first=%#v second=%#v", first, second)
	}
	for _, lease := range []Lease{first, second} {
		layout, err := ResolveManagedLayout(fixture.repository, lease.WorkspaceID)
		if err != nil {
			t.Fatal(err)
		}
		if lease.Root != layout.WorkspaceRoot || lease.Branch != layout.Branch {
			t.Fatalf("lease escaped deterministic layout: lease=%#v layout=%#v", lease, layout)
		}
		if got := strings.TrimSpace(fixture.git(t, lease.Root, "symbolic-ref", "HEAD")); got != "refs/heads/"+lease.Branch {
			t.Fatalf("worktree branch ref = %q, want %q", got, "refs/heads/"+lease.Branch)
		}
	}
}

func TestIntegrationInitializationFailureRollsBackWorktreeAndBranch(t *testing.T) {
	fixture := newCreationIntegrationRepository(t)
	writeIntegrationFile(t, filepath.Join(fixture.repository, "copy-source.txt"), "copied before failure\n", 0o600)
	writeIntegrationFile(t, filepath.Join(fixture.repository, "not-ignored.txt"), "must fail exact ignore\n", 0o600)
	manager := fixture.manager(t, InitConfig{
		Copy:        []CopyRule{{Source: "copy-source.txt", Target: ".runtime/copied.txt"}},
		IgnoredCopy: []CopyRule{{Source: "not-ignored.txt", Target: ".runtime/not-ignored.txt"}},
	})
	request := fixture.request(t, integrationWorkspaceFail, integrationOwnerOne)
	if _, err := manager.Acquire(context.Background(), request); err == nil {
		t.Fatal("Acquire succeeded for a non-ignored ignored-copy source")
	}
	layout, err := ResolveManagedLayout(fixture.repository, integrationWorkspaceFail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(layout.WorkspaceRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed initialization retained worktree: %v", err)
	}
	ref := "refs/heads/" + layout.Branch
	if output, exit := fixture.gitExit(fixture.repository, "show-ref", "--verify", "--quiet", ref); exit != 1 || strings.TrimSpace(output) != "" {
		t.Fatalf("failed initialization retained branch %q: exit=%d output=%q", ref, exit, output)
	}
	record, err := manager.Snapshot(context.Background(), integrationWorkspaceFail)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if record.State != StateDeleted || record.Settlement.ReasonCode != "initialization_rolled_back" {
		t.Fatalf("failed initialization record was not safely deleted: %#v", record)
	}
	assertIntegrationFile(t, filepath.Join(fixture.repository, "copy-source.txt"), "copied before failure\n")
}

type creationIntegrationRepository struct {
	gitPath       string
	env           []string
	repository    string
	bareRemote    string
	globalConfig  string
	readonlyCache string
}

func newCreationIntegrationRepository(t *testing.T) *creationIntegrationRepository {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("real Git hook and readonly-symlink integration requires POSIX filesystem semantics")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git executable is unavailable")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("POSIX shell required by the real Git hook is unavailable")
	}
	root := t.TempDir()
	probeTarget := filepath.Join(root, "symlink-probe-target")
	probeLink := filepath.Join(root, "symlink-probe-link")
	if err := os.Mkdir(probeTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(probeTarget, probeLink); err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("filesystem symlink capability is unavailable: %v", err)
		}
		t.Fatalf("symlink capability probe: %v", err)
	}
	if err := os.Remove(probeLink); err != nil {
		t.Fatalf("remove symlink capability probe: %v", err)
	}
	globalConfig := filepath.Join(root, "global.gitconfig")
	systemConfig := filepath.Join(root, "system.gitconfig")
	writeIntegrationFile(t, globalConfig, "", 0o600)
	writeIntegrationFile(t, systemConfig, "", 0o600)
	fixture := &creationIntegrationRepository{
		gitPath: gitPath, repository: filepath.Join(root, "main"), bareRemote: filepath.Join(root, "remote.git"),
		globalConfig: globalConfig, readonlyCache: filepath.Join(root, "readonly-cache"),
	}
	fixture.env = isolatedIntegrationGitEnv(globalConfig, systemConfig)
	if err := os.Mkdir(fixture.repository, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.git(t, fixture.repository, "init")
	fixture.git(t, fixture.repository, "checkout", "-b", "main")
	fixture.git(t, fixture.repository, "config", "user.name", "XAgent Integration")
	fixture.git(t, fixture.repository, "config", "user.email", "xagent-integration@example.invalid")
	writeIntegrationFile(t, filepath.Join(fixture.repository, ".gitignore"), strings.Join([]string{
		"/.xagent/worktrees/", "/ignored/runtime.env", "/.env", "/node_modules/", "",
	}, "\n"), 0o600)
	writeIntegrationFile(t, filepath.Join(fixture.repository, "tracked.txt"), "base\n", 0o600)
	fixture.git(t, fixture.repository, "add", ".gitignore", "tracked.txt")
	fixture.git(t, fixture.repository, "commit", "-m", "base")
	if err := os.Mkdir(fixture.bareRemote, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.git(t, fixture.bareRemote, "init", "--bare")
	fixture.git(t, fixture.repository, "remote", "add", "origin", fixture.bareRemote)
	fixture.git(t, fixture.repository, "push", "-u", "origin", "main")
	return fixture
}

func prepareCreationIntegrationSources(t *testing.T, fixture *creationIntegrationRepository) {
	t.Helper()
	writeIntegrationFile(t, filepath.Join(fixture.repository, "tracked.txt"), "staged\n", 0o600)
	fixture.git(t, fixture.repository, "add", "tracked.txt")
	writeIntegrationFile(t, filepath.Join(fixture.repository, "tracked.txt"), "unstaged\n", 0o600)
	writeIntegrationFile(t, filepath.Join(fixture.repository, ".env"), "undeclared secret\n", 0o600)
	writeIntegrationFile(t, filepath.Join(fixture.repository, "local-only.txt"), "undeclared local\n", 0o600)
	writeIntegrationFile(t, filepath.Join(fixture.repository, "node_modules", "package.bin"), "undeclared dependency\n", 0o600)
	writeIntegrationFile(t, filepath.Join(fixture.repository, ".local", "config.json"), "local config\n", 0o600)
	writeIntegrationFile(t, filepath.Join(fixture.repository, "ignored", "runtime.env"), "runtime secret\n", 0o600)
	writeIntegrationFile(t, filepath.Join(fixture.repository, ".local-hooks", "pre-commit"), "#!/bin/sh\nprintf 'target hook\\n' > hook-fired\n", 0o755)
	writeIntegrationFile(t, filepath.Join(fixture.readonlyCache, "artifact.bin"), "readonly cache\n", 0o444)
	if err := os.Chmod(fixture.readonlyCache, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(fixture.readonlyCache, 0o755)
		_ = os.Chmod(filepath.Join(fixture.readonlyCache, "artifact.bin"), 0o644)
	})
}

func (f *creationIntegrationRepository) manager(t *testing.T, initConfig InitConfig) Manager {
	t.Helper()
	layout, err := ResolveManagedLayout(f.repository, integrationWorkspaceOne)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(layout.Control)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := NewFileLockManager(layout.Control, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	git := NewGitClient(f.gitPath, integrationGitRunner{env: f.env}, 10*time.Second, defaultGitOutputBytes)
	initializer := NewFilesystemInitializer(InitializerOptions{Git: git, ReadonlyLinks: integrationReadonlyLinks{}})
	manager, err := NewManager(ManagerOptions{
		ControlRoot: layout.Control,
		Config: Config{
			Lifecycle: LifecycleConfig{InitTimeout: 10 * time.Second, RecoveryTimeout: 10 * time.Second, SettleTimeout: 10 * time.Second},
			Limits:    Limits{MaxActive: 8, MaxRetained: 8}, Init: initConfig,
		},
		Store: store, Locks: locks, GitReader: git, GitMutator: git, Initializer: initializer,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return manager
}

func (f *creationIntegrationRepository) request(t *testing.T, workspaceID, ownerID string) AcquireRequest {
	t.Helper()
	identity, err := NewRepositoryIdentity(f.repository, filepath.Join(f.repository, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	return AcquireRequest{
		RepositoryRoot: f.repository, RepositoryIdentity: identity, LogicalName: "integration/" + workspaceID[:4],
		WorkspaceID: workspaceID, OwnerID: ownerID,
	}
}

func (f *creationIntegrationRepository) git(t *testing.T, directory string, args ...string) string {
	t.Helper()
	output, exit := f.gitExit(directory, args...)
	if exit != 0 {
		t.Fatalf("git %v in %q: exit=%d output=%s", args, directory, exit, output)
	}
	return output
}

func (f *creationIntegrationRepository) gitExit(directory string, args ...string) (string, int) {
	command := exec.Command(f.gitPath, args...)
	command.Dir = directory
	command.Env = append([]string(nil), f.env...)
	output, err := command.CombinedOutput()
	if err == nil {
		return string(output), 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return string(output), exitError.ExitCode()
	}
	return fmt.Sprintf("%s (%v)", output, err), -1
}

type integrationGitRunner struct{ env []string }

func (r integrationGitRunner) Run(ctx context.Context, command GitCommand) (GitOutput, error) {
	cmd := exec.CommandContext(ctx, command.Executable, command.Args...)
	cmd.Dir = command.CWD
	cmd.Env = append([]string(nil), r.env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := GitOutput{Stdout: append([]byte(nil), stdout.Bytes()...), Stderr: append([]byte(nil), stderr.Bytes()...)}
	if stdout.Len()+stderr.Len() > command.MaxOutputBytes {
		return GitOutput{}, &GitCommandError{ExitCode: -1, cause: errors.New("integration git output exceeded limit")}
	}
	if err == nil {
		return output, nil
	}
	exitCode := -1
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		exitCode = exitError.ExitCode()
	}
	return output, &GitCommandError{ExitCode: exitCode, cause: err}
}

type integrationReadonlyLinks struct{}

func (integrationReadonlyLinks) ProtectReadonlyLink(_ context.Context, request ReadonlyLinkRequest) error {
	if !filepath.IsAbs(request.Source) || !filepath.IsAbs(request.Target) || !filepath.IsAbs(request.WorktreeRoot) {
		return errors.New("readonly link paths are not absolute")
	}
	if !sameOrDescendant(request.WorktreeRoot, request.Target) {
		return errors.New("readonly link target escaped worktree")
	}
	info, err := os.Stat(request.Source)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o222 != 0 {
		return errors.New("readonly link source is not protected")
	}
	return nil
}

func isolatedIntegrationGitEnv(globalConfig, systemConfig string) []string {
	blocked := map[string]struct{}{
		"GIT_CONFIG_GLOBAL": {}, "GIT_CONFIG_SYSTEM": {}, "GIT_CONFIG_NOSYSTEM": {}, "GIT_TERMINAL_PROMPT": {},
		"GIT_DIR": {}, "GIT_WORK_TREE": {}, "GIT_INDEX_FILE": {}, "GIT_AUTHOR_NAME": {}, "GIT_AUTHOR_EMAIL": {},
		"GIT_COMMITTER_NAME": {}, "GIT_COMMITTER_EMAIL": {},
	}
	environment := make([]string, 0, len(os.Environ())+9)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, skip := blocked[key]; !skip {
			environment = append(environment, entry)
		}
	}
	return append(environment,
		"GIT_CONFIG_GLOBAL="+globalConfig,
		"GIT_CONFIG_SYSTEM="+systemConfig,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=XAgent Integration",
		"GIT_AUTHOR_EMAIL=xagent-integration@example.invalid",
		"GIT_COMMITTER_NAME=XAgent Integration",
		"GIT_COMMITTER_EMAIL=xagent-integration@example.invalid",
	)
}

func writeIntegrationFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func assertIntegrationFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("ReadFile(%q) = %q, %v; want %q", path, data, err, want)
	}
}
