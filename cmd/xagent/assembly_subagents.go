package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
	"xagent/internal/memory"
	"xagent/internal/orchestrator"
	"xagent/internal/permission"
	"xagent/internal/proctree"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/sessionctx"
	"xagent/internal/subagent"
	"xagent/internal/tool"
	"xagent/internal/worktree"
)

// assemblySubagents is the closed process-local subagent graph. It retains
// only domain services and immutable catalogs; roots, configuration and
// security capabilities remain owned by the Assembly stage that built it.
type assemblySubagents struct {
	models     agentrole.ModelCatalog
	roles      agentrole.Manager
	runner     *orchestrator.SubagentRunnerFactory
	tasks      subagent.Service
	projector  *orchestrator.ResultProjector
	closeInbox func(error)
}

func (graph *assemblySubagents) shutdown(ctx context.Context) error {
	if graph == nil || graph.tasks == nil {
		return nil
	}
	return graph.tasks.Shutdown(ctx)
}

func (graph *assemblySubagents) closeResultInbox(context.Context) error {
	if graph != nil && graph.closeInbox != nil {
		graph.closeInbox(errors.New("assembly subagent result inbox closed"))
	}
	return nil
}

type assemblySubagentRequest struct {
	Paths                RuntimePaths
	Config               config.AppConfig
	Provider             provider.Provider
	Registry             *tool.Registry
	Executor             *tool.Executor
	ResultFactory        *tool.ResultFactory
	Authorizer           *permission.Authorizer
	SessionContext       sessionctx.StablePreparer
	ContextManager       *contextmgr.Manager
	RequestBudgeter      contextmgr.RequestBudgeter
	ContextPolicy        orchestrator.SkillHistoryPolicy
	Hooks                hook.Runtime
	RuntimeRedactor      *redact.RuntimeRedactor
	CleanupTimeout       time.Duration
	LifecycleDiagnostics diagnostics.BoundedSink
	Worktrees            *assemblyWorktreeGraph
	WorktreeTemplate     worktree.AcquireRequest
}

// assemblySubagentWorktrees is the private bridge from the T60 graph into the
// SubAgent runner. The graph itself exposes only the lifecycle interfaces used
// by orchestrator plus its package-local owner closures.
type assemblySubagentWorktrees struct {
	graph    *assemblyWorktreeGraph
	template worktree.AcquireRequest
}

func assemblySubagentWorktreeGraph(value *assemblySubagentWorktrees) *assemblyWorktreeGraph {
	if value == nil {
		return nil
	}
	return value.graph
}

func assemblySubagentWorktreeAcquireTemplate(value *assemblySubagentWorktrees) worktree.AcquireRequest {
	if value == nil {
		return worktree.AcquireRequest{}
	}
	return value.template
}

type assemblySubagentWorktreeRequest struct {
	Paths                RuntimePaths
	Config               config.AppConfig
	Registry             *tool.Registry
	ResultFactory        *tool.ResultFactory
	Provider             provider.Provider
	UserInstructions     sessionctx.InstructionLoader
	UserMemory           sessionctx.MemoryIndexProvider
	HookSnapshot         hook.Snapshot
	HookHTTP             hook.HTTPRunner
	ProcessRunner        proctree.Runner
	RuntimeRedactor      *redact.RuntimeRedactor
	LifecycleDiagnostics diagnostics.BoundedSink
	CleanupTimeout       time.Duration
	GitExecutable        string
	GitRunner            worktree.GitCommandRunner
	ReadonlyLinks        worktree.ReadonlyLinkCapability
}

// newAssemblySubagentWorktrees constructs the optional repository-scoped
// graph. A project without a .git directory remains shared-only; T63 owns the
// richer capability diagnostic and linked-worktree downgrade policy.
func newAssemblySubagentWorktrees(request assemblySubagentWorktreeRequest) (*assemblySubagentWorktrees, error) {
	if !validAssemblyPath(request.Paths.ProjectRoot) || !validAssemblyPath(request.Paths.UserConfigRoot) ||
		!validAssemblyPath(request.Paths.UserDataRoot) || !validAssemblyPath(request.Paths.UserCacheRoot) ||
		request.Registry == nil || !request.Registry.IsSealed() || request.ResultFactory == nil || request.Provider == nil ||
		request.ProcessRunner == nil || request.RuntimeRedactor == nil || request.LifecycleDiagnostics == nil ||
		request.CleanupTimeout <= 0 {
		return nil, errors.New("assembly subagent worktree dependencies are unavailable")
	}
	gitExecutable := request.GitExecutable
	if gitExecutable == "" {
		gitExecutable = "git"
	}
	available, err := probeAssemblyWorktreeCapability(context.Background(), assemblyWorktreeCapabilityOptions{
		ProjectRoot: request.Paths.ProjectRoot, UserCacheRoot: request.Paths.UserCacheRoot,
		Config: request.Config.Subagent.Worktree, GitExecutable: gitExecutable, GitRunner: request.GitRunner,
		GitMaxOutputBytes: request.Config.Tool.MaxOutputBytes, ReadonlyLinks: request.ReadonlyLinks,
	})
	if err != nil {
		return nil, err
	}
	if !available {
		return nil, nil
	}
	template, available, err := assemblySubagentWorktreeTemplate(request.Paths.ProjectRoot)
	if err != nil {
		return nil, err
	}
	if !available {
		return nil, nil
	}
	scratchBase, err := prepareAssemblySubagentPrivateDirectory(request.Paths.UserCacheRoot, "worktrees", "scratch")
	if err != nil {
		return nil, err
	}
	artifactBase, err := prepareAssemblySubagentPrivateDirectory(request.Paths.UserCacheRoot, "worktrees", "artifacts")
	if err != nil {
		return nil, err
	}
	// The process-wide loader already treats UserConfigRoot as the explicit
	// user instruction root. Preserve that exact binding for every task; the
	// config UserDir fallback is only used when no explicit root is supplied.
	instructionUserRoot := request.Paths.UserConfigRoot
	digest, err := assemblySubagentWorktreeConfigDigest(request.Config)
	if err != nil {
		return nil, err
	}
	limits := request.Config.Subagent.Limits
	background := tool.BackgroundPolicy{Enabled: true, AllowedNames: append([]string(nil), request.Config.Subagent.BackgroundTools...)}
	if request.Config.Subagent.BackgroundTools == nil {
		background.AllowedNames = nil
	}
	graph, err := newAssemblyWorktreeGraph(assemblyWorktreeGraphOptions{
		ProjectRoot: request.Paths.ProjectRoot, ScratchBase: scratchBase, ArtifactBase: artifactBase,
		Config: request.Config.Subagent.Worktree, GitExecutable: gitExecutable, GitRunner: request.GitRunner,
		GitMaxOutputBytes: request.Config.Tool.MaxOutputBytes, ReadonlyLinks: request.ReadonlyLinks,
		SourceRegistry: request.Registry, ResultFactory: request.ResultFactory,
		ReadCacheLimits: tool.ReadCacheLimits{
			MaxEntries: limits.ReadCacheMaxEntries, MaxBytes: limits.ReadCacheMaxBytes,
			MaxValueBytes: limits.ReadCacheMaxValueBytes, MaxDependenciesPerEntry: limits.ReadCacheMaxDependenciesPerEntry,
		},
		BackgroundPolicy: background, GlobalDenied: map[string]struct{}{tool.AgentToolName: {}},
		ExecutorTimeout: time.Duration(request.Config.Tool.TimeoutMS) * time.Millisecond,
		MaxOutputBytes:  request.Config.Tool.MaxOutputBytes,
		Instructions:    request.Config.Instructions, InstructionUserRoot: instructionUserRoot,
		MemoryEnabled: request.Config.Memory.Enabled,
		Memory: memory.ManagerOptions{
			MaxIndexLines: request.Config.Memory.MaxIndexLines, MaxIndexBytes: request.Config.Memory.MaxIndexBytes,
			UpdateQueueSize:   request.Config.Memory.UpdateQueueSize,
			UpdateConcurrency: request.Config.Memory.UpdateConcurrency, UpdateTimeoutMS: request.Config.Memory.UpdateTimeoutMS,
			MaxCandidateBytes: request.Config.Memory.MaxCandidateBytes, Provider: request.Provider,
			Redactor: request.RuntimeRedactor,
		},
		ConfigDigest: digest, UserInstructions: request.UserInstructions, UserMemory: request.UserMemory,
		RuntimeRedactor: request.RuntimeRedactor,
		HookSnapshot:    request.HookSnapshot,
		HookEngine: hook.EngineOptions{
			Diagnostics: request.LifecycleDiagnostics, Redactor: request.RuntimeRedactor, HTTPRunner: request.HookHTTP,
			Limits: hook.DefaultLimits(), AsyncWorkers: 4, AsyncQueue: 64, CleanupTimeout: request.CleanupTimeout,
			ShutdownGrace: request.CleanupTimeout, ShutdownJoinGrace: request.CleanupTimeout,
		},
		ProcessRunner: request.ProcessRunner,
	})
	if err != nil {
		return nil, errors.Join(errors.New("assembly subagent worktree graph is unavailable"), err)
	}
	return &assemblySubagentWorktrees{graph: graph, template: template}, nil
}

func assemblySubagentWorktreeTemplate(projectRoot string) (worktree.AcquireRequest, bool, error) {
	gitDir := filepath.Join(projectRoot, ".git")
	info, err := os.Lstat(gitDir)
	if errors.Is(err, os.ErrNotExist) {
		return worktree.AcquireRequest{}, false, nil
	}
	if err != nil {
		return worktree.AcquireRequest{}, false, errors.New("assembly subagent repository identity is unavailable")
	}
	if !info.IsDir() {
		// A linked checkout uses a .git file. Common-dir discovery and its
		// explicit capability downgrade are deliberately owned by T63.
		return worktree.AcquireRequest{}, false, nil
	}
	identity, err := worktree.NewRepositoryIdentity(projectRoot, gitDir)
	if err != nil || identity.Root != projectRoot || identity.CommonDir != gitDir {
		return worktree.AcquireRequest{}, false, errors.New("assembly subagent repository identity is invalid")
	}
	return worktree.AcquireRequest{
		RepositoryRoot: projectRoot, RepositoryIdentity: identity, LogicalName: "subagent",
	}, true, nil
}

func prepareAssemblySubagentPrivateDirectory(base string, components ...string) (prepared string, resultErr error) {
	canonical, err := canonicalAssemblyWorktreeDirectory(base)
	if err != nil || canonical != base {
		return "", errors.New("assembly subagent writable authority is invalid")
	}
	current, err := os.OpenRoot(base)
	if err != nil {
		return "", errors.New("assembly subagent writable authority is unavailable")
	}
	defer func() {
		if closeErr := current.Close(); closeErr != nil {
			prepared = ""
			resultErr = errors.Join(resultErr, errors.New("assembly subagent writable authority close failed"))
		}
	}()
	path := base
	for _, component := range components {
		if component == "" || component == "." || component == ".." || filepath.Base(component) != component {
			return "", errors.New("assembly subagent writable authority is invalid")
		}
		info, statErr := current.Lstat(component)
		if errors.Is(statErr, os.ErrNotExist) {
			if mkdirErr := current.Mkdir(component, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				return "", errors.New("assembly subagent writable authority is unavailable")
			}
			info, statErr = current.Lstat(component)
		}
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
			return "", errors.New("assembly subagent writable authority is unsafe")
		}
		child, openErr := current.OpenRoot(component)
		if openErr != nil {
			return "", errors.New("assembly subagent writable authority is unavailable")
		}
		childInfo, childErr := child.Stat(".")
		if childErr != nil || !os.SameFile(info, childInfo) {
			_ = child.Close()
			return "", errors.New("assembly subagent writable authority changed")
		}
		if closeErr := current.Close(); closeErr != nil {
			_ = child.Close()
			return "", errors.New("assembly subagent writable authority close failed")
		}
		current = child
		path = filepath.Join(path, component)
	}
	return path, nil
}

func assemblySubagentWorktreeConfigDigest(resolved config.AppConfig) (string, error) {
	payload, err := json.Marshal(struct {
		Instructions config.InstructionsConfig
		Memory       config.MemoryConfig
		Worktree     worktree.Config
	}{Instructions: resolved.Instructions, Memory: resolved.Memory, Worktree: resolved.Subagent.Worktree})
	if err != nil {
		return "", errors.New("assembly subagent worktree config snapshot is invalid")
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

// newAssemblySubagents is the sole production/candidate constructor for the
// role catalog, isolated runner, result inbox and task manager.
func newAssemblySubagents(ctx context.Context, request assemblySubagentRequest) (*assemblySubagents, error) {
	if ctx == nil || request.Registry == nil || !request.Registry.IsSealed() ||
		request.Executor == nil || request.Executor.Registry != request.Registry || request.ResultFactory == nil ||
		request.Provider == nil || request.Authorizer == nil || request.SessionContext == nil ||
		request.ContextManager == nil || request.RuntimeRedactor == nil ||
		request.CleanupTimeout <= 0 || !validAssemblyPath(request.Paths.ProjectRoot) ||
		!validAssemblyPath(request.Paths.UserConfigRoot) {
		return nil, errors.New("assembly subagent dependencies are unavailable")
	}
	if err := request.Config.Subagent.Limits.Validate(); err != nil {
		return nil, errors.New("assembly subagent runtime limits are invalid")
	}
	if err := request.Config.Subagent.RoleLimits.Validate(); err != nil {
		return nil, errors.New("assembly subagent role limits are invalid")
	}
	if err := request.RequestBudgeter.Validate(); err != nil {
		return nil, errors.New("assembly subagent request budgeter is invalid")
	}

	models, err := agentrole.NewModelCatalog(agentrole.ModelCatalogOptions{
		ProviderID:    request.Config.LLM.Protocol,
		DefaultModel:  request.Config.LLM.Model,
		Aliases:       request.Config.LLM.ModelAliases.ToAgentRole(),
		MaxModelBytes: request.Config.Subagent.RoleLimits.MaxModelBytes,
		Validate: func(string) error {
			// The concrete Provider was already constructed from this resolved
			// model configuration. ModelCatalog performs the bounded syntax and
			// control-character validation before this local acceptance seam.
			return nil
		},
	})
	if err != nil {
		return nil, errors.New("assembly subagent model catalog is invalid")
	}
	plugins, err := agentrole.NewProviderRegistry(
		request.Config.Subagent.RoleLimits.MaxProviders,
		request.Config.Subagent.RoleLimits.MaxProviderIDBytes,
	)
	if err != nil {
		return nil, errors.New("assembly subagent role provider registry is invalid")
	}
	if err := plugins.Seal(); err != nil {
		return nil, errors.New("assembly subagent role provider registry cannot be sealed")
	}
	sources, err := assemblySubagentRoleSources(request.Paths)
	if err != nil {
		return nil, err
	}
	toolMetadata, err := assemblySubagentToolMetadata(request.Registry)
	if err != nil {
		return nil, err
	}
	roles, err := agentrole.NewManager(ctx, agentrole.ManagerOptions{
		Sources: sources, Plugins: plugins, Tools: toolMetadata, Models: models,
		Limits: request.Config.Subagent.RoleLimits, Redactor: request.RuntimeRedactor,
	})
	if err != nil {
		return nil, errors.New("assembly subagent role manager is unavailable")
	}

	backgroundPolicy := tool.BackgroundPolicy{
		Enabled: true, AllowedNames: append([]string(nil), request.Config.Subagent.BackgroundTools...),
	}
	if request.Config.Subagent.BackgroundTools == nil {
		backgroundPolicy.AllowedNames = nil
	}
	runnerOptions := orchestrator.SubagentRunnerOptions{
		Provider: request.Provider, Registry: request.Registry, Executor: request.Executor,
		ResultFactory: request.ResultFactory, Roles: roles, Models: models, Authorizer: request.Authorizer,
		BackgroundPolicy: backgroundPolicy, GlobalDenied: map[string]struct{}{tool.AgentToolName: {}},
		Limits: request.Config.Subagent.Limits,
		RunOptions: orchestrator.RunOptions{
			MaxIterations: request.Config.Agent.MaxIterations, MaxUnknownToolCalls: request.Config.Agent.MaxUnknownToolCalls,
		},
		SessionContext: request.SessionContext, ContextManager: request.ContextManager,
		RequestBudgeter: request.RequestBudgeter, ContextPolicy: request.ContextPolicy,
		Thinking: request.Config.LLM.Thinking, Hooks: request.Hooks, RuntimeRedactor: request.RuntimeRedactor,
		ParentRuntime: orchestrator.ParentRuntimeSnapshotFromSubmitContext,
		ChatStreamOptions: provider.ChatStreamOptions{
			CleanupTimeout: request.CleanupTimeout, Diagnostics: request.LifecycleDiagnostics,
		},
	}
	if err := applyAssemblySubagentWorktrees(&runnerOptions, request.Worktrees, request.WorktreeTemplate); err != nil {
		return nil, err
	}
	runner, err := orchestrator.NewSubagentRunnerFactory(runnerOptions)
	if err != nil {
		return nil, errors.New("assembly subagent runner factory is unavailable")
	}

	inbox, err := subagent.NewResultInbox(subagent.ResultInboxOptions{
		Limits: request.Config.Subagent.Limits, IDGenerator: assemblySubagentID,
	})
	if err != nil {
		return nil, errors.New("assembly subagent result inbox is unavailable")
	}
	closeInbox := func(cause error) { inbox.Close(cause) }
	tasks, err := subagent.NewManager(subagent.ManagerOptions{
		Runner: runner, Limits: request.Config.Subagent.Limits, Inbox: inbox,
		Redactor: request.RuntimeRedactor, IDGenerator: assemblySubagentID,
		ShutdownTimeout: request.CleanupTimeout,
	})
	if err != nil {
		closeInbox(err)
		return nil, errors.New("assembly subagent task manager is unavailable")
	}
	planningLimit := request.Config.Context.ModelWindowTokens - request.Config.Context.AutoMarginTokens
	projector, err := orchestrator.NewResultProjector(orchestrator.ResultProjectorOptions{
		Service: tasks, Budgeter: request.RequestBudgeter, RuntimeRedactor: request.RuntimeRedactor,
		MaxRequestPlanningTokens: planningLimit,
		MaxResultBytes:           request.Config.Subagent.Limits.MaxResultBytes,
		MaxResultsPerClaim:       request.Config.Subagent.Limits.MaxResultsPerClaim,
	})
	if err != nil {
		_ = tasks.Shutdown(context.Background())
		closeInbox(err)
		return nil, errors.New("assembly subagent result projector is unavailable")
	}
	return &assemblySubagents{
		models: models, roles: roles, runner: runner, tasks: tasks, projector: projector, closeInbox: closeInbox,
	}, nil
}

func applyAssemblySubagentWorktrees(options *orchestrator.SubagentRunnerOptions, graph *assemblyWorktreeGraph, template worktree.AcquireRequest) error {
	if options == nil {
		return errors.New("assembly subagent runner options are unavailable")
	}
	if graph == nil {
		if template != (worktree.AcquireRequest{}) {
			return errors.New("assembly subagent worktree boundary is incomplete")
		}
		return nil
	}
	if graph.manager == nil || graph.workspace == nil || template == (worktree.AcquireRequest{}) {
		return errors.New("assembly subagent worktree boundary is incomplete")
	}
	options.WorktreeManager = graph.manager
	options.WorkspaceBinder = graph.workspace
	options.WorktreeAcquireTemplate = template
	return nil
}

func assemblySubagentRoleSources(paths RuntimePaths) ([]agentrole.FileSource, error) {
	if !validAssemblyPath(paths.ProjectRoot) || !validAssemblyPath(paths.UserConfigRoot) {
		return nil, errors.New("assembly subagent role roots are invalid")
	}
	return []agentrole.FileSource{
		agentrole.BuiltinSource(),
		{Source: agentrole.SourceUser, ID: "user", FS: os.DirFS(paths.UserConfigRoot), Root: "agents"},
		{
			Source: agentrole.SourceProject, ID: "project", FS: os.DirFS(paths.ProjectRoot),
			Root: filepath.ToSlash(filepath.Join(".xagent", "agents")),
		},
	}, nil
}

func assemblySubagentToolMetadata(registry *tool.Registry) ([]agentrole.ToolMetadata, error) {
	if registry == nil || !registry.IsSealed() {
		return nil, errors.New("assembly subagent tool registry is unavailable or unsealed")
	}
	names := registry.Names()
	sort.Strings(names)
	metadata := make([]agentrole.ToolMetadata, 0, len(names))
	for _, name := range names {
		descriptor, ok := registry.Descriptor(name)
		if !ok || strings.TrimSpace(descriptor.Name) != name {
			return nil, errors.New("assembly subagent tool metadata is inconsistent")
		}
		metadata = append(metadata, agentrole.ToolMetadata{
			Name: name, ReadOnly: descriptor.Policy.ReadOnly,
			SideEffectFree: descriptor.Policy.SideEffectFree, ConcurrentSafe: descriptor.Policy.ConcurrentSafe,
		})
	}
	return metadata, nil
}

func assemblySubagentID() (subagent.ID, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("assembly subagent identity generation failed")
	}
	return subagent.ID(hex.EncodeToString(value[:])), nil
}
