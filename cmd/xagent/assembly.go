package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"xagent/internal/app"
	"xagent/internal/artifact"
	"xagent/internal/budget"
	"xagent/internal/command"
	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/instructions"
	"xagent/internal/mcpclient"
	"xagent/internal/memory"
	"xagent/internal/netpolicy"
	"xagent/internal/orchestrator"
	"xagent/internal/permission"
	"xagent/internal/proctree"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/resources"
	"xagent/internal/safefs"
	"xagent/internal/sessionctx"
	"xagent/internal/skill"
	"xagent/internal/tool"
	"xagent/internal/tui"
)

type assemblyStage uint8

const (
	assemblyStageConfig assemblyStage = iota + 1
	assemblyStageSecurity
	assemblyStageExecution
	assemblyStageAdapters
	assemblyStageOrchestration
	assemblyStageUI
	assemblyStageCount = 6
)

func orderedAssemblyStages() [assemblyStageCount]assemblyStage {
	return [...]assemblyStage{
		assemblyStageConfig,
		assemblyStageSecurity,
		assemblyStageExecution,
		assemblyStageAdapters,
		assemblyStageOrchestration,
		assemblyStageUI,
	}
}

func (stage assemblyStage) valid() bool {
	return stage >= assemblyStageConfig && stage <= assemblyStageUI
}

func (stage assemblyStage) label() string {
	switch stage {
	case assemblyStageConfig:
		return "config"
	case assemblyStageSecurity:
		return "security"
	case assemblyStageExecution:
		return "execution"
	case assemblyStageAdapters:
		return "adapters"
	case assemblyStageOrchestration:
		return "orchestration"
	case assemblyStageUI:
		return "ui"
	default:
		return "unknown"
	}
}

type assemblyStageBuilder func(context.Context, *assemblyBuildState, assemblyOwnerRegistrar) error

type assemblyFactories struct {
	config        assemblyStageBuilder
	security      assemblyStageBuilder
	execution     assemblyStageBuilder
	adapters      assemblyStageBuilder
	orchestration assemblyStageBuilder
	ui            assemblyStageBuilder
}

func (factories assemblyFactories) ordered() [assemblyStageCount]assemblyStageBuilder {
	return [...]assemblyStageBuilder{
		factories.config,
		factories.security,
		factories.execution,
		factories.adapters,
		factories.orchestration,
		factories.ui,
	}
}

// Assembly is the only composition boundary exposed by cmd/xagent.  Its
// factory graph is deliberately private: callers can provide roots, config
// layers, environment lookup and I/O, but cannot replace a production
// network/client, transport, process runner or domain service.
type Assembly struct {
	factories assemblyFactories
}

// assembly is retained as a package-local alias for the stage-focused tests
// and helpers written before the public Build boundary was introduced.  It
// does not create a second production assembly root.
type assembly = Assembly

type assemblyBuildState struct {
	options       *AssemblyOptions
	configuration *assemblyConfiguration
	security      *assemblySecurity
	execution     *assemblyExecution
	adapters      *assemblyAdapters
	context       *assemblyContextServices
	orchestration *assemblyOrchestration
	ui            *assemblyUI
}

// assemblyOrchestration is the sealed, candidate-only graph produced by the
// orchestration stage. It keeps narrow domain services, never the composition
// root, resolved config, filesystem capabilities, or network owners.
type assemblyOrchestration struct {
	orchestrator       *orchestrator.Orchestrator
	contextManager     *contextmgr.Manager
	sessionContext     *sessionctx.Manager
	memory             *memory.Manager
	resources          resources.PromptProvider
	requestBudgeter    contextmgr.RequestBudgeter
	skillHistoryPolicy orchestrator.SkillHistoryPolicy
	commandRegistry    *command.Registry
}

// assemblyUI is the final, unpublished UI graph.  The App side is expressed
// exclusively through app.AppServices' four narrow interfaces; the TUI side
// retains only a capability-free ViewModel and a value-only command intent
// sink.  In particular this descriptor never stores Config, a Provider, an
// Orchestrator concrete service, an artifact Store, or a filesystem/network
// capability directly.
type assemblyUI struct {
	appServices    app.AppServices
	runtimeOptions app.RuntimeOptions
	viewModel      tui.ViewModel
	intentSink     assemblyTUIIntentSink
	// model is the fully constructed App published only after every prior
	// stage has completed and its owner has been registered.  Keeping the
	// pointer here lets the process entry run the exact instance whose
	// lifecycle action is present in the ownership registry.
	model *app.Model
}

// assemblyTUIIntentSink is the complete value-only handoff frozen by T4.3.
// Unlike command.IntentSink, it preserves submitted text and opaque session
// targets as well as navigation kinds, without exposing an App capability to
// the renderer.
type assemblyTUIIntentSink interface {
	HandleIntent(tui.Intent) error
}

// The App boundary adapters deliberately forward only AppServices' approved
// methods. Passing the broad concrete owners directly through an interface
// would let App recover their larger method sets with a type assertion.
type assemblyConversationAccess struct {
	service app.ConversationAccess
}

func (access assemblyConversationAccess) Create(ctx context.Context) (*conversation.Conversation, error) {
	return access.service.Create(ctx)
}

func (access assemblyConversationAccess) List(ctx context.Context) (conversation.ListResult, error) {
	return access.service.List(ctx)
}

func (access assemblyConversationAccess) Load(ctx context.Context, id string) (conversation.LoadResult, error) {
	return access.service.Load(ctx, id)
}

func (access assemblyConversationAccess) Save(ctx context.Context, value *conversation.Conversation) (conversation.SaveResult, error) {
	return access.service.Save(ctx, value)
}

type assemblyOrchestrationAccess struct {
	service app.Orchestration
}

func (access assemblyOrchestrationAccess) WaitIdle(ctx context.Context) error {
	return access.service.WaitIdle(ctx)
}

func (access assemblyOrchestrationAccess) BuildExecutionProfile(mode orchestrator.RunMode, activity *skill.Activity, depth int) (skill.ExecutionProfile, error) {
	return access.service.BuildExecutionProfile(mode, activity, depth)
}

func (access assemblyOrchestrationAccess) SendRequest(ctx context.Context, value *conversation.Conversation, request orchestrator.RunRequest) (<-chan events.Event, error) {
	return access.service.SendRequest(ctx, value, request)
}

func (access assemblyOrchestrationAccess) SendSkill(ctx context.Context, value *conversation.Conversation, invocation skill.Invocation, activity *skill.Activity, mode orchestrator.RunMode) (<-chan events.Event, skill.PreparedInvocation, error) {
	return access.service.SendSkill(ctx, value, invocation, activity, mode)
}

func (access assemblyOrchestrationAccess) ResolveToolConfirmation(decision events.ToolConfirmationDecision) bool {
	return access.service.ResolveToolConfirmation(decision)
}

func (access assemblyOrchestrationAccess) CompactContext(ctx context.Context, value *conversation.Conversation) (contextmgr.Result, error) {
	return access.service.CompactContext(ctx, value)
}

func (access assemblyOrchestrationAccess) PermissionStatus() orchestrator.PermissionStatus {
	return access.service.PermissionStatus()
}

type assemblyArtifactUserReader struct {
	service app.ArtifactUserReader
}

func (reader assemblyArtifactUserReader) OpenForUser(ctx context.Context, id string) (io.ReadCloser, artifact.Ref, error) {
	return reader.service.OpenForUser(ctx, id)
}

type assemblyHookLifecycle struct {
	service app.HookLifecycle
}

func (lifecycle assemblyHookLifecycle) SystemStart(ctx context.Context) {
	lifecycle.service.SystemStart(ctx)
}

func (lifecycle assemblyHookLifecycle) SessionStart(ctx context.Context, id string, state hook.SessionState) {
	lifecycle.service.SessionStart(ctx, id, state)
}

func (lifecycle assemblyHookLifecycle) SessionEnd(ctx context.Context, id string, reason hook.SessionEndReason) {
	lifecycle.service.SessionEnd(ctx, id, reason)
}

var (
	_ app.ConversationAccess = assemblyConversationAccess{}
	_ app.Orchestration      = assemblyOrchestrationAccess{}
	_ app.ArtifactUserReader = assemblyArtifactUserReader{}
	_ app.HookLifecycle      = assemblyHookLifecycle{}
	_ assemblyTUIIntentSink  = unpublishedIntentSink{}
)

// unpublishedIntentSink is deliberately fail-closed.  T4.25f prepares the
// value boundary but does not publish an App controller; T4.29a will bind the
// real controller at the sole production cutover.  Keeping this sink inert
// prevents a partially assembled candidate from performing navigation side
// effects if it is accidentally exercised by a test or an initializer.
type unpublishedIntentSink struct{}

func (unpublishedIntentSink) HandleIntent(tui.Intent) error {
	return errors.New("assembly UI candidate is unpublished")
}

type assemblyConfigRequest struct {
	userPath    string
	projectPath string
	runtime     config.PartialAppConfig
}

type assemblyConfiguration struct {
	loaded         config.LoadedConfig
	redactor       *redact.RuntimeRedactor
	diagnostics    *diagnostics.Sink
	cleanupTimeout time.Duration
}

type assemblySecurityRequest struct {
	paths        RuntimePaths
	trustedRoots *x509.CertPool
}

type assemblyRoots struct {
	project *safefs.Root
	legacy  *safefs.Root
}

type assemblyRootCapabilities struct {
	project safefs.Capabilities
	legacy  safefs.Capabilities
}

type assemblySecurity struct {
	roots           assemblyRoots
	capabilities    assemblyRootCapabilities
	artifacts       artifact.Store
	networkPolicy   netpolicy.HTTPPolicy
	networkClients  netpolicy.ClientFactory
	trustedRoots    *x509.CertPool
	processes       proctree.Runner
	protectionPlans proctree.ProtectionPlanFactory
}

type assemblyExecutionRequest struct {
	paths RuntimePaths
}

type assemblyPermissions struct {
	health     *permission.Health
	writer     permission.Writer
	authority  *permission.TicketAuthority
	authorizer *permission.Authorizer
}

type assemblyExecution struct {
	permissions   assemblyPermissions
	registry      *tool.Registry
	executor      *tool.Executor
	instructions  *instructions.CachedLoader
	skills        *skill.Manager
	resultFactory *tool.ResultFactory
	capture       func(context.Context, artifact.Metadata) (*tool.Capture, error)
}

type assemblyAdaptersRequest struct {
	paths       RuntimePaths
	hookHomeDir string
	lookupEnv   func(string) (string, bool)
	environment []string
}

type assemblyAdapters struct {
	provider      provider.Provider
	hooks         *hook.Engine
	hookResults   *hook.SyntheticResultAdapter
	mcp           *mcpclient.Manager
	conversations conversation.Store
	diagnostics   *diagnostics.Collector
}

// Runtime is the single object published by a successful Assembly.Build.  It
// retains the exact ownership registry sealed by the six-stage pipeline; all
// normal and rollback shutdown paths therefore converge on the same close
// table.  The UI graph remains candidate-only until the later atomic cutover,
// but no second registry is created here.
type Runtime struct {
	owners         *ownershipRegistry
	completed      [assemblyStageCount]bool
	cleanupTimeout time.Duration
	ui             *assemblyUI
	stdin          io.Reader
	stdout         io.Writer
	stderr         io.Writer
}

// assemblyRuntime is a compatibility alias for existing same-package stage
// tests.  Production callers use Runtime through Assembly.Build.
type assemblyRuntime = Runtime

// buildCandidate establishes the fixed composition order without publishing a
// production entry point. T4.25a-h populate the typed graph; T4.29a performs
// the single production cutover after rollback and shutdown are complete.
func (candidate assembly) buildCandidate(ctx context.Context) (*assemblyRuntime, error) {
	return candidate.buildCandidateWithOptions(ctx, nil)
}

func (candidate assembly) buildCandidateWithOptions(ctx context.Context, options *AssemblyOptions) (*assemblyRuntime, error) {
	if ctx == nil {
		return nil, errors.New("assembly context is unavailable")
	}
	builders := candidate.factories.ordered()
	for _, builder := range builders {
		if builder == nil {
			return nil, errors.New("assembly pipeline is incomplete")
		}
	}

	owners := newOwnershipRegistry()
	state := &assemblyBuildState{options: options}
	rollback := func(message string) (*assemblyRuntime, error) {
		rollbackErr := rollbackOwnershipRegistry(ctx, owners, assemblyRollbackTimeoutForState(state))
		reportAssemblyRollbackFailure(state, rollbackErr)
		return nil, errors.New(message)
	}
	stages := orderedAssemblyStages()
	var completed [assemblyStageCount]bool
	var scopes [assemblyStageCount]*assemblyOwnerScope
	for index, stage := range stages {
		select {
		case <-ctx.Done():
			return rollback("assembly build canceled")
		default:
		}
		before := owners.registrationCount()
		scope := newAssemblyOwnerScope(stage, owners)
		scopes[index] = scope
		buildErr := builders[index](ctx, state, scope.registrar())
		scope.finish()
		if buildErr != nil {
			return rollback("assembly " + stage.label() + " stage failed")
		}
		select {
		case <-ctx.Done():
			return rollback("assembly build canceled")
		default:
		}
		if anyAssemblyScopeViolated(scopes[:index+1]) {
			return rollback("assembly owner registration escaped its stage")
		}
		if owners.registrationCount() == before {
			return rollback("assembly " + stage.label() + " stage has no owner")
		}
		completed[index] = true
	}
	if err := owners.seal(); err != nil {
		return rollback("assembly ownership registry sealing failed")
	}
	if !validAssemblyUI(state.ui) {
		return rollback("assembly UI stage did not produce a candidate")
	}
	var stdin io.Reader
	var stdout, stderr io.Writer
	if options != nil {
		stdin, stdout, stderr = options.Stdin, options.Stdout, options.Stderr
	}
	return &assemblyRuntime{
		owners: owners, completed: completed, cleanupTimeout: assemblyRollbackTimeoutForState(state),
		ui: state.ui, stdin: stdin, stdout: stdout, stderr: stderr,
	}, nil
}

func assemblyRollbackTimeoutForState(state *assemblyBuildState) time.Duration {
	if state != nil && state.configuration != nil {
		if timeout := state.configuration.cleanupTimeout; timeout > 0 && timeout <= assemblyRollbackHardTimeout {
			return timeout
		}
	}
	return assemblyRollbackHardTimeout
}

func reportAssemblyRollbackFailure(state *assemblyBuildState, rollbackErr error) {
	if rollbackErr == nil || state == nil || state.configuration == nil || state.configuration.diagnostics == nil {
		return
	}
	code := "assembly_rollback_failed"
	safeErr := errors.New("assembly rollback failed")
	if errors.Is(rollbackErr, errAssemblyRollbackTimeout) {
		code = "assembly_rollback_timeout"
		safeErr = errAssemblyRollbackTimeout
	}
	state.configuration.diagnostics.Add(diagnostics.SanitizeInput{
		Code:     code,
		Source:   "assembly",
		Severity: diagnostics.SeverityError,
		Err:      safeErr,
	})
}

func newAssemblyConfigStage(request assemblyConfigRequest) assemblyStageBuilder {
	return func(_ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if state == nil || register == nil {
			return errors.New("assembly configuration stage is unavailable")
		}

		redactor := redact.NewRuntimeRedactor()

		layers := make([]config.ConfigLayer, 0, 3)
		for _, input := range []struct {
			source config.ConfigSource
			path   string
		}{
			{source: config.SourceUser, path: request.userPath},
			{source: config.SourceProject, path: request.projectPath},
		} {
			if input.path == "" {
				continue
			}
			partial, err := config.DecodePartial(input.path)
			if err != nil {
				return safeAssemblyConfigError("decode")
			}
			layers = append(layers, config.ConfigLayer{Source: input.source, Path: input.path, Value: partial})
		}
		layers = append(layers, config.ConfigLayer{Source: config.SourceRuntime, Value: request.runtime})

		merged, err := config.MergeLayers(layers...)
		if err != nil {
			return safeAssemblyConfigError("merge")
		}
		loaded, err := config.ResolveConfig(merged, config.LoadOptions{Redactor: redactor})
		if err != nil {
			return safeAssemblyConfigError("resolve")
		}
		cleanupTimeout := time.Duration(loaded.Config.Lifecycle.CleanupTimeoutMS) * time.Millisecond
		if cleanupTimeout < time.Millisecond || cleanupTimeout > 2*time.Second {
			return safeAssemblyConfigError("lifecycle")
		}
		sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
			Redactor:      redactor,
			MaxItems:      loaded.Config.Diagnostics.MaxItems,
			MaxItemBytes:  loaded.Config.Diagnostics.MaxItemBytes,
			MaxTotalBytes: loaded.Config.Diagnostics.MaxTotalBytes,
		})
		if err != nil {
			return safeAssemblyConfigError("diagnostics")
		}
		// Register Diagnostics before the redactor so reverse shutdown leaves
		// the bounded sink as the final process-level owner.
		if err := register(func(context.Context) error { return nil }); err != nil {
			return errors.New("assembly diagnostics ownership registration failed")
		}
		if err := register(func(context.Context) error { return nil }); err != nil {
			return errors.New("assembly configuration ownership registration failed")
		}

		state.configuration = &assemblyConfiguration{
			loaded:         loaded,
			redactor:       redactor,
			diagnostics:    sink,
			cleanupTimeout: cleanupTimeout,
		}
		return nil
	}
}

func safeAssemblyConfigError(step string) error {
	return errors.New("assembly configuration " + step + " failed")
}

func newAssemblySecurityStage(request assemblySecurityRequest) assemblyStageBuilder {
	return func(_ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if state == nil || state.configuration == nil || state.configuration.redactor == nil ||
			state.configuration.diagnostics == nil || register == nil {
			return errors.New("assembly security stage is unavailable")
		}
		paths := request.paths
		if !validAssemblyPath(paths.ProjectRoot) || !validAssemblyPath(paths.UserDataRoot) ||
			!validAssemblyPath(paths.UserCacheRoot) {
			return errors.New("assembly security roots are invalid")
		}

		project, err := safefs.Bootstrap(paths.ProjectRoot, tool.ProjectFilesystemPolicy())
		if err != nil || project.Root == nil {
			return safeAssemblySecurityError("project root")
		}
		if err := register(func(context.Context) error { return project.Root.Close() }); err != nil {
			_ = project.Root.Close()
			return errors.New("assembly project root ownership registration failed")
		}

		legacy, err := safefs.Bootstrap(paths.UserDataRoot, safefs.Policy{})
		if err != nil || legacy.Root == nil {
			return safeAssemblySecurityError("legacy root")
		}
		if legacy.Root.Identity() == project.Root.Identity() {
			_ = legacy.Root.Close()
			return safeAssemblySecurityError("root separation")
		}
		if err := register(func(context.Context) error { return legacy.Root.Close() }); err != nil {
			_ = legacy.Root.Close()
			return errors.New("assembly legacy root ownership registration failed")
		}

		resolved := state.configuration.loaded.Config
		artifactRoot, err := resolveArtifactRoot(paths, project.Root, resolved.Artifact.Root)
		if err != nil {
			return safeAssemblySecurityError("artifact root")
		}
		store, err := artifact.NewFileStore(artifact.FileStoreOptions{
			Root:          artifactRoot,
			WorkspaceRoot: paths.ProjectRoot,
			MaxFileBytes:  resolved.Artifact.MaxFileBytes,
			MaxTotalBytes: resolved.Artifact.MaxTotalBytes,
			Retention:     time.Duration(resolved.Artifact.RetentionDays) * 24 * time.Hour,
		})
		if err != nil {
			return safeAssemblySecurityError("artifact store")
		}
		if err := register(func(context.Context) error { return store.Close() }); err != nil {
			_ = store.Close()
			return errors.New("assembly artifact ownership registration failed")
		}

		networkPolicy := netpolicy.NewPolicy()
		if err := register(func(context.Context) error { return nil }); err != nil {
			return errors.New("assembly network policy ownership registration failed")
		}
		networkClients := netpolicy.NewClientFactory(networkPolicy)
		if networkClients == nil {
			return errors.New("assembly network client factory is unavailable")
		}
		if err := register(func(context.Context) error { return nil }); err != nil {
			return errors.New("assembly network client ownership registration failed")
		}

		processes, err := proctree.NewRunner(proctree.Options{
			CleanupTimeout: state.configuration.cleanupTimeout,
			Diagnostics:    state.configuration.diagnostics,
		})
		if err != nil || processes == nil {
			return safeAssemblySecurityError("process runner")
		}
		if err := register(func(context.Context) error { return nil }); err != nil {
			return errors.New("assembly process runner ownership registration failed")
		}

		protectionPlans, err := proctree.NewProtectionPlanFactory(
			[]*safefs.Root{project.Root},
			paths.UserCacheRoot,
		)
		if err != nil || protectionPlans == nil {
			return safeAssemblySecurityError("process protection")
		}
		if err := register(func(context.Context) error { return nil }); err != nil {
			return errors.New("assembly process protection ownership registration failed")
		}

		state.security = &assemblySecurity{
			roots:           assemblyRoots{project: project.Root, legacy: legacy.Root},
			capabilities:    assemblyRootCapabilities{project: project.Capabilities, legacy: legacy.Capabilities},
			artifacts:       store,
			networkPolicy:   networkPolicy,
			networkClients:  networkClients,
			trustedRoots:    cloneAssemblyTrustedRoots(request.trustedRoots),
			processes:       processes,
			protectionPlans: protectionPlans,
		}
		return nil
	}
}

func safeAssemblySecurityError(step string) error {
	return errors.New("assembly security " + step + " failed")
}

func newAssemblyExecutionStage(request assemblyExecutionRequest) assemblyStageBuilder {
	return func(_ context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if state == nil || state.configuration == nil || state.configuration.redactor == nil ||
			state.security == nil || state.security.roots.project == nil || state.security.artifacts == nil ||
			state.security.processes == nil || state.security.protectionPlans == nil || register == nil {
			return errors.New("assembly execution stage is unavailable")
		}
		paths := request.paths
		if !validAssemblyPath(paths.ProjectRoot) || !validAssemblyPath(paths.UserConfigRoot) {
			return errors.New("assembly execution roots are invalid")
		}

		resolved := state.configuration.loaded.Config
		redactor := state.configuration.redactor
		resultFactory, err := tool.NewResultFactory(redactor)
		if err != nil {
			return safeAssemblyExecutionError("result boundary")
		}

		var captureSpec budget.Spec
		for _, spec := range budget.AllSpecs() {
			if spec.Scope == budget.ToolCaptureBytes {
				captureSpec = spec
				break
			}
		}
		if captureSpec.Scope == "" || captureSpec.Dimension != budget.Bytes {
			return safeAssemblyExecutionError("capture budget")
		}
		captureBytes, err := captureSpec.Resolve(&resolved.Tool.CaptureBytes)
		if err != nil {
			return safeAssemblyExecutionError("capture budget")
		}
		effectiveCaptureLimits, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: captureBytes})
		if err != nil {
			return safeAssemblyExecutionError("capture budget")
		}
		hardCaptureLimits, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: captureSpec.HardCap})
		if err != nil {
			return safeAssemblyExecutionError("capture hard limit")
		}
		artifactStore := state.security.artifacts
		inlineOutputBytes := resolved.Tool.InlineOutputBytes
		capture := func(ctx context.Context, metadata artifact.Metadata) (*tool.Capture, error) {
			counter, counterErr := budget.NewCounter(effectiveCaptureLimits, hardCaptureLimits)
			if counterErr != nil {
				return nil, errors.New("assembly capture counter is unavailable")
			}
			return tool.NewCapture(ctx, tool.CaptureOptions{
				Store:       artifactStore,
				Counter:     counter,
				InlineBytes: inlineOutputBytes,
				Metadata:    metadata,
			})
		}

		authority, err := permission.NewTicketAuthority()
		if err != nil {
			return safeAssemblyExecutionError("ticket authority")
		}
		health := permission.NewHealth(authority)
		loadedRules := loadAssemblyPermissionRules(paths)
		health.Update(loadedRules.Errors)
		writer := permission.NewWriter(
			state.security.roots.project,
			state.security.capabilities.project.Protected(),
		)
		authorizer := &permission.Authorizer{
			Session: permission.NewSession(),
			User:    loadedRules.User,
			Project: loadedRules.Project,
			Local:   loadedRules.Local,
			Health:  health,
			Writer:  writer,
			Redact:  redactor.Text,
			Issuer:  authority,
		}

		registry := tool.NewSafeCandidateRegistry()
		readTool, err := tool.NewReadToolWithResultBoundary(paths.ProjectRoot, resultFactory, capture)
		if err != nil {
			return safeAssemblyExecutionError("Read tool")
		}
		writeTool, err := tool.NewWriteToolWithResultFactory(paths.ProjectRoot, resultFactory)
		if err != nil {
			return safeAssemblyExecutionError("Write tool")
		}
		editTool, err := tool.NewEditToolWithResultFactory(paths.ProjectRoot, resultFactory)
		if err != nil {
			return safeAssemblyExecutionError("Edit tool")
		}
		bashTool, err := tool.NewProtectedBashTool(paths.ProjectRoot, tool.BashRuntime{
			Runner:           state.security.processes,
			Plans:            state.security.protectionPlans,
			WorkingDirectory: state.security.roots.project,
			Capture:          capture,
			ResultFactory:    resultFactory,
		})
		if err != nil {
			return safeAssemblyExecutionError("Bash tool")
		}
		globTool, err := tool.NewGlobToolWithResultBoundary(paths.ProjectRoot, resultFactory, capture)
		if err != nil {
			return safeAssemblyExecutionError("Glob tool")
		}
		grepTool, err := tool.NewGrepToolWithResultBoundary(paths.ProjectRoot, resultFactory, capture)
		if err != nil {
			return safeAssemblyExecutionError("Grep tool")
		}
		loadSkillTool, err := tool.NewLoadSkillToolWithResultFactory(resultFactory)
		if err != nil {
			return safeAssemblyExecutionError("load-skill tool")
		}
		for _, candidate := range []struct {
			tool   tool.Tool
			policy tool.ExecutionPolicy
		}{
			{tool: readTool, policy: tool.ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}},
			{tool: writeTool},
			{tool: editTool},
			{tool: bashTool},
			{tool: globTool, policy: tool.ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}},
			{tool: grepTool, policy: tool.ExecutionPolicy{ReadOnly: true, ConcurrentSafe: true}},
			{tool: loadSkillTool},
		} {
			if err := registry.RegisterWithOptions(candidate.tool, tool.RegistrationOptions{Policy: candidate.policy}); err != nil {
				return safeAssemblyExecutionError("tool registry")
			}
		}

		executor, err := tool.NewExecutorWithWriteAccessAndResultFactory(
			registry,
			paths.ProjectRoot,
			time.Duration(resolved.Tool.TimeoutMS)*time.Millisecond,
			resolved.Tool.MaxOutputBytes,
			state.security.roots.project,
			state.security.capabilities.project.Ordinary(),
			resultFactory,
		)
		if err != nil {
			return safeAssemblyExecutionError("tool executor")
		}
		executor.ReadMaxBytes = resolved.Files.ReadMaxBytes
		executor.ReadMaxLines = resolved.Files.ScanMaxLines
		executor.ScanMaxBytes = resolved.Files.ScanMaxBytes
		executor.ScanMaxFiles = resolved.Files.ScanMaxFiles
		executor.ScanMaxDirs = resolved.Files.ScanMaxDirectories
		executor.ScanMaxLines = resolved.Files.ScanMaxLines
		executor.TicketVerifier = authority

		instructionLoader := &instructions.CachedLoader{Loader: instructions.Loader{
			ProjectRoot: paths.ProjectRoot,
			UserDir:     paths.UserConfigRoot,
			Config:      resolved.Instructions,
		}}
		skillManager, err := skill.NewManager(skill.ManagerOptions{
			Sources: []skill.SourceFS{
				skill.BuiltinSource(),
				{Source: skill.SourceUser, Root: filepath.Join(paths.UserConfigRoot, "skills")},
				{Source: skill.SourceProject, Root: filepath.Join(paths.ProjectRoot, ".xagent", "skills")},
			},
			ToolNames:       registry.Names(),
			ReservedCommand: reservedCommandNames(command.Builtins()),
			Limits:          skill.DefaultLimits(),
			Redact:          redactor.Text,
		})
		if err != nil {
			return safeAssemblyExecutionError("Skill service")
		}

		if err := register(func(context.Context) error { return nil }); err != nil {
			return errors.New("assembly execution ownership registration failed")
		}
		state.execution = &assemblyExecution{
			permissions: assemblyPermissions{
				health: health, writer: writer, authority: authority, authorizer: authorizer,
			},
			registry:      registry,
			executor:      executor,
			instructions:  instructionLoader,
			skills:        skillManager,
			resultFactory: resultFactory,
			capture:       capture,
		}
		return nil
	}
}

func safeAssemblyExecutionError(step string) error {
	return errors.New("assembly execution " + step + " failed")
}

func loadAssemblyPermissionRules(paths RuntimePaths) permission.LoadedRules {
	loaded := permission.LoadedRules{
		User: permission.RuleLayer{Source: permission.Source{
			Kind: permission.SourceUserRule, Description: "user permissions",
		}},
		Project: permission.RuleLayer{Source: permission.Source{
			Kind: permission.SourceProjectRule, Description: "project permissions",
		}},
		Local: permission.RuleLayer{Source: permission.Source{
			Kind: permission.SourceLocalRule, Description: "local permissions",
		}},
	}
	for _, input := range []struct {
		path   string
		layer  *permission.RuleLayer
		source permission.Source
	}{
		{filepath.Join(paths.UserConfigRoot, "permissions.yaml"), &loaded.User, loaded.User.Source},
		{permission.ProjectRulePath(paths.ProjectRoot), &loaded.Project, loaded.Project.Source},
		{permission.LocalRulePath(paths.ProjectRoot), &loaded.Local, loaded.Local.Source},
	} {
		file, err := permission.LoadRuleFile(input.path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				loaded.Errors = append(loaded.Errors, permission.LoadError{Source: input.source, Err: err})
			}
			continue
		}
		input.layer.Rules = append([]permission.Rule(nil), file.Rules...)
	}
	return loaded
}

func newAssemblyAdaptersStage(request assemblyAdaptersRequest) assemblyStageBuilder {
	return func(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if ctx == nil || state == nil || state.configuration == nil || state.configuration.redactor == nil ||
			state.configuration.diagnostics == nil || state.security == nil || state.security.roots.project == nil ||
			state.security.roots.legacy == nil || state.security.artifacts == nil || state.security.networkPolicy == nil ||
			state.security.networkClients == nil || state.security.processes == nil || state.security.protectionPlans == nil ||
			state.execution == nil || state.execution.registry == nil || state.execution.resultFactory == nil ||
			state.execution.capture == nil || register == nil {
			return errors.New("assembly adapters stage is unavailable")
		}
		paths := request.paths
		if !validAssemblyPath(paths.ProjectRoot) || !validAssemblyPath(paths.UserConfigRoot) ||
			!validAssemblyPath(paths.UserDataRoot) || !validAssemblyPath(request.hookHomeDir) || request.lookupEnv == nil {
			return errors.New("assembly adapter roots are invalid")
		}

		resolved := state.configuration.loaded.Config
		redactor := state.configuration.redactor
		cleanupTimeout := state.configuration.cleanupTimeout
		diagnosticCollector := diagnostics.NewCollector(diagnostics.CollectorOptions{
			Limit:    int(resolved.Diagnostics.MaxItems),
			MaxBytes: int(resolved.Diagnostics.MaxItemBytes),
			Redactor: redactor.Text,
		})

		hookLimits := hook.DefaultLimits()
		hookSnapshot, err := hook.Load(hook.LoadOptions{
			HomeDir:     request.hookHomeDir,
			ProjectRoot: paths.ProjectRoot,
			LookupEnv:   request.lookupEnv,
			Redactor:    redactor,
			Limits:      hookLimits,
		})
		if err != nil {
			return safeAssemblyAdaptersError("hook configuration")
		}
		hookResults, err := hook.NewSyntheticResultAdapter(state.execution.resultFactory)
		if err != nil {
			return safeAssemblyAdaptersError("hook result adapter")
		}

		legacySink := conversation.LegacyArtifactSink(state.security.artifacts)
		legacyImporter, err := conversation.NewLegacyArtifactImporter(
			state.security.roots.legacy,
			paths.UserDataRoot,
			legacySink,
		)
		if err != nil {
			return safeAssemblyAdaptersError("conversation migration")
		}
		conversationRoot := resolved.Session.Dir
		if !filepath.IsAbs(conversationRoot) {
			conversationRoot = filepath.Join(paths.ProjectRoot, conversationRoot)
		}
		conversationRoot = filepath.Clean(conversationRoot)
		if !validAssemblyPath(conversationRoot) {
			return safeAssemblyAdaptersError("conversation root")
		}
		conversationStore, err := conversation.NewJSONLStore(conversation.JSONLStoreOptions{
			DataDir:                conversationRoot,
			Redactor:               redactor,
			MaxRecordBytes:         resolved.Session.MaxRecordBytes,
			MaxSessionBytes:        resolved.Session.MaxSessionBytes,
			MaxScanFiles:           int64(resolved.Session.MaxScanFiles),
			MaxScanBytes:           resolved.Session.MaxScanBytes,
			RetentionDays:          int64(resolved.Session.RetentionDays),
			GapReminderDays:        int64(resolved.Session.GapReminderDays),
			LegacyArtifactImporter: legacyImporter,
		})
		if err != nil {
			return safeAssemblyAdaptersError("conversation store")
		}

		providerEndpoint, err := state.security.networkPolicy.ValidateInitial(ctx, resolved.LLM.BaseURL, netpolicy.PurposeProvider)
		if err != nil {
			return safeAssemblyAdaptersError("provider endpoint")
		}
		providerClient, err := state.security.networkClients.New(providerEndpoint, netpolicy.ClientOptions{
			Timeout:      time.Duration(resolved.LLM.RequestTimeoutMS) * time.Millisecond,
			TrustedRoots: state.security.trustedRoots,
		})
		if err != nil || providerClient == nil {
			return safeAssemblyAdaptersError("provider client")
		}
		closeProviderClient := func(context.Context) error {
			providerClient.CloseIdleConnections()
			return nil
		}
		if err := register(closeProviderClient); err != nil {
			_ = closeProviderClient(context.Background())
			return errors.New("assembly provider client ownership registration failed")
		}
		providerService, err := provider.NewWithOptions(resolved.LLM, provider.ProviderOptions{
			Endpoint:        providerEndpoint,
			Client:          providerClient,
			RuntimeRedactor: redactor,
			CleanupTimeout:  cleanupTimeout,
			Diagnostics:     state.configuration.diagnostics,
		})
		if err != nil || providerService == nil {
			return safeAssemblyAdaptersError("provider service")
		}

		hookHTTP := &hook.DefaultHTTPRunner{
			Limits:        hookLimits,
			Policy:        state.security.networkPolicy,
			ClientFactory: state.security.networkClients,
			ClientOptions: netpolicy.ClientOptions{TrustedRoots: state.security.trustedRoots},
			Redactor:      redactor,
		}
		closeHookHTTP := func(context.Context) error {
			hookHTTP.CloseIdleConnections()
			return nil
		}
		if err := register(closeHookHTTP); err != nil {
			_ = closeHookHTTP(context.Background())
			return errors.New("assembly hook HTTP ownership registration failed")
		}
		hookCommand := &hook.ShellCommandRunner{
			Limits:           hookLimits,
			Redactor:         redactor,
			Runner:           state.security.processes,
			Plans:            state.security.protectionPlans,
			WorkingDirectory: state.security.roots.project,
			ProjectRoot:      paths.ProjectRoot,
			JoinGrace:        cleanupTimeout,
		}
		hookEngine, err := hook.NewEngine(hookSnapshot, hook.EngineOptions{
			ProjectRoot:       paths.ProjectRoot,
			Diagnostics:       state.configuration.diagnostics,
			LegacyDiagnostics: diagnosticCollector,
			Redactor:          redactor,
			CommandRunner:     hookCommand,
			HTTPRunner:        hookHTTP,
			Limits:            hookLimits,
			AsyncWorkers:      4,
			AsyncQueue:        64,
			CleanupTimeout:    cleanupTimeout,
			ShutdownGrace:     cleanupTimeout,
			ShutdownJoinGrace: cleanupTimeout,
		})
		if err != nil || hookEngine == nil {
			return safeAssemblyAdaptersError("hook engine")
		}
		closeHookEngine := func(cleanupCtx context.Context) error { return hookEngine.Shutdown(cleanupCtx) }
		if err := register(closeHookEngine); err != nil {
			_ = closeHookEngine(context.Background())
			return errors.New("assembly hook ownership registration failed")
		}

		mcpManager, err := mcpclient.NewManager(resolved.MCP, mcpclient.ManagerOptions{
			MaxTools:          int(resolved.MCP.MaxTools),
			MaxPages:          int(resolved.MCP.MaxPages),
			MaxResponseBytes:  resolved.MCP.MaxResponseBytes,
			MaxProtocolErrors: resolved.MCP.MaxProtocolErrors,
			DefaultTimeout:    time.Duration(resolved.MCP.DefaultTimeoutMS) * time.Millisecond,
			CleanupTimeout:    cleanupTimeout,
			Diagnostics:       state.configuration.diagnostics,
		}, mcpclient.ManagerDependencies{
			Diagnostics:       state.configuration.diagnostics,
			RuntimeRedactor:   redactor,
			ResultFactory:     state.execution.resultFactory,
			Capture:           state.execution.capture,
			HTTPPolicy:        state.security.networkPolicy,
			HTTPClientFactory: state.security.networkClients,
			HTTPClientOptions: netpolicy.ClientOptions{TrustedRoots: state.security.trustedRoots},
			StdioRunner:       state.security.processes,
			StdioPlanFactory:  state.security.protectionPlans,
			StdioRoot:         state.security.roots.project,
			Environment:       append([]string(nil), request.environment...),
		})
		if err != nil || mcpManager == nil {
			return safeAssemblyAdaptersError("MCP manager")
		}
		closeMCP := func(cleanupCtx context.Context) error { return mcpManager.Close(cleanupCtx) }
		if err := register(closeMCP); err != nil {
			_ = closeMCP(context.Background())
			return errors.New("assembly MCP ownership registration failed")
		}
		if err := mcpManager.Start(ctx); err != nil {
			return safeAssemblyAdaptersError("MCP start")
		}
		for _, remoteTool := range mcpManager.Tools() {
			registration, ok := remoteTool.(interface {
				RegistrationOptions() tool.RegistrationOptions
			})
			if !ok {
				return safeAssemblyAdaptersError("MCP tool registration")
			}
			if err := state.execution.registry.RegisterWithOptions(remoteTool, registration.RegistrationOptions()); err != nil {
				return safeAssemblyAdaptersError("MCP tool registration")
			}
		}

		state.adapters = &assemblyAdapters{
			provider:      providerService,
			hooks:         hookEngine,
			hookResults:   hookResults,
			mcp:           mcpManager,
			conversations: conversationStore,
			diagnostics:   diagnosticCollector,
		}
		return nil
	}
}

func safeAssemblyAdaptersError(step string) error {
	return errors.New("assembly adapters " + step + " failed")
}

// newAssemblyOrchestrationStage closes the candidate's Context/Session/
// Memory graph and constructs exactly one Orchestrator over those narrow
// services. It deliberately keeps the resolved configuration local to this
// constructor; no configuration or composition-root capability is retained
// by assemblyOrchestration or forwarded as an Orchestrator dependency.
func newAssemblyOrchestrationStage(request assemblyExecutionRequest) assemblyStageBuilder {
	return func(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if ctx == nil || state == nil || state.configuration == nil || state.configuration.redactor == nil ||
			state.adapters == nil || state.adapters.provider == nil || state.adapters.hooks == nil ||
			state.adapters.conversations == nil || state.adapters.diagnostics == nil || state.execution == nil ||
			state.execution.registry == nil || state.execution.executor == nil || state.execution.instructions == nil ||
			state.execution.skills == nil || state.execution.resultFactory == nil || register == nil {
			return errors.New("assembly orchestration stage is unavailable")
		}
		if !validAssemblyOrchestrationPermissionBoundary(state.execution) {
			return errors.New("assembly orchestration permission boundary is invalid")
		}
		if !validAssemblyPath(request.paths.ProjectRoot) || !validAssemblyPath(request.paths.UserDataRoot) {
			return errors.New("assembly orchestration roots are invalid")
		}
		resolved := state.configuration.loaded.Config
		contextServices, err := newAssemblyContextServices(assemblyContextRequest{
			paths:         request.paths,
			configuration: state.configuration,
			execution:     state.execution,
			adapters:      state.adapters,
		})
		if err != nil || contextServices == nil {
			return safeAssemblyOrchestrationError("context services")
		}

		commandRegistry, err := command.New(command.Builtins()...)
		if err != nil || commandRegistry == nil {
			return safeAssemblyOrchestrationError("command registry")
		}

		orchestration := &assemblyOrchestration{
			contextManager:     contextServices.contextManager,
			sessionContext:     contextServices.sessionContext,
			memory:             contextServices.memory,
			resources:          contextServices.resources,
			requestBudgeter:    contextServices.requestBudgeter,
			skillHistoryPolicy: contextServices.skillHistoryPolicy,
			commandRegistry:    commandRegistry,
		}
		orchestration.orchestrator = orchestrator.NewWithOptions(orchestrator.OrchestratorOptions{
			Provider:             state.adapters.provider,
			Store:                state.adapters.conversations,
			Resources:            contextServices.resources,
			Thinking:             resolved.LLM.Thinking,
			Registry:             state.execution.registry,
			Executor:             state.execution.executor,
			CleanupTimeout:       state.configuration.cleanupTimeout,
			LifecycleDiagnostics: state.configuration.diagnostics,
			Authorizer:           state.execution.permissions.authorizer,
			ContextManager:       contextServices.contextManager,
			ResultFactory:        state.execution.resultFactory,
			SkillHistoryPolicy:   contextServices.skillHistoryPolicy,
			RequestBudgeter:      contextServices.requestBudgeter,
			MaxRecordBytes:       resolved.Session.MaxRecordBytes,
			MaxSessionBytes:      resolved.Session.MaxSessionBytes,
			SessionContext:       contextServices.sessionContext,
			Memory:               contextServices.memory,
			Diagnostics:          state.adapters.diagnostics,
			Agent:                resolved.Agent,
			SkillManager:         state.execution.skills,
			DefaultModel:         resolved.LLM.Model,
			RuntimeRedactor:      state.configuration.redactor,
			Redact:               state.configuration.redactor.Text,
			RedactionLookbehind:  state.configuration.redactor.MaxSecretBytes(),
			Hooks:                state.adapters.hooks,
		})
		if orchestration.orchestrator == nil {
			return safeAssemblyOrchestrationError("orchestrator")
		}
		if err := register(func(cleanupCtx context.Context) error {
			waitCtx, cancel := context.WithTimeout(cleanupCtx, state.configuration.cleanupTimeout)
			defer cancel()
			return orchestration.orchestrator.WaitIdle(waitCtx)
		}); err != nil {
			return safeAssemblyOrchestrationError("ownership registration")
		}
		state.context = contextServices
		state.orchestration = orchestration
		return nil
	}
}

func validAssemblyOrchestrationPermissionBoundary(execution *assemblyExecution) bool {
	if execution == nil || execution.executor == nil {
		return false
	}
	permissions := execution.permissions
	if permissions.authority == nil || permissions.health == nil || permissions.authorizer == nil ||
		permissions.authorizer.Health != permissions.health {
		return false
	}
	issuer, issuerOK := permissions.authorizer.Issuer.(*permission.TicketAuthority)
	verifier, verifierOK := execution.executor.TicketVerifier.(*permission.TicketAuthority)
	return issuerOK && verifierOK && issuer == permissions.authority && verifier == permissions.authority
}

func safeAssemblyOrchestrationError(step string) error {
	return errors.New("assembly orchestration " + step + " failed")
}

// newAssemblyUIStage constructs the App only after all service owners are
// available. The model is published through Runtime only after the complete
// six-stage build seals its ownership registry.
func newAssemblyUIStage() assemblyStageBuilder {
	return func(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
		if ctx == nil || state == nil || state.adapters == nil || state.adapters.conversations == nil ||
			state.adapters.hooks == nil || state.security == nil || state.security.artifacts == nil ||
			state.orchestration == nil || state.orchestration.orchestrator == nil || register == nil {
			return errors.New("assembly UI stage is unavailable")
		}
		select {
		case <-ctx.Done():
			return errors.New("assembly UI build canceled")
		default:
		}

		services := app.AppServices{
			Conversations: assemblyConversationAccess{service: state.adapters.conversations},
			Orchestrator:  assemblyOrchestrationAccess{service: state.orchestration.orchestrator},
			Artifacts:     assemblyArtifactUserReader{service: state.security.artifacts},
			Hooks:         assemblyHookLifecycle{service: state.adapters.hooks},
		}
		if !validAssemblyAppServices(services) {
			return errors.New("assembly UI App boundary is invalid")
		}
		// Package-local stage tests exercise the pre-publication candidate with
		// a nil options snapshot. Production Assembly.Build always supplies a
		// validated options value and takes the atomic App publication branch
		// below; retaining this inert branch keeps those tests side-effect free.
		var ui *assemblyUI
		var closeApp ownerClose
		viewModel := tui.NewStateViewModel(tui.ViewModelSpec{Screen: tui.ScreenList})
		if state.options == nil {
			ui = &assemblyUI{
				appServices: services,
				viewModel:   viewModel,
				intentSink:  assemblyTUIIntentSink(unpublishedIntentSink{}),
			}
			closeApp = func(context.Context) error { return nil }
		} else {
			runtimeOptions := app.RuntimeOptions{}
			runtimeOptions = app.RuntimeOptions{
				CleanupTimeout: state.configuration.cleanupTimeout,
				Diagnostics:    state.configuration.diagnostics,
			}
			deps := app.Deps{
				RuntimeOptions:       runtimeOptions,
				Config:               &state.configuration.loaded.Config,
				Provider:             state.adapters.provider,
				Store:                state.adapters.conversations,
				Artifacts:            assemblyArtifactUserReader{service: state.security.artifacts},
				Resources:            state.orchestration.resources,
				Registry:             state.execution.registry,
				Executor:             state.execution.executor,
				ContextManager:       state.orchestration.contextManager,
				SkillHistoryPolicy:   state.orchestration.skillHistoryPolicy,
				RequestBudgeter:      state.orchestration.requestBudgeter,
				SessionContext:       state.orchestration.sessionContext,
				Memory:               state.orchestration.memory,
				Diagnostics:          state.adapters.diagnostics,
				SkillManager:         state.execution.skills,
				RuntimeRedactor:      state.configuration.redactor,
				Redact:               state.configuration.redactor.Text,
				RedactionLookbehind:  state.configuration.redactor.MaxSecretBytes(),
				Hooks:                state.adapters.hooks,
				MCPStatus:            state.adapters.mcp,
				CommandRegistry:      state.orchestration.commandRegistry,
				ExistingOrchestrator: state.orchestration.orchestrator,
			}
			model := app.NewWithOptions(deps, runtimeOptions)
			ui = &assemblyUI{
				appServices: services, runtimeOptions: runtimeOptions,
				viewModel: viewModel, intentSink: assemblyTUIIntentSink(unpublishedIntentSink{}), model: &model,
			}
			closeApp = func(cleanupCtx context.Context) error { return model.Close(cleanupCtx) }
		}

		select {
		case <-ctx.Done():
			return errors.New("assembly UI build canceled")
		default:
		}
		if err := register(closeApp); err != nil {
			return errors.New("assembly UI ownership registration failed")
		}
		state.ui = ui
		return nil
	}
}

func validAssemblyAppServices(services app.AppServices) bool {
	conversations, conversationsOK := services.Conversations.(assemblyConversationAccess)
	orchestration, orchestrationOK := services.Orchestrator.(assemblyOrchestrationAccess)
	artifacts, artifactsOK := services.Artifacts.(assemblyArtifactUserReader)
	hooks, hooksOK := services.Hooks.(assemblyHookLifecycle)
	conversationStore, conversationStoreOK := conversations.service.(conversation.Store)
	orchestratorService, orchestratorServiceOK := orchestration.service.(*orchestrator.Orchestrator)
	artifactStore, artifactStoreOK := artifacts.service.(artifact.Store)
	hookService, hookServiceOK := hooks.service.(*hook.Engine)
	return conversationsOK && conversationStoreOK && !nilAssemblyInterface(conversationStore) &&
		orchestrationOK && orchestratorServiceOK && orchestratorService != nil &&
		artifactsOK && artifactStoreOK && !nilAssemblyInterface(artifactStore) &&
		hooksOK && hookServiceOK && hookService != nil
}

func validAssemblyUI(ui *assemblyUI) bool {
	if ui == nil || !validAssemblyAppServices(ui.appServices) || nilAssemblyInterface(ui.intentSink) {
		return false
	}
	screen := ui.viewModel.Screen()
	return screen == tui.ScreenList || screen == tui.ScreenChat
}

// Interfaces in the Assembly graph are normally concrete pointers.  Keep the
// nil check typed-nil safe so a failed/partially initialized owner can never be
// smuggled into the candidate through an interface value.
func nilAssemblyInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type RuntimePaths struct {
	ProjectRoot    string
	UserConfigRoot string
	UserDataRoot   string
	UserCacheRoot  string
}

// ConfigInputs is the closed set of configuration layers accepted by the
// production Assembly. Empty file paths mean that the corresponding optional
// layer is absent; a non-empty path must be absolute and canonical.
type ConfigInputs struct {
	UserPath    string
	ProjectPath string
	Runtime     config.PartialAppConfig
}

// AssemblyOptions is intentionally value-only apart from its narrow I/O,
// environment and certificate-pool inputs. TrustedRoots is the sole network
// customization point: callers cannot inject an HTTP client, transport,
// proxy, dialer, redirect policy, cookie jar or protocol handler.
type AssemblyOptions struct {
	Paths        RuntimePaths
	Config       ConfigInputs
	LookupEnv    func(string) (string, bool)
	TrustedRoots *x509.CertPool
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
}

// defaultAssembly fixes all six production stage constructors. The private
// factory table remains replaceable only by same-package rollback tests; CLI,
// configuration and E2E inputs cannot reach it.
func defaultAssembly() Assembly {
	return Assembly{factories: assemblyFactories{
		config:        defaultAssemblyConfigStage,
		security:      defaultAssemblySecurityStage,
		execution:     defaultAssemblyExecutionStage,
		adapters:      defaultAssemblyAdaptersStage,
		orchestration: defaultAssemblyOrchestrationStage,
		ui:            defaultAssemblyUIStage,
	}}
}

// Build validates the complete input contract before constructing an owner.
// The options snapshot, including a cloned certificate pool, is private to
// this build and cannot be changed by the caller after publication.
func (candidate Assembly) Build(ctx context.Context, options AssemblyOptions) (*Runtime, error) {
	if ctx == nil {
		return nil, errors.New("assembly context is unavailable")
	}
	if err := validateAssemblyOptions(options); err != nil {
		return nil, err
	}
	normalized := options
	normalized.TrustedRoots = cloneAssemblyTrustedRoots(options.TrustedRoots)
	runtime, err := candidate.buildCandidateWithOptions(ctx, &normalized)
	if err != nil {
		return nil, err
	}
	return runtime, nil
}

func validateAssemblyOptions(options AssemblyOptions) error {
	paths := options.Paths
	if !validAssemblyPath(paths.ProjectRoot) || !validAssemblyPath(paths.UserConfigRoot) ||
		!validAssemblyPath(paths.UserDataRoot) || !validAssemblyPath(paths.UserCacheRoot) {
		return errors.New("assembly runtime roots are invalid")
	}
	if !validOptionalAssemblyPath(options.Config.UserPath) || !validOptionalAssemblyPath(options.Config.ProjectPath) {
		return errors.New("assembly configuration inputs are invalid")
	}
	if options.LookupEnv == nil || nilAssemblyInterface(options.Stdin) ||
		nilAssemblyInterface(options.Stdout) || nilAssemblyInterface(options.Stderr) {
		return errors.New("assembly environment or I/O is unavailable")
	}
	return nil
}

func validOptionalAssemblyPath(path string) bool {
	return path == "" || validAssemblyPath(path)
}

func defaultAssemblyOptions(state *assemblyBuildState) (*AssemblyOptions, error) {
	if state == nil || state.options == nil {
		return nil, errors.New("assembly options are unavailable")
	}
	return state.options, nil
}

func defaultAssemblyConfigStage(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
	options, err := defaultAssemblyOptions(state)
	if err != nil {
		return err
	}
	return newAssemblyConfigStage(assemblyConfigRequest{
		userPath:    options.Config.UserPath,
		projectPath: options.Config.ProjectPath,
		runtime:     options.Config.Runtime,
	})(ctx, state, register)
}

func defaultAssemblySecurityStage(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
	options, err := defaultAssemblyOptions(state)
	if err != nil {
		return err
	}
	return newAssemblySecurityStage(assemblySecurityRequest{
		paths:        options.Paths,
		trustedRoots: options.TrustedRoots,
	})(ctx, state, register)
}

func defaultAssemblyExecutionStage(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
	options, err := defaultAssemblyOptions(state)
	if err != nil {
		return err
	}
	return newAssemblyExecutionStage(assemblyExecutionRequest{paths: options.Paths})(ctx, state, register)
}

func defaultAssemblyAdaptersStage(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
	options, err := defaultAssemblyOptions(state)
	if err != nil {
		return err
	}
	return newAssemblyAdaptersStage(assemblyAdaptersRequest{
		paths:       options.Paths,
		hookHomeDir: assemblyHookHomeDir(options.Paths.UserConfigRoot),
		lookupEnv:   options.LookupEnv,
		environment: os.Environ(),
	})(ctx, state, register)
}

func defaultAssemblyOrchestrationStage(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
	options, err := defaultAssemblyOptions(state)
	if err != nil {
		return err
	}
	return newAssemblyOrchestrationStage(assemblyExecutionRequest{paths: options.Paths})(ctx, state, register)
}

func defaultAssemblyUIStage(ctx context.Context, state *assemblyBuildState, register assemblyOwnerRegistrar) error {
	if _, err := defaultAssemblyOptions(state); err != nil {
		return err
	}
	return newAssemblyUIStage()(ctx, state, register)
}

func assemblyHookHomeDir(userConfigRoot string) string {
	root := filepath.Clean(userConfigRoot)
	parent := filepath.Dir(root)
	if filepath.Base(root) == "xagent" && filepath.Base(parent) == ".config" {
		return filepath.Dir(parent)
	}
	return parent
}

func cloneAssemblyTrustedRoots(roots *x509.CertPool) *x509.CertPool {
	if roots == nil {
		return nil
	}
	return roots.Clone()
}

func resolveArtifactRoot(paths RuntimePaths, projectRoot *safefs.Root, configuredRoot string) (string, error) {
	if !validAssemblyPath(paths.ProjectRoot) || projectRoot == nil {
		return "", errors.New("artifact project root is unavailable")
	}
	identity, err := boundProjectIdentity(paths.ProjectRoot, projectRoot)
	if err != nil {
		return "", errors.New("artifact project root is unavailable")
	}

	root := configuredRoot
	if root == "" {
		if !validAssemblyPath(paths.UserCacheRoot) {
			return "", errors.New("artifact cache root is unavailable")
		}
		digest := sha256.Sum256(identity)
		projectID := hex.EncodeToString(digest[:])
		root = filepath.Join(paths.UserCacheRoot, "xagent", "artifacts", projectID)
	}
	if !validAssemblyPath(root) {
		return "", errors.New("artifact root is invalid")
	}
	prepared, err := artifact.PrepareFileStoreRoot(root, paths.ProjectRoot)
	if err != nil {
		return "", errors.New("artifact root is unavailable")
	}
	return prepared, nil
}

func boundProjectIdentity(projectPath string, projectRoot *safefs.Root) ([]byte, error) {
	rootIdentity, err := projectRoot.Identity().MarshalBinary()
	if err != nil {
		return nil, err
	}
	opened, err := safefs.Bootstrap(projectPath, safefs.Policy{})
	if err != nil || opened.Root == nil {
		return nil, errors.New("artifact project path is unavailable")
	}
	defer opened.Root.Close()
	pathIdentity, err := opened.Root.Identity().MarshalBinary()
	if err != nil || !bytes.Equal(rootIdentity, pathIdentity) {
		return nil, errors.New("artifact project identity changed")
	}
	return rootIdentity, nil
}

func validAssemblyPath(path string) bool {
	return path != "" && utf8.ValidString(path) && !strings.ContainsRune(path, 0) &&
		filepath.IsAbs(path) && filepath.Clean(path) == path
}
