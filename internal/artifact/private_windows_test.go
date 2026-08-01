//go:build windows

package artifact

import (
	"context"
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
