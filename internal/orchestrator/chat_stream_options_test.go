package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/provider"
)

func TestOrchestratorForwardsChatStreamLifecycleOptions(t *testing.T) {
	sink := &orchestratorStreamDiagnosticSink{}
	providerRecorder := &orchestratorStreamOptionsProvider{}
	wantTimeout := 137 * time.Millisecond
	orch := NewWithOptions(OrchestratorOptions{
		Provider:             providerRecorder,
		CleanupTimeout:       wantTimeout,
		LifecycleDiagnostics: sink,
	})

	stream, err := orch.streamChat(context.Background(), provider.ChatRequest{})
	if err != nil {
		t.Fatalf("stream Provider with C7 options: %v", err)
	}
	if stream != nil {
		t.Fatalf("recording Provider stream = %T, want nil fixture result", stream)
	}
	options, optionCalls, legacyCalls := providerRecorder.snapshot()
	if optionCalls != 1 || legacyCalls != 0 {
		t.Fatalf("Provider calls = options:%d legacy:%d, want 1/0", optionCalls, legacyCalls)
	}
	if options.CleanupTimeout != wantTimeout || options.Diagnostics != sink {
		t.Fatalf("ChatStreamOptions = %#v, want timeout %s and shared sink %p", options, wantTimeout, sink)
	}
}

func TestOrchestratorKeepsLegacyProviderCompatibility(t *testing.T) {
	providerRecorder := &orchestratorLegacyProvider{}
	orch := NewWithOptions(OrchestratorOptions{
		Provider:             providerRecorder,
		CleanupTimeout:       time.Second,
		LifecycleDiagnostics: &orchestratorStreamDiagnosticSink{},
	})
	if _, err := orch.streamChat(context.Background(), provider.ChatRequest{}); err != nil {
		t.Fatalf("stream legacy Provider: %v", err)
	}
	if providerRecorder.calls != 1 {
		t.Fatalf("legacy StreamChat calls = %d, want one", providerRecorder.calls)
	}
}

type orchestratorStreamOptionsProvider struct {
	mu          sync.Mutex
	options     provider.ChatStreamOptions
	optionCalls int
	legacyCalls int
}

func (*orchestratorStreamOptionsProvider) Name() string { return "stream-options" }

func (recorder *orchestratorStreamOptionsProvider) StreamChat(context.Context, provider.ChatRequest) (provider.ChatStream, error) {
	recorder.mu.Lock()
	recorder.legacyCalls++
	recorder.mu.Unlock()
	return nil, nil
}

func (recorder *orchestratorStreamOptionsProvider) StreamChatWithOptions(
	_ context.Context,
	_ provider.ChatRequest,
	options provider.ChatStreamOptions,
) (provider.ChatStream, error) {
	recorder.mu.Lock()
	recorder.options = options
	recorder.optionCalls++
	recorder.mu.Unlock()
	return nil, nil
}

func (recorder *orchestratorStreamOptionsProvider) snapshot() (provider.ChatStreamOptions, int, int) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.options, recorder.optionCalls, recorder.legacyCalls
}

type orchestratorLegacyProvider struct {
	calls int
}

func (*orchestratorLegacyProvider) Name() string { return "legacy" }

func (recorder *orchestratorLegacyProvider) StreamChat(context.Context, provider.ChatRequest) (provider.ChatStream, error) {
	recorder.calls++
	return nil, nil
}

type orchestratorStreamDiagnosticSink struct{}

func (*orchestratorStreamDiagnosticSink) Add(diagnostics.SanitizeInput) {}

var _ provider.Provider = (*orchestratorStreamOptionsProvider)(nil)
var _ provider.ChatStreamOptionsProvider = (*orchestratorStreamOptionsProvider)(nil)
var _ provider.Provider = (*orchestratorLegacyProvider)(nil)
var _ diagnostics.BoundedSink = (*orchestratorStreamDiagnosticSink)(nil)
