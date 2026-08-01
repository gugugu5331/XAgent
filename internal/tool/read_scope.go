package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"xagent/internal/budget"
	"xagent/internal/safefs"
)

// ReadScope adds request-local read roots without changing the project root
// used by write-capable tools.
type ReadScope struct {
	ProjectRoot string
	ExtraRoots  []string
	pinned      bool
}

type readScopeContextKey struct{}
type readExecutionContextKey struct{}

type readExecution struct {
	root            *safefs.Root
	rootPath        string
	fileBytes       int64
	lines           int64
	outputBytes     int64
	scanBytes       int64
	scanFiles       int64
	scanDirectories int64
	scanLines       int64
}

type fileScanLimits struct {
	bytes       int64
	files       int64
	directories int64
	lines       int64
}

func withReadExecution(ctx context.Context, execution readExecution) context.Context {
	return context.WithValue(ctx, readExecutionContextKey{}, execution)
}

func readExecutionFromContext(ctx context.Context) readExecution {
	if ctx == nil {
		return readExecution{}
	}
	execution, _ := ctx.Value(readExecutionContextKey{}).(readExecution)
	return execution
}

// NewReadScope canonicalizes and de-duplicates all roots. Extra roots must be
// existing directories; the project root is never repeated in ExtraRoots.
func NewReadScope(projectRoot string, extraRoots []string) (ReadScope, error) {
	return newReadScope(projectRoot, extraRoots, false)
}

// NewPinnedReadScope requires extra roots to already be canonical. Skill
// activation stores canonical package roots, so a later symlink replacement
// is rejected instead of silently granting the replacement target.
func NewPinnedReadScope(projectRoot string, extraRoots []string) (ReadScope, error) {
	return newReadScope(projectRoot, extraRoots, true)
}

func newReadScope(projectRoot string, extraRoots []string, pinned bool) (ReadScope, error) {
	project, err := canonicalReadRoot(projectRoot)
	if err != nil {
		return ReadScope{}, fmt.Errorf("解析项目只读根失败: %w", err)
	}
	scope := ReadScope{ProjectRoot: project, pinned: pinned}
	seen := map[string]struct{}{project: {}}
	for _, root := range extraRoots {
		canonical, err := canonicalExtraReadRoot(root, pinned)
		if err != nil {
			return ReadScope{}, fmt.Errorf("解析额外只读根失败: %w", err)
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		scope.ExtraRoots = append(scope.ExtraRoots, canonical)
	}
	return scope, nil
}

func canonicalExtraReadRoot(root string, pinned bool) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("根目录不能为空")
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	resolved = filepath.Clean(resolved)
	if pinned && filepath.Clean(abs) != resolved {
		return "", fmt.Errorf("额外只读根不能包含符号链接")
	}
	inspectPath := resolved
	if pinned {
		inspectPath = abs
	}
	info, err := os.Lstat(inspectPath)
	if err != nil {
		return "", err
	}
	if pinned && info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%q 不是安全目录", root)
	}
	return resolved, nil
}

// WithReadScope stores a defensive copy in ctx. Read, Glob, and Grep validate
// and normalize it again at execution time.
func WithReadScope(ctx context.Context, scope ReadScope) context.Context {
	copyScope := ReadScope{ProjectRoot: scope.ProjectRoot, ExtraRoots: append([]string(nil), scope.ExtraRoots...), pinned: scope.pinned}
	return context.WithValue(ctx, readScopeContextKey{}, copyScope)
}

func ReadScopeFromContext(ctx context.Context) (ReadScope, bool) {
	if ctx == nil {
		return ReadScope{}, false
	}
	scope, ok := ctx.Value(readScopeContextKey{}).(ReadScope)
	if !ok {
		return ReadScope{}, false
	}
	scope.ExtraRoots = append([]string(nil), scope.ExtraRoots...)
	return scope, true
}

func effectiveReadScope(ctx context.Context, configuredProjectRoot string) (ReadScope, error) {
	base, err := NewReadScope(configuredProjectRoot, nil)
	if err != nil {
		return ReadScope{}, err
	}
	requested, ok := ReadScopeFromContext(ctx)
	if !ok {
		return base, nil
	}
	if requested.ProjectRoot == "" {
		requested.ProjectRoot = configuredProjectRoot
	}
	normalized, err := NewPinnedReadScope(requested.ProjectRoot, requested.ExtraRoots)
	if err != nil {
		return ReadScope{}, err
	}
	if normalized.ProjectRoot != base.ProjectRoot {
		return ReadScope{}, fmt.Errorf("%s: 请求只读范围不能更改项目根目录", ErrPathOutsideProject)
	}
	return normalized, nil
}

func canonicalReadRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("根目录不能为空")
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q 不是目录", root)
	}
	return filepath.Clean(resolved), nil
}

func openReadInScope(ctx context.Context, scope ReadScope, requestedPath string) (resolvedReadPath, *safefs.File, func(), error) {
	target, root, closeRoot, err := readRootInScope(ctx, scope, requestedPath)
	if err != nil {
		return resolvedReadPath{}, nil, func() {}, err
	}
	relative, err := relativeReadTarget(target, false)
	if err != nil {
		closeRoot()
		return resolvedReadPath{}, nil, func() {}, err
	}
	file, err := root.OpenRead(ctx, relative)
	if err != nil {
		closeRoot()
		return resolvedReadPath{}, nil, func() {}, errors.New(ErrNotFound)
	}
	return target, file, closeRoot, nil
}

func readRootInScope(ctx context.Context, scope ReadScope, requestedPath string) (resolvedReadPath, *safefs.Root, func(), error) {
	if ctx == nil || ctx.Err() != nil {
		return resolvedReadPath{}, nil, func() {}, context.Canceled
	}
	target, err := resolveReadPath(scope, requestedPath)
	if err != nil {
		return resolvedReadPath{}, nil, func() {}, err
	}

	execution := readExecutionFromContext(ctx)
	root := execution.root
	closeRoot := func() {}
	if root != nil && execution.rootPath != target.root {
		return resolvedReadPath{}, nil, closeRoot, errors.New(ErrPathOutsideProject)
	}
	if root == nil {
		opened, openErr := safefs.Bootstrap(target.root, safefs.Policy{})
		if openErr != nil {
			return resolvedReadPath{}, nil, closeRoot, errors.New(ErrPathOutsideProject)
		}
		root = opened.Root
		closeRoot = func() { _ = root.Close() }
	}
	return target, root, closeRoot, nil
}

func relativeReadTarget(target resolvedReadPath, allowRoot bool) (string, error) {
	relative, err := filepath.Rel(target.root, target.absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New(ErrPathOutsideProject)
	}
	if relative == "." && !allowRoot {
		return "", errors.New(ErrPathOutsideProject)
	}
	return filepath.ToSlash(relative), nil
}

func newFileScanCounter(limits fileScanLimits) (*budget.Counter, error) {
	scopes := []struct {
		scope     budget.Scope
		dimension budget.Dimension
		value     int64
	}{
		{budget.FilesScanMaxBytes, budget.Bytes, limits.bytes},
		{budget.FilesScanMaxFiles, budget.Files, limits.files},
		{budget.FilesScanMaxDirectories, budget.Directories, limits.directories},
		{budget.FilesScanMaxLines, budget.Lines, limits.lines},
	}
	effectiveEntries := make([]budget.Limit, 0, len(scopes))
	hardEntries := make([]budget.Limit, 0, len(scopes))
	for _, item := range scopes {
		spec, ok := fileBudgetSpec(item.scope)
		if !ok || spec.Dimension != item.dimension {
			return nil, errors.New("file scan budget specification is unavailable")
		}
		resolved, err := spec.Resolve(&item.value)
		if err != nil {
			return nil, errors.New("file scan budget is invalid")
		}
		effectiveEntries = append(effectiveEntries, budget.Limit{Dimension: item.dimension, Value: resolved})
		hardEntries = append(hardEntries, budget.Limit{Dimension: item.dimension, Value: spec.HardCap})
	}
	effective, err := budget.NewLimits(effectiveEntries...)
	if err != nil {
		return nil, err
	}
	hard, err := budget.NewLimits(hardEntries...)
	if err != nil {
		return nil, err
	}
	return budget.NewCounter(effective, hard)
}

func fileBudgetSpec(scope budget.Scope) (budget.Spec, bool) {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope == scope {
			return spec, true
		}
	}
	return budget.Spec{}, false
}

func mapFileScanLimit(err error, scope budget.Scope) error {
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) {
		return err
	}
	return &budget.LimitError{Scope: string(scope), Dimension: limitErr.Dimension, Limit: limitErr.Limit, Observed: limitErr.Observed}
}

func fileScanBudgetReason(dimension budget.Dimension) string {
	switch dimension {
	case budget.Bytes:
		return string(budget.FilesScanMaxBytes)
	case budget.Files:
		return string(budget.FilesScanMaxFiles)
	case budget.Directories:
		return string(budget.FilesScanMaxDirectories)
	case budget.Lines:
		return string(budget.FilesScanMaxLines)
	default:
		return "files.scan_limit"
	}
}
