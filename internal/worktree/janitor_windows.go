//go:build windows

package worktree

import "os"

func janitorFileUnshared(*os.File) bool { return false }
