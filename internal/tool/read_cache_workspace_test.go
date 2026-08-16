package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadCacheWorkspaceDoesNotShareSameRelativePathAcrossRoots(t *testing.T) {
	parent := t.TempDir()
	mainRoot := filepath.Join(parent, "main")
	childRoot := filepath.Join(parent, "child")
	for _, root := range []string{mainRoot, childRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeReadCacheTestFile(filepath.Join(root, "same.txt"), "same content"); err != nil {
			t.Fatal(err)
		}
	}
	cache, factory := newReadCacheWorkspaceFixture(t)
	mainCall := prepareReadCacheWorkspaceCall(t, mainRoot, "main-call", "same.txt")
	if err := cache.Store(context.Background(), mainCall, readCacheWorkspaceResult(t, factory, mainCall, "main result")); err != nil {
		t.Fatalf("store main: %v", err)
	}
	childCall := prepareReadCacheWorkspaceCall(t, childRoot, "child-call", "same.txt")
	if _, ok, err := cache.Lookup(context.Background(), childCall); err != nil || ok {
		t.Fatalf("main-root entry crossed into child root: ok=%t err=%v", ok, err)
	}
	if _, ok, err := cache.Lookup(context.Background(), mainCall); err != nil || !ok {
		t.Fatalf("main-root entry no longer available: ok=%t err=%v", ok, err)
	}
}

func TestReadCacheWorkspaceDependenciesAreCanonicalAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	if err := writeReadCacheTestFile(filepath.Join(root, "input.txt"), "input"); err != nil {
		t.Fatal(err)
	}
	cache, _ := newReadCacheWorkspaceFixture(t)
	validated := prepareReadCacheWorkspaceCall(t, root, "call", "input.txt")
	key, dependencies, err := cache.identity(context.Background(), validated)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if len(dependencies) != 1 || !filepath.IsAbs(dependencies[0].Path) || filepath.Clean(dependencies[0].Path) != dependencies[0].Path {
		t.Fatalf("dependencies = %#v", dependencies)
	}
	if len(key.ArgumentsFingerprint) != 64 || strings.Contains(key.ArgumentsFingerprint, filepath.Base(root)) ||
		strings.Contains(key.ArgumentsFingerprint, "input.txt") {
		t.Fatalf("cache key leaked cwd/path material: %#v", key)
	}
	value := readCacheTestValue(t, cache.factory, "cached", "summary", "preview")
	if err := cache.Put(ReadCacheKey{Tool: "Read", ArgumentsFingerprint: "relative"}, []FileVersion{{
		Path: "input.txt", Digest: "digest", IdentityDigest: strings.Repeat("d", 64),
	}}, value); err == nil {
		t.Fatal("relative dependency path was accepted")
	}
}

func TestReadCacheWorkspaceCanonicalizesEquivalentRootAliases(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "WorkspaceCase")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeReadCacheTestFile(filepath.Join(root, "input.txt"), "same content"); err != nil {
		t.Fatal(err)
	}
	symlinkAlias := filepath.Join(parent, "workspace-link")
	if err := os.Symlink(root, symlinkAlias); err != nil {
		t.Fatal(err)
	}
	cache, factory := newReadCacheWorkspaceFixture(t)
	original := prepareReadCacheWorkspaceCall(t, root, "original", "input.txt")
	if err := cache.Store(context.Background(), original, readCacheWorkspaceResult(t, factory, original, "same workspace")); err != nil {
		t.Fatalf("store: %v", err)
	}
	alias := prepareReadCacheWorkspaceCall(t, symlinkAlias, "symlink-alias", "input.txt")
	if _, ok, err := cache.Lookup(context.Background(), alias); err != nil || !ok {
		t.Fatalf("equivalent symlink root was not canonicalized: ok=%t err=%v", ok, err)
	}

	caseAliasPath := filepath.Join(parent, strings.ToLower(filepath.Base(root)))
	if _, err := os.Stat(caseAliasPath); err != nil {
		t.Skip("filesystem does not provide a case-insensitive alias")
	}
	caseAlias := prepareReadCacheWorkspaceCall(t, caseAliasPath, "case-alias", "input.txt")
	if _, ok, err := cache.Lookup(context.Background(), caseAlias); err != nil || !ok {
		t.Fatalf("equivalent case alias was not canonicalized: ok=%t err=%v", ok, err)
	}
}

func TestReadCacheWorkspaceRejectsRootIdentityDrift(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "input.txt")
	if err := writeReadCacheTestFile(file, "same content"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	cache, factory := newReadCacheWorkspaceFixture(t)
	validated := prepareReadCacheWorkspaceCall(t, root, "root-call", "input.txt")
	if err := cache.Store(context.Background(), validated, readCacheWorkspaceResult(t, factory, validated, "old root")); err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := os.Rename(root, filepath.Join(parent, "detached")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(root, "input.txt")
	if err := writeReadCacheTestFile(file, "same content"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.Lookup(context.Background(), validated); err == nil || ok {
		t.Fatalf("replaced root did not fail closed: ok=%t err=%v", ok, err)
	}
}

func TestReadCacheWorkspaceRejectsDependencyIdentityAndSymlinkDrift(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		prepare func(*testing.T, string) (string, func(*testing.T))
	}{
		{
			name: "same-content replacement",
			prepare: func(t *testing.T, root string) (string, func(*testing.T)) {
				t.Helper()
				path := filepath.Join(root, "input.txt")
				if err := writeReadCacheTestFile(path, "same content"); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				return "input.txt", func(t *testing.T) {
					t.Helper()
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := writeReadCacheTestFile(path, "same content"); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "symlink replacement",
			prepare: func(t *testing.T, root string) (string, func(*testing.T)) {
				t.Helper()
				path := filepath.Join(root, "input.txt")
				if err := writeReadCacheTestFile(path, "same content"); err != nil {
					t.Fatal(err)
				}
				if err := writeReadCacheTestFile(filepath.Join(root, "target.txt"), "same content"); err != nil {
					t.Fatal(err)
				}
				return "input.txt", func(t *testing.T) {
					t.Helper()
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("target.txt", path); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			relative, replace := testCase.prepare(t, root)
			cache, factory := newReadCacheWorkspaceFixture(t)
			validated := prepareReadCacheWorkspaceCall(t, root, "drift-call", relative)
			if err := cache.Store(context.Background(), validated, readCacheWorkspaceResult(t, factory, validated, "old object")); err != nil {
				t.Fatalf("store: %v", err)
			}
			replace(t)
			if _, ok, err := cache.Lookup(context.Background(), validated); err == nil || ok {
				t.Fatalf("dependency drift did not fail closed: ok=%t err=%v", ok, err)
			}
		})
	}
}

func TestReadCacheWorkspaceRejectsRelativeTaskRootWithoutUsingProcessCWD(t *testing.T) {
	cache, _ := newReadCacheWorkspaceFixture(t)
	validated := prepareReadCacheWorkspaceCall(t, t.TempDir(), "call", "missing.txt")
	validated.executionRootPath = "relative-workspace"
	if _, _, err := cache.identity(context.Background(), validated); err == nil {
		t.Fatal("relative task root was resolved through process cwd")
	}
}

func newReadCacheWorkspaceFixture(t *testing.T) (*ReadCache, *ResultFactory) {
	t.Helper()
	factory := newReadCacheTestFactory(t)
	cache, err := NewReadCache(ReadCacheLimits{
		MaxEntries: 8, MaxBytes: 128 << 10, MaxValueBytes: 16 << 10, MaxDependenciesPerEntry: 64,
	}, factory)
	if err != nil {
		t.Fatal(err)
	}
	return cache, factory
}

func prepareReadCacheWorkspaceCall(t *testing.T, root, callID, path string) ValidatedCall {
	t.Helper()
	registry, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(registry, root, time.Second, 1024)
	validated, err := executor.PrepareCall(context.Background(), Call{ID: callID, Name: "Read", ArgumentsJSON: `{"path":` + mustReadCacheJSONString(t, path) + `}`})
	if err != nil {
		t.Fatal(err)
	}
	return validated
}

func readCacheWorkspaceResult(t *testing.T, factory *ResultFactory, validated ValidatedCall, preview string) Result {
	t.Helper()
	result, err := factory.Build(ResultFactoryInput{
		CallID: validated.Call.ID, Name: validated.Call.Name, State: Completed, Status: StatusSuccess,
		Summary: "summary", Preview: preview,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mustReadCacheJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
