package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

func safeText(value string) redact.SafeText {
	return redact.NewRuntimeRedactor().Redact(value)
}

func TestProviderDTOAcceptsOnlySafeText(t *testing.T) {
	safeTextType := reflect.TypeOf(redact.SafeText{})
	messageType := reflect.TypeOf(ModelMessage{})
	for _, fieldName := range []string{"Content", "ArgumentsJSON", "ToolResult"} {
		field, ok := messageType.FieldByName(fieldName)
		if !ok || field.Type != safeTextType {
			t.Fatalf("ModelMessage.%s type = %v, want redact.SafeText", fieldName, field.Type)
		}
	}

	requestType := reflect.TypeOf(ChatRequest{})
	messages, ok := requestType.FieldByName("Messages")
	if !ok || messages.Type != reflect.TypeOf([]ModelMessage{}) {
		t.Fatalf("ChatRequest.Messages type = %v, want []provider.ModelMessage", messages.Type)
	}
	if _, ok := requestType.FieldByName("SystemPrompt"); ok {
		t.Fatal("ChatRequest still exposes a plain-string SystemPrompt")
	}

	blockContent, ok := reflect.TypeOf(SystemBlock{}).FieldByName("Content")
	if !ok || blockContent.Type != safeTextType {
		t.Fatalf("SystemBlock.Content type = %v, want redact.SafeText", blockContent.Type)
	}

	eventType := reflect.TypeOf(StreamEvent{})
	wantFields := map[string]reflect.Type{
		"Type":     reflect.TypeOf(StreamEventType("")),
		"Delta":    safeTextType,
		"Error":    reflect.TypeOf((*diagnostics.SafeError)(nil)),
		"Usage":    reflect.TypeOf((*Usage)(nil)),
		"ToolCall": reflect.TypeOf((*SafeToolCall)(nil)),
	}
	if eventType.NumField() != len(wantFields) {
		t.Fatalf("StreamEvent has %d fields, want only the five safe DTO fields", eventType.NumField())
	}
	for name, want := range wantFields {
		field, ok := eventType.FieldByName(name)
		if !ok || field.Type != want {
			t.Fatalf("StreamEvent.%s type = %v, want %v", name, field.Type, want)
		}
	}
}

func TestSafeProviderErrorHandlesNilRedactor(t *testing.T) {
	safeErr := safeProviderError(nil, "provider_unavailable", "provider.test", errors.New("token=secret-value"), false)
	if safeErr == nil || safeErr.Code != "provider_unavailable" || safeErr.Source != "provider.test" {
		t.Fatalf("safe provider error = %#v", safeErr)
	}
	if strings.Contains(safeErr.Error(), "secret-value") {
		t.Fatalf("nil-redactor fallback leaked error detail: %q", safeErr.Error())
	}
}

func TestUnsafeToolArgumentsAreRejected(t *testing.T) {
	client := &http.Client{Transport: observerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_unsafe\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\\\"value\\\":\\\"secret\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
					"data: [DONE]\n\n")),
			Request: request,
		}, nil
	})}
	provider := NewOpenAI(config.LLMConfig{
		Model:   "test",
		BaseURL: "https://example.test",
		APIKey:  `value":"secret`,
	}, borrowProviderTestClient(client), providerTestRuntimeRedactor())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}

	var safeErr *diagnostics.SafeError
	for event := range stream.Events() {
		if event.Type == StreamEventToolCall {
			t.Fatalf("unsafe tool arguments published an executable call: %#v", event.ToolCall)
		}
		if event.Type == StreamEventError {
			safeErr = event.Error
		}
	}
	if safeErr == nil || safeErr.Code != "unsafe_tool_arguments" {
		t.Fatalf("safe error = %#v, want unsafe_tool_arguments", safeErr)
	}
}
