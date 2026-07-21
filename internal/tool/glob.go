package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

type GlobTool struct {
	projectRoot string
}

func NewGlobTool(projectRoot string) Tool {
	return &GlobTool{projectRoot: projectRoot}
}

func (t *GlobTool) Name() string { return "Glob" }

func (t *GlobTool) Description() string {
	return "Find files matching a glob pattern in the project and active Skills' read-only package roots. Use this dedicated tool to discover allowed paths before reading or editing; matches must stay within an allowed root."
}

func (t *GlobTool) Risk() Risk { return RiskSafe }

func (t *GlobTool) Schema() Schema {
	return ObjectSchema([]string{"pattern"}, map[string]SchemaProperty{
		"pattern": StringProperty("Glob pattern. Relative patterns search the project and active Skill package roots; absolute patterns must stay in one allowed root."),
	})
}

func (t *GlobTool) Execute(ctx context.Context, input Input) Result {
	pattern, ok := stringArg(input.Arguments, "pattern")
	if !ok {
		return Failure(input, ErrInvalidArguments, "pattern 参数不能为空", true)
	}
	scope, err := effectiveReadScope(ctx, t.projectRoot)
	if err != nil {
		return Failure(input, errorCode(err), fmt.Sprintf("读取范围无效: %v", err), true)
	}
	files, err := globReadScope(ctx, scope, pattern)
	if err != nil {
		code := errorCode(err)
		if errors.Is(err, filepath.ErrBadPattern) {
			code = ErrInvalidArguments
		}
		return Failure(input, code, fmt.Sprintf("glob pattern 无效: %v", err), true)
	}
	return Success(input, fmt.Sprintf("Found %d files", len(files)), joinLines(files), map[string]any{
		"pattern": pattern,
		"count":   len(files),
		"files":   files,
	})
}

type scopedGlobPattern struct {
	root     string
	relative string
}

func globReadScope(ctx context.Context, scope ReadScope, pattern string) ([]string, error) {
	patterns, err := scopedGlobPatterns(scope, pattern)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	files := make([]string, 0)
	for _, candidate := range patterns {
		matches, err := safeGlob(ctx, candidate.root, candidate.relative, 200)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			target, err := resolveReadPath(scope, match)
			if err != nil {
				continue
			}
			if _, exists := seen[target.absolute]; exists {
				continue
			}
			file, err := openFileNoFollow(target.root, target.absolute, unix.O_RDONLY|unix.O_NONBLOCK, 0)
			if err != nil {
				continue
			}
			info, statErr := file.Stat()
			_ = file.Close()
			if statErr != nil || info.IsDir() || !info.Mode().IsRegular() {
				continue
			}
			seen[target.absolute] = struct{}{}
			files = append(files, target.display)
		}
	}
	sort.Strings(files)
	if len(files) > 200 {
		files = files[:200]
	}
	return files, nil
}

func scopedGlobPatterns(scope ReadScope, pattern string) ([]scopedGlobPattern, error) {
	cleaned := filepath.Clean(pattern)
	patterns := make([]scopedGlobPattern, 0, len(scope.ExtraRoots)+1)
	if filepath.IsAbs(cleaned) {
		canonical, err := canonicalGlobPattern(cleaned)
		if err != nil {
			return nil, err
		}
		cleaned = canonical
		for _, root := range readRoots(scope) {
			if !isPathInsideRoot(root.path, cleaned) {
				continue
			}
			relative, err := filepath.Rel(root.path, cleaned)
			if err != nil {
				return nil, err
			}
			patterns = append(patterns, scopedGlobPattern{root: root.path, relative: relative})
		}
		if len(patterns) == 0 {
			return nil, fmt.Errorf("%s: glob pattern 位于允许的只读根外", ErrPathOutsideProject)
		}
		return patterns, nil
	}
	for _, root := range readRoots(scope) {
		joined := filepath.Join(root.path, cleaned)
		if !isPathInsideRoot(root.path, joined) {
			return nil, fmt.Errorf("%s: glob pattern 位于允许的只读根外", ErrPathOutsideProject)
		}
		patterns = append(patterns, scopedGlobPattern{root: root.path, relative: cleaned})
	}
	return patterns, nil
}

func canonicalGlobPattern(pattern string) (string, error) {
	meta := strings.IndexAny(pattern, "*?[")
	if meta < 0 {
		return resolveWithExistingAncestor(pattern)
	}
	separator := strings.LastIndex(pattern[:meta], string(os.PathSeparator))
	if separator < 0 {
		return "", fmt.Errorf("%s: 绝对 glob pattern 缺少根目录", ErrInvalidArguments)
	}
	staticDir := pattern[:separator+1]
	remainder := pattern[separator+1:]
	resolvedDir, err := resolveWithExistingAncestor(staticDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedDir, remainder), nil
}

func safeGlob(ctx context.Context, root string, relativePattern string, limit int) ([]string, error) {
	if _, err := filepath.Match(relativePattern, relativePattern); err != nil {
		return nil, err
	}
	parts := splitPathParts(relativePattern)
	if len(parts) == 0 {
		return nil, nil
	}
	matches := make([]string, 0)
	if err := walkGlobParts(ctx, root, root, parts, 0, limit, &matches); err != nil {
		return nil, err
	}
	return matches, nil
}

func walkGlobParts(ctx context.Context, root string, current string, parts []string, index int, limit int, matches *[]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if limit > 0 && len(*matches) >= limit {
		return nil
	}
	part := parts[index]
	last := index == len(parts)-1
	if !strings.ContainsAny(part, "*?[\\") {
		next := filepath.Join(current, part)
		info, err := os.Lstat(next)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if last {
			*matches = append(*matches, next)
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, directory, err := resolveGlobDirectory(root, next)
			if err != nil {
				return err
			}
			if !directory {
				return nil
			}
			return walkGlobParts(ctx, root, resolved, parts, index+1, limit, matches)
		}
		if !info.IsDir() {
			return nil
		}
		return walkGlobParts(ctx, root, next, parts, index+1, limit, matches)
	}

	entries, err := readDirNoFollow(root, current)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		matched, err := filepath.Match(part, entry.Name())
		if err != nil {
			return err
		}
		if !matched {
			continue
		}
		next := filepath.Join(current, entry.Name())
		if last {
			*matches = append(*matches, next)
		} else {
			directory := next
			isDirectory := false
			if entry.Type()&os.ModeSymlink != 0 {
				resolved, ok, resolveErr := resolveGlobDirectory(root, next)
				if resolveErr == nil && ok {
					directory = resolved
					isDirectory = true
				}
			} else {
				info, infoErr := entry.Info()
				isDirectory = infoErr == nil && info.IsDir()
			}
			if isDirectory {
				if err := walkGlobParts(ctx, root, directory, parts, index+1, limit, matches); err != nil {
					return err
				}
			}
		}
		if limit > 0 && len(*matches) >= limit {
			return nil
		}
	}
	return nil
}

func resolveGlobDirectory(root string, path string) (string, bool, error) {
	resolved, err := resolveWithExistingAncestor(path)
	if err != nil {
		return "", false, err
	}
	if !isPathInsideRoot(root, resolved) {
		return "", false, fmt.Errorf("%s: glob pattern 经符号链接越出只读根", ErrPathOutsideProject)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", false, err
	}
	if !info.IsDir() {
		return resolved, false, nil
	}
	return resolved, true, nil
}

func readDirNoFollow(root string, absoluteDir string) ([]os.DirEntry, error) {
	var file *os.File
	var err error
	if filepath.Clean(root) == filepath.Clean(absoluteDir) {
		fd, openErr := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return nil, openErr
		}
		file = os.NewFile(uintptr(fd), root)
	} else {
		file, err = openFileNoFollow(root, absoluteDir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			return nil, err
		}
	}
	defer file.Close()
	entries, err := file.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
