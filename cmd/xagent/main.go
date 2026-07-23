package main

import (
	"context"
	"errors"
	"flag"
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
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/resources"
	"xagent/internal/sessionctx"
	"xagent/internal/skill"
	"xagent/internal/tool"
	"xagent/internal/tui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "运行错误: %v\n", err)
		os.Exit(1)
	}
}

type appCloser interface {
	Close(context.Context) error
}

type hookShutdowner interface {
	Shutdown(context.Context) error
}

type hookShutdownNoticeSource interface {
	ShutdownNotices() []diagnostics.Diagnostic
}

type mcpRuntime interface {
	Start(context.Context)
	Tools() []tool.Tool
	StatusLine() string
	Summary() mcpclient.StatusSummary
	Diagnostics() []mcpclient.Diagnostic
	Close(context.Context) error
}

type startupFactories struct {
	loadConfig      func(string, config.LoadOptions) (*config.AppConfig, error)
	getwd           func() (string, error)
	userHomeDir     func() (string, error)
	lookupEnv       func(string) (string, bool)
	loadHooks       func(hook.LoadOptions) (hook.Snapshot, error)
	buildRuntime    func(hook.Snapshot, hook.EngineOptions) (hook.Runtime, error)
	newStore        func(conversation.JSONLStoreOptions) (conversation.ConversationStore, error)
	newProvider     func(config.LLMConfig) (provider.Provider, error)
	newRegistry     func(string) (*tool.Registry, error)
	newMCP          func(config.MCPConfig, mcpclient.ManagerOptions) mcpRuntime
	newSkillManager func(string, string, *tool.Registry, func(string) string) (*skill.Manager, error)
	runTUI          func(tea.Model) (tea.Model, error)
	stderr          io.Writer
}

func defaultStartupFactories() startupFactories {
	return startupFactories{
		loadConfig:  config.LoadWithOptions,
		getwd:       os.Getwd,
		userHomeDir: os.UserHomeDir,
		lookupEnv:   os.LookupEnv,
		loadHooks:   hook.Load,
		buildRuntime: func(snapshot hook.Snapshot, options hook.EngineOptions) (hook.Runtime, error) {
			return hook.NewEngine(snapshot, options)
		},
		newStore: func(options conversation.JSONLStoreOptions) (conversation.ConversationStore, error) {
			return conversation.NewJSONLStore(options)
		},
		newProvider: provider.New,
		newRegistry: tool.NewRegistry,
		newMCP: func(cfg config.MCPConfig, options mcpclient.ManagerOptions) mcpRuntime {
			return mcpclient.NewManager(cfg, options)
		},
		newSkillManager: newSkillManager,
		runTUI:          tui.Run,
		stderr:          os.Stderr,
	}
}

func run() error {
	return runWithFactories(os.Args[1:], defaultStartupFactories())
}

func runWithFactories(args []string, factories startupFactories) (runErr error) {
	configPath, err := parseConfigPath(args)
	if err != nil {
		return err
	}
	if factories.stderr == nil {
		factories.stderr = io.Discard
	}
	redactor := redact.NewRuntimeRedactor()
	diagnosticCollector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redactor.Text})
	coordinator := newCompositeCloser(diagnosticCollector, redactor.Text, factories.stderr)
	defer func() {
		if closeErr := coordinator.Close(); closeErr != nil {
			runErr = errors.Join(runErr, closeErr)
		}
	}()

	cfg, err := factories.loadConfig(configPath, config.LoadOptions{Redactor: redactor})
	if err != nil {
		return fmt.Errorf("配置错误: %w", err)
	}
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
		ProjectRoot:   projectRoot,
		Diagnostics:   diagnosticCollector,
		Redactor:      redactor,
		Limits:        hook.DefaultLimits(),
		AsyncWorkers:  4,
		AsyncQueue:    64,
		ShutdownGrace: 2 * time.Second,
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

	store, err := factories.newStore(conversation.JSONLStoreOptions{DataDir: resolveProjectPath(projectRoot, cfg.Session.Dir)})
	if err != nil {
		return fmt.Errorf("会话存储错误: %w", err)
	}
	llm, err := factories.newProvider(cfg.LLM)
	if err != nil {
		return fmt.Errorf("Provider 错误: %w", err)
	}
	res := resources.New()
	registry, err := factories.newRegistry(projectRoot)
	if err != nil {
		return fmt.Errorf("工具注册错误: %w", err)
	}
	mcpManager := factories.newMCP(cfg.MCP, mcpclient.ManagerOptions{})
	if mcpManager == nil {
		return fmt.Errorf("MCP 错误: 构造结果为空")
	}
	coordinator.SetMCP(mcpManager)
	mcpManager.Start(context.Background())
	if summary := mcpManager.StatusLine(); summary != "" {
		fmt.Fprintf(factories.stderr, "MCP 状态: %s\n", redactor.Text(summary))
	}
	for _, mcpTool := range mcpManager.Tools() {
		if err := registry.Register(mcpTool); err != nil {
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
	executor := tool.NewExecutor(registry, projectRoot, time.Duration(cfg.Tool.TimeoutMS)*time.Millisecond, cfg.Tool.MaxOutputBytes)
	contextManager := contextmgr.New(llm, cfg.Storage.DataDir, cfg.Context)
	instructionLoader := &instructions.CachedLoader{Loader: instructions.Loader{ProjectRoot: projectRoot, Config: cfg.Instructions}}
	memoryManager := newMemoryManager(projectRoot, cfg.Memory, llm)
	sessionManager := newSessionManager(instructionLoader, memoryManager, contextManager)
	model := app.New(app.Deps{
		Config:              cfg,
		Provider:            llm,
		Store:               store,
		Resources:           res,
		Registry:            registry,
		Executor:            executor,
		ContextManager:      contextManager,
		SessionContext:      sessionManager,
		Memory:              memoryManager,
		Diagnostics:         diagnosticCollector,
		MCPStatus:           mcpManager,
		SkillManager:        skillManager,
		Redact:              redactor.Text,
		RedactionLookbehind: redactor.MaxSecretBytes(),
		Hooks:               hooks,
	})
	coordinator.SetApp(&model)
	finalModel, tuiErr := factories.runTUI(model)
	if finalCloser := closerFromFinalModel(finalModel); finalCloser != nil {
		coordinator.SetApp(finalCloser)
	}
	if tuiErr != nil {
		return fmt.Errorf("TUI 错误: %w", tuiErr)
	}
	return nil
}

func parseConfigPath(args []string) (string, error) {
	flags := flag.NewFlagSet("xagent", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", config.DefaultConfigFile, "配置文件路径")
	if err := flags.Parse(args); err != nil {
		return "", fmt.Errorf("参数错误: %w", err)
	}
	if flags.NArg() != 0 {
		return "", fmt.Errorf("参数错误: 不支持位置参数")
	}
	return *configPath, nil
}

func closerFromFinalModel(model tea.Model) appCloser {
	switch value := model.(type) {
	case *app.Model:
		return value
	case app.Model:
		final := value
		return &final
	case appCloser:
		return value
	default:
		return nil
	}
}

type shutdownTarget string

const (
	shutdownApp  shutdownTarget = "app"
	shutdownHook shutdownTarget = "hook"
	shutdownMCP  shutdownTarget = "mcp"
)

type compositeCloser struct {
	mu          sync.Mutex
	once        sync.Once
	app         appCloser
	hook        hookShutdowner
	mcp         interface{ Close(context.Context) error }
	diagnostics *diagnostics.Collector
	redact      func(string) string
	stderr      io.Writer
	newContext  func(shutdownTarget) (context.Context, context.CancelFunc)
	err         error
}

func newCompositeCloser(collector *diagnostics.Collector, redactText func(string) string, stderr io.Writer) *compositeCloser {
	return &compositeCloser{
		diagnostics: collector,
		redact:      redactText,
		stderr:      stderr,
		newContext:  newShutdownContext,
	}
}

func newShutdownContext(target shutdownTarget) (context.Context, context.CancelFunc) {
	if target == shutdownMCP {
		return context.WithTimeout(context.Background(), 2*time.Second)
	}
	return context.WithCancel(context.Background())
}

func (c *compositeCloser) SetApp(closer appCloser) {
	c.mu.Lock()
	c.app = closer
	c.mu.Unlock()
}

func (c *compositeCloser) SetHook(runtime hookShutdowner) {
	c.mu.Lock()
	c.hook = runtime
	c.mu.Unlock()
}

func (c *compositeCloser) SetMCP(closer interface{ Close(context.Context) error }) {
	c.mu.Lock()
	c.mcp = closer
	c.mu.Unlock()
}

func (c *compositeCloser) Close() error {
	if c == nil {
		return nil
	}
	c.once.Do(func() {
		c.mu.Lock()
		appResource, hookResource, mcpResource := c.app, c.hook, c.mcp
		contextFactory := c.newContext
		c.mu.Unlock()
		if contextFactory == nil {
			contextFactory = newShutdownContext
		}
		if appResource != nil {
			ctx, cancel := contextFactory(shutdownApp)
			err := appResource.Close(ctx)
			cancel()
			c.recordFailure(shutdownApp, err)
		}
		if hookResource != nil {
			ctx, cancel := contextFactory(shutdownHook)
			err := hookResource.Shutdown(ctx)
			cancel()
			if source, ok := hookResource.(hookShutdownNoticeSource); ok {
				c.writeHookShutdownNotices(source.ShutdownNotices())
			}
			c.recordFailure(shutdownHook, err)
		}
		if mcpResource != nil {
			ctx, cancel := contextFactory(shutdownMCP)
			err := mcpResource.Close(ctx)
			cancel()
			c.recordFailure(shutdownMCP, err)
		}
	})
	return c.err
}

func (c *compositeCloser) writeHookShutdownNotices(items []diagnostics.Diagnostic) {
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

func (c *compositeCloser) recordFailure(target shutdownTarget, err error) {
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
		fmt.Fprintf(c.stderr, "%s 收尾失败，详情已安全记录到 /diagnostics\n", shutdownLabel(target))
	}
	c.err = errors.Join(c.err, fmt.Errorf("%s shutdown failed", target))
}

func shutdownLabel(target shutdownTarget) string {
	switch target {
	case shutdownApp:
		return "App"
	case shutdownHook:
		return "Hook"
	case shutdownMCP:
		return "MCP"
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

func newMemoryManager(projectRoot string, memoryConfig config.MemoryConfig, updateProvider memory.UpdateProvider) *memory.Manager {
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
	})
}

func newSessionManager(instructionLoader sessionctx.InstructionLoader, memoryManager *memory.Manager, contextManager sessionctx.ContextPreparer) *sessionctx.Manager {
	manager := &sessionctx.Manager{Instructions: instructionLoader, Context: contextManager}
	if memoryManager != nil {
		manager.Memory = memoryManager
	}
	return manager
}

func reservedCommandNames(definitions []command.Definition) []string {
	names := make([]string, 0, len(definitions)*2)
	for _, definition := range definitions {
		names = append(names, definition.Name)
		names = append(names, definition.Aliases...)
	}
	return names
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
