package tool

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
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

func ReadProjectFile(projectRoot string, requestedPath string) (string, []byte, error) {
	resolved, err := ResolveProjectPath(projectRoot, requestedPath)
	if err != nil {
		return "", nil, err
	}
	file, err := openProjectFileNoFollow(projectRoot, resolved, unix.O_RDONLY, 0)
	if err != nil {
		return "", nil, fmt.Errorf("%s: 打开文件失败: %w", ErrPathOutsideProject, err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return "", nil, err
	}
	return resolved, data, nil
}

type resolvedReadPath struct {
	absolute string
	root     string
	display  string
	project  bool
}

func resolveReadPath(scope ReadScope, requestedPath string) (resolvedReadPath, error) {
	if strings.TrimSpace(requestedPath) == "" {
		return resolvedReadPath{}, fmt.Errorf("%s: 路径不能为空", ErrNotFound)
	}
	var normalized ReadScope
	var err error
	if scope.pinned {
		normalized, err = NewPinnedReadScope(scope.ProjectRoot, scope.ExtraRoots)
	} else {
		normalized, err = NewReadScope(scope.ProjectRoot, scope.ExtraRoots)
	}
	if err != nil {
		return resolvedReadPath{}, err
	}
	scope = normalized
	if filepath.IsAbs(requestedPath) {
		resolved, err := resolveWithExistingAncestor(requestedPath)
		if err != nil {
			return resolvedReadPath{}, fmt.Errorf("解析路径失败: %w", err)
		}
		root, project, ok := readRootForPath(scope, resolved)
		if !ok {
			return resolvedReadPath{}, fmt.Errorf("%s: 路径 %q 位于允许的只读根外", ErrPathOutsideProject, requestedPath)
		}
		return resolvedReadTarget(scope, root, resolved, project), nil
	}

	roots := readRoots(scope)
	var missing resolvedReadPath
	for index, root := range roots {
		candidate := filepath.Join(root.path, requestedPath)
		resolved, err := resolveWithExistingAncestor(candidate)
		if err != nil {
			return resolvedReadPath{}, fmt.Errorf("解析路径失败: %w", err)
		}
		if !isPathInsideRoot(root.path, resolved) {
			return resolvedReadPath{}, fmt.Errorf("%s: 路径 %q 位于允许的只读根外", ErrPathOutsideProject, requestedPath)
		}
		target := resolvedReadTarget(scope, root.path, resolved, root.project)
		if index == 0 {
			missing = target
		}
		if _, err := os.Lstat(resolved); err == nil {
			return target, nil
		} else if !os.IsNotExist(err) {
			return resolvedReadPath{}, err
		}
	}
	return missing, nil
}

func readFileInScope(scope ReadScope, requestedPath string) (resolvedReadPath, []byte, error) {
	target, err := resolveReadPath(scope, requestedPath)
	if err != nil {
		return resolvedReadPath{}, nil, err
	}
	file, err := openFileNoFollow(target.root, target.absolute, unix.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return resolvedReadPath{}, nil, fmt.Errorf("%s: 打开文件失败: %w", ErrNotFound, err)
		}
		return resolvedReadPath{}, nil, fmt.Errorf("%s: 打开文件失败: %w", ErrPathOutsideProject, err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return resolvedReadPath{}, nil, err
	}
	return target, data, nil
}

func WriteProjectFile(projectRoot string, requestedPath string, data []byte) (string, error) {
	resolved, err := ResolveProjectPath(projectRoot, requestedPath)
	if err != nil {
		return "", err
	}
	if err := makeProjectDirsNoFollow(projectRoot, filepath.Dir(resolved)); err != nil {
		return "", err
	}
	resolved, err = ResolveProjectPath(projectRoot, requestedPath)
	if err != nil {
		return "", err
	}
	file, err := openProjectFileNoFollow(projectRoot, resolved, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("%s: 打开文件失败: %w", ErrPathOutsideProject, err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return "", err
	}
	return resolved, nil
}

func openProjectFileNoFollow(projectRoot string, absolutePath string, flags int, perm uint32) (*os.File, error) {
	root, err := realProjectRoot(projectRoot)
	if err != nil {
		return nil, err
	}
	return openFileNoFollow(root, absolutePath, flags, perm)
}

func openFileNoFollow(root string, absolutePath string, flags int, perm uint32) (*os.File, error) {
	resolved, err := resolveWithExistingAncestor(absolutePath)
	if err != nil {
		return nil, err
	}
	if !isPathInsideRoot(root, resolved) {
		return nil, fmt.Errorf("path outside project")
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return nil, err
	}
	parts := splitPathParts(rel)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty relative path")
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	currentFD := rootFD
	for index, part := range parts {
		last := index == len(parts)-1
		if last {
			fd, err := unix.Openat(currentFD, part, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
			unix.Close(currentFD)
			if err != nil {
				return nil, err
			}
			return os.NewFile(uintptr(fd), resolved), nil
		}
		nextFD, err := unix.Openat(currentFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(currentFD)
		if err != nil {
			return nil, err
		}
		currentFD = nextFD
	}
	unix.Close(currentFD)
	return nil, fmt.Errorf("invalid path")
}

type readRoot struct {
	path    string
	project bool
}

func readRoots(scope ReadScope) []readRoot {
	roots := make([]readRoot, 0, len(scope.ExtraRoots)+1)
	roots = append(roots, readRoot{path: scope.ProjectRoot, project: true})
	for _, root := range scope.ExtraRoots {
		roots = append(roots, readRoot{path: root})
	}
	return roots
}

func readRootForPath(scope ReadScope, absolutePath string) (string, bool, bool) {
	if isPathInsideRoot(scope.ProjectRoot, absolutePath) {
		return scope.ProjectRoot, true, true
	}
	for _, root := range scope.ExtraRoots {
		if isPathInsideRoot(root, absolutePath) {
			return root, false, true
		}
	}
	return "", false, false
}

func resolvedReadTarget(scope ReadScope, root string, absolutePath string, project bool) resolvedReadPath {
	return resolvedReadPath{
		absolute: absolutePath,
		root:     root,
		display:  displayReadPath(scope, root, absolutePath, project),
		project:  project,
	}
}

func displayReadPath(scope ReadScope, root string, absolutePath string, project bool) string {
	rel, err := filepath.Rel(root, absolutePath)
	if err != nil {
		return absolutePath
	}
	rel = filepath.ToSlash(rel)
	if project {
		return rel
	}
	base := filepath.Base(root)
	duplicateIndex := 0
	duplicateCount := 0
	for index, extra := range scope.ExtraRoots {
		if filepath.Base(extra) == base {
			duplicateCount++
			if extra == root {
				duplicateIndex = index + 1
			}
		}
	}
	label := "skill:" + base
	if duplicateCount > 1 {
		label = fmt.Sprintf("%s#%d", label, duplicateIndex)
	}
	if rel == "." {
		return label
	}
	return label + "/" + rel
}

func makeProjectDirsNoFollow(projectRoot string, absoluteDir string) error {
	root, err := realProjectRoot(projectRoot)
	if err != nil {
		return err
	}
	resolved, err := resolveWithExistingAncestor(absoluteDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && !isPathInsideRoot(root, resolved) {
		return fmt.Errorf("%s: 路径位于项目根目录外", ErrPathOutsideProject)
	}
	rel, err := filepath.Rel(root, filepath.Clean(absoluteDir))
	if err != nil {
		return err
	}
	current := root
	for _, part := range splitPathParts(rel) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("%s: 路径 %q 位于项目根目录外", ErrPathOutsideProject, current)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.Mkdir(current, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func splitPathParts(rel string) []string {
	rel = filepath.Clean(rel)
	if rel == "." || rel == "" {
		return nil
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" && part != "." {
			out = append(out, part)
		}
	}
	return out
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
