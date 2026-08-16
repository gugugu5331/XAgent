//go:build windows

package worktree

// Windows worktree initialization is currently unsupported by the underlying
// handle-relative initializer, so management-file validation fails closed too.
func readGitManagementFile(string, func()) ([]byte, error) {
	return nil, ErrIdentityMismatch
}
