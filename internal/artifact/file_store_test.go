package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
