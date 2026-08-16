//go:build darwin || linux

package worktree

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func readGitManagementFile(worktreeRoot string, beforeRead func()) ([]byte, error) {
	rootFD, err := unix.Open(worktreeRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrIdentityMismatch
	}
	defer unix.Close(rootFD)
	if beforeRead != nil {
		beforeRead()
	}
	fileFD, err := unix.Openat(rootFD, ".git", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrIdentityMismatch
	}
	file := os.NewFile(uintptr(fileFD), ".git")
	if file == nil {
		_ = unix.Close(fileFD)
		return nil, ErrIdentityMismatch
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fileFD, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size <= 0 || stat.Size > maxMetadataPath {
		return nil, ErrIdentityMismatch
	}
	data, err := io.ReadAll(io.LimitReader(file, maxMetadataPath+1))
	if err != nil || len(data) > maxMetadataPath {
		return nil, ErrIdentityMismatch
	}
	return data, nil
}
