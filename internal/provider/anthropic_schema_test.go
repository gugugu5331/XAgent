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

func TestAnthropicRequestUsesSafeDTO(t *testing.T) {
	metadataCanary := "/private/project/.xagent/hooks.yaml#anthropic"
	params, err := anthropicMessageParams("fallback-model", ChatRequest{
		Model: "  request-model  ",
		System: []SystemBlock{
			{Name: "fixed", Content: safeText("fixed rules"), Cacheable: true},
			{Name: metadataCanary, Content: safeText("runtime rules"), Cacheable: true},
		},
		Messages: []ModelMessage{
			{Role: ModelMessageRoleUser, Content: safeText("user message")},
			{Role: ModelMessageRoleAssistant, Content: safeText("assistant message")},
			{Role: ModelMessageRoleToolCall, ToolCallID: "call-1", ToolName: "Read", ArgumentsJSON: safeText(`{"path":"go.mod"}`)},
			{Role: ModelMessageRoleToolResult, ToolCallID: "call-1", ToolName: "Read", ToolResult: safeText("tool result"), ToolResultStatus: string(tool.StatusError)},
			{Role: ModelMessageRoleContextSummary, Content: safeText("summary")},
			{Role: ModelMessageRoleContextBoundary, Content: safeText("boundary")},
		},
		Thinking: config.ThinkingConfig{Enabled: true, Show: true},
		Tools: []ToolDefinition{
			{Name: "Read", Description: "read", Schema: tool.Schema{Type: "object"}},
			{Name: "Write", Description: "write", Schema: tool.Schema{Type: "object"}},
		},
		Cache: CachePolicy{
			EnablePromptCache:    true,
			SystemBreakpointName: metadataCanary,
			CacheTools:           true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	wire, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal Anthropic request: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatalf("decode Anthropic request: %v", err)
	}
	if body["model"] != "request-model" || body["max_tokens"] != float64(64000) {
		t.Fatalf("request model/max_tokens = %#v/%#v", body["model"], body["max_tokens"])
	}

	system, ok := body["system"].([]any)
	if !ok || len(system) != 2 {
		t.Fatalf("system = %#v, want two safe blocks", body["system"])
	}
	firstSystem := system[0].(map[string]any)
	secondSystem := system[1].(map[string]any)
	if firstSystem["text"] != "fixed rules" || firstSystem["cache_control"] != nil {
		t.Fatalf("first system block = %#v", firstSystem)
	}
	if secondSystem["text"] != "runtime rules" || secondSystem["cache_control"] == nil {
		t.Fatalf("cache breakpoint system block = %#v", secondSystem)
	}

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 6 {
		t.Fatalf("messages = %#v, want six safe messages", body["messages"])
	}
	assertAnthropicTextMessage(t, messages[0], "user", "user message")
	assertAnthropicTextMessage(t, messages[1], "assistant", "assistant message")
	toolCallContent := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolCallContent["type"] != "tool_use" || toolCallContent["id"] != "call-1" || toolCallContent["name"] != "Read" {
		t.Fatalf("tool call block = %#v", toolCallContent)
	}
	if !jsonValuesEqual(toolCallContent["input"], map[string]any{"path": "go.mod"}) {
		t.Fatalf("tool arguments = %#v", toolCallContent["input"])
	}
	toolResultContent := messages[3].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolResultContent["type"] != "tool_result" || toolResultContent["tool_use_id"] != "call-1" || toolResultContent["is_error"] != true {
		t.Fatalf("tool result block = %#v", toolResultContent)
	}
	resultText := toolResultContent["content"].([]any)[0].(map[string]any)
	if resultText["text"] != "tool result" {
		t.Fatalf("tool result content = %#v", resultText)
	}
	assertAnthropicTextMessage(t, messages[4], "user", "summary")
	assertAnthropicTextMessage(t, messages[5], "user", "boundary")

	thinking, ok := body["thinking"].(map[string]any)
	if !ok || thinking["type"] != "adaptive" || thinking["display"] != "summarized" {
		t.Fatalf("thinking = %#v", body["thinking"])
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v, want two definitions", body["tools"])
	}
	if tools[0].(map[string]any)["cache_control"] != nil || tools[1].(map[string]any)["cache_control"] == nil {
		t.Fatalf("tool cache boundary = %#v", tools)
	}

	for _, forbidden := range []string{
		metadataCanary,
		"legacy system must not win",
		"legacy dynamic must not win",
		`"cacheable"`,
		`"system_breakpoint"`,
	} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("Anthropic request leaked local metadata %q: %s", forbidden, wire)
		}
	}
}

func TestAnthropicSchemaCompatibility(t *testing.T) {
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
	overridden, err := anthropicMessageParams("configured-model", ChatRequest{
		Model: "  skill-model  ",
		Tools: []ToolDefinition{{
			Name:        "Read",
			Description: "read",
			Schema:      tool.Schema{Type: "object"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(overridden.Model) != "skill-model" {
		t.Fatalf("request model did not override config: %q", overridden.Model)
	}
	if len(overridden.Tools) != 1 || overridden.Tools[0].OfTool == nil || overridden.Tools[0].OfTool.Name != "Read" {
		t.Fatalf("model override dropped tools: %#v", overridden.Tools)
	}

	defaulted, err := anthropicMessageParams("configured-model", ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if string(defaulted.Model) != "configured-model" {
		t.Fatalf("empty request model did not restore config model: %q", defaulted.Model)
	}
	fallback, err := anthropicMessageParams("", ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
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
	provider := NewAnthropic(config.LLMConfig{Model: "configured-model", BaseURL: "https://example.test", APIKey: "test"}, borrowProviderTestClient(client), providerTestRuntimeRedactor())
	for _, request := range []ChatRequest{
		{Model: "skill-model", Tools: []ToolDefinition{{Name: "Read", Description: "read", Schema: tool.Schema{Type: "object"}}}},
		{},
	} {
		stream, err := provider.StreamChat(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		for range stream.Events() {
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

func assertAnthropicTextMessage(t *testing.T, raw any, role, content string) {
	t.Helper()
	message, ok := raw.(map[string]any)
	if !ok || message["role"] != role {
		t.Fatalf("message = %#v, want role %q", raw, role)
	}
	blocks, ok := message["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("message content = %#v, want one block", message["content"])
	}
	block, ok := blocks[0].(map[string]any)
	if !ok || block["type"] != "text" || block["text"] != content {
		t.Fatalf("message block = %#v, want text %q", blocks[0], content)
	}
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
