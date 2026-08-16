//go:build windows

package worktree

import "os"

// Windows 版本在安全的 handle-relative 创建能力接入前保持失败关闭。
func newInitializerRoot(string) (initializerRoot, error) {
	return nil, ErrUnsupportedPlatform
}

func initializerIdentityFromFile(*os.File) (string, error) {
	return "", ErrUnsupportedPlatform
}
