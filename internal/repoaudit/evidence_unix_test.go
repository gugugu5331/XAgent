//go:build unix

package repoaudit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestEvidenceLeaseSerializesProcessesAndIsNotInherited(t *testing.T) {
	config := newEvidenceTestConfig(t)
	first, err := OpenEvidenceOwner(config)
	if err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(first.lease.Fd(), unix.F_GETFD, 0)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		first.Close()
		t.Fatal("evidence lease is inheritable")
	}

	entered := make(chan *EvidenceOwner, 1)
	errors := make(chan error, 1)
	go func() {
		owner, openErr := OpenEvidenceOwner(config)
		if openErr != nil {
			errors <- openErr
			return
		}
		entered <- owner
	}()
	select {
	case owner := <-entered:
		owner.Close()
		first.Close()
		t.Fatal("second evidence owner entered before lease release")
	case err := <-errors:
		first.Close()
		t.Fatalf("second evidence owner failed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case owner := <-entered:
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	case err := <-errors:
		t.Fatalf("second evidence owner failed after release: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("second evidence owner did not enter after lease release")
	}
}

func TestEvidenceDACLAndModesAreExact(t *testing.T) {
	config := newEvidenceTestConfig(t)
	owner, err := OpenEvidenceOwner(config)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	checks := map[string]os.FileMode{
		config.ControlRoot: 0o700,
		config.EvidenceDir: 0o700,
		filepath.Join(config.ControlRoot, evidenceLeaseName):  0o600,
		filepath.Join(config.ControlRoot, evidenceActiveName): 0o400,
	}
	for path, want := range checks {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("mode(%s) = %o, want %o", filepath.Base(path), got, want)
		}
	}
}
