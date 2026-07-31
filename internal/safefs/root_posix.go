//go:build darwin || linux

package safefs

import (
	"errors"
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
