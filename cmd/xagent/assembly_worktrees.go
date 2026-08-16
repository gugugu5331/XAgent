package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/config"
	"xagent/internal/hook"
	"xagent/internal/memory"
	"xagent/internal/orchestrator"
	"xagent/internal/permission"
	"xagent/internal/proctree"
	"xagent/internal/redact"
	"xagent/internal/safefs"
	"xagent/internal/sessionctx"
	"xagent/internal/tool"
	"xagent/internal/workspace"
	"xagent/internal/worktree"
)

// assemblyWorktreeGraphOptions is the narrow T60 construction authority. T61
// supplies these resolved roots and borrowed process/context dependencies; the
// graph never derives writable task roots from cwd or the shared artifact store.
type assemblyWorktreeGraphOptions struct {
	ProjectRoot   string
	ScratchBase   string
	ArtifactBase  string
	ReadonlyRoots []string

	Config            worktree.Config
	GitExecutable     string
	GitRunner         worktree.GitCommandRunner
	GitMaxOutputBytes int
	ReadonlyLinks     worktree.ReadonlyLinkCapability
	Clock             func() time.Time

	SourceRegistry   *tool.Registry
	ResultFactory    *tool.ResultFactory
	ReadCacheLimits  tool.ReadCacheLimits
	BackgroundPolicy tool.BackgroundPolicy
	GlobalDenied     map[string]struct{}
	ExecutorTimeout  time.Duration
	MaxOutputBytes   int

	Instructions        config.InstructionsConfig
	InstructionUserRoot string
	MemoryEnabled       *bool
	Memory              memory.ManagerOptions
	ConfigDigest        string
	UserInstructions    sessionctx.InstructionLoader
	UserMemory          sessionctx.MemoryIndexProvider
	RuntimeRedactor     *redact.RuntimeRedactor
	MaxSectionBytes     int

	HookSnapshot  hook.Snapshot
	HookEngine    hook.EngineOptions
	ProcessRunner proctree.Runner
}

type assemblyWorktreeOwners struct {
	worktrees ownerClose
	workspace ownerClose
	janitor   ownerClose
}

type assemblyWorktreeCapabilityOptions struct {
	ProjectRoot       string
	UserCacheRoot     string
	Config            worktree.Config
	GitExecutable     string
	GitRunner         worktree.GitCommandRunner
	GitMaxOutputBytes int
	ReadonlyLinks     worktree.ReadonlyLinkCapability
	FilesystemProbe   func(context.Context, string, string) error
}

var errAssemblyWorktreeCapabilityUnsafe = errors.New("assembly worktree capability boundary is unsafe")

const assemblyWorktreeMetadataMaxBytes = 4096

// probeAssemblyWorktreeCapability removes every unchanged probe artifact
// before reporting available/unavailable. It separates an optional runtime
// capability from configuration and security validity: ordinary
// Git/lock/filesystem unavailability disables isolated roles, while unsafe
// roots, identity drift and invalid configuration remain fatal and are not
// granted deletion authority.
func probeAssemblyWorktreeCapability(ctx context.Context, options assemblyWorktreeCapabilityOptions) (bool, error) {
	if ctx == nil || ctx.Err() != nil || options.Config.Validate() != nil {
		return false, errors.New("assembly worktree capability options are invalid")
	}
	if !filepath.IsAbs(options.ProjectRoot) || filepath.Clean(options.ProjectRoot) != options.ProjectRoot ||
		!filepath.IsAbs(options.UserCacheRoot) || filepath.Clean(options.UserCacheRoot) != options.UserCacheRoot {
		return false, errors.New("assembly worktree capability roots are invalid")
	}
	// A missing marker is a side-effect-free optional capability miss. Do not
	// require canonical writable-root authority from projects that will remain
	// shared-only (notably macOS temporary-directory aliases).
	if _, err := os.Lstat(filepath.Join(options.ProjectRoot, ".git")); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, errors.New("assembly worktree repository metadata is unavailable")
	}

	projectRoot, err := canonicalAssemblyWorktreeDirectory(options.ProjectRoot)
	if err != nil || projectRoot != options.ProjectRoot {
		return false, errors.New("assembly worktree project capability root is unsafe")
	}
	userCacheRoot, err := canonicalAssemblyWorktreeDirectory(options.UserCacheRoot)
	if err != nil || userCacheRoot != options.UserCacheRoot || assemblyWorktreePathsOverlap(projectRoot, userCacheRoot) {
		return false, errors.New("assembly worktree cache capability root is unsafe")
	}
	linked, present, err := validateAssemblyWorktreeRepositoryMetadata(projectRoot)
	if err != nil {
		return false, err
	}
	if !present {
		return false, nil
	}
	if options.GitMaxOutputBytes <= 0 || options.GitExecutable == "" || filepath.Base(options.GitExecutable) != "git" {
		return false, errors.New("assembly worktree capability options are invalid")
	}
	if !assemblyWorktreeInitializerSupported() || (len(options.Config.Init.Link) > 0 && options.ReadonlyLinks == nil) {
		return false, nil
	}
	client := worktree.NewGitClient(options.GitExecutable, options.GitRunner, options.Config.Lifecycle.GitTimeout, options.GitMaxOutputBytes)
	if _, err := client.ResolveHEAD(ctx, projectRoot); err != nil {
		if ctx.Err() != nil {
			return false, errors.New("assembly worktree capability probe was canceled")
		}
		return false, nil
	}
	// The current manager supports only the main checkout. A linked checkout is
	// downgraded only after its metadata and the actual Git command both prove
	// that it is a valid repository, so malformed metadata cannot masquerade as
	// an optional capability miss.
	if linked {
		return false, nil
	}

	filesystemProbe := options.FilesystemProbe
	if filesystemProbe == nil {
		filesystemProbe = probeAssemblyWorktreeFilesystem
	}
	if err := filesystemProbe(ctx, projectRoot, userCacheRoot); err != nil {
		if ctx.Err() != nil || errors.Is(err, errAssemblyWorktreeCapabilityUnsafe) ||
			errors.Is(err, worktree.ErrUnsafePath) || errors.Is(err, worktree.ErrInvalidConfig) {
			return false, errors.New("assembly worktree capability boundary is unsafe")
		}
		return false, nil
	}
	return true, nil
}

func probeAssemblyWorktreeFilesystem(ctx context.Context, projectRoot, userCacheRoot string) error {
	if err := probeAssemblyWorktreeFilesystemRoot(ctx, projectRoot, true); err != nil {
		return err
	}
	return probeAssemblyWorktreeFilesystemRoot(ctx, userCacheRoot, false)
}

func probeAssemblyWorktreeFilesystemRoot(ctx context.Context, root string, verifyLock bool) (resultErr error) {
	if ctx == nil || ctx.Err() != nil {
		return context.Canceled
	}
	parent, err := os.OpenRoot(root)
	if err != nil {
		return errAssemblyWorktreeCapabilityUnsafe
	}
	defer func() {
		if err := parent.Close(); err != nil {
			resultErr = errors.Join(resultErr, errAssemblyWorktreeCapabilityUnsafe)
		}
	}()
	workspaceID, err := worktree.GenerateWorkspaceID(nil)
	if err != nil {
		return err
	}
	name := ".xagent-worktree-capability-" + workspaceID
	opened, err := parent.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	createdInfo, err := opened.Stat()
	if err != nil || !createdInfo.Mode().IsRegular() || createdInfo.Mode().Perm()&0o077 != 0 {
		_ = opened.Close()
		return errAssemblyWorktreeCapabilityUnsafe
	}
	defer func() {
		if err := opened.Close(); err != nil {
			resultErr = errors.Join(resultErr, errAssemblyWorktreeCapabilityUnsafe)
		}
		currentInfo, err := parent.Lstat(name)
		if err != nil || !os.SameFile(createdInfo, currentInfo) || parent.Remove(name) != nil {
			resultErr = errors.Join(resultErr, errAssemblyWorktreeCapabilityUnsafe)
		}
	}()
	currentInfo, err := parent.Lstat(name)
	if err != nil || !os.SameFile(createdInfo, currentInfo) || !currentInfo.Mode().IsRegular() {
		return errAssemblyWorktreeCapabilityUnsafe
	}
	if !verifyLock {
		return nil
	}
	return probeAssemblyWorktreePlatformLock(opened)
}

func validateAssemblyWorktreeRepositoryMetadata(projectRoot string) (linked, present bool, resultErr error) {
	root, err := os.OpenRoot(projectRoot)
	if err != nil {
		return false, false, errors.New("assembly worktree repository metadata is unavailable")
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("assembly worktree repository metadata changed"))
		}
	}()
	rootInfo, err := root.Stat(".")
	pathInfo, pathErr := os.Stat(projectRoot)
	if err != nil || pathErr != nil || !os.SameFile(rootInfo, pathInfo) {
		return false, false, errors.New("assembly worktree repository metadata changed")
	}
	gitInfo, err := root.Lstat(".git")
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, errors.New("assembly worktree repository metadata is unavailable")
	}
	if gitInfo.Mode()&os.ModeSymlink != 0 || (!gitInfo.IsDir() && !gitInfo.Mode().IsRegular()) {
		return false, false, errors.New("assembly worktree repository metadata is unsafe")
	}
	gitPath := filepath.Join(projectRoot, ".git")
	if gitInfo.IsDir() {
		canonicalGit, err := canonicalAssemblyWorktreeDirectory(gitPath)
		if err != nil || canonicalGit != gitPath {
			return false, false, errors.New("assembly worktree repository metadata changed")
		}
		if _, err := worktree.NewRepositoryIdentity(projectRoot, canonicalGit); err != nil {
			return false, false, errors.New("assembly worktree repository identity is invalid")
		}
		return false, true, nil
	}

	metadata, fileInfo, err := readAssemblyWorktreeMetadataFile(projectRoot, ".git")
	if err != nil || !os.SameFile(gitInfo, fileInfo) {
		return false, false, errors.New("assembly worktree repository metadata is unsafe")
	}
	adminText, ok := parseAssemblyWorktreeMetadataLine(metadata, "gitdir: ")
	if !ok {
		return false, false, errors.New("assembly worktree repository metadata is invalid")
	}
	adminPath, err := resolveAssemblyWorktreeMetadataDirectory(projectRoot, adminText)
	if err != nil {
		return false, false, errors.New("assembly worktree repository metadata is invalid")
	}
	commonMetadata, _, err := readAssemblyWorktreeMetadataFile(adminPath, "commondir")
	if err != nil {
		return false, false, errors.New("assembly worktree repository metadata is invalid")
	}
	commonText, ok := parseAssemblyWorktreeMetadataLine(commonMetadata, "")
	if !ok {
		return false, false, errors.New("assembly worktree repository metadata is invalid")
	}
	commonDir, err := resolveAssemblyWorktreeMetadataDirectory(adminPath, commonText)
	if err != nil || filepath.Dir(adminPath) != filepath.Join(commonDir, "worktrees") || filepath.Base(adminPath) == "." {
		return false, false, errors.New("assembly worktree repository metadata is invalid")
	}
	backMetadata, _, err := readAssemblyWorktreeMetadataFile(adminPath, "gitdir")
	if err != nil {
		return false, false, errors.New("assembly worktree repository metadata is invalid")
	}
	backText, ok := parseAssemblyWorktreeMetadataLine(backMetadata, "")
	if !ok {
		return false, false, errors.New("assembly worktree repository metadata is invalid")
	}
	backPath := backText
	if !filepath.IsAbs(backPath) {
		backPath = filepath.Join(adminPath, backPath)
	}
	if filepath.Clean(backPath) != gitPath {
		return false, false, errors.New("assembly worktree repository metadata is invalid")
	}
	if _, err := worktree.NewRepositoryIdentity(projectRoot, commonDir); err != nil {
		return false, false, errors.New("assembly worktree repository identity is invalid")
	}
	return true, true, nil
}

func readAssemblyWorktreeMetadataFile(directory, name string) (contents []byte, resultInfo os.FileInfo, resultErr error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, errAssemblyWorktreeCapabilityUnsafe
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errAssemblyWorktreeCapabilityUnsafe)
		}
	}()
	rootInfo, err := root.Stat(".")
	pathInfo, pathErr := os.Stat(directory)
	if err != nil || pathErr != nil || !os.SameFile(rootInfo, pathInfo) {
		return nil, nil, errAssemblyWorktreeCapabilityUnsafe
	}
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() <= 0 || before.Size() > assemblyWorktreeMetadataMaxBytes {
		return nil, nil, errAssemblyWorktreeCapabilityUnsafe
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, nil, errAssemblyWorktreeCapabilityUnsafe
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errAssemblyWorktreeCapabilityUnsafe)
		}
	}()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, nil, errAssemblyWorktreeCapabilityUnsafe
	}
	contents, err = io.ReadAll(io.LimitReader(file, assemblyWorktreeMetadataMaxBytes+1))
	if err != nil || len(contents) == 0 || len(contents) > assemblyWorktreeMetadataMaxBytes {
		return nil, nil, errAssemblyWorktreeCapabilityUnsafe
	}
	after, afterErr := root.Lstat(name)
	pathInfo, pathErr = os.Stat(directory)
	if afterErr != nil || pathErr != nil || !os.SameFile(before, after) || !os.SameFile(rootInfo, pathInfo) {
		return nil, nil, errAssemblyWorktreeCapabilityUnsafe
	}
	return contents, before, nil
}

func parseAssemblyWorktreeMetadataLine(contents []byte, prefix string) (string, bool) {
	line := string(contents)
	if strings.HasSuffix(line, "\n") {
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
	}
	if line == "" || strings.ContainsAny(line, "\r\n\x00") || !strings.HasPrefix(line, prefix) {
		return "", false
	}
	value := strings.TrimPrefix(line, prefix)
	if value == "" || strings.TrimSpace(value) != value {
		return "", false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return "", false
		}
	}
	return value, true
}

func resolveAssemblyWorktreeMetadataDirectory(base, value string) (string, error) {
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	canonical, err := canonicalAssemblyWorktreeDirectory(path)
	if err != nil || canonical != path {
		return "", errAssemblyWorktreeCapabilityUnsafe
	}
	return canonical, nil
}

// assemblyWorktreeGraph intentionally retains only the three interfaces used
// by T61 and their close actions. Git, Store, Lock and Initializer concretes do
// not escape the constructor.
type assemblyWorktreeGraph struct {
	manager   orchestrator.WorktreeLifecycleManager
	workspace orchestrator.WorktreeWorkspaceBinder
	janitor   worktree.Janitor
	owners    assemblyWorktreeOwners
}

func newAssemblyWorktreeGraph(options assemblyWorktreeGraphOptions) (*assemblyWorktreeGraph, error) {
	projectRoot, err := canonicalAssemblyWorktreeDirectory(options.ProjectRoot)
	if err != nil || projectRoot != options.ProjectRoot || options.SourceRegistry == nil ||
		!options.SourceRegistry.IsSealed() || options.ResultFactory == nil || options.RuntimeRedactor == nil ||
		options.ExecutorTimeout <= 0 || options.MaxOutputBytes <= 0 || options.ProcessRunner == nil ||
		strings.TrimSpace(options.ConfigDigest) == "" || options.ConfigDigest != strings.TrimSpace(options.ConfigDigest) ||
		options.Config.Validate() != nil || (len(options.Config.Init.Link) > 0 && options.ReadonlyLinks == nil) {
		return nil, errors.New("assembly worktree graph options are invalid")
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	scratchBase, err := canonicalAssemblyWorktreeDirectory(options.ScratchBase)
	if err != nil || scratchBase != options.ScratchBase {
		return nil, errors.New("assembly worktree scratch authority is invalid")
	}
	artifactBase, err := canonicalAssemblyWorktreeDirectory(options.ArtifactBase)
	if err != nil || artifactBase != options.ArtifactBase || sameAssemblyWorktreeObject(scratchBase, artifactBase) ||
		assemblyWorktreePathsOverlap(scratchBase, artifactBase) || assemblyWorktreePathsOverlap(projectRoot, scratchBase) ||
		assemblyWorktreePathsOverlap(projectRoot, artifactBase) {
		return nil, errors.New("assembly worktree artifact authority is invalid")
	}
	readonly, err := canonicalAssemblyWorktreeReadonly(projectRoot, options.ReadonlyRoots)
	if err != nil {
		return nil, err
	}
	for _, path := range readonly {
		if path != projectRoot && (assemblyWorktreePathsOverlap(path, scratchBase) || assemblyWorktreePathsOverlap(path, artifactBase)) {
			return nil, errors.New("assembly worktree readonly and writable authorities overlap")
		}
	}

	layout, err := worktree.ResolveManagedLayout(projectRoot, "00000000000000000000000000000000")
	if err != nil {
		return nil, errors.New("assembly worktree managed layout is invalid")
	}
	store, err := worktree.NewFileStore(layout.Control)
	if err != nil {
		return nil, errors.New("assembly worktree store is unavailable")
	}
	locks, err := worktree.NewFileLockManager(layout.Control, options.Config.Lifecycle.LockTimeout)
	if err != nil {
		return nil, errors.New("assembly worktree locks are unavailable")
	}
	git := worktree.NewGitClient(options.GitExecutable, options.GitRunner, options.Config.Lifecycle.GitTimeout, options.GitMaxOutputBytes)
	initializer := worktree.NewFilesystemInitializer(worktree.InitializerOptions{
		Git: git, ReadonlyLinks: options.ReadonlyLinks, Clock: options.Clock,
	})
	manager, err := worktree.NewManager(worktree.ManagerOptions{
		ControlRoot: layout.Control, Config: options.Config.Clone(), Store: store, Locks: locks,
		GitReader: git, GitMutator: git, Initializer: initializer, Clock: options.Clock,
	})
	if err != nil {
		return nil, errors.New("assembly worktree manager is unavailable")
	}
	managerOwned := true
	defer func() {
		if managerOwned {
			_ = manager.Shutdown(context.Background())
		}
	}()

	contextFactory, err := workspace.NewContextFactory(workspace.ContextFactoryOptions{
		Instructions: options.Instructions, InstructionUserRoot: options.InstructionUserRoot,
		MemoryEnabled: options.MemoryEnabled, Memory: options.Memory, ConfigDigest: options.ConfigDigest,
		UserInstructions: options.UserInstructions, UserMemory: options.UserMemory,
		Redactor: options.RuntimeRedactor, MaxSectionBytes: options.MaxSectionBytes,
	})
	if err != nil {
		return nil, errors.New("assembly workspace context factory is unavailable")
	}
	hookFactory, err := hook.NewWorkspaceFactory(hook.WorkspaceFactoryOptions{
		Snapshot: options.HookSnapshot, Engine: options.HookEngine, Runner: options.ProcessRunner,
	})
	if err != nil {
		return nil, errors.New("assembly workspace hook factory is unavailable")
	}
	workspaceFactory, err := workspace.NewFactory(workspace.FactoryOptions{
		SourceRegistry: options.SourceRegistry, ResultFactory: options.ResultFactory,
		ReadCacheLimits: options.ReadCacheLimits, BackgroundPolicy: options.BackgroundPolicy,
		GlobalDenied: options.GlobalDenied, ExecutorTimeout: options.ExecutorTimeout, MaxOutputBytes: options.MaxOutputBytes,
		ContextFactory: contextFactory, HookFactory: hookFactory,
	})
	if err != nil {
		return nil, errors.New("assembly workspace factory is unavailable")
	}
	adapter, err := newAssemblyWorktreeWorkspaceAdapter(assemblyWorktreeWorkspaceAdapterOptions{
		Factory: workspaceFactory, ProjectRoot: projectRoot, ScratchBase: scratchBase,
		ArtifactBase: artifactBase, ReadonlyRoots: readonly[1:],
	})
	if err != nil {
		_ = workspaceFactory.Close(context.Background())
		return nil, err
	}
	workspaceOwned := true
	defer func() {
		if workspaceOwned {
			_ = adapter.Close(context.Background())
		}
	}()
	janitor, err := worktree.NewJanitor(worktree.JanitorOptions{
		ControlRoot: layout.Control, Config: options.Config.Clone(), Store: store, Manager: manager, Clock: options.Clock,
	})
	if err != nil {
		return nil, errors.New("assembly worktree janitor is unavailable")
	}
	managerOwned = false
	workspaceOwned = false
	return &assemblyWorktreeGraph{
		manager: manager, workspace: adapter, janitor: janitor,
		owners: assemblyWorktreeOwners{
			worktrees: manager.Shutdown,
			workspace: adapter.Close,
			janitor:   janitor.Stop,
		},
	}, nil
}

type assemblyWorktreeWorkspaceAdapterOptions struct {
	Factory              workspace.Factory
	ProjectRoot          string
	ScratchBase          string
	ArtifactBase         string
	ReadonlyRoots        []string
	BeforeTaskRootRemove func(string)
}

type assemblyTaskRootAuthority struct {
	path                 string
	root                 *os.Root
	info                 os.FileInfo
	beforeTaskRootRemove func(string)
}

type assemblyTaskRootToken struct {
	authority *assemblyTaskRootAuthority
	name      string
	path      string
	info      os.FileInfo
	identity  safefs.Identity
}

type assemblyFrozenDirectory struct {
	path     string
	info     os.FileInfo
	identity safefs.Identity
}

type assemblyWorktreeWorkspaceAdapter struct {
	factory        workspace.Factory
	mainRoot       string
	readonly       []string
	frozenReadonly []assemblyFrozenDirectory
	scratch        *assemblyTaskRootAuthority
	artifact       *assemblyTaskRootAuthority

	mu        sync.Mutex
	closed    bool
	active    uint64
	idle      chan struct{}
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func newAssemblyWorktreeWorkspaceAdapter(options assemblyWorktreeWorkspaceAdapterOptions) (*assemblyWorktreeWorkspaceAdapter, error) {
	mainRoot, err := canonicalAssemblyWorktreeDirectory(options.ProjectRoot)
	if err != nil || mainRoot != options.ProjectRoot || options.Factory == nil {
		return nil, errors.New("assembly workspace adapter options are invalid")
	}
	scratch, err := openAssemblyTaskRootAuthority(options.ScratchBase)
	if err != nil {
		return nil, err
	}
	artifact, err := openAssemblyTaskRootAuthority(options.ArtifactBase)
	if err != nil {
		_ = scratch.root.Close()
		return nil, err
	}
	if os.SameFile(scratch.info, artifact.info) {
		_ = artifact.root.Close()
		_ = scratch.root.Close()
		return nil, errors.New("assembly workspace writable authorities overlap")
	}
	scratch.beforeTaskRootRemove = options.BeforeTaskRootRemove
	artifact.beforeTaskRootRemove = options.BeforeTaskRootRemove
	readonly, err := canonicalAssemblyWorktreeReadonly(mainRoot, options.ReadonlyRoots)
	if err != nil {
		_ = artifact.root.Close()
		_ = scratch.root.Close()
		return nil, err
	}
	frozenReadonly := make([]assemblyFrozenDirectory, 0, len(readonly))
	for _, path := range readonly {
		frozen, freezeErr := freezeAssemblyWorktreeDirectory(path)
		if freezeErr != nil {
			_ = artifact.root.Close()
			_ = scratch.root.Close()
			return nil, freezeErr
		}
		frozenReadonly = append(frozenReadonly, frozen)
	}
	idle := make(chan struct{})
	close(idle)
	return &assemblyWorktreeWorkspaceAdapter{
		factory: options.Factory, mainRoot: mainRoot, readonly: readonly, frozenReadonly: frozenReadonly,
		scratch: scratch, artifact: artifact, idle: idle, closeDone: make(chan struct{}),
	}, nil
}

func (a *assemblyWorktreeWorkspaceAdapter) Bind(ctx context.Context, request orchestrator.WorktreeWorkspaceBindRequest) (workspace.Runtime, error) {
	if a == nil || ctx == nil || ctx.Err() != nil || !a.validRequest(request) || !a.beginBind() {
		return nil, errors.New("assembly workspace bind request is invalid")
	}
	finished := false
	defer func() {
		if !finished {
			a.finishBind()
		}
	}()
	scratch, err := a.scratch.create(request.Lease.WorkspaceID)
	if err != nil {
		return nil, err
	}
	artifact, err := a.artifact.create(request.Lease.WorkspaceID)
	if err != nil {
		_ = scratch.rollback()
		return nil, err
	}
	definition := request.Role.Definition.Clone()
	bound, err := a.factory.Bind(ctx, workspace.BindRequest{
		TaskID: request.TaskID, Isolation: agentrole.IsolationWorktree,
		Root: request.Lease.Root, ScratchRoot: scratch.path, ArtifactRoot: artifact.path,
		ReadonlyRoots: append([]string(nil), a.readonly...), Lease: request.Lease,
		Role: &definition, PlanMode: request.PlanMode, Verifier: request.Verifier,
	})
	if err != nil && bound != nil {
		cleanupErr := closeAssemblyBoundRuntime(bound)
		bound = nil
		err = errors.Join(errors.New("assembly workspace binding failed"), cleanupErr)
	} else if err != nil {
		err = errors.New("assembly workspace binding failed")
	}
	if err == nil && !assemblyRuntimeMatchesTaskRoots(bound, scratch, artifact, a.frozenReadonly) {
		var cleanupErr error
		if bound != nil {
			cleanupErr = closeAssemblyBoundRuntime(bound)
		}
		bound = nil
		err = errors.Join(errors.New("assembly workspace task roots changed during binding"), cleanupErr)
	}
	if err != nil {
		_ = artifact.rollback()
		_ = scratch.rollback()
		a.finishBind()
		finished = true
		return nil, err
	}
	closing := a.finishBind()
	finished = true
	if closing {
		cleanupErr := closeAssemblyBoundRuntime(bound)
		return nil, errors.Join(errors.New("assembly workspace adapter is closing"), cleanupErr)
	}
	return bound, nil
}

func closeAssemblyBoundRuntime(runtime workspace.Runtime) error {
	if runtime == nil {
		return nil
	}
	if err := runtime.Close(context.Background()); err != nil {
		// Runtime and dependency errors may include project paths or remote
		// payloads. The adapter reports the failed lifecycle phase without
		// promoting those details into the user-facing assembly error.
		return errors.New("assembly workspace runtime cleanup failed")
	}
	return nil
}

func (a *assemblyWorktreeWorkspaceAdapter) validRequest(request orchestrator.WorktreeWorkspaceBindRequest) bool {
	if request.TaskID == "" || !worktree.ValidWorkspaceID(request.Lease.WorkspaceID) ||
		!worktree.ValidWorkspaceID(request.Lease.OwnerID) || request.Lease.AcquiredAt.IsZero() ||
		request.Role.Definition.Isolation != agentrole.IsolationWorktree || request.Verifier == nil {
		return false
	}
	scoped, ok := request.Verifier.(permission.ScopedTicketVerifier)
	if !ok || scoped.ScopeID() != request.TaskID {
		return false
	}
	for _, frozen := range a.frozenReadonly {
		if !frozen.live() {
			return false
		}
	}
	layout, err := worktree.ResolveManagedLayout(a.mainRoot, request.Lease.WorkspaceID)
	if err != nil || request.Lease.Root != layout.WorkspaceRoot || request.Lease.Branch != layout.Branch {
		return false
	}
	if !validAssemblyWorktreeOID(request.Lease.BaseOID) {
		return false
	}
	root, err := canonicalAssemblyWorktreeDirectory(request.Lease.Root)
	return err == nil && root == request.Lease.Root
}

func (a *assemblyWorktreeWorkspaceAdapter) beginBind() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.active == ^uint64(0) {
		return false
	}
	if a.active == 0 {
		a.idle = make(chan struct{})
	}
	a.active++
	return true
}

func (a *assemblyWorktreeWorkspaceAdapter) finishBind() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active > 0 {
		a.active--
		if a.active == 0 {
			close(a.idle)
		}
	}
	return a.closed
}

func (a *assemblyWorktreeWorkspaceAdapter) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("assembly workspace close context is invalid")
	}
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.closed = true
		idle := a.idle
		a.mu.Unlock()
		go a.closeOwned(idle)
	})
	select {
	case <-a.closeDone:
		return a.closeErr
	default:
	}
	select {
	case <-a.closeDone:
		return a.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *assemblyWorktreeWorkspaceAdapter) closeOwned(idle <-chan struct{}) {
	<-idle
	var failures []error
	failures = append(failures, a.factory.Close(context.Background()))
	if a.artifact != nil && a.artifact.root != nil {
		failures = append(failures, a.artifact.root.Close())
	}
	if a.scratch != nil && a.scratch.root != nil {
		failures = append(failures, a.scratch.root.Close())
	}
	a.closeErr = errors.Join(failures...)
	close(a.closeDone)
}

func openAssemblyTaskRootAuthority(path string) (*assemblyTaskRootAuthority, error) {
	canonical, err := canonicalAssemblyWorktreeDirectory(path)
	if err != nil || canonical != path {
		return nil, errors.New("assembly task root authority is invalid")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, errors.New("assembly task root authority is unavailable")
	}
	info, rootErr := root.Stat(".")
	pathInfo, pathErr := os.Stat(path)
	if rootErr != nil || pathErr != nil || !os.SameFile(info, pathInfo) || info.Mode().Perm() != 0o700 {
		_ = root.Close()
		return nil, errors.New("assembly task root authority changed")
	}
	return &assemblyTaskRootAuthority{path: path, root: root, info: info}, nil
}

func (a *assemblyTaskRootAuthority) create(workspaceID string) (*assemblyTaskRootToken, error) {
	if a == nil || a.root == nil || !worktree.ValidWorkspaceID(workspaceID) || !a.live() {
		return nil, errors.New("assembly task root authority is unavailable")
	}
	if _, err := a.root.Lstat(workspaceID); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("assembly task root already exists or is unsafe")
	}
	if err := a.root.Mkdir(workspaceID, 0o700); err != nil {
		return nil, errors.New("assembly task root creation failed")
	}
	var cleanup *assemblyTaskRootToken
	defer func() {
		if cleanup != nil {
			_ = cleanup.rollback()
		}
	}()
	info, err := a.root.Lstat(workspaceID)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return nil, errors.New("assembly task root is unsafe")
	}
	cleanup = &assemblyTaskRootToken{authority: a, name: workspaceID, path: filepath.Join(a.path, workspaceID), info: info}
	child, err := a.root.OpenRoot(workspaceID)
	if err != nil {
		return nil, errors.New("assembly task root is unavailable")
	}
	childInfo, childErr := child.Stat(".")
	closeErr := child.Close()
	if childErr != nil || closeErr != nil || !os.SameFile(info, childInfo) {
		return nil, errors.New("assembly task root identity changed")
	}
	path := cleanup.path
	pathInfo, pathErr := os.Lstat(path)
	if pathErr != nil || !os.SameFile(info, pathInfo) || !a.live() {
		return nil, errors.New("assembly task root pathname changed")
	}
	opened, err := safefs.Bootstrap(path, safefs.Policy{})
	if err != nil {
		return nil, errors.New("assembly task root safety binding failed")
	}
	identity := opened.Root.Identity()
	closeErr = opened.Root.Close()
	pathInfo, pathErr = os.Lstat(path)
	if closeErr != nil || identity == (safefs.Identity{}) || pathErr != nil || !os.SameFile(info, pathInfo) || !a.live() {
		return nil, errors.New("assembly task root safety binding failed")
	}
	cleanup.identity = identity
	token := cleanup
	cleanup = nil
	return token, nil
}

func (a *assemblyTaskRootAuthority) live() bool {
	if a == nil || a.root == nil || a.info == nil {
		return false
	}
	rootInfo, rootErr := a.root.Stat(".")
	pathInfo, pathErr := os.Stat(a.path)
	return rootErr == nil && pathErr == nil && os.SameFile(a.info, rootInfo) && os.SameFile(a.info, pathInfo)
}

func (t *assemblyTaskRootToken) rollback() error {
	if t == nil || t.authority == nil || !t.authority.live() {
		return nil
	}
	info, err := t.authority.root.Lstat(t.name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, t.info) {
		return nil
	}
	child, err := t.authority.root.OpenRoot(t.name)
	if err != nil {
		return nil
	}
	childInfo, statErr := child.Stat(".")
	file, openErr := child.Open(".")
	empty := false
	if openErr == nil {
		_, readErr := file.Readdirnames(1)
		empty = errors.Is(readErr, io.EOF)
		_ = file.Close()
	}
	_ = child.Close()
	if statErr != nil || !os.SameFile(childInfo, t.info) || !empty {
		return nil
	}
	current, err := t.authority.root.Lstat(t.name)
	if err != nil || !os.SameFile(current, t.info) {
		return nil
	}
	if t.authority.beforeTaskRootRemove != nil {
		t.authority.beforeTaskRootRemove(t.path)
	}
	// POSIX and Windows do not expose an identity-conditional rmdir. Even after
	// the checks above, a rename swap could replace this name before Remove.
	// Preserve the workspace-ID directory as bounded task metadata rather than
	// risk deleting an unknown filesystem object. A later owner with a stronger
	// process-level cleanup policy may collect it.
	return nil
}

func assemblyRuntimeMatchesTaskRoots(runtime workspace.Runtime, scratch, artifact *assemblyTaskRootToken, readonly []assemblyFrozenDirectory) bool {
	if runtime == nil || runtime.Protection() == nil || scratch == nil || artifact == nil {
		return false
	}
	if runtime.ScratchRoot() != scratch.path || runtime.ArtifactRoot() != artifact.path ||
		runtime.Protection().Scratch().Root.Identity() != scratch.identity ||
		runtime.Protection().Artifact().Root.Identity() != artifact.identity {
		return false
	}
	boundReadonly := runtime.Protection().Readonly()
	if len(boundReadonly) != len(readonly) {
		return false
	}
	for index, frozen := range readonly {
		if boundReadonly[index].Path != frozen.path || boundReadonly[index].Root.Identity() != frozen.identity {
			return false
		}
	}
	return true
}

func freezeAssemblyWorktreeDirectory(path string) (assemblyFrozenDirectory, error) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return assemblyFrozenDirectory{}, errors.New("assembly readonly root is unavailable")
	}
	opened, err := safefs.Bootstrap(path, safefs.Policy{})
	if err != nil {
		return assemblyFrozenDirectory{}, errors.New("assembly readonly root is unavailable")
	}
	identity := opened.Root.Identity()
	closeErr := opened.Root.Close()
	if closeErr != nil || identity == (safefs.Identity{}) {
		return assemblyFrozenDirectory{}, errors.New("assembly readonly root is unavailable")
	}
	return assemblyFrozenDirectory{path: path, info: info, identity: identity}, nil
}

func (f assemblyFrozenDirectory) live() bool {
	info, err := os.Stat(f.path)
	if err != nil || !os.SameFile(f.info, info) {
		return false
	}
	opened, err := safefs.Bootstrap(f.path, safefs.Policy{})
	if err != nil {
		return false
	}
	identity := opened.Root.Identity()
	return opened.Root.Close() == nil && identity == f.identity
}

func canonicalAssemblyWorktreeDirectory(path string) (string, error) {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("assembly worktree directory is invalid")
	}
	identity, err := memory.NewProjectIdentity(path, "assembly-worktree-directory")
	if err != nil || identity.RootRealPath != path {
		return "", errors.New("assembly worktree directory is not canonical")
	}
	return identity.RootRealPath, nil
}

func canonicalAssemblyWorktreeReadonly(mainRoot string, additional []string) ([]string, error) {
	result := []string{mainRoot}
	for _, path := range additional {
		canonical, err := canonicalAssemblyWorktreeDirectory(path)
		if err != nil || canonical != path {
			return nil, errors.New("assembly worktree readonly root is invalid")
		}
		for _, existing := range result {
			if sameAssemblyWorktreeObject(existing, canonical) {
				return nil, errors.New("assembly worktree readonly roots overlap")
			}
		}
		result = append(result, canonical)
	}
	return result, nil
}

func sameAssemblyWorktreeObject(first, second string) bool {
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	return firstErr == nil && secondErr == nil && os.SameFile(firstInfo, secondInfo)
}

func assemblyWorktreePathsOverlap(first, second string) bool {
	if first == second {
		return true
	}
	within := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && relative != "." && relative != ".." &&
			!filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return within(first, second) || within(second, first)
}

func validAssemblyWorktreeOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

var _ orchestrator.WorktreeWorkspaceBinder = (*assemblyWorktreeWorkspaceAdapter)(nil)
