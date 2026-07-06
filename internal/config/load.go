package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	DefaultStartMode                   = "list"
	DefaultDataDir                     = ".xagent/conversations"
	DefaultThinkingBudget              = 4096
	DefaultConfigFile                  = "config.yaml"
	DefaultContextToolResultThreshold  = 32 * 1024
	DefaultContextToolResultsThreshold = 64 * 1024
	DefaultContextModelWindowTokens    = 200000
	DefaultContextAutoMarginTokens     = 13000
	DefaultContextManualMarginTokens   = 3000
	DefaultContextRecentKeepTokens     = 10000
	DefaultContextRecentKeepMessages   = 5
	DefaultContextSummaryFailureLimit  = 3
	DefaultContextPreviewChars         = 2000
	DefaultInstructionsProjectFile     = "MEWCODE.md"
	DefaultInstructionsProjectDir      = ".mewcode"
	DefaultInstructionsUserDir         = ".mewcode"
	DefaultInstructionsMaxIncludeDepth = 5
	DefaultInstructionsMaxFileBytes    = 64 * 1024
	DefaultSessionDir                  = ".mewcode/sessions"
	DefaultSessionRetentionDays        = 30
	DefaultSessionMaxScanFiles         = 1000
	DefaultSessionMaxScanBytes         = 10 * 1024 * 1024
	DefaultSessionGapReminderDays      = 7
	DefaultMemoryUserDir               = ".mewcode/memory"
	DefaultMemoryProjectDir            = ".mewcode/memory"
	DefaultMemoryMaxIndexLines         = 200
	DefaultMemoryMaxIndexBytes         = 25 * 1024
	DefaultMemoryUpdateQueueSize       = 8
	DefaultMemoryUpdateConcurrency     = 1
	DefaultMemoryUpdateTimeoutMS       = 30000
	DefaultMemoryMaxCandidateBytes     = 64 * 1024
)

func Load(path string) (*AppConfig, error) {
	if path == "" {
		path = DefaultConfigFile
	}

	projectCfg, err := loadSingle(path)
	if err != nil {
		return nil, err
	}
	projectCfg.MCP = markMCPSource(projectCfg.MCP, "project")

	cfg := projectCfg
	if userPath := userConfigPath(); userPath != "" && userPath != path {
		if userCfg, err := loadSingle(userPath); err == nil {
			userCfg.MCP = markMCPSource(userCfg.MCP, "user")
			cfg.MCP = MergeMCPConfig(userCfg.MCP, projectCfg.MCP)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	applyDefaults(cfg)
	if err := Validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func loadSingle(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取配置文件 %q: %w", path, err)
	}

	var cfg AppConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("配置文件格式无效: %w", err)
	}
	return &cfg, nil
}

func userConfigPath() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "xagent", "config.yaml")
}

func markMCPSource(cfg MCPConfig, source string) MCPConfig {
	for name, server := range cfg.Servers {
		server.Source = source
		cfg.Servers[name] = server
	}
	return cfg
}

func applyDefaults(cfg *AppConfig) {
	if cfg.UI.StartMode == "" {
		cfg.UI.StartMode = DefaultStartMode
	}
	if !cfg.UI.ShowResponseTimer {
		cfg.UI.ShowResponseTimer = true
	}
	if cfg.Storage.DataDir == "" {
		cfg.Storage.DataDir = DefaultDataDir
	}
	if cfg.LLM.Thinking.BudgetTokens <= 0 {
		cfg.LLM.Thinking.BudgetTokens = DefaultThinkingBudget
	}
	applyContextDefaults(&cfg.Context)
	applyInstructionsDefaults(&cfg.Instructions)
	applySessionDefaults(&cfg.Session)
	applyMemoryDefaults(&cfg.Memory)
}

func applyInstructionsDefaults(cfg *InstructionsConfig) {
	cfg.Enabled = true
	if cfg.ProjectFile == "" {
		cfg.ProjectFile = DefaultInstructionsProjectFile
	}
	if cfg.ProjectDir == "" {
		cfg.ProjectDir = DefaultInstructionsProjectDir
	}
	if cfg.UserDir == "" {
		cfg.UserDir = DefaultInstructionsUserDir
	}
	if cfg.MaxIncludeDepth <= 0 {
		cfg.MaxIncludeDepth = DefaultInstructionsMaxIncludeDepth
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = DefaultInstructionsMaxFileBytes
	}
}

func applySessionDefaults(cfg *SessionConfig) {
	if cfg.Dir == "" {
		cfg.Dir = DefaultSessionDir
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = DefaultSessionRetentionDays
	}
	if cfg.MaxScanFiles <= 0 {
		cfg.MaxScanFiles = DefaultSessionMaxScanFiles
	}
	if cfg.MaxScanBytes <= 0 {
		cfg.MaxScanBytes = DefaultSessionMaxScanBytes
	}
	if cfg.GapReminderDays <= 0 {
		cfg.GapReminderDays = DefaultSessionGapReminderDays
	}
}

func applyMemoryDefaults(cfg *MemoryConfig) {
	cfg.Enabled = true
	if cfg.UserDir == "" {
		cfg.UserDir = DefaultMemoryUserDir
	}
	if cfg.ProjectDir == "" {
		cfg.ProjectDir = DefaultMemoryProjectDir
	}
	if cfg.MaxIndexLines <= 0 {
		cfg.MaxIndexLines = DefaultMemoryMaxIndexLines
	}
	if cfg.MaxIndexBytes <= 0 {
		cfg.MaxIndexBytes = DefaultMemoryMaxIndexBytes
	}
	if cfg.UpdateQueueSize <= 0 {
		cfg.UpdateQueueSize = DefaultMemoryUpdateQueueSize
	}
	if cfg.UpdateConcurrency <= 0 {
		cfg.UpdateConcurrency = DefaultMemoryUpdateConcurrency
	}
	if cfg.UpdateTimeoutMS <= 0 {
		cfg.UpdateTimeoutMS = DefaultMemoryUpdateTimeoutMS
	}
	if cfg.MaxCandidateBytes <= 0 {
		cfg.MaxCandidateBytes = DefaultMemoryMaxCandidateBytes
	}
}

func applyContextDefaults(cfg *ContextConfig) {
	cfg.Enabled = true
	if cfg.ToolResultThresholdChars <= 0 {
		cfg.ToolResultThresholdChars = DefaultContextToolResultThreshold
	}
	if cfg.ToolResultsThresholdChars <= 0 {
		cfg.ToolResultsThresholdChars = DefaultContextToolResultsThreshold
	}
	if cfg.ModelWindowTokens <= 0 {
		cfg.ModelWindowTokens = DefaultContextModelWindowTokens
	}
	if cfg.AutoMarginTokens <= 0 {
		cfg.AutoMarginTokens = DefaultContextAutoMarginTokens
	}
	if cfg.ManualMarginTokens <= 0 {
		cfg.ManualMarginTokens = DefaultContextManualMarginTokens
	}
	if cfg.RecentKeepTokens <= 0 {
		cfg.RecentKeepTokens = DefaultContextRecentKeepTokens
	}
	if cfg.RecentKeepMessages <= 0 {
		cfg.RecentKeepMessages = DefaultContextRecentKeepMessages
	}
	if cfg.SummaryFailureLimit <= 0 {
		cfg.SummaryFailureLimit = DefaultContextSummaryFailureLimit
	}
	if cfg.PreviewChars <= 0 {
		cfg.PreviewChars = DefaultContextPreviewChars
	}
}
