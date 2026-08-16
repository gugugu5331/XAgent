//go:build darwin || linux

package worktree

import (
	"os"

	"golang.org/x/sys/unix"
)

func janitorFileUnshared(file *os.File) bool {
	if file == nil {
		return false
	}
	var stat unix.Stat_t
	return unix.Fstat(int(file.Fd()), &stat) == nil && stat.Nlink == 1
}
