package provider

import (
	"fmt"
	"net/http"
	"time"

	"xagent/internal/config"
)

func New(cfg config.LLMConfig) (Provider, error) {
	return NewWithOptions(cfg, ProviderOptions{})
}

type ProviderOptions struct{}

func NewWithOptions(cfg config.LLMConfig, _ ProviderOptions) (Provider, error) {
	timeout := time.Duration(cfg.RequestTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Duration(config.DefaultLLMRequestTimeoutMS) * time.Millisecond
	}
	client := &http.Client{Timeout: timeout}
	switch cfg.Protocol {
	case config.ProtocolAnthropic:
		return NewAnthropic(cfg, client), nil
	case config.ProtocolOpenAI:
		return NewOpenAI(cfg, client), nil
	default:
		return nil, fmt.Errorf("未知 Provider 协议: %s", cfg.Protocol)
	}
}
