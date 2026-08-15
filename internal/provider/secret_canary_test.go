package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

const providerSecretCanary = "sk-ant-provider-secret-canary-7f31"

func TestOpenAIProviderSecretCanary(t *testing.T) {
	var authorizationSeen bool
	body := newTrackingOpenAIBody(strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"answer ` + providerSecretCanary + `","tool_calls":[{"index":0,"id":"call-1","function":{"name":"Read","arguments":"{\"token\":\"` + providerSecretCanary + `\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")))
	client := &http.Client{Transport: providerCanaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		authorizationSeen = request.Header.Get("Authorization") == "Bearer "+providerSecretCanary
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    request,
		}, nil
	})}
	provider := NewOpenAI(config.LLMConfig{
		Model:   "test",
		BaseURL: "https://example.test",
		APIKey:  providerSecretCanary,
	}, borrowProviderTestClient(client), providerTestRuntimeRedactor())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("start OpenAI canary stream: %v", err)
	}
	events := collectProviderCanaryEvents(t, stream)
	closeTrackingOpenAIStream(t, stream)
	body.assertClosedOnce(t)
	if !authorizationSeen {
		t.Fatal("OpenAI request did not use the configured API key")
	}
	if len(events) != 2 || events[0].Type != StreamEventTextDelta || events[1].Type != StreamEventToolCall {
		t.Fatalf("OpenAI canary events = %#v, want text and tool call", events)
	}
	assertProviderCanaryAbsent(t, "OpenAI events", events)

	httpBody := newTrackingOpenAIBody(strings.NewReader("upstream rejected " + providerSecretCanary))
	httpClient := &http.Client{Transport: providerCanaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Header:     make(http.Header),
			Body:       httpBody,
			Request:    request,
		}, nil
	})}
	httpProvider := NewOpenAI(config.LLMConfig{
		Model:   "test",
		BaseURL: "https://example.test",
		APIKey:  providerSecretCanary,
	}, borrowProviderTestClient(httpClient), providerTestRuntimeRedactor())
	_, httpErr := httpProvider.StreamChat(context.Background(), ChatRequest{})
	if httpErr == nil {
		t.Fatal("OpenAI canary HTTP error was not surfaced")
	}
	httpBody.assertClosedOnce(t)
	assertProviderCanaryAbsent(t, "OpenAI HTTP SafeError", httpErr)

	assertProviderCanaryDiagnostics(t, events, httpErr)
}

func TestAnthropicProviderSecretCanary(t *testing.T) {
	var apiKeySeen bool
	body := newTrackingAnthropicBody(strings.NewReader(
		anthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-1","name":"Read","input":{}}}`) +
			anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer `+providerSecretCanary+`"}}`) +
			anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"token\":\"`+providerSecretCanary+`\"}"}}`) +
			anthropicSSE("message_stop", `{"type":"message_stop"}`),
	))
	client := &http.Client{Transport: providerCanaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		apiKeySeen = request.Header.Get("X-Api-Key") == providerSecretCanary
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    request,
		}, nil
	})}
	provider := NewAnthropic(config.LLMConfig{
		Model:   "test",
		BaseURL: "https://example.test",
		APIKey:  providerSecretCanary,
	}, borrowProviderTestClient(client), providerTestRuntimeRedactor())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("start Anthropic canary stream: %v", err)
	}
	events := collectProviderCanaryEvents(t, stream)
	closeTrackingAnthropicStream(t, stream)
	body.assertClosedOnce(t)
	if !apiKeySeen {
		t.Fatal("Anthropic request did not use the configured API key")
	}
	if len(events) != 2 || events[0].Type != StreamEventTextDelta || events[1].Type != StreamEventToolCall {
		t.Fatalf("Anthropic canary events = %#v, want text and tool call", events)
	}
	assertProviderCanaryAbsent(t, "Anthropic events", events)

	errorBody := newTrackingAnthropicBody(strings.NewReader(anthropicSSE(
		"error",
		`{"type":"error","error":{"type":"api_error","message":"SDK rejected `+providerSecretCanary+`"}}`,
	)))
	errorClient := &http.Client{Transport: providerCanaryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       errorBody,
			Request:    request,
		}, nil
	})}
	errorProvider := NewAnthropic(config.LLMConfig{
		Model:   "test",
		BaseURL: "https://example.test",
		APIKey:  providerSecretCanary,
	}, borrowProviderTestClient(errorClient), providerTestRuntimeRedactor())
	errorStream, err := errorProvider.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("start Anthropic SDK error stream: %v", err)
	}
	errorEvents := collectProviderCanaryEvents(t, errorStream)
	closeTrackingAnthropicStream(t, errorStream)
	errorBody.assertClosedOnce(t)
	if len(errorEvents) != 1 || errorEvents[0].Type != StreamEventError || errorEvents[0].Error == nil {
		t.Fatalf("Anthropic SDK error events = %#v", errorEvents)
	}
	assertProviderCanaryAbsent(t, "Anthropic SDK SafeError", errorEvents)

	assertProviderCanaryDiagnostics(t, append(events, errorEvents...), errorEvents[0].Error)
}

func collectProviderCanaryEvents(t *testing.T, stream ChatStream) []StreamEvent {
	t.Helper()
	var events []StreamEvent
	for event := range stream.Events() {
		assertProviderCanaryAbsent(t, "published Provider event", event)
		events = append(events, event)
	}
	return events
}

func assertProviderCanaryDiagnostics(t *testing.T, events []StreamEvent, providerErr error) {
	t.Helper()
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(providerSecretCanary)
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      8,
		MaxItemBytes:  2048,
		MaxTotalBytes: 8192,
	})
	if err != nil {
		t.Fatalf("create Provider canary diagnostics: %v", err)
	}
	sink.Add(diagnostics.SanitizeInput{
		Code:     "provider_canary_error",
		Source:   "provider",
		Hint:     "transport " + providerSecretCanary,
		Severity: diagnostics.SeverityError,
		Err:      fmt.Errorf("provider boundary %s: %w", providerSecretCanary, providerErr),
	})
	for _, event := range events {
		if event.Error != nil {
			sink.Add(diagnostics.SanitizeInput{
				Code:     event.Error.Code,
				Source:   event.Error.Source,
				Severity: diagnostics.SeverityError,
				Err:      event.Error,
			})
		}
	}
	snapshot := sink.Snapshot()
	if len(snapshot.Items()) == 0 {
		t.Fatal("Provider canary diagnostics snapshot is empty")
	}
	assertProviderCanaryAbsent(t, "Provider diagnostics snapshot", snapshot.Items())

	var output bytes.Buffer
	if _, err := fmt.Fprintf(&output, "events=%#v error=%v diagnostics=%#v", events, providerErr, snapshot.Items()); err != nil {
		t.Fatalf("format Provider canary output: %v", err)
	}
	assertProviderCanaryAbsent(t, "formatted Provider output", output.String())
}

func assertProviderCanaryAbsent(t *testing.T, channel string, value any) {
	t.Helper()
	if strings.Contains(fmt.Sprintf("%#v", value), providerSecretCanary) {
		t.Fatalf("%s retained the Provider secret canary", channel)
	}
}

type providerCanaryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f providerCanaryRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	if f == nil {
		return nil, errors.New("Provider canary transport is unavailable")
	}
	return f(request)
}
