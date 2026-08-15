//go:build darwin || linux

package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type posixArtifactIdentity struct {
	device uint64
	inode  uint64
}

type posixPrivateArtifactRoot struct {
	mu     sync.RWMutex
	fd     int
	closed bool
}

func openPrivateArtifactRoot(root, workspace string, create bool) (privateArtifactRoot, string, error) {
	if !validPOSIXArtifactPath(root) || !validPOSIXArtifactPath(workspace) {
		return nil, "", errors.New("artifact private root path is invalid")
	}

	workspaceFD, workspaceChain, err := openPOSIXDirectoryChain(workspace, false, posixArtifactIdentity{})
	if err != nil {
		return nil, "", errors.New("artifact workspace root is unavailable")
	}
	workspaceIdentity, err := posixDirectoryIdentity(workspaceFD)
	_ = unix.Close(workspaceFD)
	if err != nil {
		return nil, "", errors.New("artifact workspace identity is unavailable")
	}

	rootFD, rootChain, err := openPOSIXDirectoryChain(root, create, workspaceIdentity)
	if err != nil {
		return nil, "", errors.New("artifact private root is unavailable")
	}
	rootIdentity, err := posixDirectoryIdentity(rootFD)
	if err != nil {
		_ = unix.Close(rootFD)
		return nil, "", errors.New("artifact private root identity is unavailable")
	}
	if posixIdentityInChain(workspaceIdentity, rootChain) || posixIdentityInChain(rootIdentity, workspaceChain) {
		_ = unix.Close(rootFD)
		return nil, "", errors.New("artifact private root must be outside the workspace")
	}
	if err := verifyPOSIXDirectoryFD(rootFD, 0o700); err != nil {
		_ = unix.Close(rootFD)
		return nil, "", errors.New("artifact private root validation failed")
	}

	return &posixPrivateArtifactRoot{fd: rootFD}, filepath.Clean(root), nil
}

func validPOSIXArtifactPath(path string) bool {
	return path != "" && utf8.ValidString(path) && !strings.ContainsRune(path, 0) &&
		filepath.IsAbs(path) && filepath.Clean(path) == path
}

// openPOSIXDirectoryChain opens every path component relative to its already
// opened parent. Missing components are created only when requested, and no
// component is ever followed through a symbolic link.
func openPOSIXDirectoryChain(path string, create bool, forbidden posixArtifactIdentity) (int, []posixArtifactIdentity, error) {
	current, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, nil, err
	}
	chain := make([]posixArtifactIdentity, 0, strings.Count(path, string(filepath.Separator))+1)
	rootIdentity, err := posixDirectoryIdentity(current)
	if err != nil {
		_ = unix.Close(current)
		return -1, nil, err
	}
	chain = append(chain, rootIdentity)
	if forbidden != (posixArtifactIdentity{}) && rootIdentity == forbidden {
		_ = unix.Close(current)
		return -1, nil, errors.New("artifact directory enters the workspace")
	}

	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		created := false
		if openErr != nil && create && errors.Is(openErr, unix.ENOENT) {
			mkdirErr := unix.Mkdirat(current, component, 0o700)
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(current)
				return -1, nil, mkdirErr
			}
			created = mkdirErr == nil
			next, openErr = unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			_ = unix.Close(current)
			return -1, nil, openErr
		}
		_ = unix.Close(current)
		current = next
		if created {
			if err := verifyPOSIXDirectoryFD(current, 0o700); err != nil {
				_ = unix.Close(current)
				return -1, nil, err
			}
		}
		identity, err := posixDirectoryIdentity(current)
		if err != nil {
			_ = unix.Close(current)
			return -1, nil, err
		}
		chain = append(chain, identity)
		if forbidden != (posixArtifactIdentity{}) && identity == forbidden {
			_ = unix.Close(current)
			return -1, nil, errors.New("artifact directory enters the workspace")
		}
	}
	return current, chain, nil
}

func posixDirectoryIdentity(fd int) (posixArtifactIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return posixArtifactIdentity{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return posixArtifactIdentity{}, errors.New("artifact object is not a directory")
	}
	return posixArtifactIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}

func posixIdentityInChain(identity posixArtifactIdentity, chain []posixArtifactIdentity) bool {
	for _, candidate := range chain {
		if candidate == identity {
			return true
		}
	}
	return false
}

func verifyPOSIXDirectoryFD(fd int, permissions uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || uint32(stat.Mode)&0o777 != permissions || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("artifact permissions are not private")
	}
	return nil
}

func (r *posixPrivateArtifactRoot) create(name string) (*os.File, error) {
	if !validPOSIXArtifactLeaf(name) {
		return nil, errors.New("artifact private file name is invalid")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || r.fd < 0 {
		return nil, errors.New("artifact private root is closed")
	}
	fd, err := unix.Openat(r.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	return posixArtifactFile(fd, "artifact-staging")
}

func (r *posixPrivateArtifactRoot) open(name string) (*os.File, error) {
	if !validPOSIXArtifactLeaf(name) {
		return nil, errors.New("artifact private file name is invalid")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || r.fd < 0 {
		return nil, errors.New("artifact private root is closed")
	}
	fd, err := unix.Openat(r.fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return posixArtifactFile(fd, "artifact")
}

func (r *posixPrivateArtifactRoot) rename(oldName, newName string) error {
	if !validPOSIXArtifactLeaf(oldName) || !validPOSIXArtifactLeaf(newName) {
		return errors.New("artifact private file name is invalid")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || r.fd < 0 {
		return errors.New("artifact private root is closed")
	}
	if err := verifyPOSIXArtifactAt(r.fd, oldName); err != nil {
		return err
	}
	if err := unix.Renameat(r.fd, oldName, r.fd, newName); err != nil {
		return err
	}
	return verifyPOSIXArtifactAt(r.fd, newName)
}

func (r *posixPrivateArtifactRoot) remove(name string) error {
	if !validPOSIXArtifactLeaf(name) {
		return errors.New("artifact private file name is invalid")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || r.fd < 0 {
		return errors.New("artifact private root is closed")
	}
	if err := verifyPOSIXArtifactAt(r.fd, name); err != nil {
		return err
	}
	return unix.Unlinkat(r.fd, name, 0)
}

func (r *posixPrivateArtifactRoot) close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	fd := r.fd
	r.fd = -1
	return unix.Close(fd)
}

func validPOSIXArtifactLeaf(name string) bool {
	return name != "" && name != "." && name != ".." && utf8.ValidString(name) &&
		!strings.ContainsRune(name, 0) && filepath.Base(name) == name &&
		!strings.ContainsRune(name, filepath.Separator)
}

func posixArtifactFile(fd int, label string) (*os.File, error) {
	if err := verifyPOSIXArtifactFD(fd); err != nil {
		_ = unix.Close(fd)
		return nil, errors.New("artifact private file validation failed")
	}
	file := os.NewFile(uintptr(fd), label)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("artifact private file is unavailable")
	}
	return file, nil
}

func verifyPOSIXArtifactAt(rootFD int, name string) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(rootFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	return verifyPOSIXArtifactStat(&stat)
}

func verifyPOSIXArtifactFD(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	return verifyPOSIXArtifactStat(&stat)
}

func verifyPOSIXArtifactStat(stat *unix.Stat_t) error {
	if stat == nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("artifact permissions are not private")
	}
	return nil
}
