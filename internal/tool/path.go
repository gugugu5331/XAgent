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

	root, err := realProjectRoot(projectRoot)
	if err != nil {
		return "", err
	}

	candidate := requestedPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = resolveWithExistingAncestor(candidate)
	if err != nil {
		return "", fmt.Errorf("解析路径失败: %w", err)
	}

	if !isPathInsideRoot(root, candidate) {
		return "", fmt.Errorf("%s: 路径 %q 位于项目根目录外", ErrPathOutsideProject, requestedPath)
	}
	return candidate, nil
}

func RelativeToRoot(projectRoot string, absolutePath string) string {
	root, err := realProjectRoot(projectRoot)
	if err != nil {
		return absolutePath
	}
	resolved, err := resolveWithExistingAncestor(absolutePath)
	if err != nil {
		resolved = absolutePath
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return absolutePath
	}
	return filepath.ToSlash(rel)
}

func realProjectRoot(projectRoot string) (string, error) {
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("解析项目根目录失败: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("解析项目根目录符号链接失败: %w", err)
	}
	return root, nil
}

func resolveWithExistingAncestor(path string) (string, error) {
	candidate, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}

	current := candidate
	missingParts := []string{}
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missingParts) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missingParts[i])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", os.ErrNotExist
		}
		missingParts = append(missingParts, filepath.Base(current))
		current = parent
	}
}

func isPathInsideRoot(root string, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}
