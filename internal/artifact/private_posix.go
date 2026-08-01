//go:build darwin || linux

package artifact

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func ensurePrivateArtifactRoot(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("artifact private root is invalid")
	}
	return verifyPOSIXPrivateInfo(info, 0o700, true)
}

func createPrivateArtifactFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "artifact-staging")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("artifact private file is unavailable")
	}
	info, err := file.Stat()
	if err != nil || verifyPOSIXPrivateInfo(info, 0o600, false) != nil {
		_ = file.Close()
		return nil, errors.New("artifact private file validation failed")
	}
	return file, nil
}

func openPrivateArtifactFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "artifact")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("artifact private file is unavailable")
	}
	info, err := file.Stat()
	if err != nil || verifyPOSIXPrivateInfo(info, 0o600, false) != nil {
		_ = file.Close()
		return nil, errors.New("artifact private file validation failed")
	}
	return file, nil
}

func verifyPOSIXPrivateInfo(info os.FileInfo, permissions os.FileMode, directory bool) error {
	if info == nil || info.Mode().Perm() != permissions || directory != info.IsDir() {
		return errors.New("artifact permissions are not private")
	}
	if !directory && !info.Mode().IsRegular() {
		return errors.New("artifact permissions are not private")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("artifact owner is not the current user")
	}
	return nil
}
