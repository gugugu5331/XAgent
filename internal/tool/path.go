package tool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ResolveProjectPath(projectRoot string, requestedPath string) (string, error) {
	if strings.TrimSpace(requestedPath) == "" {
		return "", fmt.Errorf("%s: 路径不能为空", ErrNotFound)
	}

	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("解析项目根目录失败: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("解析项目根目录符号链接失败: %w", err)
	}

	candidate := requestedPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate = filepath.Clean(candidate)
	if evaluated, err := filepath.EvalSymlinks(candidate); err == nil {
		candidate = evaluated
	} else {
		parent := filepath.Dir(candidate)
		if evaluatedParent, parentErr := filepath.EvalSymlinks(parent); parentErr == nil {
			candidate = filepath.Join(evaluatedParent, filepath.Base(candidate))
		}
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("解析路径失败: %w", err)
	}

	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return "", fmt.Errorf("解析相对路径失败: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s: 路径 %q 位于项目根目录外", ErrPathOutsideProject, requestedPath)
	}
	return candidate, nil
}

func RelativeToRoot(projectRoot string, absolutePath string) string {
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return absolutePath
	}
	rel, err := filepath.Rel(root, absolutePath)
	if err != nil {
		return absolutePath
	}
	return filepath.ToSlash(rel)
}
