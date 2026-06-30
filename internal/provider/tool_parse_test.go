package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xagent/internal/config"
	"xagent/internal/tool"
)

func TestOpenAIProviderParsesToolCallDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\\\"path\\\"\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":\\\"go.mod\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	registry, err := tool.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: server.URL, APIKey: "test"}, server.Client())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{ToolDefs: registry})
	if err != nil {
		t.Fatal(err)
	}

	var call *tool.Call
	for event := range stream {
		if event.Type == StreamEventToolCall {
			call = event.ToolCall
		}
		if event.Type == StreamEventError {
			t.Fatal(event.Err)
		}
	}
	if call == nil {
		t.Fatal("expected tool call")
	}
	if call.ID != "call_1" || call.Name != "Read" || call.ArgumentsJSON != `{"path":"go.mod"}` {
		t.Fatalf("unexpected call: %#v", call)
	}
}

func TestOpenAIProviderRejectsMultipleToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"Read\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"Glob\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: server.URL, APIKey: "test"}, server.Client())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for event := range stream {
		if event.Type == StreamEventError && event.Err != nil {
			got = event.Err.Error()
		}
	}
	if !strings.Contains(got, tool.ErrMultipleToolCallsUnsupported) {
		t.Fatalf("expected multiple tool call error, got %q", got)
	}
}
