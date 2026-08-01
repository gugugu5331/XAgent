//go:build darwin || linux

package artifact

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

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
