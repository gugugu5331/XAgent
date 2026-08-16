package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const initializerWorkspaceID = "0123456789abcdef0123456789abcdef"

func TestInitCopyCopiesOnlyExplicitRegularFilesAndDirectories(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	writeInitFixture(t, filepath.Join(repositoryRoot, "config", "app.yaml"), "mode: test\n", 0o640)
	writeInitFixture(t, filepath.Join(repositoryRoot, "runtime", "nested", "state.json"), "{}\n", 0o600)
	writeInitFixture(t, filepath.Join(repositoryRoot, ".env"), "SECRET=not-copied\n", 0o600)

	initializer := NewFilesystemInitializer(InitializerOptions{Clock: func() time.Time {
		return time.Unix(100, 0).UTC()
	}})
	manifest, err := initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID:    initializerWorkspaceID,
		RepositoryRoot: repositoryRoot,
		WorktreeRoot:   worktreeRoot,
		ManagedRoot:    managedRoot,
		Config: InitConfig{Copy: []CopyRule{
			{Source: "config/app.yaml", Target: "config/app.yaml"},
			{Source: "runtime", Target: "var/runtime"},
		}},
		Limits:  Limits{MaxInitFiles: 8, MaxInitBytes: 1024, MaxInitDepth: 4},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("prepare copy: %v", err)
	}
	for path, want := range map[string]string{
		"config/app.yaml":               "mode: test\n",
		"var/runtime/nested/state.json": "{}\n",
	} {
		data, readErr := os.ReadFile(filepath.Join(worktreeRoot, filepath.FromSlash(path)))
		if readErr != nil || string(data) != want {
			t.Fatalf("copied %s = %q, %v", path, data, readErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(worktreeRoot, ".env")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unlisted .env was copied: %v", statErr)
	}
	if manifest.WorkspaceID != initializerWorkspaceID || manifest.CreatedAt.IsZero() || len(manifest.Entries) != 6 {
		t.Fatalf("manifest = %#v", manifest)
	}
	for _, entry := range manifest.Entries {
		if filepath.IsAbs(entry.Path) || strings.Contains(entry.Path, repositoryRoot) || entry.Mode != 0 {
			t.Fatalf("manifest leaked non-relative metadata: %#v", entry)
		}
		if entry.Type == ManifestFile && (entry.Size == 0 || !validDigest(entry.Digest)) {
			t.Fatalf("file manifest lacks size/digest: %#v", entry)
		}
	}
}

func TestInitCopyRejectsTraversalSymlinkSpecialPermissionsAndBudgets(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	writeInitFixture(t, filepath.Join(repositoryRoot, "large.bin"), strings.Repeat("x", 17), 0o600)
	writeInitFixture(t, filepath.Join(repositoryRoot, "special.sh"), "echo no\n", os.ModeSetuid|0o755)
	writeInitFixture(t, filepath.Join(repositoryRoot, "deep", "one", "two", "value"), "v", 0o600)
	if err := os.Symlink(filepath.Join(repositoryRoot, "large.bin"), filepath.Join(repositoryRoot, "alias")); err != nil {
		t.Fatal(err)
	}
	initializer := NewFilesystemInitializer(InitializerOptions{})
	tests := []struct {
		name   string
		rule   CopyRule
		limits Limits
		ctx    context.Context
	}{
		{name: "source traversal", rule: CopyRule{Source: "../large.bin", Target: "copy"}},
		{name: "target traversal", rule: CopyRule{Source: "large.bin", Target: "../copy"}},
		{name: "source symlink", rule: CopyRule{Source: "alias", Target: "copy"}},
		{name: "special permissions", rule: CopyRule{Source: "special.sh", Target: "copy"}},
		{name: "byte limit", rule: CopyRule{Source: "large.bin", Target: "copy"}, limits: Limits{MaxInitFiles: 2, MaxInitBytes: 16, MaxInitDepth: 2}},
		{name: "depth limit", rule: CopyRule{Source: "deep", Target: "copy"}, limits: Limits{MaxInitFiles: 8, MaxInitBytes: 64, MaxInitDepth: 2}},
		{name: "canceled", rule: CopyRule{Source: "large.bin", Target: "copy"}, ctx: canceledContext()},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := testCase.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			_, err := initializer.Prepare(ctx, InitRequest{
				WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, WorktreeRoot: worktreeRoot, ManagedRoot: managedRoot,
				Config: InitConfig{Copy: []CopyRule{testCase.rule}}, Limits: testCase.limits, Timeout: time.Second,
			})
			if err == nil {
				t.Fatal("unsafe copy unexpectedly succeeded")
			}
		})
	}
}

func TestInitCopyRejectsRuntimeReadonlySlotsAndTheirAncestors(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		config InitConfig
	}{
		{name: "copy control source", config: InitConfig{Copy: []CopyRule{{Source: ".control/value", Target: "value"}}}},
		{name: "copy control target", config: InitConfig{Copy: []CopyRule{{Source: "value", Target: ".control/value"}}}},
		{name: "copy managed ancestor source", config: InitConfig{Copy: []CopyRule{{Source: ".xagent", Target: "copied"}}}},
		{name: "copy managed target", config: InitConfig{Copy: []CopyRule{{Source: "value", Target: ".xagent/worktrees/value"}}}},
		{name: "ignored managed source", config: InitConfig{IgnoredCopy: []CopyRule{{Source: ".xagent/worktrees/value", Target: "value"}}}},
		{name: "link control target", config: InitConfig{Link: []LinkRule{{Source: "/tmp/shared", Target: ".control/dependency"}}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
			writeInitFixture(t, filepath.Join(repositoryRoot, "value"), "value", 0o600)
			writeInitFixture(t, filepath.Join(repositoryRoot, ".control", "value"), "control", 0o600)
			writeInitFixture(t, filepath.Join(repositoryRoot, ".xagent", "worktrees", "value"), "managed", 0o600)
			git := &initializerGitFixture{ignored: map[string]bool{".xagent/worktrees/value": true}}
			initializer := NewFilesystemInitializer(InitializerOptions{Git: git, ReadonlyLinks: &readonlyLinkFixture{}})
			_, err := initializer.Prepare(context.Background(), InitRequest{
				WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
				Config: testCase.config, Timeout: time.Second,
			})
			if err == nil {
				t.Fatal("runtime readonly slot was accepted")
			}
			if len(git.checkIgnore) != 0 {
				t.Fatalf("unsafe ignored rule reached git: %#v", git.checkIgnore)
			}
		})
	}
}

func TestInitCopyAllowsOnlyExplicitManagedWorktreeDescendant(t *testing.T) {
	repositoryRoot := t.TempDir()
	managedRoot := filepath.Join(repositoryRoot, ".xagent", "worktrees")
	worktreeRoot := filepath.Join(managedRoot, "tasks", initializerWorkspaceID[:2], initializerWorkspaceID)
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeInitFixture(t, filepath.Join(repositoryRoot, "local.conf"), "local\n", 0o600)
	initializer := NewFilesystemInitializer(InitializerOptions{})
	_, err := initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
		Config: InitConfig{Copy: []CopyRule{{Source: "local.conf", Target: "local.conf"}}}, Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("managed worktree copy: %v", err)
	}
	other := filepath.Join(repositoryRoot, "other-child")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: other,
		Config: InitConfig{Copy: []CopyRule{{Source: "local.conf", Target: "local.conf"}}}, Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("repository descendant outside explicit managed root was accepted")
	}

	for _, testCase := range []struct {
		name        string
		managedRoot string
		worktree    string
	}{
		{name: "missing managed root", worktree: worktreeRoot},
		{name: "wrong workspace", managedRoot: managedRoot, worktree: filepath.Join(managedRoot, "tasks", "ff", initializerWorkspaceID)},
		{name: "external worktree", managedRoot: managedRoot, worktree: filepath.Join(t.TempDir(), "external")},
		{name: "managed root contains repository", managedRoot: filepath.Dir(repositoryRoot), worktree: worktreeRoot},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := os.MkdirAll(testCase.worktree, 0o700); err != nil {
				t.Fatal(err)
			}
			_, err := initializer.Prepare(context.Background(), InitRequest{
				WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot,
				ManagedRoot: testCase.managedRoot, WorktreeRoot: testCase.worktree,
				Config: InitConfig{Copy: []CopyRule{{Source: "local.conf", Target: "another.conf"}}}, Timeout: time.Second,
			})
			if err == nil {
				t.Fatal("non-canonical managed workspace was accepted")
			}
		})
	}
}

func TestInitCopyAndInitRollbackNeverDeletesPreexistingTargets(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		rule   CopyRule
		create func(*testing.T, string)
		check  func(*testing.T, string)
	}{
		{
			name: "empty directory", rule: CopyRule{Source: "source-dir", Target: "target"},
			create: func(t *testing.T, target string) { t.Helper(); mustMkdir(t, target) },
			check: func(t *testing.T, target string) {
				t.Helper()
				info, err := os.Stat(target)
				if err != nil || !info.IsDir() {
					t.Fatalf("preexisting directory changed: %v", err)
				}
			},
		},
		{
			name: "same file", rule: CopyRule{Source: "source.txt", Target: "target"},
			create: func(t *testing.T, target string) { t.Helper(); writeInitFixture(t, target, "same\n", 0o600) },
			check: func(t *testing.T, target string) {
				t.Helper()
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "same\n" {
					t.Fatalf("preexisting file changed: %q %v", data, err)
				}
			},
		},
		{
			name: "symlink", rule: CopyRule{Source: "source.txt", Target: "target"},
			create: func(t *testing.T, target string) {
				t.Helper()
				if err := os.Symlink("outside", target); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, target string) {
				t.Helper()
				info, err := os.Lstat(target)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("preexisting symlink changed: %v", err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
			writeInitFixture(t, filepath.Join(repositoryRoot, "source.txt"), "same\n", 0o600)
			if err := os.Mkdir(filepath.Join(repositoryRoot, "source-dir"), 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(worktreeRoot, "target")
			testCase.create(t, target)
			initializer := NewFilesystemInitializer(InitializerOptions{})
			if _, err := initializer.Prepare(context.Background(), InitRequest{
				WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, WorktreeRoot: worktreeRoot, ManagedRoot: managedRoot,
				Config: InitConfig{Copy: []CopyRule{testCase.rule}}, Timeout: time.Second,
			}); err == nil {
				t.Fatal("copy overwrote a preexisting target")
			}
			testCase.check(t, target)
		})
	}
}

func TestInitRollbackRecordsBeforeMutationAndRemovesUnchangedArtifacts(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	writeInitFixture(t, filepath.Join(repositoryRoot, "first.txt"), "first\n", 0o600)
	writeInitFixture(t, filepath.Join(repositoryRoot, "second.txt"), "second\n", 0o600)
	var recorded []Manifest
	injected := errors.New("manifest store unavailable")
	initializer := NewFilesystemInitializer(InitializerOptions{})
	manifest, err := initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
		Config: InitConfig{Copy: []CopyRule{
			{Source: "first.txt", Target: "output/first.txt"},
			{Source: "second.txt", Target: "output/second.txt"},
		}},
		Timeout: time.Second,
		RecordManifest: func(_ context.Context, candidate Manifest) error {
			last := candidate.Entries[len(candidate.Entries)-1]
			_, statErr := os.Lstat(filepath.Join(worktreeRoot, filepath.FromSlash(last.Path)))
			switch last.State {
			case ManifestEntryPlanned:
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("planned manifest recorded after mutation for %q: %v", last.Path, statErr)
				}
			case ManifestEntryCreated:
				if statErr != nil || !validDigest(last.IdentityDigest) {
					t.Fatalf("created manifest lacks object identity for %q: %v", last.Path, statErr)
				}
			default:
				t.Fatalf("manifest state = %q", last.State)
			}
			recorded = append(recorded, candidate.Clone())
			if len(candidate.Entries) == 3 && last.State == ManifestEntryPlanned {
				return injected
			}
			return nil
		},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("prepare error = %v", err)
	}
	if len(recorded) != 5 || len(manifest.Entries) != 2 {
		t.Fatalf("recorded=%d manifest=%#v", len(recorded), manifest)
	}
	if _, statErr := os.Lstat(filepath.Join(worktreeRoot, "output")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unchanged attributable artifacts retained: %v", statErr)
	}
}

func TestInitRollbackRetainsChangedOrUnknownArtifacts(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T, string)
		check  func(*testing.T, string)
	}{
		{
			name: "changed initialized file",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeInitFixture(t, filepath.Join(root, "output", "first.txt"), "user change\n", 0o600)
			},
			check: func(t *testing.T, root string) {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(root, "output", "first.txt"))
				if err != nil || string(data) != "user change\n" {
					t.Fatalf("changed file was removed: %q %v", data, err)
				}
			},
		},
		{
			name: "unknown file",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				writeInitFixture(t, filepath.Join(root, "output", "unknown.txt"), "user work\n", 0o600)
			},
			check: func(t *testing.T, root string) {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(root, "output", "unknown.txt"))
				if err != nil || string(data) != "user work\n" {
					t.Fatalf("unknown file was removed: %q %v", data, err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
			writeInitFixture(t, filepath.Join(repositoryRoot, "first.txt"), "first\n", 0o600)
			writeInitFixture(t, filepath.Join(repositoryRoot, "second.txt"), "second\n", 0o600)
			injected := errors.New("manifest store unavailable")
			initializer := NewFilesystemInitializer(InitializerOptions{})
			manifest, err := initializer.Prepare(context.Background(), InitRequest{
				WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
				Config: InitConfig{Copy: []CopyRule{
					{Source: "first.txt", Target: "output/first.txt"},
					{Source: "second.txt", Target: "output/second.txt"},
				}},
				Timeout: time.Second,
				RecordManifest: func(_ context.Context, candidate Manifest) error {
					if len(candidate.Entries) == 3 {
						testCase.mutate(t, worktreeRoot)
						return injected
					}
					return nil
				},
			})
			if !errors.Is(err, injected) || !errors.Is(err, ErrInitializationRetained) {
				t.Fatalf("prepare error = %v", err)
			}
			if len(manifest.Entries) != 2 {
				t.Fatalf("manifest = %#v", manifest)
			}
			testCase.check(t, worktreeRoot)
		})
	}
}

func TestInitRollbackDoesNotDeletePreexistingFileFromPlannedManifest(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	writeInitFixture(t, filepath.Join(repositoryRoot, "source.txt"), "same\n", 0o600)
	target := filepath.Join(worktreeRoot, "target.txt")
	writeInitFixture(t, target, "same\n", 0o600)
	var captured Manifest
	initializer := NewFilesystemInitializer(InitializerOptions{})
	_, err := initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
		Config: InitConfig{Copy: []CopyRule{{Source: "source.txt", Target: "target.txt"}}}, Timeout: time.Second,
		RecordManifest: func(_ context.Context, candidate Manifest) error {
			captured = candidate.Clone()
			captured.Entries[len(captured.Entries)-1].State = ManifestEntryPlanned
			captured.Entries[len(captured.Entries)-1].IdentityDigest = ""
			return nil
		},
	})
	if err == nil || len(captured.Entries) != 1 {
		t.Fatalf("preexisting target prepare = %#v, %v", captured, err)
	}
	if rollbackErr := initializer.Rollback(context.Background(), RollbackRequest{
		RepositoryRoot: repositoryRoot,
		ManagedRoot:    managedRoot,
		WorktreeRoot:   worktreeRoot,
		Manifest:       captured,
	}); rollbackErr != nil {
		t.Fatalf("planned rollback: %v", rollbackErr)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "same\n" {
		t.Fatalf("planned manifest deleted preexisting target: %q %v", data, err)
	}
}

func TestInitRollbackReportsRetainedWhenCopyCreatesThenContextFails(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	writeInitFixture(t, filepath.Join(repositoryRoot, "source.txt"), strings.Repeat("x", 128*1024), 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	initializer := NewFilesystemInitializer(InitializerOptions{})
	_, err := initializer.Prepare(ctx, InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
		Config: InitConfig{Copy: []CopyRule{{Source: "source.txt", Target: "partial.txt"}}}, Timeout: time.Second,
		RecordManifest: func(_ context.Context, candidate Manifest) error {
			if candidate.Entries[len(candidate.Entries)-1].Type == ManifestFile {
				cancel()
			}
			return nil
		},
	})
	if err == nil {
		t.Fatal("canceled copy reported success")
	}
	_, statErr := os.Lstat(filepath.Join(worktreeRoot, "partial.txt"))
	if statErr == nil && !errors.Is(err, ErrInitializationRetained) {
		t.Fatalf("partial created file was retained without classification: %v", err)
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial target stat: %v", statErr)
	}
}

func TestInitRollbackCreatedPersistenceFailureRemovesOnlyIdentityMatchedObject(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	writeInitFixture(t, filepath.Join(repositoryRoot, "source.txt"), "created\n", 0o600)
	injected := errors.New("created checkpoint unavailable")
	initializer := NewFilesystemInitializer(InitializerOptions{})
	manifest, err := initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
		Config: InitConfig{Copy: []CopyRule{{Source: "source.txt", Target: "created.txt"}}}, Timeout: time.Second,
		RecordManifest: func(_ context.Context, candidate Manifest) error {
			entry := candidate.Entries[len(candidate.Entries)-1]
			switch entry.State {
			case ManifestEntryPlanned:
				return nil
			case ManifestEntryCreated:
				if !validDigest(entry.IdentityDigest) {
					return errors.New("created checkpoint missing identity")
				}
				return injected
			default:
				return errors.New("checkpoint missing ownership state")
			}
		},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("prepare = %#v, %v", manifest, err)
	}
	if _, statErr := os.Lstat(filepath.Join(worktreeRoot, "created.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("identity-matched created object was not rolled back: %v", statErr)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].State != ManifestEntryPlanned {
		t.Fatalf("last durable manifest = %#v", manifest)
	}
}

func TestInitRollbackRetainsIdentityReplacementEvenWithSameContent(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	writeInitFixture(t, filepath.Join(repositoryRoot, "source.txt"), "same content\n", 0o600)
	initializer := NewFilesystemInitializer(InitializerOptions{})
	manifest, err := initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
		Config: InitConfig{Copy: []CopyRule{{Source: "source.txt", Target: "target.txt"}}}, Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	target := filepath.Join(worktreeRoot, "target.txt")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	writeInitFixture(t, target, "same content\n", 0o600)
	err = initializer.Rollback(context.Background(), RollbackRequest{
		RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot, Manifest: manifest,
	})
	if !errors.Is(err, ErrInitializationRetained) {
		t.Fatalf("identity replacement rollback = %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "same content\n" {
		t.Fatalf("identity replacement was deleted: %q %v", data, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestIgnoredCopyRequiresExplicitGitIgnoredSource(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	writeInitFixture(t, filepath.Join(repositoryRoot, ".runtime", "state.json"), "runtime\n", 0o600)
	writeInitFixture(t, filepath.Join(repositoryRoot, ".env"), "SECRET=not-scanned\n", 0o600)
	git := &initializerGitFixture{ignored: map[string]bool{".runtime": true}}
	initializer := NewFilesystemInitializer(InitializerOptions{Git: git})
	manifest, err := initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
		Config: InitConfig{IgnoredCopy: []CopyRule{{Source: ".runtime", Target: ".runtime"}}}, Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("ignored copy: %v", err)
	}
	if len(git.checkIgnore) != 1 || git.checkIgnore[0] != ".runtime" {
		t.Fatalf("check-ignore calls = %#v", git.checkIgnore)
	}
	if _, err := os.Stat(filepath.Join(worktreeRoot, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ignored-copy scanned unlisted .env: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(worktreeRoot, ".runtime", "state.json"))
	if err != nil || string(data) != "runtime\n" {
		t.Fatalf("ignored file copy = %q, %v", data, err)
	}
	if len(manifest.Entries) != 2 || manifest.Entries[1].Type != ManifestFile || !validDigest(manifest.Entries[1].Digest) {
		t.Fatalf("ignored manifest = %#v", manifest)
	}
}

func TestIgnoredCopyRejectsTrackedOrUnknownSource(t *testing.T) {
	for _, testCase := range []struct {
		name string
		git  *initializerGitFixture
	}{
		{name: "tracked", git: &initializerGitFixture{ignored: map[string]bool{}}},
		{name: "git error", git: &initializerGitFixture{checkIgnoreErr: errors.New("git unavailable")}},
		{name: "missing git", git: nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
			writeInitFixture(t, filepath.Join(repositoryRoot, "runtime.json"), "{}", 0o600)
			options := InitializerOptions{}
			if testCase.git != nil {
				options.Git = testCase.git
			}
			initializer := NewFilesystemInitializer(options)
			_, err := initializer.Prepare(context.Background(), InitRequest{
				WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
				Config: InitConfig{IgnoredCopy: []CopyRule{{Source: "runtime.json", Target: "runtime.json"}}}, Timeout: time.Second,
			})
			if err == nil {
				t.Fatal("non-ignored source was copied")
			}
			if _, statErr := os.Stat(filepath.Join(worktreeRoot, "runtime.json")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejected ignored-copy created target: %v", statErr)
			}
		})
	}
}

func TestInitLinkCreatesOnlyCapabilityProtectedReadonlyDependency(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	parent := filepath.Dir(repositoryRoot)
	shared := filepath.Join(parent, "shared", "node_modules")
	for _, directory := range []string{shared} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	capability := &readonlyLinkFixture{}
	initializer := NewFilesystemInitializer(InitializerOptions{ReadonlyLinks: capability})
	manifest, err := initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, WorktreeRoot: worktreeRoot, ManagedRoot: managedRoot,
		Config: InitConfig{Link: []LinkRule{{Source: shared, Target: "vendor/node_modules"}}}, Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("prepare readonly link: %v", err)
	}
	canonicalShared, err := filepath.EvalSymlinks(shared)
	if err != nil {
		t.Fatal(err)
	}
	canonicalWorktree, err := filepath.EvalSymlinks(worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(capability.requests) != 1 || capability.requests[0].Source != canonicalShared ||
		capability.requests[0].Target != filepath.Join(canonicalWorktree, "vendor", "node_modules") {
		t.Fatalf("readonly capability requests = %#v", capability.requests)
	}
	target, err := os.Readlink(filepath.Join(worktreeRoot, "vendor", "node_modules"))
	if err != nil || target != canonicalShared {
		t.Fatalf("readonly link target = %q, %v", target, err)
	}
	if len(manifest.Entries) != 2 || manifest.Entries[1].Type != ManifestSymlink ||
		manifest.Entries[1].Path != "vendor/node_modules" || !validDigest(manifest.Entries[1].Digest) {
		t.Fatalf("link manifest = %#v", manifest)
	}
}

func TestInitLinkRejectsUnsafeOrUnprotectedSources(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	parent := filepath.Dir(repositoryRoot)
	externalRoot := filepath.Join(parent, "external")
	for _, directory := range []string{externalRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeInitFixture(t, filepath.Join(externalRoot, "file"), "not a directory", 0o600)
	if err := os.MkdirAll(filepath.Join(repositoryRoot, "main-dep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(managedRoot, "tasks", "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(externalRoot, ".git", "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalRoot, filepath.Join(parent, "external-link")); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		source     string
		capability ReadonlyLinkCapability
	}{
		{name: "missing capability", source: externalRoot},
		{name: "capability rejects", source: externalRoot, capability: &readonlyLinkFixture{err: errors.New("not enforceable")}},
		{name: "main root", source: filepath.Join(repositoryRoot, "main-dep"), capability: &readonlyLinkFixture{}},
		{name: "managed root", source: filepath.Join(managedRoot, "tasks", "other"), capability: &readonlyLinkFixture{}},
		{name: "git metadata", source: filepath.Join(externalRoot, ".git", "objects"), capability: &readonlyLinkFixture{}},
		{name: "source symlink", source: filepath.Join(parent, "external-link"), capability: &readonlyLinkFixture{}},
		{name: "source file", source: filepath.Join(externalRoot, "file"), capability: &readonlyLinkFixture{}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			initializer := NewFilesystemInitializer(InitializerOptions{ReadonlyLinks: testCase.capability})
			_, err := initializer.Prepare(context.Background(), InitRequest{
				WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, WorktreeRoot: worktreeRoot, ManagedRoot: managedRoot,
				Config: InitConfig{Link: []LinkRule{{Source: testCase.source, Target: "linked"}}}, Timeout: time.Second,
			})
			if err == nil {
				t.Fatal("unsafe readonly link succeeded")
			}
			if _, statErr := os.Lstat(filepath.Join(worktreeRoot, "linked")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejected link created target: %v", statErr)
			}
		})
	}
}

func TestGitHooksUsesOnlyWorktreeConfigAndIsIdempotent(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	if err := os.Mkdir(filepath.Join(worktreeRoot, ".githooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	canonicalRepository, _ := filepath.EvalSymlinks(repositoryRoot)
	git := &initializerGitFixture{values: map[string]map[string]string{
		canonicalRepository: {"core.hooksPath": "main-hooks"},
	}}
	initializer := NewFilesystemInitializer(InitializerOptions{Git: git})
	request := InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
		Config: InitConfig{GitHooks: GitHooksRule{Enabled: true, Path: ".githooks"}}, Timeout: time.Second,
	}
	for attempt := 0; attempt < 2; attempt++ {
		manifest, err := initializer.Prepare(context.Background(), request)
		if err != nil {
			t.Fatalf("hooks attempt %d: %v", attempt+1, err)
		}
		if len(manifest.Entries) != 0 {
			t.Fatalf("git config leaked into filesystem manifest: %#v", manifest)
		}
	}
	canonicalWorktree, _ := filepath.EvalSymlinks(worktreeRoot)
	if got := git.value(canonicalRepository, "core.hooksPath"); got != "main-hooks" {
		t.Fatalf("main worktree hooks changed to %q", got)
	}
	if got := git.value(canonicalWorktree, "core.hooksPath"); got != ".githooks" {
		t.Fatalf("worktree hooks = %q", got)
	}
	if got := git.value(canonicalRepository, "extensions.worktreeConfig"); got != "true" {
		t.Fatalf("worktreeConfig extension = %q", got)
	}
}

func TestGitHooksRejectsUnsafePathAndRestoresConfigOnFailure(t *testing.T) {
	for _, hookPath := range []string{"../hooks", "/tmp/hooks", ".git/hooks", ".control", ".xagent", ".xagent/worktrees/hooks", "missing"} {
		t.Run(strings.ReplaceAll(hookPath, "/", "_"), func(t *testing.T) {
			repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
			if hookPath == ".control" || strings.HasPrefix(hookPath, ".xagent") {
				if err := os.MkdirAll(filepath.Join(worktreeRoot, filepath.FromSlash(hookPath)), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			git := &initializerGitFixture{}
			initializer := NewFilesystemInitializer(InitializerOptions{Git: git})
			_, err := initializer.Prepare(context.Background(), InitRequest{
				WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
				Config: InitConfig{GitHooks: GitHooksRule{Enabled: true, Path: hookPath}}, Timeout: time.Second,
			})
			if err == nil {
				t.Fatal("unsafe hooks path accepted")
			}
			if len(git.calls) != 0 {
				t.Fatalf("unsafe hooks path reached git: %#v", git.calls)
			}
		})
	}

	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	if err := os.Mkdir(filepath.Join(worktreeRoot, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	git := &initializerGitFixture{errAt: "set:core.hooksPath=hooks"}
	initializer := NewFilesystemInitializer(InitializerOptions{Git: git})
	_, err := initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
		Config: InitConfig{GitHooks: GitHooksRule{Enabled: true, Path: "hooks"}}, Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("failed hooks config reported success")
	}
	canonicalRepository, _ := filepath.EvalSymlinks(repositoryRoot)
	if got := git.value(canonicalRepository, "extensions.worktreeConfig"); got != "" {
		t.Fatalf("failed hooks init retained repository extension %q", got)
	}
	if !containsString(git.calls, "restore-extension:<unset>") {
		t.Fatalf("failed hooks init did not restore extension: %#v", git.calls)
	}
}

func TestGitHooksRestoresPreviousValuesInReverseOrderWhenVerificationFails(t *testing.T) {
	repositoryRoot, managedRoot, worktreeRoot := initManagedFixture(t)
	if err := os.Mkdir(filepath.Join(worktreeRoot, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	canonicalRepository, _ := filepath.EvalSymlinks(repositoryRoot)
	canonicalWorktree, _ := filepath.EvalSymlinks(worktreeRoot)
	git := &initializerGitFixture{
		values: map[string]map[string]string{
			canonicalRepository: {"extensions.worktreeConfig": "false"},
			canonicalWorktree:   {"core.hooksPath": "old-hooks"},
		},
		failGetNumber: map[string]int{"worktree:core.hooksPath": 2},
	}
	initializer := NewFilesystemInitializer(InitializerOptions{Git: git})
	_, err := initializer.Prepare(context.Background(), InitRequest{
		WorkspaceID: initializerWorkspaceID, RepositoryRoot: repositoryRoot, ManagedRoot: managedRoot, WorktreeRoot: worktreeRoot,
		Config: InitConfig{GitHooks: GitHooksRule{Enabled: true, Path: "hooks"}}, Timeout: time.Second,
	})
	if err == nil {
		t.Fatal("hooks verification failure reported success")
	}
	if got := git.value(canonicalRepository, "extensions.worktreeConfig"); got != "false" {
		t.Fatalf("extension restored to %q", got)
	}
	if got := git.value(canonicalWorktree, "core.hooksPath"); got != "old-hooks" {
		t.Fatalf("hooks restored to %q", got)
	}
	wantSuffix := []string{"restore-hooks:old-hooks", "restore-extension:false"}
	if len(git.calls) < len(wantSuffix) || strings.Join(git.calls[len(git.calls)-len(wantSuffix):], ",") != strings.Join(wantSuffix, ",") {
		t.Fatalf("restore order = %#v", git.calls)
	}
}

func initManagedFixture(t *testing.T) (string, string, string) {
	t.Helper()
	repositoryRoot := filepath.Join(t.TempDir(), "repo")
	managedRoot := filepath.Join(repositoryRoot, ".xagent", "worktrees")
	worktreeRoot := filepath.Join(managedRoot, "tasks", initializerWorkspaceID[:2], initializerWorkspaceID)
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	return repositoryRoot, managedRoot, worktreeRoot
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type readonlyLinkFixture struct {
	requests []ReadonlyLinkRequest
	err      error
}

func (f *readonlyLinkFixture) ProtectReadonlyLink(_ context.Context, request ReadonlyLinkRequest) error {
	f.requests = append(f.requests, request)
	return f.err
}

type initializerGitFixture struct {
	ignored        map[string]bool
	checkIgnore    []string
	checkIgnoreErr error
	values         map[string]map[string]string
	calls          []string
	errAt          string
	getCounts      map[string]int
	failGetNumber  map[string]int
}

func (g *initializerGitFixture) CheckIgnore(_ context.Context, _ string, path string) (bool, error) {
	g.checkIgnore = append(g.checkIgnore, path)
	if g.checkIgnoreErr != nil {
		return false, g.checkIgnoreErr
	}
	return g.ignored[path], nil
}

func (g *initializerGitFixture) ConfigGet(_ context.Context, cwd string, key string) (string, error) {
	g.calls = append(g.calls, "get:"+key)
	return g.getConfig(cwd, key, key)
}

func (g *initializerGitFixture) WorktreeConfigGet(_ context.Context, cwd string, key string) (string, error) {
	g.calls = append(g.calls, "get-worktree:"+key)
	return g.getConfig(cwd, key, "worktree:"+key)
}

func (g *initializerGitFixture) getConfig(cwd, key, counterKey string) (string, error) {
	if g.getCounts == nil {
		g.getCounts = map[string]int{}
	}
	g.getCounts[counterKey]++
	if g.failGetNumber[counterKey] == g.getCounts[counterKey] {
		return "", errors.New("git get failed")
	}
	if g.errAt == "get:"+key || g.errAt == "get-worktree:"+key {
		return "", errors.New("git get failed")
	}
	value := g.value(cwd, key)
	if value == "" {
		return "", ErrNotFound
	}
	return value, nil
}

func (g *initializerGitFixture) EnableWorktreeConfig(_ context.Context, cwd string) error {
	g.calls = append(g.calls, "enable")
	if g.errAt == "enable" {
		return errors.New("git enable failed")
	}
	g.setValue(cwd, "extensions.worktreeConfig", "true")
	return nil
}

func (g *initializerGitFixture) SetWorktreeConfig(_ context.Context, cwd string, key, value string) error {
	call := "set:" + key + "=" + value
	g.calls = append(g.calls, call)
	if g.errAt == "set" || g.errAt == call {
		return errors.New("git set failed")
	}
	g.setValue(cwd, key, value)
	return nil
}

func (g *initializerGitFixture) RestoreWorktreeConfigExtension(_ context.Context, cwd, previous string, wasSet bool) error {
	value := previous
	if !wasSet {
		value = ""
	}
	g.calls = append(g.calls, "restore-extension:"+map[bool]string{true: previous, false: "<unset>"}[wasSet])
	g.setValue(cwd, "extensions.worktreeConfig", value)
	return nil
}

func (g *initializerGitFixture) RestoreWorktreeHooksPath(_ context.Context, cwd, previous string, wasSet bool) error {
	value := previous
	if !wasSet {
		value = ""
	}
	g.calls = append(g.calls, "restore-hooks:"+map[bool]string{true: previous, false: "<unset>"}[wasSet])
	g.setValue(cwd, "core.hooksPath", value)
	return nil
}

func (g *initializerGitFixture) value(cwd, key string) string {
	if g.values == nil {
		return ""
	}
	return g.values[cwd][key]
}

func (g *initializerGitFixture) setValue(cwd, key, value string) {
	if g.values == nil {
		g.values = map[string]map[string]string{}
	}
	if g.values[cwd] == nil {
		g.values[cwd] = map[string]string{}
	}
	if value == "" {
		delete(g.values[cwd], key)
		return
	}
	g.values[cwd][key] = value
}

func writeInitFixture(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
