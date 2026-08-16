//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package worktree

import (
	"errors"
	"testing"
)

func TestFileStoreFailsClosedWithoutPlatformLocks(t *testing.T) {
	if _, err := NewFileStore(t.TempDir()); !errors.Is(err, ErrPlatformLockUnsupported) {
		t.Fatalf("FileStore did not fail closed without authoritative OS locks: %v", err)
	}
}
