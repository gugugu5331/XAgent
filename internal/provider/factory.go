package provider

import (
	"fmt"
	"net/http"

	"xagent/internal/config"
)

func New(cfg config.LLMConfig) (Provider, error) {
	client := &http.Client{}
	switch cfg.Protocol {
	case config.ProtocolAnthropic:
		return NewAnthropic(cfg, client), nil
	case config.ProtocolOpenAI:
		return NewOpenAI(cfg, client), nil
	default:
		return nil, fmt.Errorf("未知 Provider 协议: %s", cfg.Protocol)
	}
}
