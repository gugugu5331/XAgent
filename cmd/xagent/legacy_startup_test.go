package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/app"
	"xagent/internal/command"
	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
	"xagent/internal/instructions"
	"xagent/internal/mcpclient"
	"xagent/internal/memory"
	"xagent/internal/netpolicy"
	"xagent/internal/orchestrator"
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

type legacyAppCloser interface {
	Close(context.Context) error
}

type legacyHookShutdowner interface {
	Shutdown(context.Context) error
}

type legacyHookShutdownNoticeSource interface {
	ShutdownNotices() []diagnostics.Diagnostic
}

type legacyMCPRuntime interface {
	Start(context.Context) error
	Tools() []tool.Tool
	StatusLine() string
	Summary() mcpclient.StatusSummary
	Diagnostics() []mcpclient.Diagnostic
	Close(context.Context) error
}

type legacyStartupFactories struct {
	loadConfig      func(string, config.LoadOptions) (*config.AppConfig, error)
	getwd           func() (string, error)
	userHomeDir     func() (string, error)
	lookupEnv       func(string) (string, bool)
	loadHooks       func(hook.LoadOptions) (hook.Snapshot, error)
	buildRuntime    func(hook.Snapshot, hook.EngineOptions) (hook.Runtime, error)
	newStore        func(conversation.JSONLStoreOptions) (conversation.Store, error)
	newProvider     func(config.LLMConfig, provider.ProviderOptions) (provider.Provider, error)
	newRegistry     func(string) (*tool.Registry, error)
	newMCP          func(config.MCPConfig, mcpclient.ManagerOptions, mcpclient.ManagerDependencies) (legacyMCPRuntime, error)
	newSkillManager func(string, string, *tool.Registry, func(string) string) (*skill.Manager, error)
	runTUI          func(tea.Model) (tea.Model, error)
	stderr          io.Writer
}

func legacyStartupFactoriesDefault() legacyStartupFactories {
	return legacyStartupFactories{
		loadConfig:  config.LoadWithOptions,
		getwd:       os.Getwd,
		userHomeDir: os.UserHomeDir,
		lookupEnv:   os.LookupEnv,
		loadHooks:   hook.Load,
		buildRuntime: func(snapshot hook.Snapshot, options hook.EngineOptions) (hook.Runtime, error) {
			return hook.NewEngine(snapshot, options)
		},
		newStore: func(options conversation.JSONLStoreOptions) (conversation.Store, error) {
			return conversation.NewJSONLStore(options)
		},
		newProvider: provider.NewWithOptions,
		newRegistry: tool.NewRegistry,
		newMCP: func(cfg config.MCPConfig, options mcpclient.ManagerOptions, dependencies mcpclient.ManagerDependencies) (legacyMCPRuntime, error) {
			return mcpclient.NewManager(cfg, options, dependencies)
		},
		newSkillManager: newSkillManager,
		runTUI:          tui.Run,
		stderr:          os.Stderr,
	}
}

func legacyRunWithFactories(args []string, factories legacyStartupFactories) (runErr error) {
	configPath, err := parseConfigPath(args)
	if err != nil {
		return err
	}
	if factories.stderr == nil {
		factories.stderr = io.Discard
	}
	redactor := redact.NewRuntimeRedactor()
	diagnosticCollector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redactor.Text})
	coordinator := newLegacyCompositeCloser(diagnosticCollector, redactor.Text, factories.stderr)
	defer func() {
		if closeErr := coordinator.Close(); closeErr != nil {
			runErr = errors.Join(runErr, closeErr)
		}
	}()

	cfg, err := factories.loadConfig(configPath, config.LoadOptions{Redactor: redactor})
	if err != nil {
		return fmt.Errorf("配置错误: %w", err)
	}
	boundedDiagnostics, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      cfg.Diagnostics.MaxItems,
		MaxItemBytes:  cfg.Diagnostics.MaxItemBytes,
		MaxTotalBytes: cfg.Diagnostics.MaxTotalBytes,
	})
	if err != nil {
		return fmt.Errorf("诊断边界错误: %w", err)
	}
	cleanupTimeout := time.Duration(cfg.Lifecycle.CleanupTimeoutMS) * time.Millisecond
	projectRoot, err := factories.getwd()
	if err != nil {
		return fmt.Errorf("项目目录错误: %w", err)
	}
	projectRoot, err = filepath.Abs(projectRoot)
	if err != nil {
		return fmt.Errorf("项目目录错误: %w", err)
	}
	homeDir, err := factories.userHomeDir()
	if err != nil {
		return fmt.Errorf("用户目录错误: %w", err)
	}
	homeDir, err = filepath.Abs(homeDir)
	if err != nil {
		return fmt.Errorf("用户目录错误: %w", err)
	}

	// Hook configuration is deliberately the first feature-specific input
	// loaded after the main config and path/redaction setup. A bad project Hook
	// therefore cannot start MCP, spawn Hook workers, or construct providers.
	snapshot, err := factories.loadHooks(hook.LoadOptions{
		HomeDir:     homeDir,
		ProjectRoot: projectRoot,
		LookupEnv:   factories.lookupEnv,
		Redactor:    redactor,
		Limits:      hook.DefaultLimits(),
	})
	if err != nil {
		return fmt.Errorf("Hook 配置错误: %w", err)
	}
	hooks, err := factories.buildRuntime(snapshot, hook.EngineOptions{
		ProjectRoot:       projectRoot,
		Diagnostics:       boundedDiagnostics,
		LegacyDiagnostics: diagnosticCollector,
		Redactor:          redactor,
		Limits:            hook.DefaultLimits(),
		AsyncWorkers:      4,
		AsyncQueue:        64,
		CleanupTimeout:    cleanupTimeout,
		ShutdownGrace:     cleanupTimeout,
	})
	if hooks != nil {
		coordinator.SetHook(hooks)
	}
	if err != nil {
		return fmt.Errorf("Hook Runtime 错误: %w", err)
	}
	if hooks == nil {
		return fmt.Errorf("Hook Runtime 错误: 构造结果为空")
	}

	store, err := factories.newStore(conversationStoreOptions(cfg.Session, projectRoot, redactor))
	if err != nil {
		return fmt.Errorf("会话存储错误: %w", err)
	}
	networkPolicy := netpolicy.NewPolicy()
	networkClients := netpolicy.NewClientFactory(networkPolicy)
	providerEndpoint, err := networkPolicy.ValidateInitial(context.Background(), cfg.LLM.BaseURL, netpolicy.PurposeProvider)
	if err != nil {
		return fmt.Errorf("Provider 端点错误: %w", err)
	}
	providerClient, err := networkClients.New(providerEndpoint, netpolicy.ClientOptions{Timeout: time.Duration(cfg.LLM.RequestTimeoutMS) * time.Millisecond})
	if err != nil {
		return fmt.Errorf("Provider 网络客户端错误: %w", err)
	}
	coordinator.SetProviderClient(providerClient)
	llm, err := factories.newProvider(cfg.LLM, provider.ProviderOptions{
		Endpoint:        providerEndpoint,
		Client:          providerClient,
		RuntimeRedactor: redactor,
		CleanupTimeout:  cleanupTimeout,
		Diagnostics:     boundedDiagnostics,
	})
	if err != nil {
		return fmt.Errorf("Provider 错误: %w", err)
	}
	res := resources.New()
	registry, err := factories.newRegistry(projectRoot)
	if err != nil {
		return fmt.Errorf("工具注册错误: %w", err)
	}
	permissionDir := filepath.Join(projectRoot, ".xagent")
	if err := os.Mkdir(permissionDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("工具文件系统错误: %w", err)
	}
	openedProject, err := safefs.Bootstrap(projectRoot, tool.ProjectFilesystemPolicy())
	if err != nil {
		return fmt.Errorf("工具文件系统错误: %w", err)
	}
	defer func() {
		if closeErr := openedProject.Root.Close(); closeErr != nil {
			runErr = errors.Join(runErr, closeErr)
		}
	}()
	processRunner, err := proctree.NewRunner(proctree.Options{CleanupTimeout: cleanupTimeout, Diagnostics: boundedDiagnostics})
	if err != nil {
		return fmt.Errorf("MCP 进程执行器错误: %w", err)
	}
	planFactory, err := proctree.NewProtectionPlanFactory([]*safefs.Root{openedProject.Root}, filepath.Clean(os.TempDir()))
	if err != nil {
		return fmt.Errorf("MCP 进程保护错误: %w", err)
	}
	resultFactory, err := tool.NewResultFactory(redactor)
	if err != nil {
		return fmt.Errorf("MCP 结果边界错误: %w", err)
	}
	mcpManager, err := factories.newMCP(cfg.MCP, mcpclient.ManagerOptions{
		MaxTools:          int(cfg.MCP.MaxTools),
		MaxPages:          int(cfg.MCP.MaxPages),
		MaxResponseBytes:  cfg.MCP.MaxResponseBytes,
		MaxProtocolErrors: cfg.MCP.MaxProtocolErrors,
		DefaultTimeout:    time.Duration(cfg.MCP.DefaultTimeoutMS) * time.Millisecond,
		CleanupTimeout:    cleanupTimeout,
		Diagnostics:       boundedDiagnostics,
	}, mcpclient.ManagerDependencies{
		Diagnostics:       boundedDiagnostics,
		RuntimeRedactor:   redactor,
		ResultFactory:     resultFactory,
		HTTPPolicy:        networkPolicy,
		HTTPClientFactory: networkClients,
		HTTPClientOptions: netpolicy.ClientOptions{},
		StdioRunner:       processRunner,
		StdioPlanFactory:  planFactory,
		StdioRoot:         openedProject.Root,
		Environment:       os.Environ(),
	})
	if err != nil {
		return fmt.Errorf("MCP 错误: %w", err)
	}
	if mcpManager == nil {
		return fmt.Errorf("MCP 错误: 构造结果为空")
	}
	coordinator.SetMCP(mcpManager)
	if err := mcpManager.Start(context.Background()); err != nil {
		return fmt.Errorf("MCP 启动错误: %w", err)
	}
	if summary := mcpManager.StatusLine(); summary != "" {
		fmt.Fprintf(factories.stderr, "MCP 状态: %s\n", redactor.Text(summary))
	}
	for _, mcpTool := range mcpManager.Tools() {
		registration, ok := mcpTool.(interface {
			RegistrationOptions() tool.RegistrationOptions
		})
		if !ok {
			return fmt.Errorf("MCP 工具注册错误: 缺少安全注册元数据")
		}
		if err := registry.RegisterWithOptions(mcpTool, registration.RegistrationOptions()); err != nil {
			return fmt.Errorf("MCP 工具注册错误: %w", err)
		}
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		return fmt.Errorf("Skill 系统工具注册错误: %w", err)
	}
	skillManager, err := factories.newSkillManager(projectRoot, filepath.Join(homeDir, ".config", "xagent", "skills"), registry, redactor.Text)
	if err != nil {
		return fmt.Errorf("Skill 配置错误: %w", err)
	}
	executor := tool.NewExecutorWithWriteAccess(
		registry,
		projectRoot,
		time.Duration(cfg.Tool.TimeoutMS)*time.Millisecond,
		cfg.Tool.MaxOutputBytes,
		openedProject.Root,
		openedProject.Capabilities.Ordinary(),
	)
	contextManager, err := contextmgr.New(llm, contextmgr.ManagerOptions{
		Context:           cfg.Context,
		InlineOutputBytes: cfg.Tool.InlineOutputBytes,
		RuntimeRedactor:   redactor,
	})
	if err != nil {
		return fmt.Errorf("上下文管理器错误: %w", err)
	}
	skillHistoryPolicy, err := orchestrator.NewSkillHistoryPolicy(
		cfg.Session.MaxSessionBytes,
		cfg.Context.ModelWindowTokens,
		cfg.Context.AutoMarginTokens,
	)
	if err != nil {
		return fmt.Errorf("Skill 历史策略错误: %w", err)
	}
	requestBudgeter := contextmgr.NewRequestBudgeter()
	instructionLoader := &instructions.CachedLoader{Loader: instructions.Loader{ProjectRoot: projectRoot, Config: cfg.Instructions}}
	memoryManager := newMemoryManager(projectRoot, cfg.Memory, llm, redactor)
	sessionManager := newSessionManager(instructionLoader, memoryManager, contextManager)
	model := app.New(app.Deps{
		RuntimeOptions: app.RuntimeOptions{
			CleanupTimeout: cleanupTimeout,
			Diagnostics:    boundedDiagnostics,
		},
		Config:              cfg,
		Provider:            llm,
		Store:               store,
		Resources:           res,
		Registry:            registry,
		Executor:            executor,
		ContextManager:      contextManager,
		SkillHistoryPolicy:  skillHistoryPolicy,
		RequestBudgeter:     requestBudgeter,
		SessionContext:      sessionManager,
		Memory:              memoryManager,
		Diagnostics:         diagnosticCollector,
		MCPStatus:           mcpManager,
		SkillManager:        skillManager,
		RuntimeRedactor:     redactor,
		Redact:              redactor.Text,
		RedactionLookbehind: redactor.MaxSecretBytes(),
		Hooks:               hooks,
	})
	coordinator.SetApp(&model)
	finalModel, tuiErr := factories.runTUI(model)
	if finalCloser := legacyCloserFromFinalModel(finalModel); finalCloser != nil {
		coordinator.SetApp(finalCloser)
	}
	if tuiErr != nil {
		return fmt.Errorf("TUI 错误: %w", tuiErr)
	}
	return nil
}

func conversationStoreOptions(session config.SessionConfig, projectRoot string, redactor *redact.RuntimeRedactor) conversation.JSONLStoreOptions {
	return conversation.JSONLStoreOptions{
		DataDir:         resolveProjectPath(projectRoot, session.Dir),
		MaxRecordBytes:  session.MaxRecordBytes,
		MaxSessionBytes: session.MaxSessionBytes,
		MaxScanFiles:    int64(session.MaxScanFiles),
		MaxScanBytes:    session.MaxScanBytes,
		RetentionDays:   int64(session.RetentionDays),
		GapReminderDays: int64(session.GapReminderDays),
		Redactor:        redactor,
	}
}

func legacyCloserFromFinalModel(model tea.Model) legacyAppCloser {
	switch value := model.(type) {
	case *app.Model:
		return value
	case app.Model:
		final := value
		return &final
	case legacyAppCloser:
		return value
	default:
		return nil
	}
}

type legacyShutdownTarget string

const (
	legacyShutdownApp            legacyShutdownTarget = "app"
	legacyShutdownHook           legacyShutdownTarget = "hook"
	legacyShutdownMCP            legacyShutdownTarget = "mcp"
	legacyShutdownProviderClient legacyShutdownTarget = "provider_client"
)

type legacyCompositeCloser struct {
	mu             sync.Mutex
	once           sync.Once
	app            legacyAppCloser
	hook           legacyHookShutdowner
	mcp            interface{ Close(context.Context) error }
	providerClient netpolicy.Client
	diagnostics    *diagnostics.Collector
	redact         func(string) string
	stderr         io.Writer
	newContext     func(legacyShutdownTarget) (context.Context, context.CancelFunc)
	err            error
}

func newLegacyCompositeCloser(collector *diagnostics.Collector, redactText func(string) string, stderr io.Writer) *legacyCompositeCloser {
	return &legacyCompositeCloser{
		diagnostics: collector,
		redact:      redactText,
		stderr:      stderr,
		newContext:  newLegacyShutdownContext,
	}
}

func newLegacyShutdownContext(target legacyShutdownTarget) (context.Context, context.CancelFunc) {
	if target == legacyShutdownMCP {
		return context.WithTimeout(context.Background(), 2*time.Second)
	}
	return context.WithCancel(context.Background())
}

func (c *legacyCompositeCloser) SetApp(closer legacyAppCloser) {
	c.mu.Lock()
	c.app = closer
	c.mu.Unlock()
}

func (c *legacyCompositeCloser) SetHook(runtime legacyHookShutdowner) {
	c.mu.Lock()
	c.hook = runtime
	c.mu.Unlock()
}

func (c *legacyCompositeCloser) SetMCP(closer interface{ Close(context.Context) error }) {
	c.mu.Lock()
	c.mcp = closer
	c.mu.Unlock()
}

func (c *legacyCompositeCloser) SetProviderClient(client netpolicy.Client) {
	c.mu.Lock()
	c.providerClient = client
	c.mu.Unlock()
}

func (c *legacyCompositeCloser) Close() error {
	if c == nil {
		return nil
	}
	c.once.Do(func() {
		c.mu.Lock()
		appResource, hookResource, mcpResource, providerClient := c.app, c.hook, c.mcp, c.providerClient
		contextFactory := c.newContext
		c.mu.Unlock()
		if contextFactory == nil {
			contextFactory = newLegacyShutdownContext
		}
		if appResource != nil {
			ctx, cancel := contextFactory(legacyShutdownApp)
			err := appResource.Close(ctx)
			cancel()
			c.recordFailure(legacyShutdownApp, err)
		}
		if mcpResource != nil {
			ctx, cancel := contextFactory(legacyShutdownMCP)
			err := mcpResource.Close(ctx)
			cancel()
			c.recordFailure(legacyShutdownMCP, err)
		}
		if providerClient != nil {
			providerClient.CloseIdleConnections()
		}
		if hookResource != nil {
			ctx, cancel := contextFactory(legacyShutdownHook)
			err := hookResource.Shutdown(ctx)
			cancel()
			if source, ok := hookResource.(legacyHookShutdownNoticeSource); ok {
				c.writeHookShutdownNotices(source.ShutdownNotices())
			}
			c.recordFailure(legacyShutdownHook, err)
		}
	})
	return c.err
}

func (c *legacyCompositeCloser) writeHookShutdownNotices(items []diagnostics.Diagnostic) {
	if c == nil || c.stderr == nil || len(items) == 0 {
		return
	}
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{
		Limit:    len(items),
		MaxBytes: diagnostics.DefaultDiagnosticFieldMaxBytes,
		Redactor: c.redact,
	})
	for _, item := range items {
		if item.Code == hook.DiagnosticShutdownCancelled {
			collector.Add(item)
		}
	}
	for _, item := range collector.List() {
		fmt.Fprintf(c.stderr, "Hook 收尾诊断: %s\n", item.Text())
	}
}

func (c *legacyCompositeCloser) recordFailure(target legacyShutdownTarget, err error) {
	if err == nil {
		return
	}
	detail := err.Error()
	if c.redact != nil {
		detail = c.redact(detail)
	}
	if c.diagnostics != nil {
		c.diagnostics.Add(diagnostics.New(
			"process_shutdown_failed",
			diagnostics.SeverityError,
			fmt.Sprintf("%s shutdown failed: %s", target, detail),
		).WithAttributes(map[string]string{"resource": string(target)}))
	}
	if c.stderr != nil {
		fmt.Fprintf(c.stderr, "%s 收尾失败，详情已安全记录到 /diagnostics\n", legacyShutdownLabel(target))
	}
	c.err = errors.Join(c.err, fmt.Errorf("%s shutdown failed", target))
}

func legacyShutdownLabel(target legacyShutdownTarget) string {
	switch target {
	case legacyShutdownApp:
		return "App"
	case legacyShutdownHook:
		return "Hook"
	case legacyShutdownMCP:
		return "MCP"
	case legacyShutdownProviderClient:
		return "Provider 网络客户端"
	default:
		return "资源"
	}
}

func newSkillManager(projectRoot string, userSkillsRoot string, registry *tool.Registry, redactor func(string) string) (*skill.Manager, error) {
	if registry == nil {
		return nil, fmt.Errorf("工具注册中心不能为空")
	}
	return skill.NewManager(skill.ManagerOptions{
		Sources: []skill.SourceFS{
			skill.BuiltinSource(),
			{Source: skill.SourceUser, Root: userSkillsRoot},
			{Source: skill.SourceProject, Root: filepath.Join(projectRoot, ".xagent", "skills")},
		},
		ToolNames:       registry.Names(),
		ReservedCommand: reservedCommandNames(command.Builtins()),
		Limits:          skill.DefaultLimits(),
		Redact:          redactor,
	})
}

func newMemoryManager(projectRoot string, memoryConfig config.MemoryConfig, updateProvider memory.UpdateProvider, runtimeRedactor *redact.RuntimeRedactor) *memory.Manager {
	if !config.Enabled(memoryConfig.Enabled, true) {
		return nil
	}
	return memory.NewManager(memory.ManagerOptions{
		UserDir:           resolveUserPath(memoryConfig.UserDir),
		ProjectDir:        resolveProjectPath(projectRoot, memoryConfig.ProjectDir),
		MaxIndexLines:     memoryConfig.MaxIndexLines,
		MaxIndexBytes:     memoryConfig.MaxIndexBytes,
		UpdateQueueSize:   memoryConfig.UpdateQueueSize,
		UpdateConcurrency: memoryConfig.UpdateConcurrency,
		UpdateTimeoutMS:   memoryConfig.UpdateTimeoutMS,
		MaxCandidateBytes: memoryConfig.MaxCandidateBytes,
		Provider:          updateProvider,
		Redactor:          runtimeRedactor,
	})
}

func newSessionManager(instructionLoader sessionctx.InstructionLoader, memoryManager *memory.Manager, contextManager sessionctx.ContextPreparer) *sessionctx.Manager {
	manager := &sessionctx.Manager{Instructions: instructionLoader, Context: contextManager}
	if memoryManager != nil {
		manager.Memory = memoryManager
	}
	return manager
}

func resolveProjectPath(projectRoot string, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(projectRoot, path)
}

func resolveUserPath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	return filepath.Join(home, path)
}
