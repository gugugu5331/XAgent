//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package worktree

func repositoryDirectoryObjectIdentity(string) ([]byte, error) {
	return nil, ErrPlatformLockUnsupported
}
