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

// ResolveReadPath applies the same project-first lookup used by the read-only
// tools while keeping extra Skill package roots distinct in permission rules
// and grant fingerprints.
func ResolveReadPath(projectRoot string, extraRoots []string, requestedPath string) (PathResolution, error) {
	if strings.TrimSpace(requestedPath) == "" {
		return PathResolution{}, fmt.Errorf("path is empty")
	}
	project, err := realProjectRoot(projectRoot)
	if err != nil {
		return PathResolution{}, err
	}
	type allowedRoot struct {
		path  string
		label string
	}
	roots := []allowedRoot{{path: project}}
	seen := map[string]struct{}{project: {}}
	for _, root := range extraRoots {
		canonical, err := realProjectRoot(root)
		if err != nil {
			return PathResolution{}, err
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		roots = append(roots, allowedRoot{path: canonical, label: fmt.Sprintf("@skill/%d", len(roots))})
	}

	resolve := func(root allowedRoot, candidate string) (PathResolution, error) {
		resolved, err := resolveWithExistingAncestor(candidate)
		if err != nil {
			return PathResolution{}, err
		}
		if !isPathInsideRoot(root.path, resolved) {
			return PathResolution{}, fmt.Errorf("path %q is outside allowed read roots", requestedPath)
		}
		rel, err := filepath.Rel(root.path, resolved)
		if err != nil {
			return PathResolution{}, err
		}
		rel = filepath.ToSlash(rel)
		if root.label != "" {
			rel = root.label + "/" + rel
		}
		return PathResolution{Original: requestedPath, Absolute: resolved, Relative: rel}, nil
	}

	if filepath.IsAbs(requestedPath) {
		resolved, err := resolveWithExistingAncestor(requestedPath)
		if err != nil {
			return PathResolution{}, err
		}
		for _, root := range roots {
			if isPathInsideRoot(root.path, resolved) {
				return resolve(root, resolved)
			}
		}
		return PathResolution{}, fmt.Errorf("path %q is outside allowed read roots", requestedPath)
	}

	var projectFallback PathResolution
	for index, root := range roots {
		candidate := filepath.Join(root.path, requestedPath)
		resolved, err := resolve(root, candidate)
		if err != nil {
			return PathResolution{}, err
		}
		if index == 0 {
			projectFallback = resolved
		}
		if _, err := os.Lstat(resolved.Absolute); err == nil {
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return PathResolution{}, err
		}
	}
	return projectFallback, nil
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

func normalizeReadPathArgument(toolName string, arguments map[string]any, projectRoot string, extraRoots []string) (string, string, error) {
	pathParam := DefaultPathParam(toolName, arguments)
	pathValue := "."
	if pathParam != "" {
		if value, ok := arguments[pathParam].(string); ok && strings.TrimSpace(value) != "" {
			pathValue = value
		}
	}
	resolved, err := ResolveReadPath(projectRoot, extraRoots, pathValue)
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
