package worktree

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	defaultMaxNameBytes    = 192
	defaultMaxSegmentBytes = 64
	defaultMaxDepth        = 8
	hardMaxNameBytes       = 1_024
	hardMaxSegmentBytes    = 255
	hardMaxDepth           = 32
	maxRootGitIgnoreBytes  = 256 << 10
)

var logicalSegmentPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`)

type ManagedLayout struct {
	Root          string
	Control       string
	Repository    string
	Records       string
	Locks         string
	Diagnostics   string
	Tasks         string
	WorkspaceRoot string
	Branch        string
}

// managedWorkspaceParent keeps no-follow directory handles open across the
// final pre-AddWorktree validation. Git CLI still resolves the pathname itself,
// so callers must revalidate immediately before invoking it.
type managedWorkspaceParent struct {
	layout     ManagedLayout
	root       *os.Root
	tasks      *os.Root
	prefix     *os.Root
	rootInfo   os.FileInfo
	tasksInfo  os.FileInfo
	prefixInfo os.FileInfo
	prefixName string
	workspace  string
}

type managedIgnoreAuthority struct {
	repositoryRoot string
	root           *os.Root
	file           *os.File
	rootInfo       os.FileInfo
	ignoreInfo     os.FileInfo
	contents       []byte
}

func ValidateLogicalName(name string, limits Limits) error {
	maxName := limits.MaxNameBytes
	if maxName == 0 {
		maxName = defaultMaxNameBytes
	}
	maxSegment := limits.MaxSegmentBytes
	if maxSegment == 0 {
		maxSegment = defaultMaxSegmentBytes
	}
	maxDepth := limits.MaxDepth
	if maxDepth == 0 {
		maxDepth = defaultMaxDepth
	}
	if name == "" || len(name) > maxName || filepath.IsAbs(name) || strings.Contains(name, `\`) {
		return ErrInvalidLogicalName
	}
	if filepath.ToSlash(filepath.Clean(name)) != name {
		return ErrInvalidLogicalName
	}
	segments := strings.Split(name, "/")
	if len(segments) > maxDepth {
		return ErrInvalidLogicalName
	}
	for _, segment := range segments {
		lower := strings.ToLower(segment)
		if segment == "" || len(segment) > maxSegment || segment == "." || segment == ".." ||
			lower == ".git" || lower == ".control" || strings.HasSuffix(lower, ".lock") || !logicalSegmentPattern.MatchString(segment) {
			return ErrInvalidLogicalName
		}
	}
	return nil
}

func ResolveManagedLayout(projectRoot, workspaceID string) (ManagedLayout, error) {
	root, err := canonicalDirectory(projectRoot)
	if err != nil {
		return ManagedLayout{}, fmt.Errorf("project root: %w", err)
	}
	if !ValidWorkspaceID(workspaceID) {
		return ManagedLayout{}, ErrIdentityMismatch
	}
	managedRoot := filepath.Join(root, ".xagent", "worktrees")
	control := filepath.Join(managedRoot, ".control")
	tasks := filepath.Join(managedRoot, "tasks")
	return ManagedLayout{
		Root:          managedRoot,
		Control:       control,
		Repository:    filepath.Join(control, "repository.json"),
		Records:       filepath.Join(control, "records"),
		Locks:         filepath.Join(control, "locks"),
		Diagnostics:   filepath.Join(control, "diagnostics"),
		Tasks:         tasks,
		WorkspaceRoot: filepath.Join(tasks, workspaceID[:2], workspaceID),
		Branch:        "xagent/worktree/" + workspaceID,
	}, nil
}

func verifyExactManagedIgnoreRule(repositoryRoot string) error {
	authority, err := openExactManagedIgnoreRule(repositoryRoot)
	if err != nil {
		return err
	}
	return authority.close()
}

func openExactManagedIgnoreRule(repositoryRoot string) (*managedIgnoreAuthority, error) {
	rootPathInfo, err := os.Lstat(repositoryRoot)
	if err != nil || rootPathInfo.Mode()&os.ModeSymlink != 0 || !rootPathInfo.IsDir() || isSpecialMode(rootPathInfo.Mode()) {
		return nil, ErrUnsafePath
	}
	root, err := os.OpenRoot(repositoryRoot)
	if err != nil {
		return nil, ErrUnsafePath
	}
	authority := &managedIgnoreAuthority{repositoryRoot: repositoryRoot, root: root, rootInfo: rootPathInfo}
	closeOnFailure := true
	defer func() {
		if closeOnFailure {
			_ = authority.close()
		}
	}()
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(rootPathInfo, rootInfo) {
		return nil, ErrUnsafePath
	}
	ignoreInfo, err := root.Lstat(".gitignore")
	if err != nil || !ignoreInfo.Mode().IsRegular() || ignoreInfo.Mode()&os.ModeSymlink != 0 ||
		isSpecialMode(ignoreInfo.Mode()) || ignoreInfo.Size() < 0 || ignoreInfo.Size() > maxRootGitIgnoreBytes {
		return nil, ErrUnsafePath
	}
	file, err := root.Open(".gitignore")
	if err != nil {
		return nil, ErrUnsafePath
	}
	authority.file, authority.ignoreInfo = file, ignoreInfo
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(ignoreInfo, openedInfo) {
		return nil, ErrUnsafePath
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxRootGitIgnoreBytes+1))
	if err != nil || len(contents) > maxRootGitIgnoreBytes || !utf8.Valid(contents) || strings.IndexByte(string(contents), 0) >= 0 {
		return nil, ErrUnsafePath
	}
	if !hasExactManagedIgnoreRule(contents) {
		return nil, ErrUnsafePath
	}
	authority.contents = append([]byte(nil), contents...)
	if err := authority.revalidate(); err != nil {
		return nil, err
	}
	closeOnFailure = false
	return authority, nil
}

func (a *managedIgnoreAuthority) revalidate() error {
	if a == nil || a.root == nil || a.file == nil || a.rootInfo == nil || a.ignoreInfo == nil {
		return ErrUnsafePath
	}
	openedAfter, openedAfterErr := a.file.Stat()
	ignoreAfter, ignoreAfterErr := a.root.Lstat(".gitignore")
	rootAfter, rootAfterErr := a.root.Stat(".")
	rootPathAfter, rootPathAfterErr := os.Lstat(a.repositoryRoot)
	if openedAfterErr != nil || ignoreAfterErr != nil || rootAfterErr != nil || rootPathAfterErr != nil ||
		!os.SameFile(a.ignoreInfo, openedAfter) || !os.SameFile(a.ignoreInfo, ignoreAfter) ||
		a.ignoreInfo.Size() != openedAfter.Size() || !a.ignoreInfo.ModTime().Equal(openedAfter.ModTime()) ||
		!os.SameFile(a.rootInfo, rootAfter) || !os.SameFile(a.rootInfo, rootPathAfter) {
		return ErrUnsafePath
	}
	current := make([]byte, len(a.contents))
	if len(current) > 0 {
		read, err := a.file.ReadAt(current, 0)
		if err != nil || read != len(current) {
			return ErrUnsafePath
		}
	}
	if !bytes.Equal(current, a.contents) || !hasExactManagedIgnoreRule(current) {
		return ErrUnsafePath
	}
	return nil
}

func (a *managedIgnoreAuthority) close() error {
	if a == nil {
		return nil
	}
	var failures []error
	if a.file != nil {
		failures = append(failures, a.file.Close())
	}
	if a.root != nil {
		failures = append(failures, a.root.Close())
	}
	return errors.Join(failures...)
}

func hasExactManagedIgnoreRule(contents []byte) bool {
	const exact = "/.xagent/worktrees/"
	found := false
	for _, raw := range strings.Split(string(contents), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if line == exact {
			found = true
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		pattern := strings.TrimPrefix(strings.TrimSuffix(line, "/"), "/")
		segments := strings.Split(pattern, "/")
		matchesXAgent := false
		if len(segments) > 0 {
			matchesXAgent, _ = path.Match(segments[0], ".xagent")
		}
		if matchesXAgent && (len(segments) == 1 || managedIgnoreSegmentMatches(segments[1], "worktrees")) {
			return false
		}
	}
	return found
}

func managedIgnoreSegmentMatches(pattern, value string) bool {
	matched, err := path.Match(pattern, value)
	return err != nil || matched
}

func prepareManagedWorkspaceParent(layout ManagedLayout) (*managedWorkspaceParent, error) {
	workspace := filepath.Base(layout.WorkspaceRoot)
	prefixPath := filepath.Dir(layout.WorkspaceRoot)
	prefixName := filepath.Base(prefixPath)
	if !ValidWorkspaceID(workspace) || prefixName != workspace[:2] || filepath.Dir(prefixPath) != layout.Tasks ||
		filepath.Dir(layout.Tasks) != layout.Root || filepath.Base(layout.Tasks) != "tasks" {
		return nil, ErrUnsafePath
	}
	rootPathInfo, err := os.Lstat(layout.Root)
	if err != nil || rootPathInfo.Mode()&os.ModeSymlink != 0 || !rootPathInfo.IsDir() || isSpecialMode(rootPathInfo.Mode()) {
		return nil, ErrUnsafePath
	}
	root, err := os.OpenRoot(layout.Root)
	if err != nil {
		return nil, ErrUnsafePath
	}
	authority := &managedWorkspaceParent{layout: layout, root: root, rootInfo: rootPathInfo, prefixName: prefixName, workspace: workspace}
	closeOnFailure := true
	defer func() {
		if closeOnFailure {
			_ = authority.close()
		}
	}()
	if rootInfo, statErr := root.Stat("."); statErr != nil || !os.SameFile(rootPathInfo, rootInfo) {
		return nil, ErrUnsafePath
	}
	tasks, tasksInfo, err := ensureManagedDirectory(root, "tasks")
	if err != nil {
		return nil, err
	}
	authority.tasks, authority.tasksInfo = tasks, tasksInfo
	prefix, prefixInfo, err := ensureManagedDirectory(tasks, prefixName)
	if err != nil {
		return nil, err
	}
	authority.prefix, authority.prefixInfo = prefix, prefixInfo
	if _, err := prefix.Lstat(workspace); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil, ErrUnsafePath
	}
	if err := authority.revalidate(); err != nil {
		return nil, err
	}
	closeOnFailure = false
	return authority, nil
}

func ensureManagedDirectory(parent *os.Root, name string) (*os.Root, os.FileInfo, error) {
	if parent == nil || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, nil, ErrUnsafePath
	}
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		if err := parent.Mkdir(name, 0o700); err != nil {
			return nil, nil, ErrUnsafePath
		}
		info, err = parent.Lstat(name)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || isSpecialMode(info.Mode()) || info.Mode().Perm() != 0o700 {
		return nil, nil, ErrUnsafePath
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, nil, ErrUnsafePath
	}
	childInfo, err := child.Stat(".")
	if err != nil || !os.SameFile(info, childInfo) {
		_ = child.Close()
		return nil, nil, ErrUnsafePath
	}
	return child, info, nil
}

func (p *managedWorkspaceParent) revalidate() error {
	if p == nil || p.root == nil || p.tasks == nil || p.prefix == nil {
		return ErrUnsafePath
	}
	rootPathInfo, rootPathErr := os.Lstat(p.layout.Root)
	rootInfo, rootErr := p.root.Stat(".")
	tasksLinkInfo, tasksLinkErr := p.root.Lstat("tasks")
	tasksInfo, tasksErr := p.tasks.Stat(".")
	tasksPathInfo, tasksPathErr := os.Lstat(p.layout.Tasks)
	prefixLinkInfo, prefixLinkErr := p.tasks.Lstat(p.prefixName)
	prefixInfo, prefixErr := p.prefix.Stat(".")
	prefixPathInfo, prefixPathErr := os.Lstat(filepath.Dir(p.layout.WorkspaceRoot))
	_, workspaceErr := p.prefix.Lstat(p.workspace)
	if rootPathErr != nil || rootErr != nil || tasksLinkErr != nil || tasksErr != nil || tasksPathErr != nil ||
		prefixLinkErr != nil || prefixErr != nil || prefixPathErr != nil || !errors.Is(workspaceErr, os.ErrNotExist) ||
		!os.SameFile(p.rootInfo, rootPathInfo) || !os.SameFile(p.rootInfo, rootInfo) ||
		!os.SameFile(p.tasksInfo, tasksLinkInfo) || !os.SameFile(p.tasksInfo, tasksInfo) || !os.SameFile(p.tasksInfo, tasksPathInfo) ||
		!os.SameFile(p.prefixInfo, prefixLinkInfo) || !os.SameFile(p.prefixInfo, prefixInfo) || !os.SameFile(p.prefixInfo, prefixPathInfo) {
		return ErrUnsafePath
	}
	return nil
}

func (p *managedWorkspaceParent) close() error {
	if p == nil {
		return nil
	}
	var failures []error
	if p.prefix != nil {
		failures = append(failures, p.prefix.Close())
	}
	if p.tasks != nil {
		failures = append(failures, p.tasks.Close())
	}
	if p.root != nil {
		failures = append(failures, p.root.Close())
	}
	return errors.Join(failures...)
}

// PathIdentity 保存检查时的文件对象身份，供持锁后的实际操作前重验。
type PathIdentity struct {
	Root       string
	Path       string
	exists     bool
	info       os.FileInfo
	parentPath string
	parentInfo os.FileInfo
}

func (p PathIdentity) Exists() bool { return p.exists }

func (p PathIdentity) Revalidate() error {
	current, err := ValidateManagedPath(p.Root, p.Path, !p.exists)
	if err != nil {
		return err
	}
	if current.exists != p.exists || current.parentPath != p.parentPath ||
		p.parentInfo == nil || current.parentInfo == nil || !os.SameFile(p.parentInfo, current.parentInfo) {
		return ErrIdentityMismatch
	}
	if p.exists && (p.info == nil || current.info == nil || !os.SameFile(p.info, current.info)) {
		return ErrIdentityMismatch
	}
	return nil
}

func ValidateManagedPath(root, target string, allowMissing bool) (PathIdentity, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return PathIdentity{}, ErrUnsafePath
	}
	absRoot = filepath.Clean(absRoot)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return PathIdentity{}, ErrUnsafePath
	}
	absTarget = filepath.Clean(absTarget)
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return PathIdentity{}, ErrUnsafePath
	}

	components := []string{absRoot}
	if rel != "." {
		current := absRoot
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if part == "" || part == "." || part == ".." {
				return PathIdentity{}, ErrUnsafePath
			}
			current = filepath.Join(current, part)
			components = append(components, current)
		}
	}

	var lastPath string
	var lastInfo os.FileInfo
	for index, component := range components {
		info, statErr := os.Lstat(component)
		if errors.Is(statErr, os.ErrNotExist) {
			if !allowMissing {
				return PathIdentity{}, ErrUnsafePath
			}
			if lastInfo == nil {
				parent, parentErr := nearestExistingParent(filepath.Dir(component))
				if parentErr != nil {
					return PathIdentity{}, parentErr
				}
				lastPath, lastInfo = parent.path, parent.info
			}
			return PathIdentity{Root: absRoot, Path: absTarget, parentPath: lastPath, parentInfo: lastInfo}, nil
		}
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || isSpecialMode(info.Mode()) {
			return PathIdentity{}, ErrUnsafePath
		}
		if index < len(components)-1 && !info.IsDir() {
			return PathIdentity{}, ErrUnsafePath
		}
		lastPath, lastInfo = component, info
	}
	return PathIdentity{Root: absRoot, Path: absTarget, exists: true, info: lastInfo, parentPath: lastPath, parentInfo: lastInfo}, nil
}

type existingParent struct {
	path string
	info os.FileInfo
}

func nearestExistingParent(path string) (existingParent, error) {
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return existingParent{}, ErrUnsafePath
			}
			return existingParent{path: current, info: info}, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return existingParent{}, ErrUnsafePath
		}
		parent := filepath.Dir(current)
		if parent == current {
			return existingParent{}, ErrUnsafePath
		}
		current = parent
	}
}

func isSpecialMode(mode os.FileMode) bool {
	return mode&(os.ModeDevice|os.ModeNamedPipe|os.ModeSocket|os.ModeCharDevice|os.ModeIrregular) != 0
}

func canonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", ErrUnsafePath
	}
	info, err := os.Lstat(abs)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", ErrUnsafePath
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", ErrUnsafePath
	}
	return filepath.Clean(real), nil
}
