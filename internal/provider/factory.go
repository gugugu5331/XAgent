package provider

import (
	"fmt"
	"time"

	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/netpolicy"
	"xagent/internal/redact"
)

type ProviderOptions struct {
	Endpoint        netpolicy.Endpoint
	Client          netpolicy.Client
	RuntimeRedactor *redact.RuntimeRedactor
	CleanupTimeout  time.Duration
	Diagnostics     diagnostics.BoundedSink
}

func NewWithOptions(cfg config.LLMConfig, options ProviderOptions) (Provider, error) {
	if options.Client == nil {
		return nil, fmt.Errorf("Provider 需要受控网络客户端")
	}
	if options.RuntimeRedactor == nil {
		return nil, fmt.Errorf("Provider 需要运行时脱敏器")
	}
	if options.Endpoint.URL == nil || options.Endpoint.Origin == "" || options.Endpoint.Purpose != netpolicy.PurposeProvider {
		return nil, fmt.Errorf("Provider 需要已验证的网络端点")
	}
	cfg.BaseURL = options.Endpoint.URL.String()
	streamOptions := ChatStreamOptions{
		CleanupTimeout: options.CleanupTimeout,
		Diagnostics:    options.Diagnostics,
	}
	switch cfg.Protocol {
	case config.ProtocolAnthropic:
		provider := NewAnthropic(cfg, options.Client, options.RuntimeRedactor)
		provider.streamOptions = streamOptions
		return provider, nil
	case config.ProtocolOpenAI:
		provider := NewOpenAI(cfg, options.Client, options.RuntimeRedactor)
		provider.streamOptions = streamOptions
		return provider, nil
	default:
		return nil, fmt.Errorf("未知 Provider 协议: %s", cfg.Protocol)
	}
}
