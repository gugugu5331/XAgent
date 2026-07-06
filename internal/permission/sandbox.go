package permission

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type PathResolution struct {
	Original string
	Absolute string
	Relative string
}

func ResolveProjectPath(projectRoot string, requestedPath string) (PathResolution, error) {
	if strings.TrimSpace(requestedPath) == "" {
		return PathResolution{}, fmt.Errorf("path is empty")
	}
	root, err := realProjectRoot(projectRoot)
	if err != nil {
		return PathResolution{}, err
	}
	candidate := requestedPath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	resolved, err := resolveWithExistingAncestor(candidate)
	if err != nil {
		return PathResolution{}, err
	}
	if !isPathInsideRoot(root, resolved) {
		return PathResolution{}, fmt.Errorf("path %q is outside project", requestedPath)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return PathResolution{}, err
	}
	return PathResolution{Original: requestedPath, Absolute: resolved, Relative: filepath.ToSlash(rel)}, nil
}

func normalizePathArgument(toolName string, arguments map[string]any, projectRoot string) (string, string, error) {
	pathParam := DefaultPathParam(toolName, arguments)
	pathValue := "."
	if pathParam != "" {
		if value, ok := arguments[pathParam].(string); ok && strings.TrimSpace(value) != "" {
			pathValue = value
		}
	}
	if toolName == "Glob" && pathParam == "" {
		pathValue = "."
	}
	resolved, err := ResolveProjectPath(projectRoot, pathValue)
	if err != nil {
		return "", pathValue, err
	}
	return resolved.Relative, pathValue, nil
}

func realProjectRoot(projectRoot string) (string, error) {
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
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
