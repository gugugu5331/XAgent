//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package worktree

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

func repositoryDirectoryObjectIdentity(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	identity := make([]byte, 16)
	binary.LittleEndian.PutUint64(identity[:8], uint64(stat.Dev))
	binary.LittleEndian.PutUint64(identity[8:], uint64(stat.Ino))
	return identity, nil
}
