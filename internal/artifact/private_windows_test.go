//go:build windows

package artifact

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactWindowsACLIsCurrentUserOnly(t *testing.T) {
	store := newArtifactTestStore(t, 64, 128)
	writer, err := store.Begin(context.Background(), Metadata{})
	if err != nil {
		t.Fatal("begin artifact failed")
	}
	if err := verifyWindowsPrivatePath(store.root, true); err != nil {
		t.Fatal("artifact root ACL was not current-user-only")
	}
	if _, err := writer.Write([]byte("private")); err != nil {
		t.Fatal("write private artifact failed")
	}
	ref, err := writer.Commit(context.Background())
	if err != nil {
		t.Fatal("commit private artifact failed")
	}
	if err := verifyWindowsPrivatePath(filepath.Join(store.root, ref.ID+".artifact"), false); err != nil {
		t.Fatal("artifact file ACL was not current-user-only")
	}
}

func TestPrivateArtifactWindowsHandleRelativeOperations(t *testing.T) {
	workspace := t.TempDir()
	parent := t.TempDir()
	rootPath := filepath.Join(parent, "artifacts")
	root, canonical, err := openPrivateArtifactRoot(rootPath, workspace, true)
	if err != nil || root == nil || canonical == "" {
		t.Fatal("open private Windows artifact root failed")
	}
	t.Cleanup(func() { _ = root.close() })
	if err := verifyWindowsPrivatePath(canonical, true); err != nil {
		t.Fatal("created artifact root did not have a private DACL")
	}

	staging := ".fixture.staging"
	file, err := root.create(staging)
	if err != nil {
		t.Fatal("handle-relative artifact creation failed")
	}
	if _, err := file.Write([]byte("private")); err != nil {
		_ = file.Close()
		t.Fatal("write handle-relative artifact failed")
	}
	if err := file.Close(); err != nil {
		t.Fatal("close handle-relative artifact failed")
	}
	if err := root.rename(staging, "fixture.artifact"); err != nil {
		t.Fatal("handle-relative artifact rename failed")
	}
	opened, err := root.open("fixture.artifact")
	if err != nil {
		t.Fatal("handle-relative artifact open failed")
	}
	payload, readErr := io.ReadAll(opened)
	closeErr := opened.Close()
	if readErr != nil || closeErr != nil || string(payload) != "private" {
		t.Fatal("handle-relative artifact payload changed")
	}
	if err := root.remove("fixture.artifact"); err != nil {
		t.Fatal("handle-relative artifact remove failed")
	}
	if _, err := os.Stat(filepath.Join(canonical, "fixture.artifact")); !os.IsNotExist(err) {
		t.Fatal("handle-relative remove retained the artifact")
	}

	movedPath := filepath.Join(parent, "moved-artifacts")
	if err := os.Rename(canonical, movedPath); err != nil {
		t.Fatal("rename opened artifact root fixture failed")
	}
	if err := os.Mkdir(canonical, 0o700); err != nil {
		t.Fatal("replace original artifact path fixture failed")
	}
	file, err = root.create("pinned.artifact")
	if err != nil {
		t.Fatal("pinned root rejected handle-relative creation")
	}
	if err := file.Close(); err != nil {
		t.Fatal("close pinned artifact failed")
	}
	if _, err := os.Stat(filepath.Join(movedPath, "pinned.artifact")); err != nil {
		t.Fatal("artifact owner did not retain the opened directory identity")
	}
	if _, err := os.Stat(filepath.Join(canonical, "pinned.artifact")); !os.IsNotExist(err) {
		t.Fatal("artifact owner returned to the replaced absolute path")
	}
}

func TestPrivateArtifactWindowsRejectsReparseAndBroadACL(t *testing.T) {
	workspace := t.TempDir()

	publicRoot := filepath.Join(t.TempDir(), "inherited-artifacts")
	if err := os.Mkdir(publicRoot, 0o700); err != nil {
		t.Fatal("create inherited-DACL root fixture failed")
	}
	if root, prepared, err := openPrivateArtifactRoot(publicRoot, workspace, true); err == nil || root != nil || prepared != "" {
		if root != nil {
			_ = root.close()
		}
		t.Fatal("artifact root with an inherited broad DACL was accepted")
	}

	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "workspace-link")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal("create Windows reparse fixture failed")
	}
	for _, candidate := range []string{alias, filepath.Join(alias, "artifacts")} {
		root, prepared, err := openPrivateArtifactRoot(candidate, workspace, true)
		if root != nil {
			_ = root.close()
		}
		if err == nil || root != nil || prepared != "" {
			t.Fatal("artifact root followed a Windows reparse point")
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "artifacts")); !os.IsNotExist(err) {
		t.Fatal("reparse rejection created an artifact directory in the workspace")
	}
}
