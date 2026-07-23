package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"xagent/internal/config"
)

func TestSystemBlockSelection(t *testing.T) {
	tests := []struct {
		name string
		req  ChatRequest
		want []string
	}{
		{
			name: "ordered wins",
			req: ChatRequest{
				System:        []SystemBlock{{Name: "ordered-1", Content: "one"}, {Name: "ordered-2", Content: "two"}},
				StableSystem:  []SystemBlock{{Content: "stable"}},
				DynamicSystem: []SystemBlock{{Content: "dynamic"}},
				SystemPrompt:  "legacy",
			},
			want: []string{"one", "two"},
		},
		{
			name: "stable dynamic fallback",
			req: ChatRequest{
				StableSystem:  []SystemBlock{{Content: "stable"}},
				DynamicSystem: []SystemBlock{{Content: "dynamic"}},
				SystemPrompt:  "legacy",
			},
			want: []string{"stable", "dynamic"},
		},
		{
			name: "legacy fallback",
			req:  ChatRequest{SystemPrompt: " legacy "},
			want: []string{"legacy"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks := systemBlocks(tt.req)
			got := make([]string, 0, len(blocks))
			for _, block := range blocks {
				got = append(got, block.Content)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("system selection = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestOpenAIOrderedSystemBlocks(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if err := json.Unmarshal(data, &requestBody); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	localCanary := "/Users/private/project/.xagent/hooks.yaml#7"
	provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: server.URL, APIKey: "test"}, server.Client())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{
		System: []SystemBlock{
			{Name: "fixed", Content: "fixed rules", Cacheable: true},
			{Name: localCanary, Content: "hook rules"},
			{Name: "runtime", Content: "runtime reminder"},
		},
		StableSystem: []SystemBlock{{Content: "LEGACY STABLE CANARY"}},
		SystemPrompt: "LEGACY PROMPT CANARY",
	})
	if err != nil {
		t.Fatal(err)
	}
	for event := range stream {
		if event.Type == StreamEventError {
			t.Fatal(event.Err)
		}
	}
	messages, ok := requestBody["messages"].([]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("ordered system messages = %#v", requestBody["messages"])
	}
	for index, want := range []string{"fixed rules", "hook rules", "runtime reminder"} {
		message, ok := messages[index].(map[string]any)
		if !ok || message["role"] != "system" || message["content"] != want {
			t.Fatalf("message %d = %#v, want system/%q", index, messages[index], want)
		}
	}
	wire, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{localCanary, "LEGACY STABLE CANARY", "LEGACY PROMPT CANARY", `"name"`, `"cacheable"`} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("local/legacy metadata leaked onto OpenAI wire (%q): %s", forbidden, wire)
		}
	}
}

func TestAnthropicHookCacheBoundary(t *testing.T) {
	localCanary := "/Users/private/project/.xagent/hooks.yaml#2"
	req := ChatRequest{
		System: []SystemBlock{
			{Name: "fixed-first", Content: "fixed one", Cacheable: true},
			{Name: "fixed-last", Content: "fixed two", Cacheable: true},
			{Name: localCanary, Content: "hook", Cacheable: false},
			{Name: "fixed-last", Content: "optional duplicate name", Cacheable: true},
		},
		Tools: []ToolDefinition{{Name: "Read", Description: "read"}},
		Cache: CachePolicy{
			EnablePromptCache:    true,
			SystemBreakpointName: "fixed-last",
			CacheTools:           false,
		},
	}
	blocks := toAnthropicSystemBlocks(req)
	if len(blocks) != 4 {
		t.Fatalf("Anthropic system blocks = %#v", blocks)
	}
	for index, block := range blocks {
		cached := block.CacheControl.Type != ""
		if cached != (index == 1) {
			t.Fatalf("block %d cacheable=%v, want only index 1: %#v", index, cached, blocks)
		}
	}
	tools := toAnthropicTools(req)
	if len(tools) != 1 || tools[0].OfTool == nil {
		t.Fatalf("Anthropic tools = %#v", tools)
	}
	if tools[0].OfTool.CacheControl.Type != "" {
		t.Fatalf("ordered Hook request cached tool definition: %#v", tools[0].OfTool)
	}
	wire, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{localCanary, "fixed-last", `"name"`, `"cacheable"`} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("local block metadata leaked onto Anthropic wire (%q): %s", forbidden, wire)
		}
	}
}
