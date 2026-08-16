package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"xagent/internal/config"
	"xagent/internal/netpolicy"
	"xagent/internal/redact"
)

type OpenAIProvider struct {
	cfg           config.LLMConfig
	client        netpolicy.Client
	redactor      *redact.RuntimeRedactor
	streamOptions ChatStreamOptions
}

func NewOpenAI(cfg config.LLMConfig, client netpolicy.Client, runtimeRedactor *redact.RuntimeRedactor) *OpenAIProvider {
	runtimeRedactor.RegisterSecret(cfg.APIKey)
	return &OpenAIProvider{cfg: cfg, client: client, redactor: runtimeRedactor}
}

func (p *OpenAIProvider) Name() string {
	return "OpenAI"
}

func (p *OpenAIProvider) StreamChat(ctx context.Context, req ChatRequest) (ChatStream, error) {
	streamOptions := ChatStreamOptions{}
	if p != nil {
		streamOptions = p.streamOptions
	}
	return p.StreamChatWithOptions(ctx, req, streamOptions)
}

// StreamChatWithOptions is the lifecycle-aware Provider entry point used by
// Orchestrator.  Keeping StreamChat as a compatibility wrapper means existing
// callers still get the provider's configured defaults, while the composition
// root can supply one resolved CleanupTimeout/Diagnostics sink per request.
func (p *OpenAIProvider) StreamChatWithOptions(ctx context.Context, req ChatRequest, streamOptions ChatStreamOptions) (ChatStream, error) {
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
	if p == nil || p.client == nil {
		var redactor *redact.RuntimeRedactor
		if p != nil {
			redactor = p.redactor
		}
		return nil, safeProviderError(redactor, "openai_controlled_client_required", "openai", errors.New("OpenAI 受控网络客户端不可用"), false)
	}

	out := make(chan StreamEvent)
	limits, err := newStreamLimits(p.cfg.Stream)
	if err != nil {
		return nil, safeProviderError(p.redactor, "openai_stream_limits_invalid", "openai", err, false)
	}
	wireRequest, err := newOpenAIRequest(req, p.cfg.Model)
	if err != nil {
		return nil, safeProviderError(p.redactor, "openai_request_invalid", "openai", err, false)
	}
	body, err := json.Marshal(wireRequest)
	if err != nil {
		return nil, safeProviderError(p.redactor, "openai_request_encode_failed", "openai", fmt.Errorf("构造 OpenAI 请求失败: %w", err), false)
	}

	endpoint := strings.TrimRight(p.cfg.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}

	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, safeProviderError(p.redactor, "openai_request_create_failed", "openai", fmt.Errorf("创建 OpenAI 请求失败: %w", err), false)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, safeProviderError(p.redactor, "openai_request_failed", "openai", fmt.Errorf("OpenAI 请求失败: %w", err), true)
	}
	// A response proves the request crossed the transport boundary even when a
	// custom RoundTripper does not expose net/http trace callbacks.
	attempt.markSent()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, safeProviderError(p.redactor, "openai_http_status", "openai", fmt.Errorf("OpenAI 返回错误状态 %d: %s", resp.StatusCode, strings.TrimSpace(string(data))), resp.StatusCode >= 500)
	}

	var bodyCloseOnce sync.Once
	closeBody := func() {
		bodyCloseOnce.Do(func() {
			_ = resp.Body.Close()
		})
	}
	producerDone := make(chan struct{})
	stream, err := newChatStream(out, streamOptions, func(cleanupCtx context.Context) error {
		cancelStream()
		closeBody()
		select {
		case <-producerDone:
			return nil
		case <-cleanupCtx.Done():
			return cleanupCtx.Err()
		}
	}, func() {
		cancelStream()
		closeBody()
	})
	if err != nil {
		closeBody()
		return nil, err
	}

	go func() {
		defer close(producerDone)
		defer close(out)
		defer closeBody()
		defer cancelStream()
		defer attempt.finish()
		processor := newOpenAIStreamProcessor(streamCtx, stream, limits, p.redactor, attempt)
		if failure := processor.run(resp.Body); failure != nil {
			attempt.finish()
			stream.emit(streamCtx, StreamEvent{
				Type:  StreamEventError,
				Error: safeProviderError(p.redactor, failure.code, "openai", failure.err, failure.recoverable),
			})
		}
	}()
	returnedStream = true
	return stream, nil
}
