//go:build !darwin && !linux && !windows

package worktree

func readGitManagementFile(string, func()) ([]byte, error) {
	return nil, ErrIdentityMismatch
}
