//go:build darwin || linux

package artifact

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPOSIXPrivateArtifactRootUsesRelativePrivateOperations(t *testing.T) {
	workspace := t.TempDir()
	rootPath := filepath.Join(t.TempDir(), "cache", "xagent", "artifacts")
	workspace = canonicalPOSIXTestPath(t, workspace)
	rootPath = canonicalPOSIXTestPath(t, rootPath)
	root, prepared, err := openPrivateArtifactRoot(rootPath, workspace, true)
	if err != nil || prepared != rootPath {
		t.Fatal("open private artifact root failed")
	}
	t.Cleanup(func() { _ = root.close() })
	assertPOSIXModeAndOwner(t, prepared, 0o700)

	staging, err := root.create(".private.staging")
	if err != nil {
		t.Fatal("create private artifact failed")
	}
	if _, err := staging.Write([]byte("private artifact")); err != nil {
		_ = staging.Close()
		t.Fatal("write private artifact failed")
	}
	if err := staging.Close(); err != nil {
		t.Fatal("close private artifact failed")
	}
	assertPOSIXModeAndOwner(t, filepath.Join(prepared, ".private.staging"), 0o600)

	if err := root.rename(".private.staging", "private.artifact"); err != nil {
		t.Fatal("rename private artifact failed")
	}
	reader, err := root.open("private.artifact")
	if err != nil {
		t.Fatal("open private artifact failed")
	}
	payload, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err != nil || closeErr != nil || string(payload) != "private artifact" {
		t.Fatal("read private artifact failed")
	}
	if err := root.remove("private.artifact"); err != nil {
		t.Fatal("remove private artifact failed")
	}
	if _, err := os.Lstat(filepath.Join(prepared, "private.artifact")); !os.IsNotExist(err) {
		t.Fatal("removed private artifact remained visible")
	}

	for _, invalid := range []string{"", ".", "..", "nested/artifact", "/absolute"} {
		if file, err := root.create(invalid); err == nil || file != nil {
			t.Fatal("invalid private artifact name was accepted")
		}
	}
}

func TestPOSIXPrivateArtifactRootSurvivesAncestorReplacement(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	workspace = canonicalPOSIXTestPath(t, workspace)
	outside = canonicalPOSIXTestPath(t, outside)
	ancestor := filepath.Join(outside, "selected-cache")
	rootPath := filepath.Join(ancestor, "artifacts")
	root, _, err := openPrivateArtifactRoot(rootPath, workspace, true)
	if err != nil {
		t.Fatal("open private artifact root failed")
	}
	t.Cleanup(func() { _ = root.close() })

	originalAncestor := filepath.Join(outside, "opened-cache")
	if err := os.Rename(ancestor, originalAncestor); err != nil {
		t.Fatal("move opened artifact ancestor failed")
	}
	if err := os.Symlink(workspace, ancestor); err != nil {
		t.Fatal("replace artifact ancestor with workspace link failed")
	}

	file, err := root.create("bound.staging")
	if err != nil {
		t.Fatal("descriptor-relative create failed after ancestor replacement")
	}
	if _, err := file.Write([]byte("bound to opened root")); err != nil {
		_ = file.Close()
		t.Fatal("descriptor-relative write failed")
	}
	if err := file.Close(); err != nil {
		t.Fatal("close descriptor-relative artifact failed")
	}
	if err := root.rename("bound.staging", "bound.artifact"); err != nil {
		t.Fatal("descriptor-relative rename failed")
	}

	boundPath := filepath.Join(originalAncestor, "artifacts", "bound.artifact")
	payload, err := os.ReadFile(boundPath)
	if err != nil || string(payload) != "bound to opened root" {
		t.Fatal("artifact was not retained by the opened root identity")
	}
	if _, err := os.Lstat(filepath.Join(workspace, "artifacts")); !os.IsNotExist(err) {
		t.Fatal("artifact operation followed the replacement link into workspace")
	}
}

func TestPOSIXPrivateArtifactOperationsNeverFollowSymlinkLeaf(t *testing.T) {
	workspace := canonicalPOSIXTestPath(t, t.TempDir())
	rootPath := canonicalPOSIXTestPath(t, filepath.Join(t.TempDir(), "artifacts"))
	root, prepared, err := openPrivateArtifactRoot(rootPath, workspace, true)
	if err != nil {
		t.Fatal("open private artifact root failed")
	}
	t.Cleanup(func() { _ = root.close() })

	target := filepath.Join(workspace, "sentinel")
	if err := os.WriteFile(target, []byte("workspace sentinel"), 0o600); err != nil {
		t.Fatal("create workspace sentinel failed")
	}
	if err := os.Symlink(target, filepath.Join(prepared, "linked.artifact")); err != nil {
		t.Fatal("create artifact leaf link failed")
	}

	if file, err := root.open("linked.artifact"); err == nil || file != nil {
		if file != nil {
			_ = file.Close()
		}
		t.Fatal("artifact open followed a symbolic link leaf")
	}
	if file, err := root.create("linked.artifact"); err == nil || file != nil {
		if file != nil {
			_ = file.Close()
		}
		t.Fatal("artifact create replaced a symbolic link leaf")
	}
	if err := root.rename("linked.artifact", "published.artifact"); err == nil {
		t.Fatal("artifact rename accepted a symbolic link leaf")
	}
	if err := root.remove("linked.artifact"); err == nil {
		t.Fatal("artifact remove accepted a symbolic link leaf")
	}
	if payload, err := os.ReadFile(target); err != nil || string(payload) != "workspace sentinel" {
		t.Fatal("artifact leaf operation reached the workspace target")
	}
}

func canonicalPOSIXTestPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := canonicalFuturePath(path)
	if err != nil {
		t.Fatal("canonicalize POSIX artifact fixture failed")
	}
	return canonical
}

func TestArtifactPOSIXPermissions(t *testing.T) {
	store := newArtifactTestStore(t, 64, 128)
	writer, err := store.Begin(context.Background(), Metadata{})
	if err != nil {
		t.Fatal("begin artifact failed")
	}
	assertPOSIXModeAndOwner(t, store.root, 0o700)
	entries, err := os.ReadDir(store.root)
	if err != nil || len(entries) != 1 {
		t.Fatal("private staging artifact was unavailable")
	}
	assertPOSIXModeAndOwner(t, filepath.Join(store.root, entries[0].Name()), 0o600)
	if _, err := writer.Write([]byte("private")); err != nil {
		t.Fatal("write private artifact failed")
	}
	ref, err := writer.Commit(context.Background())
	if err != nil {
		t.Fatal("commit private artifact failed")
	}
	assertPOSIXModeAndOwner(t, filepath.Join(store.root, ref.ID+".artifact"), 0o600)

	insecureRoot := filepath.Join(t.TempDir(), "insecure-artifacts")
	if err := os.Mkdir(insecureRoot, 0o755); err != nil {
		t.Fatal("create insecure root fixture failed")
	}
	insecure, err := NewFileStore(FileStoreOptions{
		Root: insecureRoot, WorkspaceRoot: t.TempDir(), MaxFileBytes: 1, MaxTotalBytes: 1,
	})
	if err != nil {
		t.Fatal("construct insecure-root store failed before use")
	}
	defer insecure.Close()
	if _, err := insecure.Begin(context.Background(), Metadata{}); err == nil {
		t.Fatal("existing root with broad permissions was accepted")
	}
	if info, err := os.Stat(insecureRoot); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatal("insecure root permissions were silently changed")
	}
}

func assertPOSIXModeAndOwner(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != mode {
		t.Fatal("artifact POSIX mode was not private")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		t.Fatal("artifact POSIX owner was not the current user")
	}
}
