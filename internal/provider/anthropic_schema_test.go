package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"xagent/internal/config"
	"xagent/internal/tool"
)

func TestAnthropicToolsUseRawSchema(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"nested":{"type":"object","properties":{"items":{"type":"array","items":{"type":"number"}},"choice":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}},"required":["nested"],"additionalProperties":false}`)
	tools := toAnthropicTools(ChatRequest{Tools: []ToolDefinition{{Name: "mcp__server__tool", Description: "tool", Schema: tool.Schema{Raw: raw}}}})
	if len(tools) != 1 || tools[0].OfTool == nil {
		t.Fatalf("expected one anthropic tool: %#v", tools)
	}
	data, err := json.Marshal(tools[0].OfTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"nested", "items", "oneOf", "additionalProperties"} {
		if !jsonContains(data, want) {
			t.Fatalf("raw schema field %q missing from Anthropic schema: %s", want, data)
		}
	}
}

func TestRequestModelOverrideAnthropic(t *testing.T) {
	overridden := anthropicMessageParams("configured-model", ChatRequest{
		Model: "  skill-model  ",
		Tools: []ToolDefinition{{
			Name:        "Read",
			Description: "read",
			Schema:      tool.Schema{Type: "object"},
		}},
	})
	if string(overridden.Model) != "skill-model" {
		t.Fatalf("request model did not override config: %q", overridden.Model)
	}
	if len(overridden.Tools) != 1 || overridden.Tools[0].OfTool == nil || overridden.Tools[0].OfTool.Name != "Read" {
		t.Fatalf("model override dropped tools: %#v", overridden.Tools)
	}

	defaulted := anthropicMessageParams("configured-model", ChatRequest{})
	if string(defaulted.Model) != "configured-model" {
		t.Fatalf("empty request model did not restore config model: %q", defaulted.Model)
	}
	fallback := anthropicMessageParams("", ChatRequest{})
	if string(fallback.Model) != "claude-opus-4-7" {
		t.Fatalf("empty config fallback changed: %q", fallback.Model)
	}

	requestBodies := make([]map[string]any, 0, 2)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			return nil, err
		}
		requestBodies = append(requestBodies, body)
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","message":"test"}}`)),
			Request:    request,
		}, nil
	})}
	provider := NewAnthropic(config.LLMConfig{Model: "configured-model", BaseURL: "https://example.test", APIKey: "test"}, client)
	for _, request := range []ChatRequest{
		{Model: "skill-model", Tools: []ToolDefinition{{Name: "Read", Description: "read", Schema: tool.Schema{Type: "object"}}}},
		{},
	} {
		stream, err := provider.StreamChat(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		for range stream {
		}
	}
	if len(requestBodies) != 2 {
		t.Fatalf("captured %d Anthropic requests, want 2", len(requestBodies))
	}
	if requestBodies[0]["model"] != "skill-model" {
		t.Fatalf("serialized override missing: %#v", requestBodies[0])
	}
	if tools, ok := requestBodies[0]["tools"].([]any); !ok || len(tools) != 1 {
		t.Fatalf("serialized override dropped tools: %#v", requestBodies[0])
	}
	if requestBodies[1]["model"] != "configured-model" {
		t.Fatalf("Anthropic default request was polluted: %#v", requestBodies[1])
	}
	if provider.cfg.Model != "configured-model" {
		t.Fatalf("Anthropic provider config was mutated: %#v", provider.cfg)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func jsonContains(data []byte, value string) bool {
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return false
	}
	return containsString(decoded, value)
}

func containsString(value any, want string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if key == want || containsString(item, want) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if containsString(item, want) {
				return true
			}
		}
	case string:
		return typed == want
	}
	return false
}
