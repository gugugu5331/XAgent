//go:build darwin || linux

package safefs

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

type posixRoot struct {
	fd     int
	rootID objectIdentity
}

func openPlatformRoot(rootPath string) (rootBackend, error) {
	fd, err := unix.Open(rootPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("safefs platform root open failed")
	}
	identity, err := posixHandleIdentity(fd)
	if err != nil {
		_ = unix.Close(fd)
		return nil, errors.New("safefs platform root identity failed")
	}
	return &posixRoot{fd: fd, rootID: identity}, nil
}

func (r *posixRoot) identity() objectIdentity {
	if r == nil {
		return objectIdentity{}
	}
	return r.rootID
}

func (r *posixRoot) bind(relative string) (bindingResolution, error) {
	current, owned, parent, leaf, err := r.openParent(relative)
	if err != nil {
		return bindingResolution{}, err
	}
	if owned {
		defer unix.Close(current)
	}
	canonicalLeaf := platformCanonicalLeaf(leaf)
	target, err := unix.Openat(current, leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return bindingResolution{kind: missingBinding, parent: parent, leaf: canonicalLeaf}, nil
		}
		return bindingResolution{}, errors.New("safefs platform target open failed")
	}
	defer unix.Close(target)
	object, err := posixHandleIdentity(target)
	if err != nil {
		return bindingResolution{}, errors.New("safefs platform target identity failed")
	}
	return bindingResolution{
		kind:   existingBinding,
		object: object,
		parent: parent,
		leaf:   canonicalLeaf,
	}, nil
}

func (r *posixRoot) openRead(relative string) (platformOpenedFile, error) {
	current, owned, _, leaf, err := r.openParent(relative)
	if err != nil {
		return platformOpenedFile{}, err
	}
	if owned {
		defer unix.Close(current)
	}
	target, err := unix.Openat(current, leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return platformOpenedFile{}, errors.New("safefs platform target open failed")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(target, &stat); err != nil {
		_ = unix.Close(target)
		return platformOpenedFile{}, errors.New("safefs platform target identity failed")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(target)
		return platformOpenedFile{}, errors.New("safefs platform target type rejected")
	}
	file := os.NewFile(uintptr(target), "")
	if file == nil {
		_ = unix.Close(target)
		return platformOpenedFile{}, errors.New("safefs platform file conversion failed")
	}
	return platformOpenedFile{
		file:     file,
		identity: objectIdentityFromNumbers(uint64(stat.Dev), uint64(stat.Ino)),
	}, nil
}

func (r *posixRoot) openDirectory(relative string) (*os.File, error) {
	if r == nil || r.fd < 0 {
		return nil, errors.New("safefs platform root is closed")
	}
	current := r.fd
	owned := false
	components := []string{"."}
	if relative != "." {
		components = strings.Split(relative, "/")
	}
	for _, component := range components {
		next, err := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			if owned {
				_ = unix.Close(current)
			}
			return nil, errors.New("safefs platform directory open failed")
		}
		if owned {
			_ = unix.Close(current)
		}
		current = next
		owned = true
	}
	file := os.NewFile(uintptr(current), "")
	if file == nil {
		_ = unix.Close(current)
		return nil, errors.New("safefs platform directory conversion failed")
	}
	return file, nil
}

func (r *posixRoot) openParent(relative string) (int, bool, objectIdentity, string, error) {
	if r == nil || r.fd < 0 {
		return -1, false, objectIdentity{}, "", errors.New("safefs platform root is closed")
	}
	components := strings.Split(relative, "/")
	leaf := components[len(components)-1]
	current := r.fd
	owned := false
	for _, component := range components[:len(components)-1] {
		next, err := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			if owned {
				_ = unix.Close(current)
			}
			return -1, false, objectIdentity{}, "", errors.New("safefs platform parent open failed")
		}
		if owned {
			_ = unix.Close(current)
		}
		current = next
		owned = true
	}
	parent, err := posixHandleIdentity(current)
	if err != nil {
		if owned {
			_ = unix.Close(current)
		}
		return -1, false, objectIdentity{}, "", errors.New("safefs platform parent identity failed")
	}
	return current, owned, parent, leaf, nil
}

func (r *posixRoot) leafRelation(first, second string) leafRelation {
	return platformLeafRelation(first, second)
}

func (r *posixRoot) close() error {
	if r == nil || r.fd < 0 {
		return nil
	}
	err := unix.Close(r.fd)
	r.fd = -1
	return err
}

func posixHandleIdentity(fd int) (objectIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return objectIdentity{}, err
	}
	return objectIdentityFromNumbers(uint64(stat.Dev), uint64(stat.Ino)), nil
}

func platformDirectoryEntryInfo(directory *os.File, name string) (directoryEntryInfo, error) {
	if directory == nil || name == "" {
		return directoryEntryInfo{}, errors.New("safefs platform directory entry is invalid")
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return directoryEntryInfo{}, err
	}
	return directoryEntryInfo{mode: posixFileMode(uint32(stat.Mode)), size: stat.Size}, nil
}

func posixFileMode(mode uint32) fs.FileMode {
	result := fs.FileMode(mode & 0o777)
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		result |= fs.ModeDir
	case unix.S_IFLNK:
		result |= fs.ModeSymlink
	case unix.S_IFIFO:
		result |= fs.ModeNamedPipe
	case unix.S_IFSOCK:
		result |= fs.ModeSocket
	case unix.S_IFBLK:
		result |= fs.ModeDevice
	case unix.S_IFCHR:
		result |= fs.ModeDevice | fs.ModeCharDevice
	}
	if mode&unix.S_ISUID != 0 {
		result |= fs.ModeSetuid
	}
	if mode&unix.S_ISGID != 0 {
		result |= fs.ModeSetgid
	}
	if mode&unix.S_ISVTX != 0 {
		result |= fs.ModeSticky
	}
	return result
}
