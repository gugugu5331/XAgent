//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package worktree

type platformHeldLock struct{}

func platformLockSupported() bool { return false }

func tryPlatformFileLock(string, bool) (*platformHeldLock, bool, error) {
	return nil, false, ErrPlatformLockUnsupported
}

func releasePlatformFileLock(*platformHeldLock) error { return ErrPlatformLockUnsupported }
