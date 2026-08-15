package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"xagent/internal/budget"
)

func TestStoreRejectsWorkspaceRoot(t *testing.T) {
	workspace := t.TempDir()
	cases := []struct {
		name string
		root string
	}{
		{name: "workspace", root: workspace},
		{name: "workspace child", root: filepath.Join(workspace, "private", "artifacts")},
		{name: "workspace parent", root: filepath.Dir(workspace)},
	}
	outside := t.TempDir()
	alias := filepath.Join(outside, "workspace-alias")
	if err := os.Symlink(workspace, alias); err == nil {
		cases = append(cases, struct {
			name string
			root string
		}{name: "symlink alias child", root: filepath.Join(alias, "artifacts")})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewFileStore(FileStoreOptions{Root: test.root, WorkspaceRoot: workspace})
			if err == nil || store != nil {
				t.Fatal("workspace artifact root was accepted")
			}
			if strings.Contains(err.Error(), workspace) || strings.Contains(err.Error(), test.root) {
				t.Fatal("root rejection exposed a private path")
			}
		})
	}

	outsideRoot := filepath.Join(t.TempDir(), "artifacts")
	store, err := NewFileStore(FileStoreOptions{
		Root: outsideRoot, WorkspaceRoot: workspace,
		MaxFileBytes: 1, MaxTotalBytes: 1,
	})
	if err != nil {
		t.Fatal("outside artifact root was rejected")
	}
	if _, err := os.Stat(outsideRoot); !os.IsNotExist(err) {
		t.Fatal("store validation created the artifact root")
	}
	if err := store.Close(); err != nil {
		t.Fatal("store close failed")
	}
	if err := store.Close(); err != nil {
		t.Fatal("repeated store close changed the result")
	}
}

func TestPrepareFileStoreRootUsesPrivateCanonicalBoundary(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(t.TempDir(), "artifacts")
	prepared, err := PrepareFileStoreRoot(root, workspace)
	canonicalParent, canonicalErr := filepath.EvalSymlinks(filepath.Dir(root))
	want := filepath.Join(canonicalParent, filepath.Base(root))
	if err != nil || canonicalErr != nil || prepared != want {
		t.Fatal("prepare safe artifact root failed")
	}
	info, err := os.Stat(prepared)
	if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
		t.Fatal("prepared artifact root is not a private directory")
	}
	if repeated, err := PrepareFileStoreRoot(root, workspace); err != nil || repeated != prepared {
		t.Fatal("repeated artifact root preparation changed the result")
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatal("change artifact root permissions failed")
		}
		if prepared, err := PrepareFileStoreRoot(root, workspace); err == nil || prepared != "" {
			t.Fatal("non-private artifact root was accepted")
		}
	}
	if prepared, err := PrepareFileStoreRoot(filepath.Join(workspace, "artifacts"), workspace); err == nil || prepared != "" {
		t.Fatal("workspace artifact root was prepared")
	}
}

func TestFileStoreUsesVerifiedRootHandleAfterAncestorReplacement(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	slot := filepath.Join(outside, "slot")
	root := filepath.Join(slot, "artifacts")
	created, err := NewFileStore(FileStoreOptions{
		Root: root, WorkspaceRoot: workspace, MaxFileBytes: 64, MaxTotalBytes: 128,
	})
	if err != nil {
		t.Fatal("construct ancestor replacement store failed")
	}
	store := created.(*fileStore)
	t.Cleanup(func() { _ = store.Close() })
	first, err := store.Begin(context.Background(), Metadata{})
	if err != nil {
		t.Fatal("open verified artifact root failed")
	}
	if err := first.Abort(); err != nil {
		t.Fatal("abort root bootstrap writer failed")
	}

	moved := filepath.Join(outside, "moved-slot")
	if err := os.Rename(slot, moved); err != nil {
		t.Fatal("move verified artifact ancestor failed")
	}
	if err := os.Symlink(workspace, slot); err != nil {
		return
	}
	writer, err := store.Begin(context.Background(), Metadata{})
	if err != nil {
		t.Fatal("verified root handle became path-dependent")
	}
	if _, err := writer.Write([]byte("handle-relative")); err != nil {
		t.Fatal("write through verified root handle failed")
	}
	ref, err := writer.Commit(context.Background())
	if err != nil || !ref.Available {
		t.Fatal("commit through verified root handle failed")
	}
	if _, err := os.Stat(filepath.Join(workspace, "artifacts")); !os.IsNotExist(err) {
		t.Fatal("ancestor replacement redirected artifact data into workspace")
	}
	if _, err := os.Stat(filepath.Join(moved, "artifacts", ref.ID+".artifact")); err != nil {
		t.Fatal("artifact did not remain under the verified directory handle")
	}
}

func TestWriterCommitAbortExactlyOnce(t *testing.T) {
	store := newArtifactTestStore(t, 64, 128)

	aborted, err := store.Begin(context.Background(), Metadata{MediaType: "text/plain"})
	if err != nil {
		t.Fatal("begin aborted writer failed")
	}
	if _, err := aborted.Write([]byte("discarded")); err != nil {
		t.Fatal("write before abort failed")
	}
	if err := aborted.Abort(); err != nil {
		t.Fatal("first abort failed")
	}
	if !errors.Is(aborted.Abort(), errWriterFinalized) {
		t.Fatal("second abort was accepted")
	}
	if _, err := aborted.Commit(context.Background()); !errors.Is(err, errWriterFinalized) {
		t.Fatal("commit after abort was accepted")
	}

	committed, err := store.Begin(context.Background(), Metadata{MediaType: "text/plain"})
	if err != nil {
		t.Fatal("begin committed writer failed")
	}
	payload := []byte("committed")
	if written, err := committed.Write(payload); err != nil || written != len(payload) {
		t.Fatal("artifact write failed")
	}
	ref, err := committed.Commit(context.Background())
	if err != nil {
		t.Fatal("first commit failed")
	}
	if !ref.Available || !ref.Complete || ref.Bytes != int64(len(payload)) || !validArtifactID(ref.ID) {
		t.Fatal("committed ref did not describe the stored artifact")
	}
	if _, err := committed.Commit(context.Background()); !errors.Is(err, errWriterFinalized) {
		t.Fatal("second commit was accepted")
	}
	if !errors.Is(committed.Abort(), errWriterFinalized) {
		t.Fatal("abort after commit was accepted")
	}
	reader, opened, err := store.OpenForUser(context.Background(), ref.ID)
	if err != nil || opened != ref {
		t.Fatal("open committed artifact failed")
	}
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != string(payload) {
		t.Fatal("committed artifact content changed")
	}
	if err := reader.Close(); err != nil {
		t.Fatal("close artifact reader failed")
	}
}

func TestWriterHardLimitMarksIncomplete(t *testing.T) {
	store := newArtifactTestStore(t, 4, 5)
	first, err := store.Begin(context.Background(), Metadata{})
	if err != nil {
		t.Fatal("begin first writer failed")
	}
	entries, err := os.ReadDir(store.root)
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".staging") {
		t.Fatal("writer did not create private staging before its first byte")
	}
	if info, err := entries[0].Info(); err != nil || info.Size() != 0 {
		t.Fatal("new staging artifact was not empty")
	}
	written, writeErr := first.Write([]byte("abcdef"))
	var limitErr *budget.LimitError
	if written != 4 || !errors.As(writeErr, &limitErr) {
		t.Fatal("single-file budget did not stop the write at its hard limit")
	}
	firstRef, err := first.Commit(context.Background())
	if err != nil {
		t.Fatal("commit incomplete file-limited artifact failed")
	}
	if !firstRef.Available || firstRef.Complete || firstRef.Bytes != 4 {
		t.Fatal("file-limited artifact ref was not marked incomplete")
	}

	second, err := store.Begin(context.Background(), Metadata{})
	if err != nil {
		t.Fatal("begin second writer failed")
	}
	written, writeErr = second.Write([]byte("xy"))
	limitErr = nil
	if written != 1 || !errors.As(writeErr, &limitErr) || limitErr.Scope != string(budget.ArtifactMaxTotalBytes) {
		t.Fatal("total budget did not stop the write before exceeding capacity")
	}
	secondRef, err := second.Commit(context.Background())
	if err != nil {
		t.Fatal("commit incomplete total-limited artifact failed")
	}
	if !secondRef.Available || secondRef.Complete || secondRef.Bytes != 1 {
		t.Fatal("total-limited artifact ref was not marked incomplete")
	}
	if store.totalBytes != 5 {
		t.Fatal("store capacity accounting did not match accepted bytes")
	}
}

func TestArtifactRefSerializationContainsNoPath(t *testing.T) {
	store := newArtifactTestStore(t, 64, 128)
	writer, err := store.Begin(context.Background(), Metadata{})
	if err != nil {
		t.Fatal("begin artifact failed")
	}
	if _, err := writer.Write([]byte("opaque")); err != nil {
		t.Fatal("write artifact failed")
	}
	ref, err := writer.Commit(context.Background())
	if err != nil {
		t.Fatal("commit artifact failed")
	}
	encoded, err := json.Marshal(ref)
	if err != nil {
		t.Fatal("serialize artifact ref failed")
	}
	if strings.Contains(string(encoded), store.root) || strings.Contains(string(encoded), filepath.Base(store.root)) || strings.Contains(string(encoded), ".artifact") || strings.Contains(string(encoded), ".staging") {
		t.Fatal("artifact ref serialization contained a filesystem path")
	}
}

func TestCleanupHonorsRetentionCapacityAndActiveWriters(t *testing.T) {
	store := newArtifactTestStore(t, 64, 64)
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	now = now.Add(-48 * time.Hour)
	expired := commitArtifact(t, store, "old")
	now = now.Add(36 * time.Hour)
	oldestRetained := commitArtifact(t, store, "12345")
	now = now.Add(11 * time.Hour)
	newestRetained := commitArtifact(t, store, "123456")
	now = now.Add(-71 * time.Hour)
	active, err := store.Begin(context.Background(), Metadata{})
	if err != nil {
		t.Fatal("begin active artifact failed")
	}
	if _, err := active.Write([]byte("active")); err != nil {
		t.Fatal("write active artifact failed")
	}
	activeWriter := active.(*fileWriter)

	now = time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	store.options.MaxTotalBytes = 12
	result, err := store.Cleanup(context.Background())
	if err != nil {
		t.Fatal("artifact cleanup failed")
	}
	if result.Removed != 2 || result.ReclaimedBytes != 8 || result.Failed != 0 || store.totalBytes != 12 {
		t.Fatal("cleanup did not deterministically apply retention and capacity")
	}
	for _, removed := range []Ref{expired, oldestRetained} {
		if _, _, err := store.OpenForUser(context.Background(), removed.ID); err == nil {
			t.Fatal("cleanup retained an expired or over-capacity artifact")
		}
	}
	reader, opened, err := store.OpenForUser(context.Background(), newestRetained.ID)
	if err != nil || opened != newestRetained {
		t.Fatal("cleanup removed the newest retained artifact")
	}
	if err := reader.Close(); err != nil {
		t.Fatal("close retained artifact failed")
	}
	activePath := filepath.Join(store.root, activeWriter.stagingName)
	if _, err := os.Stat(activePath); err != nil {
		t.Fatal("cleanup removed an active writer")
	}

	retainedPath := filepath.Join(store.root, newestRetained.ID+".artifact")
	if err := store.Close(); err != nil {
		t.Fatal("artifact store close failed")
	}
	if err := store.Close(); err != nil {
		t.Fatal("repeated artifact store close changed the result")
	}
	if _, err := os.Stat(retainedPath); err != nil {
		t.Fatal("store close deleted a confirmed artifact")
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatal("store close deleted an unconfirmed artifact")
	}
	if err := active.Abort(); err != nil {
		t.Fatal("active writer could not abort after store close")
	}

	t.Run("bounded deletion failure", func(t *testing.T) {
		failedStore := newArtifactTestStore(t, 64, 64)
		failedNow := now.Add(-48 * time.Hour)
		failedStore.now = func() time.Time { return failedNow }
		ref := commitArtifact(t, failedStore, "failure")
		path := filepath.Join(failedStore.root, ref.ID+".artifact")
		if err := os.Remove(path); err != nil {
			t.Fatal("remove cleanup failure fixture failed")
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal("create cleanup failure directory failed")
		}
		if err := os.WriteFile(filepath.Join(path, "child"), []byte("fixture"), 0o600); err != nil {
			t.Fatal("create cleanup failure child failed")
		}
		failedNow = now
		result, err := failedStore.Cleanup(context.Background())
		if err == nil || result.Removed != 0 || result.ReclaimedBytes != 0 || result.Failed != 1 {
			t.Fatal("cleanup failure did not return a bounded partial result")
		}
		if strings.Contains(err.Error(), failedStore.root) || strings.Contains(err.Error(), ref.ID) || len(err.Error()) > 64 {
			t.Fatal("cleanup failure exposed private or unbounded detail")
		}
	})
}

func commitArtifact(t *testing.T, store *fileStore, payload string) Ref {
	t.Helper()
	writer, err := store.Begin(context.Background(), Metadata{})
	if err != nil {
		t.Fatal("begin cleanup artifact failed")
	}
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatal("write cleanup artifact failed")
	}
	ref, err := writer.Commit(context.Background())
	if err != nil {
		t.Fatal("commit cleanup artifact failed")
	}
	return ref
}

func newArtifactTestStore(t *testing.T, maxFileBytes, maxTotalBytes int64) *fileStore {
	t.Helper()
	workspace := t.TempDir()
	root := filepath.Join(t.TempDir(), "artifacts")
	created, err := NewFileStore(FileStoreOptions{
		Root:          root,
		WorkspaceRoot: workspace,
		MaxFileBytes:  maxFileBytes,
		MaxTotalBytes: maxTotalBytes,
		Retention:     24 * time.Hour,
	})
	if err != nil {
		t.Fatal("create artifact test store failed")
	}
	store, ok := created.(*fileStore)
	if !ok {
		t.Fatal("file store constructor returned an unexpected implementation")
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error("close artifact test store failed")
		}
	})
	return store
}
