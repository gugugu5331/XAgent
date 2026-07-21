package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"xagent/internal/app"
	"xagent/internal/command"
	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
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
	configPath := flag.String("config", config.DefaultConfigFile, "配置文件路径")
	flag.Parse()

	redactor := redact.NewRuntimeRedactor()
	diagnosticCollector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redactor.Text})
	cfg, err := config.LoadWithOptions(*configPath, config.LoadOptions{Redactor: redactor})
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		os.Exit(1)
	}

	projectRoot, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "项目目录错误: %v\n", err)
		os.Exit(1)
	}

	store, err := conversation.NewJSONLStore(conversation.JSONLStoreOptions{DataDir: resolveProjectPath(projectRoot, cfg.Session.Dir)})
	if err != nil {
		fmt.Fprintf(os.Stderr, "会话存储错误: %v\n", err)
		os.Exit(1)
	}

	llm, err := provider.New(cfg.LLM)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Provider 错误: %v\n", err)
		os.Exit(1)
	}

	res := resources.New()
	registry, err := tool.NewRegistry(projectRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "工具注册错误: %v\n", err)
		os.Exit(1)
	}
	mcpManager := mcpclient.NewManager(cfg.MCP, mcpclient.ManagerOptions{})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := mcpManager.Close(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "MCP 关闭错误: %v\n", err)
		}
	}()
	mcpManager.Start(context.Background())
	if summary := mcpManager.StatusLine(); summary != "" {
		fmt.Fprintf(os.Stderr, "MCP 状态: %s\n", summary)
	}
	for _, mcpTool := range mcpManager.Tools() {
		if err := registry.Register(mcpTool); err != nil {
			fmt.Fprintf(os.Stderr, "MCP 工具注册错误: %v\n", err)
			os.Exit(1)
		}
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		fmt.Fprintf(os.Stderr, "Skill 系统工具注册错误: %v\n", err)
		os.Exit(1)
	}
	skillManager, err := newSkillManager(projectRoot, resolveUserPath(".config/xagent/skills"), registry, redactor.Text)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Skill 配置错误: %v\n", err)
		os.Exit(1)
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
		Closer:              mcpManager,
		MCPStatus:           mcpManager,
		SkillManager:        skillManager,
		Redact:              redactor.Text,
		RedactionLookbehind: redactor.MaxSecretBytes(),
	})
	if err := tui.Run(model); err != nil {
		fmt.Fprintf(os.Stderr, "运行错误: %v\n", err)
		os.Exit(1)
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
