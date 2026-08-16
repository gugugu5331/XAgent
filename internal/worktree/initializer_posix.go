//go:build darwin || linux

package worktree

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type posixInitializerRoot struct {
	fd int
}

func (r *posixInitializerRoot) Identity(relative string) (string, error) {
	parent, leaf, closeParent, err := r.openParent(relative)
	if err != nil {
		return "", err
	}
	if closeParent {
		defer unix.Close(parent)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(parent, leaf, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", err
	}
	return initializerIdentityFromStat(&stat), nil
}

func initializerIdentityFromFile(file *os.File) (string, error) {
	if file == nil {
		return "", ErrIntegrityMismatch
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return "", err
	}
	return initializerIdentityFromStat(&stat), nil
}

func initializerIdentityFromStat(stat *unix.Stat_t) string {
	payload := make([]byte, 20)
	binary.LittleEndian.PutUint64(payload[0:8], uint64(stat.Dev))
	binary.LittleEndian.PutUint64(payload[8:16], uint64(stat.Ino))
	binary.LittleEndian.PutUint32(payload[16:20], uint32(stat.Mode)&unix.S_IFMT)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func newInitializerRoot(root string) (initializerRoot, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &posixInitializerRoot{fd: fd}, nil
}

func (r *posixInitializerRoot) Close() error {
	if r == nil || r.fd < 0 {
		return nil
	}
	err := unix.Close(r.fd)
	r.fd = -1
	return err
}

func (r *posixInitializerRoot) Stat(relative string) (os.FileInfo, error) {
	fd, err := r.open(relative, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "")
	if file == nil {
		_ = unix.Close(fd)
		return nil, ErrUnsafePath
	}
	defer file.Close()
	return file.Stat()
}

func (r *posixInitializerRoot) OpenRead(relative string) (*os.File, os.FileInfo, error) {
	fd, err := r.open(relative, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), "")
	if file == nil {
		_ = unix.Close(fd)
		return nil, nil, ErrUnsafePath
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, ErrUnsafePath
	}
	return file, info, nil
}

func (r *posixInitializerRoot) ReadDir(relative string) ([]os.DirEntry, error) {
	fd, err := r.open(relative, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "")
	if file == nil {
		_ = unix.Close(fd)
		return nil, ErrUnsafePath
	}
	defer file.Close()
	return file.ReadDir(-1)
}

func (r *posixInitializerRoot) Mkdir(relative string, mode os.FileMode) error {
	parent, leaf, closeParent, err := r.openParent(relative)
	if err != nil {
		return err
	}
	if closeParent {
		defer unix.Close(parent)
	}
	if err := unix.Mkdirat(parent, leaf, uint32(mode.Perm())); err != nil {
		return err
	}
	fd, err := unix.Openat(parent, leaf, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

func (r *posixInitializerRoot) CreateFile(relative string, mode os.FileMode) (*os.File, error) {
	parent, leaf, closeParent, err := r.openParent(relative)
	if err != nil {
		return nil, err
	}
	if closeParent {
		defer unix.Close(parent)
	}
	fd, err := unix.Openat(parent, leaf,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "")
	if file == nil {
		_ = unix.Close(fd)
		return nil, ErrUnsafePath
	}
	return file, nil
}

func (r *posixInitializerRoot) Symlink(relative, target string) error {
	parent, leaf, closeParent, err := r.openParent(relative)
	if err != nil {
		return err
	}
	if closeParent {
		defer unix.Close(parent)
	}
	return unix.Symlinkat(target, parent, leaf)
}

func (r *posixInitializerRoot) Readlink(relative string) (string, error) {
	parent, leaf, closeParent, err := r.openParent(relative)
	if err != nil {
		return "", err
	}
	if closeParent {
		defer unix.Close(parent)
	}
	buffer := make([]byte, maxMetadataPath+1)
	count, err := unix.Readlinkat(parent, leaf, buffer)
	if err != nil {
		return "", err
	}
	if count > maxMetadataPath {
		return "", ErrMetadataTooLarge
	}
	return string(buffer[:count]), nil
}

func (r *posixInitializerRoot) Remove(relative string, directory bool) error {
	parent, leaf, closeParent, err := r.openParent(relative)
	if err != nil {
		return err
	}
	if closeParent {
		defer unix.Close(parent)
	}
	flags := 0
	if directory {
		flags = unix.AT_REMOVEDIR
	}
	return unix.Unlinkat(parent, leaf, flags)
}

func (r *posixInitializerRoot) open(relative string, flags int, mode uint32) (int, error) {
	parent, leaf, closeParent, err := r.openParent(relative)
	if err != nil {
		return -1, err
	}
	if closeParent {
		defer unix.Close(parent)
	}
	return unix.Openat(parent, leaf, flags, mode)
}

func (r *posixInitializerRoot) openParent(relative string) (int, string, bool, error) {
	if r == nil || r.fd < 0 || !validInitRelative(relative) {
		return -1, "", false, ErrUnsafePath
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	current := r.fd
	owned := false
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if owned {
				_ = unix.Close(current)
			}
			return -1, "", false, err
		}
		if owned {
			_ = unix.Close(current)
		}
		current, owned = next, true
	}
	return current, parts[len(parts)-1], owned, nil
}

var _ initializerRoot = (*posixInitializerRoot)(nil)
