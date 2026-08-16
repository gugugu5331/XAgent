//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func probeAssemblyWorktreePlatformLock(file *os.File) error {
	if file == nil {
		return errAssemblyWorktreeCapabilityUnsafe
	}
	handle := windows.Handle(file.Fd())
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		return err
	}
	return windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
}
