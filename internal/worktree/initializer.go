package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultInitFiles   = 1_024
	defaultInitBytes   = int64(64 << 20)
	defaultInitDepth   = 16
	defaultInitTimeout = 30 * time.Second
	hardMaxInitFiles   = 10_000
	hardMaxInitBytes   = int64(1 << 30)
	hardMaxInitDepth   = 64
	hardMaxInitTimeout = 5 * time.Minute
)

var (
	ErrInitializationFailed   = errors.New("worktree initialization failed")
	ErrInitializationRetained = errors.New("worktree initialization artifacts retained")
	ErrUnsupportedPlatform    = errors.New("worktree initialization unsupported on this platform")
)

// InitializerGit 是初始化阶段所需的最小 Git 能力集合。
type InitializerGit interface {
	CheckIgnore(context.Context, string, string) (bool, error)
	ConfigGet(context.Context, string, string) (string, error)
	WorktreeConfigGet(context.Context, string, string) (string, error)
	EnableWorktreeConfig(context.Context, string) error
	SetWorktreeConfig(context.Context, string, string, string) error
	RestoreWorktreeConfigExtension(context.Context, string, string, bool) error
	RestoreWorktreeHooksPath(context.Context, string, string, bool) error
}

type ReadonlyLinkRequest struct {
	Source       string
	Target       string
	WorktreeRoot string
}

// ReadonlyLinkCapability 必须由本地可信的工作区保护层实现。
type ReadonlyLinkCapability interface {
	ProtectReadonlyLink(context.Context, ReadonlyLinkRequest) error
}

type InitializerOptions struct {
	Git           InitializerGit
	ReadonlyLinks ReadonlyLinkCapability
	Clock         func() time.Time
}

type ManifestRecorder func(context.Context, Manifest) error

type InitRequest struct {
	WorkspaceID    string
	RepositoryRoot string
	WorktreeRoot   string
	ManagedRoot    string
	Config         InitConfig
	Limits         Limits
	Timeout        time.Duration
	RecordManifest ManifestRecorder
}

type VerifyRequest struct {
	RepositoryRoot string
	ManagedRoot    string
	WorktreeRoot   string
	Manifest       Manifest
}

type RollbackRequest struct {
	RepositoryRoot string
	ManagedRoot    string
	WorktreeRoot   string
	Manifest       Manifest
}

type initializerRoot interface {
	Stat(string) (os.FileInfo, error)
	Identity(string) (string, error)
	OpenRead(string) (*os.File, os.FileInfo, error)
	ReadDir(string) ([]os.DirEntry, error)
	Mkdir(string, os.FileMode) error
	CreateFile(string, os.FileMode) (*os.File, error)
	Symlink(string, string) error
	Readlink(string) (string, error)
	Remove(string, bool) error
	Close() error
}

type Initializer interface {
	Prepare(context.Context, InitRequest) (Manifest, error)
	Verify(context.Context, VerifyRequest) error
	Rollback(context.Context, RollbackRequest) error
}

type FilesystemInitializer struct {
	options InitializerOptions
}

func NewFilesystemInitializer(options InitializerOptions) *FilesystemInitializer {
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &FilesystemInitializer{options: options}
}

type initBudget struct {
	files    int
	bytes    int64
	maxFiles int
	maxBytes int64
	maxDepth int
}

func (i *FilesystemInitializer) Prepare(parent context.Context, request InitRequest) (Manifest, error) {
	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		WorkspaceID:   request.WorkspaceID,
		CreatedAt:     i.options.Clock().UTC(),
	}
	ctx, cancel, repositoryRoot, worktreeRoot, managedRoot, budget, err := normalizeInitRequest(parent, request)
	if err != nil {
		return manifest, err
	}
	defer cancel()
	source, err := newInitializerRoot(repositoryRoot)
	if err != nil {
		return manifest, fmt.Errorf("%w: source root", ErrInitializationFailed)
	}
	defer source.Close()
	target, err := newInitializerRoot(worktreeRoot)
	if err != nil {
		return manifest, fmt.Errorf("%w: target root", ErrInitializationFailed)
	}
	defer target.Close()
	created := make(map[string]ManifestEntry)

	if err := validateInitTargets(request.Config); err != nil {
		return manifest, err
	}
	for _, rule := range request.Config.Copy {
		if err := i.copyRule(ctx, source, target, rule, request.RecordManifest, &manifest, budget, created); err != nil {
			return i.rollbackFailure(ctx, target, manifest, created, err)
		}
	}
	for _, rule := range request.Config.IgnoredCopy {
		if i.options.Git == nil {
			return i.rollbackFailure(ctx, target, manifest, created, fmt.Errorf("%w: git reader unavailable", ErrInitializationFailed))
		}
		ignored, err := i.options.Git.CheckIgnore(ctx, repositoryRoot, rule.Source)
		if err != nil || !ignored {
			return i.rollbackFailure(ctx, target, manifest, created, fmt.Errorf("%w: ignored source not verified", ErrInitializationFailed))
		}
		if err := i.copyRule(ctx, source, target, rule, request.RecordManifest, &manifest, budget, created); err != nil {
			return i.rollbackFailure(ctx, target, manifest, created, err)
		}
	}
	for _, rule := range request.Config.Link {
		if err := i.linkRule(ctx, target, repositoryRoot, worktreeRoot, managedRoot, rule,
			request.RecordManifest, &manifest, budget, created); err != nil {
			return i.rollbackFailure(ctx, target, manifest, created, err)
		}
	}
	if request.Config.GitHooks.Enabled {
		if err := i.configureGitHooks(ctx, target, repositoryRoot, worktreeRoot, request.Config.GitHooks); err != nil {
			return i.rollbackFailure(ctx, target, manifest, created, err)
		}
	}
	return manifest, nil
}

func normalizeInitRequest(parent context.Context, request InitRequest) (
	context.Context, context.CancelFunc, string, string, string, *initBudget, error,
) {
	if parent == nil || !ValidWorkspaceID(request.WorkspaceID) {
		return nil, nil, "", "", "", nil, ErrInvalidConfig
	}
	repositoryRoot, managedRoot, worktreeRoot, err := normalizeManagedWorkspace(
		request.RepositoryRoot, request.ManagedRoot, request.WorktreeRoot, request.WorkspaceID,
	)
	if err != nil {
		return nil, nil, "", "", "", nil, ErrUnsafePath
	}
	maxFiles := request.Limits.MaxInitFiles
	if maxFiles == 0 {
		maxFiles = defaultInitFiles
	}
	maxBytes := request.Limits.MaxInitBytes
	if maxBytes == 0 {
		maxBytes = defaultInitBytes
	}
	maxDepth := request.Limits.MaxInitDepth
	if maxDepth == 0 {
		maxDepth = defaultInitDepth
	}
	timeout := request.Timeout
	if timeout == 0 {
		timeout = defaultInitTimeout
	}
	if maxFiles < 1 || maxFiles > hardMaxInitFiles || maxBytes < 1 || maxBytes > hardMaxInitBytes ||
		maxDepth < 1 || maxDepth > hardMaxInitDepth || timeout < time.Millisecond || timeout > hardMaxInitTimeout {
		return nil, nil, "", "", "", nil, ErrInvalidConfig
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	return ctx, cancel, repositoryRoot, worktreeRoot, managedRoot, &initBudget{
		maxFiles: maxFiles, maxBytes: maxBytes, maxDepth: maxDepth,
	}, nil
}

func normalizeManagedWorkspace(repositoryPath, managedPath, worktreePath, workspaceID string) (string, string, string, error) {
	if !ValidWorkspaceID(workspaceID) || managedPath == "" {
		return "", "", "", ErrUnsafePath
	}
	repositoryRoot, err := canonicalDirectory(repositoryPath)
	if err != nil {
		return "", "", "", ErrUnsafePath
	}
	managedRoot, err := canonicalDirectory(managedPath)
	if err != nil {
		return "", "", "", ErrUnsafePath
	}
	expectedManaged := filepath.Join(repositoryRoot, ".xagent", "worktrees")
	if managedRoot != expectedManaged {
		return "", "", "", ErrUnsafePath
	}
	if _, err := ValidateManagedPath(repositoryRoot, expectedManaged, false); err != nil {
		return "", "", "", ErrUnsafePath
	}
	worktreeRoot, err := canonicalDirectory(worktreePath)
	if err != nil {
		return "", "", "", ErrUnsafePath
	}
	expectedWorktree := filepath.Join(managedRoot, "tasks", workspaceID[:2], workspaceID)
	if worktreeRoot != expectedWorktree {
		return "", "", "", ErrUnsafePath
	}
	if _, err := ValidateManagedPath(managedRoot, expectedWorktree, false); err != nil {
		return "", "", "", ErrUnsafePath
	}
	return repositoryRoot, managedRoot, worktreeRoot, nil
}

func validateInitTargets(config InitConfig) error {
	seen := map[string]struct{}{}
	all := make([]string, 0, len(config.Copy)+len(config.IgnoredCopy)+len(config.Link))
	for _, rule := range append(append([]CopyRule(nil), config.Copy...), config.IgnoredCopy...) {
		if !validInitRelative(rule.Source) || !validInitRelative(rule.Target) {
			return ErrUnsafePath
		}
		all = append(all, rule.Target)
	}
	for _, rule := range config.Link {
		if !filepath.IsAbs(rule.Source) || !validInitRelative(rule.Target) {
			return ErrUnsafePath
		}
		all = append(all, rule.Target)
	}
	if config.GitHooks.Enabled && !validInitRelative(config.GitHooks.Path) {
		return ErrUnsafePath
	}
	sort.Strings(all)
	for _, target := range all {
		if _, exists := seen[target]; exists {
			return ErrInvalidConfig
		}
		for previous := range seen {
			if strings.HasPrefix(target, previous+"/") || strings.HasPrefix(previous, target+"/") {
				return ErrInvalidConfig
			}
		}
		seen[target] = struct{}{}
	}
	return nil
}

type gitConfigSnapshot struct {
	value  string
	wasSet bool
}

func (i *FilesystemInitializer) configureGitHooks(
	ctx context.Context,
	target initializerRoot,
	repositoryRoot, worktreeRoot string,
	rule GitHooksRule,
) error {
	if i.options.Git == nil {
		return fmt.Errorf("%w: git unavailable", ErrInitializationFailed)
	}
	info, err := target.Stat(rule.Path)
	if err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return ErrUnsafePath
	}
	extension, err := readGitConfig(func() (string, error) {
		return i.options.Git.ConfigGet(ctx, repositoryRoot, "extensions.worktreeConfig")
	})
	if err != nil || (extension.wasSet && extension.value != "true" && extension.value != "false") {
		return fmt.Errorf("%w: extension snapshot", ErrInitializationFailed)
	}
	extensionAttempted := true
	if err := i.options.Git.EnableWorktreeConfig(ctx, repositoryRoot); err != nil {
		return i.restoreGitHooks(ctx, repositoryRoot, worktreeRoot, extension, gitConfigSnapshot{}, extensionAttempted, false, err)
	}
	value, err := i.options.Git.ConfigGet(ctx, repositoryRoot, "extensions.worktreeConfig")
	if err != nil || value != "true" {
		return i.restoreGitHooks(ctx, repositoryRoot, worktreeRoot, extension, gitConfigSnapshot{}, extensionAttempted, false,
			fmt.Errorf("%w: extension verification", ErrInitializationFailed))
	}
	hooks, err := readGitConfig(func() (string, error) {
		return i.options.Git.WorktreeConfigGet(ctx, worktreeRoot, "core.hooksPath")
	})
	if err != nil {
		return i.restoreGitHooks(ctx, repositoryRoot, worktreeRoot, extension, hooks, extensionAttempted, false,
			fmt.Errorf("%w: hooks snapshot", ErrInitializationFailed))
	}
	hooksAttempted := true
	if err := i.options.Git.SetWorktreeConfig(ctx, worktreeRoot, "core.hooksPath", rule.Path); err != nil {
		return i.restoreGitHooks(ctx, repositoryRoot, worktreeRoot, extension, hooks, extensionAttempted, hooksAttempted, err)
	}
	value, err = i.options.Git.WorktreeConfigGet(ctx, worktreeRoot, "core.hooksPath")
	if err != nil || value != rule.Path {
		return i.restoreGitHooks(ctx, repositoryRoot, worktreeRoot, extension, hooks, extensionAttempted, hooksAttempted,
			fmt.Errorf("%w: hooks verification", ErrInitializationFailed))
	}
	return nil
}

func readGitConfig(get func() (string, error)) (gitConfigSnapshot, error) {
	value, err := get()
	if err == nil {
		return gitConfigSnapshot{value: value, wasSet: true}, nil
	}
	if errors.Is(err, ErrNotFound) || commandExitCode(err) == 1 {
		return gitConfigSnapshot{}, nil
	}
	return gitConfigSnapshot{}, err
}

func (i *FilesystemInitializer) restoreGitHooks(
	ctx context.Context,
	repositoryRoot, worktreeRoot string,
	extension, hooks gitConfigSnapshot,
	extensionAttempted, hooksAttempted bool,
	cause error,
) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultInitTimeout)
	defer cancel()
	var restoreErrors []error
	if hooksAttempted {
		if err := i.options.Git.RestoreWorktreeHooksPath(rollbackCtx, worktreeRoot, hooks.value, hooks.wasSet); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
	}
	if extensionAttempted {
		if err := i.options.Git.RestoreWorktreeConfigExtension(rollbackCtx, repositoryRoot, extension.value, extension.wasSet); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
	}
	if len(restoreErrors) == 0 {
		return cause
	}
	restoreErrors = append([]error{cause, ErrInitializationRetained}, restoreErrors...)
	return errors.Join(restoreErrors...)
}

func validInitRelative(value string) bool {
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, `\`) || strings.ContainsRune(value, 0) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, readonly := range []string{".control", ".xagent/worktrees"} {
		if clean == readonly || strings.HasPrefix(clean, readonly+"/") || strings.HasPrefix(readonly, clean+"/") {
			return false
		}
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}

func (i *FilesystemInitializer) copyRule(
	ctx context.Context,
	source, target initializerRoot,
	rule CopyRule,
	recorder ManifestRecorder,
	manifest *Manifest,
	budget *initBudget,
	created map[string]ManifestEntry,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return i.copyEntry(ctx, source, target, rule.Source, rule.Target, 0, recorder, manifest, budget, created)
}

func (i *FilesystemInitializer) copyEntry(
	ctx context.Context,
	source, target initializerRoot,
	sourceRelative, targetRelative string,
	depth int,
	recorder ManifestRecorder,
	manifest *Manifest,
	budget *initBudget,
	created map[string]ManifestEntry,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > budget.maxDepth {
		return ErrMetadataTooLarge
	}
	info, err := source.Stat(sourceRelative)
	if err != nil || info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || isSpecialMode(info.Mode()) {
		return ErrUnsafePath
	}
	if info.IsDir() {
		if err := i.ensureParents(ctx, target, targetRelative, recorder, manifest, budget, created); err != nil {
			return err
		}
		if err := i.appendAndApply(ctx, recorder, manifest, budget,
			ManifestEntry{Path: targetRelative, Type: ManifestDirectory},
			func() (string, bool, error) {
				if err := target.Mkdir(targetRelative, info.Mode().Perm()); err != nil {
					return "", false, err
				}
				identity, err := target.Identity(targetRelative)
				return identity, true, err
			}, created); err != nil {
			return err
		}
		entries, err := source.ReadDir(sourceRelative)
		if err != nil {
			return ErrUnsafePath
		}
		sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
		for _, entry := range entries {
			if entry.Name() == "." || entry.Name() == ".." || strings.Contains(entry.Name(), "/") {
				return ErrUnsafePath
			}
			if err := i.copyEntry(ctx, source, target,
				filepath.ToSlash(filepath.Join(sourceRelative, entry.Name())),
				filepath.ToSlash(filepath.Join(targetRelative, entry.Name())),
				depth+1, recorder, manifest, budget, created); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > budget.maxBytes-budget.bytes {
		return ErrMetadataTooLarge
	}
	if err := i.ensureParents(ctx, target, targetRelative, recorder, manifest, budget, created); err != nil {
		return err
	}
	sourceFile, openedInfo, err := source.OpenRead(sourceRelative)
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		if sourceFile != nil {
			_ = sourceFile.Close()
		}
		return ErrUnsafePath
	}
	defer sourceFile.Close()
	digest, bytesRead, err := digestReader(ctx, sourceFile, budget.maxBytes-budget.bytes)
	if err != nil || bytesRead != info.Size() {
		return ErrInitializationFailed
	}
	if _, err := sourceFile.Seek(0, io.SeekStart); err != nil {
		return ErrInitializationFailed
	}
	entry := ManifestEntry{Path: targetRelative, Type: ManifestFile, Size: bytesRead, Digest: digest}
	return i.appendAndApply(ctx, recorder, manifest, budget, entry, func() (string, bool, error) {
		destination, err := target.CreateFile(targetRelative, info.Mode().Perm())
		if err != nil {
			return "", false, err
		}
		identity, err := initializerIdentityFromFile(destination)
		if err != nil {
			_ = destination.Close()
			return "", true, err
		}
		hash := sha256.New()
		written, copyErr := copyWithContext(ctx, io.MultiWriter(destination, hash), sourceFile, budget.maxBytes-budget.bytes)
		closeErr := destination.Close()
		if copyErr != nil || closeErr != nil || written != bytesRead || hex.EncodeToString(hash.Sum(nil)) != digest {
			return identity, true, ErrInitializationFailed
		}
		pathIdentity, err := target.Identity(targetRelative)
		if err != nil || pathIdentity != identity {
			return identity, true, ErrIntegrityMismatch
		}
		return identity, true, nil
	}, created)
}

func (i *FilesystemInitializer) ensureParents(
	ctx context.Context,
	target initializerRoot,
	relative string,
	recorder ManifestRecorder,
	manifest *Manifest,
	budget *initBudget,
	created map[string]ManifestEntry,
) error {
	parts := strings.Split(relative, "/")
	for index := 1; index < len(parts); index++ {
		parent := strings.Join(parts[:index], "/")
		info, err := target.Stat(parent)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return ErrUnsafePath
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return ErrUnsafePath
		}
		if err := i.appendAndApply(ctx, recorder, manifest, budget,
			ManifestEntry{Path: parent, Type: ManifestDirectory},
			func() (string, bool, error) {
				if err := target.Mkdir(parent, 0o700); err != nil {
					return "", false, err
				}
				identity, err := target.Identity(parent)
				return identity, true, err
			}, created); err != nil {
			return err
		}
	}
	return nil
}

func (i *FilesystemInitializer) linkRule(
	ctx context.Context,
	target initializerRoot,
	repositoryRoot, worktreeRoot, managedRoot string,
	rule LinkRule,
	recorder ManifestRecorder,
	manifest *Manifest,
	budget *initBudget,
	created map[string]ManifestEntry,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if i.options.ReadonlyLinks == nil || containsPathSegment(rule.Source, ".git") {
		return ErrUnsafePath
	}
	info, err := os.Lstat(rule.Source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return ErrUnsafePath
	}
	source, err := canonicalDirectory(rule.Source)
	if err != nil || sameOrDescendant(repositoryRoot, source) || sameOrDescendant(worktreeRoot, source) ||
		(managedRoot != "" && sameOrDescendant(managedRoot, source)) {
		return ErrUnsafePath
	}
	sourceHandle, err := newInitializerRoot(source)
	if err != nil {
		return ErrUnsafePath
	}
	_ = sourceHandle.Close()
	targetAbsolute := filepath.Join(worktreeRoot, filepath.FromSlash(rule.Target))
	if err := i.options.ReadonlyLinks.ProtectReadonlyLink(ctx, ReadonlyLinkRequest{
		Source: source, Target: targetAbsolute, WorktreeRoot: worktreeRoot,
	}); err != nil {
		return fmt.Errorf("%w: readonly protection unavailable", ErrInitializationFailed)
	}
	if err := i.ensureParents(ctx, target, rule.Target, recorder, manifest, budget, created); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(source))
	entry := ManifestEntry{
		Path: rule.Target, Type: ManifestSymlink, Digest: hex.EncodeToString(digest[:]),
	}
	return i.appendAndApply(ctx, recorder, manifest, budget, entry,
		func() (string, bool, error) {
			if err := target.Symlink(rule.Target, source); err != nil {
				return "", false, err
			}
			identity, err := target.Identity(rule.Target)
			return identity, true, err
		}, created)
}

func containsPathSegment(path, segment string) bool {
	clean := filepath.Clean(path)
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if strings.EqualFold(part, segment) {
			return true
		}
	}
	return false
}

func (i *FilesystemInitializer) appendAndApply(
	ctx context.Context,
	recorder ManifestRecorder,
	manifest *Manifest,
	budget *initBudget,
	entry ManifestEntry,
	apply func() (identity string, actuallyCreated bool, err error),
	created map[string]ManifestEntry,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if budget.files >= budget.maxFiles || entry.Size > budget.maxBytes-budget.bytes {
		return ErrMetadataTooLarge
	}
	entry.State = ManifestEntryPlanned
	entry.IdentityDigest = ""
	planned := manifest.Clone()
	planned.Entries = append(planned.Entries, entry)
	if recorder != nil {
		if err := recorder(ctx, planned); err != nil {
			return err
		}
	}
	*manifest = planned
	budget.files++
	budget.bytes += entry.Size
	identity, actuallyCreated, err := apply()
	if actuallyCreated {
		owned := entry
		owned.State = ManifestEntryCreated
		owned.IdentityDigest = identity
		created[entry.Path] = owned
	}
	if err != nil {
		return err
	}
	if !actuallyCreated || !validDigest(identity) {
		return ErrIntegrityMismatch
	}
	committed := manifest.Clone()
	committed.Entries[len(committed.Entries)-1].State = ManifestEntryCreated
	committed.Entries[len(committed.Entries)-1].IdentityDigest = identity
	if recorder != nil {
		if err := recorder(ctx, committed); err != nil {
			return err
		}
	}
	*manifest = committed
	return nil
}

func digestReader(ctx context.Context, reader io.Reader, limit int64) (string, int64, error) {
	hash := sha256.New()
	count, err := copyWithContext(ctx, hash, reader, limit)
	if err != nil {
		return "", count, err
	}
	return hex.EncodeToString(hash.Sum(nil)), count, nil
}

func copyWithContext(ctx context.Context, writer io.Writer, reader io.Reader, limit int64) (int64, error) {
	if limit < 0 {
		return 0, ErrMetadataTooLarge
	}
	buffer := make([]byte, 32*1024)
	limited := &io.LimitedReader{R: reader, N: limit + 1}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := limited.Read(buffer)
		if count > 0 {
			if total+int64(count) > limit {
				return total, ErrMetadataTooLarge
			}
			written, writeErr := writer.Write(buffer[:count])
			total += int64(written)
			if writeErr != nil || written != count {
				return total, ErrInitializationFailed
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func (i *FilesystemInitializer) Verify(ctx context.Context, request VerifyRequest) error {
	if ctx == nil || validateManifest(request.Manifest, false) != nil {
		return ErrInvalidConfig
	}
	_, _, rootPath, err := normalizeManagedWorkspace(
		request.RepositoryRoot, request.ManagedRoot, request.WorktreeRoot, request.Manifest.WorkspaceID,
	)
	if err != nil {
		return ErrUnsafePath
	}
	root, err := newInitializerRoot(rootPath)
	if err != nil {
		return ErrUnsafePath
	}
	defer root.Close()
	for _, entry := range request.Manifest.Entries {
		if err := verifyInitEntry(ctx, root, entry); err != nil {
			return err
		}
	}
	return nil
}

func (i *FilesystemInitializer) Rollback(ctx context.Context, request RollbackRequest) error {
	if ctx == nil || validateManifest(request.Manifest, false) != nil {
		return ErrInvalidConfig
	}
	_, _, rootPath, err := normalizeManagedWorkspace(
		request.RepositoryRoot, request.ManagedRoot, request.WorktreeRoot, request.Manifest.WorkspaceID,
	)
	if err != nil {
		return ErrUnsafePath
	}
	root, err := newInitializerRoot(rootPath)
	if err != nil {
		return ErrUnsafePath
	}
	defer root.Close()
	retained := false
	for index := len(request.Manifest.Entries) - 1; index >= 0; index-- {
		entry := request.Manifest.Entries[index]
		if entry.State != ManifestEntryCreated {
			continue
		}
		if err := verifyInitEntry(ctx, root, entry); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			retained = true
			continue
		}
		if err := root.Remove(entry.Path, entry.Type == ManifestDirectory); err != nil {
			retained = true
		}
	}
	if retained {
		return ErrInitializationRetained
	}
	return nil
}

func (i *FilesystemInitializer) rollbackFailure(
	ctx context.Context,
	target initializerRoot,
	manifest Manifest,
	created map[string]ManifestEntry,
	cause error,
) (Manifest, error) {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultInitTimeout)
	defer cancel()
	retained := false
	for index := len(manifest.Entries) - 1; index >= 0; index-- {
		entry, ok := created[manifest.Entries[index].Path]
		if !ok {
			continue
		}
		if err := verifyInitEntry(rollbackCtx, target, entry); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				retained = true
			}
			continue
		}
		if err := target.Remove(entry.Path, entry.Type == ManifestDirectory); err != nil {
			retained = true
		}
	}
	if retained {
		return manifest, errors.Join(cause, ErrInitializationRetained)
	}
	return manifest, cause
}

func verifyInitEntry(ctx context.Context, root initializerRoot, entry ManifestEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if entry.State != ManifestEntryCreated || !validDigest(entry.IdentityDigest) {
		return ErrIntegrityMismatch
	}
	identity, err := root.Identity(entry.Path)
	if err != nil {
		return err
	}
	if identity != entry.IdentityDigest {
		return ErrIntegrityMismatch
	}
	switch entry.Type {
	case ManifestDirectory:
		info, err := root.Stat(entry.Path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return ErrIntegrityMismatch
		}
		return nil
	case ManifestFile:
		file, info, err := root.OpenRead(entry.Path)
		if err != nil {
			return err
		}
		defer file.Close()
		if !info.Mode().IsRegular() || info.Size() != entry.Size {
			return ErrIntegrityMismatch
		}
		digest, count, err := digestReader(ctx, file, entry.Size)
		if err != nil || count != entry.Size || digest != entry.Digest {
			return ErrIntegrityMismatch
		}
		return nil
	case ManifestSymlink:
		target, err := root.Readlink(entry.Path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(target))
		if hex.EncodeToString(digest[:]) != entry.Digest {
			return ErrIntegrityMismatch
		}
		return nil
	default:
		return ErrInvalidMetadata
	}
}

func sameOrDescendant(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

var _ Initializer = (*FilesystemInitializer)(nil)
var _ InitializerGit = (*GitClient)(nil)
