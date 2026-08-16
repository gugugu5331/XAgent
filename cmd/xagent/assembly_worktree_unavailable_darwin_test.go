//go:build darwin

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWorktreeUnavailableProbeRootSwapLeavesNoArtifacts(t *testing.T) {
	if !assemblyWorktreeInitializerSupported() {
		t.Fatal("Darwin initializer capability was reported unavailable")
	}
	base := t.TempDir()
	root := filepath.Join(base, "root")
	replacement := filepath.Join(base, "replacement")
	for _, path := range []string{root, replacement} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = unix.RenameatxNp(unix.AT_FDCWD, root, unix.AT_FDCWD, replacement, unix.RENAME_SWAP)
			}
		}
	}()
	for range 500 {
		_ = probeAssemblyWorktreeFilesystemRoot(context.Background(), root, false)
	}
	close(stop)
	wait.Wait()
	for _, path := range []string{root, replacement} {
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".xagent-worktree-capability-") {
				t.Fatalf("root swap left probe artifact %q", filepath.Join(path, entry.Name()))
			}
		}
	}
}
