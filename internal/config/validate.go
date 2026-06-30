package config

import (
	"errors"
	"fmt"
	"strings"
)

const (
	ProtocolAnthropic = "anthropic"
	ProtocolOpenAI    = "openai"
	StartModeList     = "list"
	StartModeNew      = "new"
)

func Validate(config *AppConfig) error {
	if config == nil {
		return errors.New("配置为空")
	}

	protocol := strings.TrimSpace(config.LLM.Protocol)
	if protocol != ProtocolAnthropic && protocol != ProtocolOpenAI {
		return fmt.Errorf("llm.protocol 必须是 %q 或 %q", ProtocolAnthropic, ProtocolOpenAI)
	}
	config.LLM.Protocol = protocol

	config.LLM.Model = strings.TrimSpace(config.LLM.Model)
	if config.LLM.Model == "" {
		return errors.New("llm.model 不能为空")
	}

	config.LLM.BaseURL = strings.TrimSpace(config.LLM.BaseURL)
	if config.LLM.BaseURL == "" {
		return errors.New("llm.base_url 不能为空")
	}

	config.LLM.APIKey = strings.TrimSpace(config.LLM.APIKey)
	if config.LLM.APIKey == "" {
		return errors.New("llm.api_key 不能为空")
	}

	config.UI.StartMode = strings.TrimSpace(config.UI.StartMode)
	if config.UI.StartMode != StartModeList && config.UI.StartMode != StartModeNew {
		return fmt.Errorf("ui.start_mode 必须是 %q 或 %q", StartModeList, StartModeNew)
	}

	if config.LLM.Thinking.BudgetTokens <= 0 {
		config.LLM.Thinking.BudgetTokens = DefaultThinkingBudget
	}

	return nil
}
