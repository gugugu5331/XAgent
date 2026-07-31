//go:build unix

package repoaudit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func createPrivateDirectory(path string, mode os.FileMode) error {
	if err := os.Mkdir(path, mode); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	return validatePrivateDirectory(path, mode)
}

func createPrivateFile(path string, mode os.FileMode, readWrite bool) (*os.File, error) {
	flag := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if readWrite {
		flag = os.O_RDWR | os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(path, flag, mode)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		file.Close()
		return nil, fs.ErrInvalid
	}
	return file, nil
}

func lockEvidenceLease(file *os.File) error {
	unix.CloseOnExec(int(file.Fd()))
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock evidence lease: %w", err)
	}
	return nil
}

func unlockEvidenceLease(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		return fmt.Errorf("unlock evidence lease: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validatePrivateDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode || info.Mode()&os.ModeSetuid != 0 || info.Mode()&os.ModeSetgid != 0 || info.Mode()&os.ModeSticky != 0 {
		return errors.New("private directory type or mode is invalid")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("private directory owner is invalid")
	}
	return nil
}

func validatePrivateFile(path string, mode os.FileMode, empty bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		return errors.New("private file type or mode is invalid")
	}
	if empty && info.Size() != 0 {
		return errors.New("private sentinel must be empty")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return errors.New("private file owner or link count is invalid")
	}
	return nil
}
