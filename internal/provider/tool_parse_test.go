package provider

import (
	"context"
	"encoding/json"
	"io"
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

func TestOpenAIProviderEmitsMultipleToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"Glob\",\"arguments\":\"{}\"}},{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"Read\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: server.URL, APIKey: "test"}, server.Client())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var calls []tool.Call
	for event := range stream {
		if event.Type == StreamEventToolCall {
			calls = event.ToolCalls
		}
		if event.Type == StreamEventError {
			t.Fatal(event.Err)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("expected two tool calls, got %#v", calls)
	}
	if calls[0].ID != "a" || calls[0].Name != "Read" || calls[1].ID != "b" || calls[1].Name != "Glob" {
		t.Fatalf("unexpected calls: %#v", calls)
	}
}

func TestOpenAIProviderEmitsUsage(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &requestBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":34}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: server.URL, APIKey: "test"}, server.Client())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var usage *Usage
	for event := range stream {
		if event.Type == StreamEventUsage {
			usage = event.Usage
		}
		if event.Type == StreamEventError {
			t.Fatal(event.Err)
		}
	}
	if usage == nil || usage.InputTokens != 12 || usage.OutputTokens != 34 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
	streamOptions, ok := requestBody["stream_options"].(map[string]any)
	if !ok || streamOptions["include_usage"] != true {
		t.Fatalf("expected include_usage stream option, got %#v", requestBody)
	}
}
func TestAnthropicToolCallsAreSorted(t *testing.T) {
	calls := anthropicToolCalls(map[int64]*anthropicToolCallState{
		2: {ID: "b", Name: "Glob"},
		1: {ID: "a", Name: "Read"},
	})
	if len(calls) != 2 {
		t.Fatalf("expected two calls, got %#v", calls)
	}
	if calls[0].ID != "a" || calls[0].Name != "Read" || calls[1].ID != "b" || calls[1].Name != "Glob" {
		t.Fatalf("unexpected calls: %#v", calls)
	}
}

func TestOpenAIProviderMergesSystemBlocksWithoutAnthropicFields(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &requestBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: server.URL, APIKey: "test"}, server.Client())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{
		StableSystem:  []SystemBlock{{Name: "stable", Content: "stable rules", Cacheable: true}},
		DynamicSystem: []SystemBlock{{Name: "dynamic", Content: "dynamic reminder"}},
		Cache:         CachePolicy{EnablePromptCache: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for event := range stream {
		if event.Type == StreamEventError {
			t.Fatal(event.Err)
		}
	}
	data, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Contains(body, "cache_control") || strings.Contains(body, "anthropic_beta") {
		t.Fatalf("OpenAI request leaked Anthropic fields: %s", body)
	}
	messages, ok := requestBody["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("missing messages: %#v", requestBody)
	}
	first, ok := messages[0].(map[string]any)
	if !ok || first["role"] != "system" {
		t.Fatalf("first message is not system: %#v", messages[0])
	}
	content, _ := first["content"].(string)
	if !strings.Contains(content, "stable rules") || !strings.Contains(content, "dynamic reminder") {
		t.Fatalf("system message missing blocks: %q", content)
	}
}

func TestAnthropicSystemBlocksCacheControl(t *testing.T) {
	blocks := toAnthropicSystemBlocks(ChatRequest{
		StableSystem: []SystemBlock{
			{Name: "stable-1", Content: "stable one", Cacheable: true},
			{Name: "stable-2", Content: "stable two", Cacheable: true},
		},
		DynamicSystem: []SystemBlock{{Name: "dynamic", Content: "dynamic reminder"}},
		Cache:         CachePolicy{EnablePromptCache: true},
	})
	if len(blocks) != 3 {
		t.Fatalf("unexpected blocks: %#v", blocks)
	}
	if blocks[0].Text != "stable one" || blocks[1].Text != "stable two" || blocks[2].Text != "dynamic reminder" {
		t.Fatalf("unexpected block order: %#v", blocks)
	}
	if blocks[0].CacheControl.Type != "" {
		t.Fatalf("first stable block should not have cache control: %#v", blocks[0].CacheControl)
	}
	if blocks[1].CacheControl.Type == "" {
		t.Fatalf("last stable block missing cache control: %#v", blocks[1])
	}
	if blocks[2].CacheControl.Type != "" {
		t.Fatalf("dynamic block should not have cache control: %#v", blocks[2].CacheControl)
	}
}

func TestAnthropicToolsCacheControl(t *testing.T) {
	defs := []ToolDefinition{
		{Name: "Read", Description: "read", Schema: tool.Schema{Type: "object"}},
		{Name: "Write", Description: "write", Schema: tool.Schema{Type: "object"}},
	}
	tools := toAnthropicTools(ChatRequest{Tools: defs, Cache: CachePolicy{EnablePromptCache: true}})
	if len(tools) != 2 {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	if tools[0].OfTool.CacheControl.Type != "" {
		t.Fatalf("first tool should not have cache control: %#v", tools[0].OfTool.CacheControl)
	}
	if tools[1].OfTool.CacheControl.Type == "" {
		t.Fatalf("last tool missing cache control: %#v", tools[1].OfTool)
	}
}

func TestSystemPromptCompatibilityDoesNotDuplicateBlocks(t *testing.T) {
	content := joinedSystemBlocks(ChatRequest{
		StableSystem: []SystemBlock{{Name: "stable", Content: "new stable", Cacheable: true}},
		SystemPrompt: "legacy prompt",
	})
	if strings.Contains(content, "legacy prompt") || !strings.Contains(content, "new stable") {
		t.Fatalf("unexpected compatibility content: %q", content)
	}
}
