package provider

import (
	"encoding/json"
	"testing"

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
