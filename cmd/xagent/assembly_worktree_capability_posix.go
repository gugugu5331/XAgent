//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func probeAssemblyWorktreePlatformLock(file *os.File) error {
	if file == nil {
		return errAssemblyWorktreeCapabilityUnsafe
	}
	fd := int(file.Fd())
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	if err := unix.Flock(fd, unix.LOCK_UN); err != nil {
		return err
	}
	return nil
}
