//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package worktree

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

type platformHeldLock struct {
	file *os.File
}

func platformLockSupported() bool { return true }

func tryPlatformFileLock(path string, shared bool) (*platformHeldLock, bool, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, false, ErrUnsafePath
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, false, ErrUnsafePath
	}
	operation := unix.LOCK_EX | unix.LOCK_NB
	if shared {
		operation = unix.LOCK_SH | unix.LOCK_NB
	}
	if err := unix.Flock(fd, operation); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &platformHeldLock{file: file}, true, nil
}

func releasePlatformFileLock(lock *platformHeldLock) error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
