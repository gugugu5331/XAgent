package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/netpolicy"
	"xagent/internal/redact"
)

func TestProviderFactoryRetainsChatStreamLifecycleOptions(t *testing.T) {
	wantTimeout := 137 * time.Millisecond
	sink := &providerFactoryDiagnosticSink{}

	tests := []struct {
		name     string
		protocol string
		client   func() *trackingProviderPolicyClient
	}{
		{
			name:     "OpenAI",
			protocol: config.ProtocolOpenAI,
			client: func() *trackingProviderPolicyClient {
				return &trackingProviderPolicyClient{do: func(request *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
						Request:    request,
					}, nil
				}}
			},
		},
		{
			name:     "Anthropic",
			protocol: config.ProtocolAnthropic,
			client: func() *trackingProviderPolicyClient {
				bridge := &http.Client{Transport: providerFactoryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body: io.NopCloser(strings.NewReader(
							"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
						)),
						Request: request,
					}, nil
				})}
				return &trackingProviderPolicyClient{sdk: bridge}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint := validatedProviderEndpoint(t, "http://127.0.0.1:18447/provider/v1")
			llm, err := NewWithOptions(config.LLMConfig{
				Protocol: test.protocol,
				Model:    "test",
				APIKey:   "factory-lifecycle-test-key",
			}, ProviderOptions{
				Endpoint:        endpoint,
				Client:          test.client(),
				RuntimeRedactor: redact.NewRuntimeRedactor(),
				CleanupTimeout:  wantTimeout,
				Diagnostics:     sink,
			})
			if err != nil {
				t.Fatalf("construct %s Provider: %v", test.name, err)
			}

			var retained ChatStreamOptions
			switch concrete := llm.(type) {
			case *OpenAIProvider:
				retained = concrete.streamOptions
			case *AnthropicProvider:
				retained = concrete.streamOptions
			default:
				t.Fatalf("Provider type = %T, want a supported concrete Provider", llm)
			}
			if retained.CleanupTimeout != wantTimeout || retained.Diagnostics != sink {
				t.Fatalf("retained ChatStreamOptions = %#v, want timeout %s and sink %p", retained, wantTimeout, sink)
			}

			stream, err := llm.StreamChat(context.Background(), ChatRequest{})
			if err != nil {
				t.Fatalf("start %s stream: %v", test.name, err)
			}
			owned, ok := stream.(*chatStream)
			if !ok {
				t.Fatalf("stream type = %T, want *chatStream", stream)
			}
			if owned.timeout != wantTimeout || owned.diagnostics != sink {
				t.Fatalf("owned stream lifecycle = timeout:%s sink:%p, want %s/%p", owned.timeout, owned.diagnostics, wantTimeout, sink)
			}
			for range stream.Events() {
			}
			closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			closeErr := stream.Close(closeCtx)
			cancel()
			if closeErr != nil {
				t.Fatalf("close %s stream: %v", test.name, closeErr)
			}
		})
	}
}

func TestFactoryUsesInjectedNetpolicyClient(t *testing.T) {
	endpoint := validatedProviderEndpoint(t, "http://127.0.0.1:18443/provider/v1")
	var requestedURL string
	client := &trackingProviderPolicyClient{do: func(request *http.Request) (*http.Response, error) {
		requestedURL = request.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			Request:    request,
		}, nil
	}}

	llm, err := NewWithOptions(config.LLMConfig{
		Protocol: config.ProtocolOpenAI,
		Model:    "test",
		BaseURL:  "https://unvalidated.example.invalid/ignored",
		APIKey:   "sk-test-secret",
	}, ProviderOptions{Endpoint: endpoint, Client: client, RuntimeRedactor: redact.NewRuntimeRedactor()})
	if err != nil {
		t.Fatalf("construct provider with controlled client: %v", err)
	}
	stream, err := llm.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("start controlled OpenAI stream: %v", err)
	}
	for range stream.Events() {
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close controlled OpenAI stream: %v", err)
	}
	if requestedURL != "http://127.0.0.1:18443/provider/v1/chat/completions" {
		t.Fatalf("OpenAI request URL = %q, want validated endpoint", requestedURL)
	}
	if client.doCalls.Load() != 1 || client.sdkCalls.Load() != 0 {
		t.Fatalf("OpenAI client calls = Do:%d SDK:%d", client.doCalls.Load(), client.sdkCalls.Load())
	}
	if client.closeCalls.Load() != 0 {
		t.Fatal("borrowed Provider client was closed by the Provider")
	}
	if _, err := NewWithOptions(config.LLMConfig{Protocol: config.ProtocolOpenAI}, ProviderOptions{}); err == nil {
		t.Fatal("Factory created an implicit default HTTP client")
	}
	if _, err := NewWithOptions(config.LLMConfig{Protocol: config.ProtocolOpenAI}, ProviderOptions{Endpoint: endpoint, Client: client}); err == nil {
		t.Fatal("Factory created a Provider without the process RuntimeRedactor")
	}

	client.CloseIdleConnections()
	if client.closeCalls.Load() != 1 {
		t.Fatal("client creator could not perform the single ownership close")
	}
}

func TestAnthropicKeepsGuardedSDKClient(t *testing.T) {
	endpoint := validatedProviderEndpoint(t, "http://127.0.0.1:18444/provider")
	var roundTrips atomic.Int32
	var redirects atomic.Int32
	bridge := &http.Client{
		Transport: providerFactoryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			if roundTrips.Add(1) != 1 {
				return nil, errors.New("guarded SDK transport followed a rejected redirect")
			}
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Status:     "307 Temporary Redirect",
				Header:     http.Header{"Location": []string{"http://127.0.0.1:18445/escape"}},
				Body:       io.NopCloser(strings.NewReader("redirect rejected")),
				Request:    request,
			}, nil
		}),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			redirects.Add(1)
			return http.ErrUseLastResponse
		},
	}
	client := &trackingProviderPolicyClient{
		sdk: bridge,
		do: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("Anthropic bypassed SDKHTTPClient")
		},
	}

	llm, err := NewWithOptions(config.LLMConfig{
		Protocol: config.ProtocolAnthropic,
		Model:    "test",
		BaseURL:  "https://unvalidated.example.invalid/ignored",
		APIKey:   "sk-test-secret",
	}, ProviderOptions{Endpoint: endpoint, Client: client, RuntimeRedactor: redact.NewRuntimeRedactor()})
	if err != nil {
		t.Fatalf("construct Anthropic provider: %v", err)
	}
	stream, err := llm.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("start Anthropic stream: %v", err)
	}
	for range stream.Events() {
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close Anthropic stream: %v", err)
	}
	if client.sdkCalls.Load() != 1 || client.doCalls.Load() != 0 {
		t.Fatalf("Anthropic client calls = SDK:%d Do:%d", client.sdkCalls.Load(), client.doCalls.Load())
	}
	if roundTrips.Load() != 1 || redirects.Load() != 1 {
		t.Fatalf("SDK bridge policy calls = RoundTrip:%d Redirect:%d", roundTrips.Load(), redirects.Load())
	}
	if client.closeCalls.Load() != 0 {
		t.Fatal("Anthropic closed its borrowed netpolicy client")
	}
}

func TestFactoryUsesInjectedRuntimeRedactor(t *testing.T) {
	const lateSecret = "assembly-provider-late-secret-canary-82d1"
	endpoint := validatedProviderEndpoint(t, "http://127.0.0.1:18446/provider/v1")
	client := &trackingProviderPolicyClient{do: func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				`data: {"choices":[{"delta":{"content":"` + lateSecret + `"}}]}` + "\n\n" +
					"data: [DONE]\n\n",
			)),
			Request: request,
		}, nil
	}}
	runtimeRedactor := redact.NewRuntimeRedactor()
	llm, err := NewWithOptions(config.LLMConfig{
		Protocol: config.ProtocolOpenAI,
		Model:    "test",
		APIKey:   "factory-runtime-redactor-key",
	}, ProviderOptions{Endpoint: endpoint, Client: client, RuntimeRedactor: runtimeRedactor})
	if err != nil {
		t.Fatalf("construct provider with runtime redactor: %v", err)
	}
	openAI, ok := llm.(*OpenAIProvider)
	if !ok || openAI.redactor != runtimeRedactor {
		t.Fatal("Factory did not retain the injected RuntimeRedactor identity")
	}

	// Registration after construction proves the Provider retained the same
	// process object instead of copying secrets into a private redactor.
	runtimeRedactor.RegisterSecret(lateSecret)
	stream, err := llm.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("start provider stream: %v", err)
	}
	var published string
	for event := range stream.Events() {
		if event.Type == StreamEventTextDelta {
			published += event.Delta.Text()
		}
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close provider stream: %v", err)
	}
	if strings.Contains(published, lateSecret) || !strings.Contains(published, "[redacted]") {
		t.Fatalf("Provider did not use the live injected RuntimeRedactor: %q", published)
	}
}

func validatedProviderEndpoint(t *testing.T, rawURL string) netpolicy.Endpoint {
	t.Helper()
	endpoint, err := netpolicy.NewPolicy().ValidateInitial(context.Background(), rawURL, netpolicy.PurposeProvider)
	if err != nil {
		t.Fatalf("validate Provider endpoint: %v", err)
	}
	return endpoint
}

type trackingProviderPolicyClient struct {
	do  func(*http.Request) (*http.Response, error)
	sdk *http.Client

	doCalls    atomic.Int32
	sdkCalls   atomic.Int32
	closeCalls atomic.Int32
}

func (c *trackingProviderPolicyClient) Do(request *http.Request) (*http.Response, error) {
	c.doCalls.Add(1)
	if c.do == nil {
		return nil, errors.New("controlled Do is unavailable")
	}
	return c.do(request)
}

func (c *trackingProviderPolicyClient) SDKHTTPClient() *http.Client {
	c.sdkCalls.Add(1)
	return c.sdk
}

func (c *trackingProviderPolicyClient) CloseIdleConnections() {
	c.closeCalls.Add(1)
}

type providerFactoryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f providerFactoryRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type providerFactoryDiagnosticSink struct{}

func (*providerFactoryDiagnosticSink) Add(diagnostics.SanitizeInput) {}

var _ diagnostics.BoundedSink = (*providerFactoryDiagnosticSink)(nil)
