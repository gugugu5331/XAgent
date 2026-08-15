package provider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"xagent/internal/tool"
)

func TestOpenAIRequestUsesSafeDTO(t *testing.T) {
	metadataCanary := "/private/project/.xagent/hooks.yaml#openai"
	req := ChatRequest{
		Model: "  request-model  ",
		System: []SystemBlock{
			{Name: "fixed", Content: safeText("fixed rules"), Cacheable: true},
			{Name: metadataCanary, Content: safeText("runtime rules")},
		},
		StableSystem:  []SystemBlock{{Name: "legacy", Content: safeText("legacy system must not win")}},
		DynamicSystem: []SystemBlock{{Name: "legacy-dynamic", Content: safeText("legacy dynamic must not win")}},
		Messages: []ModelMessage{
			{Role: ModelMessageRoleUser, Content: safeText("user message")},
			{Role: ModelMessageRoleAssistant, Content: safeText("assistant message")},
			{Role: ModelMessageRoleToolCall, ToolCallID: "call-1", ToolName: "Read", ArgumentsJSON: safeText(`{"path":"go.mod"}`)},
			{Role: ModelMessageRoleToolResult, ToolCallID: "call-1", ToolResult: safeText("tool result")},
			{Role: ModelMessageRoleContextSummary, Content: safeText("summary")},
			{Role: ModelMessageRoleContextBoundary, Content: safeText("boundary")},
		},
		Cache: CachePolicy{
			EnablePromptCache:    true,
			SystemBreakpointName: metadataCanary,
			CacheTools:           true,
		},
	}

	wire, err := json.Marshal(newOpenAIRequest(req, "fallback-model"))
	if err != nil {
		t.Fatalf("marshal OpenAI request: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatalf("decode OpenAI request: %v", err)
	}
	if body["model"] != "request-model" || body["stream"] != true {
		t.Fatalf("request model/stream = %#v/%#v", body["model"], body["stream"])
	}
	streamOptions, ok := body["stream_options"].(map[string]any)
	if !ok || streamOptions["include_usage"] != true {
		t.Fatalf("stream options = %#v, want include_usage", body["stream_options"])
	}

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 8 {
		t.Fatalf("messages = %#v, want 8 safe wire messages", body["messages"])
	}
	assertOpenAIMessage(t, messages[0], "system", "fixed rules", "")
	assertOpenAIMessage(t, messages[1], "system", "runtime rules", "")
	assertOpenAIMessage(t, messages[2], "user", "user message", "")
	assertOpenAIMessage(t, messages[3], "assistant", "assistant message", "")
	toolCallMessage, ok := messages[4].(map[string]any)
	if !ok || toolCallMessage["role"] != "assistant" {
		t.Fatalf("tool call message = %#v", messages[4])
	}
	calls, ok := toolCallMessage["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool calls = %#v", toolCallMessage["tool_calls"])
	}
	call := calls[0].(map[string]any)
	function := call["function"].(map[string]any)
	if call["id"] != "call-1" || call["type"] != "function" || function["name"] != "Read" || function["arguments"] != `{"path":"go.mod"}` {
		t.Fatalf("tool call = %#v", call)
	}
	assertOpenAIMessage(t, messages[5], "tool", "tool result", "call-1")
	assertOpenAIMessage(t, messages[6], "user", "summary", "")
	assertOpenAIMessage(t, messages[7], "user", "boundary", "")

	for _, forbidden := range []string{
		metadataCanary,
		"legacy system must not win",
		"legacy dynamic must not win",
		"cache_control",
		"cacheable",
		"system_breakpoint",
		"anthropic_beta",
	} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("OpenAI request leaked local or provider-specific metadata %q: %s", forbidden, wire)
		}
	}

	withoutCache := req
	withoutCache.Cache = CachePolicy{}
	wireWithoutCache, err := json.Marshal(newOpenAIRequest(withoutCache, "fallback-model"))
	if err != nil {
		t.Fatalf("marshal request without cache policy: %v", err)
	}
	if !reflect.DeepEqual(wire, wireWithoutCache) {
		t.Fatalf("unsupported cache policy changed OpenAI wire JSON:\nwith:    %s\nwithout: %s", wire, wireWithoutCache)
	}
}

func TestOpenAIToolSchemaCompatibility(t *testing.T) {
	rawSchema := json.RawMessage(`{"type":"object","properties":{"nested":{"type":"object","properties":{"items":{"type":"array","items":{"type":"number"}},"choice":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}},"required":["nested"],"additionalProperties":false}`)
	req := ChatRequest{Tools: []ToolDefinition{
		{Name: "mcp__server__raw", Description: "raw tool", Schema: tool.Schema{Raw: rawSchema}},
		{Name: "Read", Description: "structured tool", Schema: tool.ObjectSchema(
			[]string{"path"},
			map[string]tool.SchemaProperty{"path": tool.StringProperty("path to read")},
		)},
	}}

	wire, err := json.Marshal(newOpenAIRequest(req, "model"))
	if err != nil {
		t.Fatalf("marshal OpenAI request: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatalf("decode OpenAI request: %v", err)
	}
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v, want two definitions", body["tools"])
	}

	rawFunction := tools[0].(map[string]any)["function"].(map[string]any)
	var wantRaw map[string]any
	if err := json.Unmarshal(rawSchema, &wantRaw); err != nil {
		t.Fatalf("decode expected raw schema: %v", err)
	}
	if rawFunction["name"] != "mcp__server__raw" || rawFunction["description"] != "raw tool" || !reflect.DeepEqual(rawFunction["parameters"], wantRaw) {
		t.Fatalf("raw schema mapping changed: %#v", rawFunction)
	}

	structuredFunction := tools[1].(map[string]any)["function"].(map[string]any)
	parameters, ok := structuredFunction["parameters"].(map[string]any)
	if !ok || parameters["type"] != "object" || !reflect.DeepEqual(parameters["required"], []any{"path"}) {
		t.Fatalf("structured schema mapping changed: %#v", structuredFunction)
	}
	properties, ok := parameters["properties"].(map[string]any)
	if !ok || !reflect.DeepEqual(properties["path"], map[string]any{"type": "string", "description": "path to read"}) {
		t.Fatalf("structured schema properties changed: %#v", parameters["properties"])
	}
	if strings.Contains(string(wire), "cache_control") {
		t.Fatalf("OpenAI tool schema gained provider-specific cache metadata: %s", wire)
	}
}

func assertOpenAIMessage(t *testing.T, raw any, role, content, toolCallID string) {
	t.Helper()
	message, ok := raw.(map[string]any)
	if !ok || message["role"] != role || message["content"] != content {
		t.Fatalf("message = %#v, want role=%q content=%q", raw, role, content)
	}
	if toolCallID == "" {
		if _, present := message["tool_call_id"]; present {
			t.Fatalf("message unexpectedly contains tool_call_id: %#v", message)
		}
		return
	}
	if message["tool_call_id"] != toolCallID {
		t.Fatalf("tool_call_id = %#v, want %q", message["tool_call_id"], toolCallID)
	}
}
