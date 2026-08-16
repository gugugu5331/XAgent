package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"xagent/internal/config"
	"xagent/internal/netpolicy"
	"xagent/internal/redact"
)

type AnthropicProvider struct {
	cfg           config.LLMConfig
	client        anthropic.Client
	ready         bool
	redactor      *redact.RuntimeRedactor
	streamOptions ChatStreamOptions
}

func NewAnthropic(cfg config.LLMConfig, client netpolicy.Client, runtimeRedactor *redact.RuntimeRedactor) *AnthropicProvider {
	options := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	var sdkClient *http.Client
	if client != nil {
		sdkClient = client.SDKHTTPClient()
	}
	if sdkClient != nil {
		options = append(options, option.WithHTTPClient(sdkClient))
	}
	if strings.TrimRight(cfg.BaseURL, "/") != "https://api.anthropic.com" {
		options = append(options, option.WithBaseURL(strings.TrimRight(cfg.BaseURL, "/")))
	}
	runtimeRedactor.RegisterSecret(cfg.APIKey)
	return &AnthropicProvider{cfg: cfg, client: anthropic.NewClient(options...), ready: sdkClient != nil, redactor: runtimeRedactor}
}

func (p *AnthropicProvider) Name() string {
	return "Anthropic Claude"
}

func (p *AnthropicProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	streamOptions := ChatStreamOptions{}
	if p != nil {
		streamOptions = p.streamOptions
	}
	return p.StreamChatWithOptions(ctx, req, streamOptions)
}

// StreamChatWithOptions is the lifecycle-aware Provider entry point used by
// Orchestrator.  StreamChat remains a compatibility wrapper for callers that
// do not yet provide the process-level C7 options.
func (p *AnthropicProvider) StreamChatWithOptions(ctx context.Context, req ChatRequest, streamOptions ChatStreamOptions) (ChatStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	attempt := newRequestAttempt(req.Observer)
	streamCtx = attempt.traceContext(streamCtx)
	returnedStream := false
	defer func() {
		if !returnedStream {
			cancelStream()
			attempt.finish()
		}
	}()
	if p == nil || !p.ready {
		var redactor *redact.RuntimeRedactor
		if p != nil {
			redactor = p.redactor
		}
		return nil, safeProviderError(redactor, "anthropic_controlled_client_required", "anthropic", errors.New("Anthropic 受控网络客户端不可用"), false)
	}

	limits, err := newStreamLimits(p.cfg.Stream)
	if err != nil {
		return nil, safeProviderError(p.redactor, "anthropic_stream_limits_invalid", "anthropic", err, false)
	}
	out := make(chan StreamEvent)
	params, err := anthropicMessageParams(p.cfg.Model, req)
	if err != nil {
		return nil, safeProviderError(p.redactor, "anthropic_request_invalid", "anthropic", err, false)
	}
	sdkStream := newAnthropicSDKStreamOwner(p.client.Messages.NewStreaming(streamCtx, params))
	producerDone := make(chan struct{})
	stream, err := newChatStream(out, streamOptions, func(cleanupCtx context.Context) error {
		cancelStream()
		closeErr := sdkStream.close()
		select {
		case <-producerDone:
			return closeErr
		case <-cleanupCtx.Done():
			return cleanupCtx.Err()
		}
	}, func() {
		cancelStream()
		_ = sdkStream.close()
	})
	if err != nil {
		_ = sdkStream.close()
		return nil, err
	}

	go func() {
		defer close(producerDone)
		defer close(out)
		defer cancelStream()
		defer sdkStream.close()
		defer attempt.finish()
		processor := newAnthropicStreamProcessor(streamCtx, stream, limits, p.redactor, attempt)
		if failure := processor.run(sdkStream); failure != nil {
			attempt.finish()
			stream.emit(streamCtx, StreamEvent{
				Type:  StreamEventError,
				Error: safeProviderError(p.redactor, failure.code, "anthropic", failure.err, failure.recoverable),
			})
		}
	}()
	returnedStream = true
	return stream, nil
}
