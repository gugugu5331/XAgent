package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/worktree"
)

type worktreeCapabilityGitRunner struct {
	calls    int
	commands []worktree.GitCommand
	output   worktree.GitOutput
	err      error
}

func (runner *worktreeCapabilityGitRunner) Run(_ context.Context, command worktree.GitCommand) (worktree.GitOutput, error) {
	runner.calls++
	runner.commands = append(runner.commands, command.Clone())
	if command.CWD == "" || !filepath.IsAbs(command.CWD) {
		return worktree.GitOutput{}, errors.New("invalid test command cwd")
	}
	return runner.output, runner.err
}

func TestWorktreeUnavailableProbeExecutesNarrowReadOnlyGit(t *testing.T) {
	project := canonicalAssemblyWorktreeTestDir(t, "project")
	cache := canonicalAssemblyWorktreeTestDir(t, "cache")
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &worktreeCapabilityGitRunner{output: worktree.GitOutput{Stdout: []byte(strings.Repeat("a", 40) + "\n")}}
	available, err := probeAssemblyWorktreeCapability(context.Background(), assemblyWorktreeCapabilityOptions{
		ProjectRoot: project, UserCacheRoot: cache, Config: assemblyWorktreeGraphFixture(t).Config,
		GitExecutable: "git", GitRunner: runner, GitMaxOutputBytes: 4096,
		FilesystemProbe: func(context.Context, string, string) error { return nil },
	})
	if err != nil || !available || len(runner.commands) != 1 {
		t.Fatalf("probe = available %v commands %#v err %v", available, runner.commands, err)
	}
	command := runner.commands[0]
	if command.CWD != project || command.Executable != "git" || strings.Join(command.Args, "\x00") != "rev-parse\x00--verify\x00HEAD" {
		t.Fatalf("probe command is not the narrow read-only HEAD query: %#v", command)
	}
}

func TestWorktreeUnavailableProbeLeavesNonGitAndLinkedCheckoutSharedOnly(t *testing.T) {
	for _, test := range []struct {
		name        string
		linked      bool
		expectCalls int
	}{
		{name: "non-git"},
		{name: "linked-checkout", linked: true, expectCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := canonicalAssemblyWorktreeTestDir(t, "project")
			cache := canonicalAssemblyWorktreeTestDir(t, "cache")
			if test.linked {
				prepareValidLinkedWorktreeMetadata(t, project)
			}
			runner := &worktreeCapabilityGitRunner{output: worktree.GitOutput{Stdout: []byte(strings.Repeat("a", 40) + "\n")}}
			available, err := probeAssemblyWorktreeCapability(context.Background(), assemblyWorktreeCapabilityOptions{
				ProjectRoot: project, UserCacheRoot: cache, Config: assemblyWorktreeGraphFixture(t).Config,
				GitExecutable: "git", GitRunner: runner, GitMaxOutputBytes: 4096,
			})
			if err != nil || available || runner.calls != test.expectCalls {
				t.Fatalf("probe = available %v calls %d err %v", available, runner.calls, err)
			}
			assertNoWorktreeWritableRoots(t, project, cache)
		})
	}
}

func TestWorktreeUnavailableRejectsMalformedGitFile(t *testing.T) {
	project := canonicalAssemblyWorktreeTestDir(t, "project")
	cache := canonicalAssemblyWorktreeTestDir(t, "cache")
	if err := os.WriteFile(filepath.Join(project, ".git"), []byte("not a linked checkout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	available, err := probeAssemblyWorktreeCapability(context.Background(), assemblyWorktreeCapabilityOptions{
		ProjectRoot: project, UserCacheRoot: cache, Config: assemblyWorktreeGraphFixture(t).Config,
		GitExecutable: "git", GitMaxOutputBytes: 4096,
	})
	if err == nil || available {
		t.Fatalf("malformed .git file was downgraded: available=%v err=%v", available, err)
	}
	assertNoWorktreeWritableRoots(t, project, cache)
}

func TestWorktreeUnavailableRejectsLinkedMetadataIdentityMismatchBeforeGit(t *testing.T) {
	project := canonicalAssemblyWorktreeTestDir(t, "project")
	cache := canonicalAssemblyWorktreeTestDir(t, "cache")
	prepareValidLinkedWorktreeMetadata(t, project)
	contents, err := os.ReadFile(filepath.Join(project, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	admin := strings.TrimSpace(strings.TrimPrefix(string(contents), "gitdir: "))
	if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte(filepath.Join(project, "other")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &worktreeCapabilityGitRunner{err: errors.New("must not execute git for untrusted metadata")}
	available, err := probeAssemblyWorktreeCapability(context.Background(), assemblyWorktreeCapabilityOptions{
		ProjectRoot: project, UserCacheRoot: cache, Config: assemblyWorktreeGraphFixture(t).Config,
		GitExecutable: "git", GitRunner: runner, GitMaxOutputBytes: 4096,
	})
	if err == nil || available || runner.calls != 0 {
		t.Fatalf("identity-mismatched metadata = available=%v calls=%d err=%v", available, runner.calls, err)
	}
	assertNoWorktreeWritableRoots(t, project, cache)
}

func TestWorktreeUnavailableRejectsNonfunctionalGitExecutable(t *testing.T) {
	project := canonicalAssemblyWorktreeTestDir(t, "project")
	cache := canonicalAssemblyWorktreeTestDir(t, "cache")
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := canonicalAssemblyWorktreeTestDir(t, "bin")
	git := filepath.Join(bin, "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\nexit 127\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	available, err := probeAssemblyWorktreeCapability(context.Background(), assemblyWorktreeCapabilityOptions{
		ProjectRoot: project, UserCacheRoot: cache, Config: assemblyWorktreeGraphFixture(t).Config,
		GitExecutable: "git", GitMaxOutputBytes: 4096,
	})
	if err != nil || available {
		t.Fatalf("nonfunctional Git classification = available=%v err=%v", available, err)
	}
	assertNoWorktreeWritableRoots(t, project, cache)
}

func TestWorktreeUnavailableMissingReadonlyLinkCapabilityIsOptionalAndSideEffectFree(t *testing.T) {
	fixture := assemblyWorktreeGraphFixture(t)
	if err := os.Mkdir(filepath.Join(fixture.ProjectRoot, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	cache := canonicalAssemblyWorktreeTestDir(t, "cache")
	request := assemblyWorktreeUnavailableRequest(t, fixture, cache)
	request.Config.Subagent.Worktree.Init.Link = []worktree.LinkRule{{Source: "vendor", Target: "vendor"}}
	built, err := newAssemblySubagentWorktrees(request)
	if err != nil || built != nil {
		t.Fatalf("missing readonly-link capability = graph=%T err=%v", built, err)
	}
	assertNoWorktreeWritableRoots(t, fixture.ProjectRoot, cache)
}

func TestWorktreeUnavailableAssemblyContinuesWithoutOptionalGraphOrWritableRoots(t *testing.T) {
	for _, test := range []struct {
		name                 string
		prepareGit           func(*testing.T, string)
		withoutGitExecutable bool
	}{
		{name: "non-git"},
		{name: "linked-checkout", prepareGit: func(t *testing.T, project string) {
			prepareValidLinkedWorktreeMetadata(t, project)
		}},
		{name: "git-executable-unavailable", withoutGitExecutable: true, prepareGit: func(t *testing.T, project string) {
			if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := assemblyWorktreeGraphFixture(t)
			if test.prepareGit != nil {
				test.prepareGit(t, fixture.ProjectRoot)
			}
			if test.withoutGitExecutable {
				t.Setenv("PATH", "")
			}
			userCache := canonicalAssemblyWorktreeTestDir(t, "user-cache")
			request := assemblyWorktreeUnavailableRequest(t, fixture, userCache)
			if test.name == "linked-checkout" {
				request.GitRunner = &worktreeCapabilityGitRunner{output: worktree.GitOutput{Stdout: []byte(strings.Repeat("a", 40) + "\n")}}
			}
			built, err := newAssemblySubagentWorktrees(request)
			if err != nil || built != nil {
				t.Fatalf("optional graph = %T err %v", built, err)
			}
			assertNoWorktreeWritableRoots(t, fixture.ProjectRoot, userCache)
		})
	}
}

func prepareValidLinkedWorktreeMetadata(t *testing.T, project string) {
	t.Helper()
	common := canonicalAssemblyWorktreeTestDir(t, "common-git")
	admin := filepath.Join(common, "worktrees", "linked")
	if err := os.MkdirAll(admin, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(project, ".git"):    "gitdir: " + admin + "\n",
		filepath.Join(admin, "commondir"): "../..\n",
		filepath.Join(admin, "gitdir"):    filepath.Join(project, ".git") + "\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorktreeUnavailableProbeContainsGitAndFilesystemCapabilityFailures(t *testing.T) {
	project := canonicalAssemblyWorktreeTestDir(t, "project")
	cache := canonicalAssemblyWorktreeTestDir(t, "cache")
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(project, "private-git-stderr")
	for _, test := range []struct {
		name            string
		runner          *worktreeCapabilityGitRunner
		filesystemProbe func(context.Context, string, string) error
	}{
		{name: "git-executable", runner: &worktreeCapabilityGitRunner{err: errors.New(secret)}},
		{name: "filesystem-lock", runner: &worktreeCapabilityGitRunner{output: worktree.GitOutput{Stdout: []byte(strings.Repeat("a", 40) + "\n")}}, filesystemProbe: func(context.Context, string, string) error {
			return errors.New(secret)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			available, err := probeAssemblyWorktreeCapability(context.Background(), assemblyWorktreeCapabilityOptions{
				ProjectRoot: project, UserCacheRoot: cache, Config: assemblyWorktreeGraphFixture(t).Config,
				GitExecutable: "git", GitRunner: test.runner, GitMaxOutputBytes: 4096,
				FilesystemProbe: test.filesystemProbe,
			})
			if err != nil || available {
				t.Fatalf("probe = available %v err %v", available, err)
			}
			assertNoWorktreeWritableRoots(t, project, cache)
		})
	}
}

func TestWorktreeUnavailableProbeDoesNotMaskInvalidOrUnsafeConfiguration(t *testing.T) {
	project := canonicalAssemblyWorktreeTestDir(t, "project")
	cache := canonicalAssemblyWorktreeTestDir(t, "cache")
	config := assemblyWorktreeGraphFixture(t).Config
	config.Lifecycle.GitTimeout = -1
	if available, err := probeAssemblyWorktreeCapability(context.Background(), assemblyWorktreeCapabilityOptions{
		ProjectRoot: project, UserCacheRoot: cache, Config: config, GitExecutable: "git", GitMaxOutputBytes: 4096,
	}); err == nil || available {
		t.Fatalf("invalid config was downgraded: available=%v err=%v", available, err)
	}

	if err := os.Symlink(cache, filepath.Join(project, ".git")); err != nil {
		t.Fatal(err)
	}
	config = assemblyWorktreeGraphFixture(t).Config
	if available, err := probeAssemblyWorktreeCapability(context.Background(), assemblyWorktreeCapabilityOptions{
		ProjectRoot: project, UserCacheRoot: cache, Config: config, GitExecutable: "git", GitMaxOutputBytes: 4096,
	}); err == nil || available {
		t.Fatalf("unsafe git metadata was downgraded: available=%v err=%v", available, err)
	}
	assertNoWorktreeWritableRoots(t, project, cache)
}

func assertNoWorktreeWritableRoots(t *testing.T, project, cache string) {
	t.Helper()
	for _, path := range []string{filepath.Join(project, ".xagent"), filepath.Join(cache, "worktrees")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unavailable capability created writable root %q: %v", path, err)
		}
	}
}

func assemblyWorktreeUnavailableRequest(t *testing.T, fixture assemblyWorktreeGraphOptions, userCache string) assemblySubagentWorktreeRequest {
	t.Helper()
	return assemblySubagentWorktreeRequest{
		Paths: RuntimePaths{
			ProjectRoot:    fixture.ProjectRoot,
			UserConfigRoot: canonicalAssemblyWorktreeTestDir(t, "user-config"),
			UserDataRoot:   canonicalAssemblyWorktreeTestDir(t, "user-data"),
			UserCacheRoot:  userCache,
		},
		Config: config.AppConfig{
			Tool:     config.ToolConfig{TimeoutMS: 1000, MaxOutputBytes: 4096},
			Subagent: config.SubagentConfig{Limits: subagent.DefaultLimits(), Worktree: fixture.Config},
		},
		Registry: fixture.SourceRegistry, ResultFactory: fixture.ResultFactory, Provider: startupProvider{},
		HookSnapshot: fixture.HookSnapshot, ProcessRunner: fixture.ProcessRunner,
		RuntimeRedactor: redact.NewRuntimeRedactor(), LifecycleDiagnostics: fixture.HookEngine.Diagnostics,
		CleanupTimeout: time.Second,
	}
}
