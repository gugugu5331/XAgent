package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

const (
	DefaultStartMode      = "list"
	DefaultDataDir        = ".xagent/conversations"
	DefaultThinkingBudget = 4096
	DefaultConfigFile     = "config.yaml"
)

func Load(path string) (*AppConfig, error) {
	if path == "" {
		path = DefaultConfigFile
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取配置文件 %q: %w", path, err)
	}

	var cfg AppConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("配置文件格式无效: %w", err)
	}

	applyDefaults(&cfg)
	if err := Validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
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
}
