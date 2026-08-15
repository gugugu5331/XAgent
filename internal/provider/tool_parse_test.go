package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/tool"
)

func TestEmitStreamEventStopsWhenConsumerDisappears(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	out := make(chan StreamEvent)
	stream, err := newChatStream(out, ChatStreamOptions{}, func(context.Context) error { return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		done <- stream.emit(ctx, StreamEvent{Type: StreamEventTextDelta, Delta: safeText("blocked")})
	}()
	cancel()
	select {
	case sent := <-done:
		if sent {
			t.Fatal("event unexpectedly sent without a consumer")
		}
	case <-time.After(time.Second):
		t.Fatal("provider event sender did not stop after cancellation")
	}
}

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
	provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: server.URL, APIKey: "test"}, borrowProviderTestClient(server.Client()), providerTestRuntimeRedactor())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{ToolDefs: registry})
	if err != nil {
		t.Fatal(err)
	}

	var call *SafeToolCall
	for event := range stream.Events() {
		if event.Type == StreamEventToolCall {
			call = event.ToolCall
		}
		if event.Type == StreamEventError {
			t.Fatal(event.Error)
		}
	}
	if call == nil {
		t.Fatal("expected tool call")
	}
	if call.ID != "call_1" || call.Name != "Read" || call.ArgumentsJSON.Text() != `{"path":"go.mod"}` {
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

	provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: server.URL, APIKey: "test"}, borrowProviderTestClient(server.Client()), providerTestRuntimeRedactor())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var calls []SafeToolCall
	for event := range stream.Events() {
		if event.Type == StreamEventToolCall && event.ToolCall != nil {
			calls = append(calls, *event.ToolCall)
		}
		if event.Type == StreamEventError {
			t.Fatal(event.Error)
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

	provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: server.URL, APIKey: "test"}, borrowProviderTestClient(server.Client()), providerTestRuntimeRedactor())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var usage *Usage
	for event := range stream.Events() {
		if event.Type == StreamEventUsage {
			usage = event.Usage
		}
		if event.Type == StreamEventError {
			t.Fatal(event.Error)
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

func TestRequestModelOverrideOpenAI(t *testing.T) {
	requestBodies := make([]map[string]any, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			t.Error(err)
			return
		}
		requestBodies = append(requestBodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	provider := NewOpenAI(config.LLMConfig{Model: "configured-model", BaseURL: server.URL, APIKey: "test"}, borrowProviderTestClient(server.Client()), providerTestRuntimeRedactor())
	requests := []ChatRequest{
		{
			Model: "  skill-model  ",
			Tools: []ToolDefinition{{Name: "Read", Description: "read", Schema: tool.Schema{Type: "object"}}},
		},
		{},
	}
	for _, request := range requests {
		stream, err := provider.StreamChat(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		for event := range stream.Events() {
			if event.Type == StreamEventError {
				t.Fatal(event.Error)
			}
		}
	}
	if len(requestBodies) != 2 {
		t.Fatalf("captured %d requests, want 2", len(requestBodies))
	}
	if requestBodies[0]["model"] != "skill-model" {
		t.Fatalf("override request used wrong model: %#v", requestBodies[0])
	}
	tools, ok := requestBodies[0]["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("override request dropped tools: %#v", requestBodies[0])
	}
	if requestBodies[1]["model"] != "configured-model" {
		t.Fatalf("default request was polluted by prior override: %#v", requestBodies[1])
	}
	if provider.cfg.Model != "configured-model" {
		t.Fatalf("provider config was mutated: %#v", provider.cfg)
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
	if calls[0].id != "a" || calls[0].name != "Read" || calls[1].id != "b" || calls[1].name != "Glob" {
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

	provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: server.URL, APIKey: "test"}, borrowProviderTestClient(server.Client()), providerTestRuntimeRedactor())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{
		StableSystem:  []SystemBlock{{Name: "stable", Content: safeText("stable rules"), Cacheable: true}},
		DynamicSystem: []SystemBlock{{Name: "dynamic", Content: safeText("dynamic reminder")}},
		Cache:         CachePolicy{EnablePromptCache: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for event := range stream.Events() {
		if event.Type == StreamEventError {
			t.Fatal(event.Error)
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
			{Name: "stable-1", Content: safeText("stable one"), Cacheable: true},
			{Name: "stable-2", Content: safeText("stable two"), Cacheable: true},
		},
		DynamicSystem: []SystemBlock{{Name: "dynamic", Content: safeText("dynamic reminder")}},
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
