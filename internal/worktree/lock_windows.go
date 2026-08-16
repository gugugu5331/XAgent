//go:build windows

package worktree

import (
	"errors"

	"golang.org/x/sys/windows"
)

type platformHeldLock struct {
	handle     windows.Handle
	overlapped windows.Overlapped
}

func platformLockSupported() bool { return true }

func tryPlatformFileLock(path string, shared bool) (*platformHeldLock, bool, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	attributes, err := windows.GetFileAttributes(pathPointer)
	if err == nil && attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, false, ErrUnsafePath
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, false, err
	}
	held := &platformHeldLock{handle: handle}
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if err := windows.LockFileEx(handle, flags, 0, 1, 0, &held.overlapped); err != nil {
		_ = windows.CloseHandle(handle)
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return held, true, nil
}

func releasePlatformFileLock(lock *platformHeldLock) error {
	if lock == nil || lock.handle == windows.InvalidHandle {
		return nil
	}
	unlockErr := windows.UnlockFileEx(lock.handle, 0, 1, 0, &lock.overlapped)
	closeErr := windows.CloseHandle(lock.handle)
	lock.handle = windows.InvalidHandle
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
