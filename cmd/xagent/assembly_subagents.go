package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"xagent/internal/orchestrator"
	"xagent/internal/permission"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tool"
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
	SessionContext       orchestrator.StableContextPreparer
	ContextManager       *contextmgr.Manager
	RequestBudgeter      contextmgr.RequestBudgeter
	ContextPolicy        orchestrator.SkillHistoryPolicy
	Hooks                hook.Runtime
	RuntimeRedactor      *redact.RuntimeRedactor
	CleanupTimeout       time.Duration
	LifecycleDiagnostics diagnostics.BoundedSink
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
	runner, err := orchestrator.NewSubagentRunnerFactory(orchestrator.SubagentRunnerOptions{
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
	})
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
