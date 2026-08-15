package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

const providerSubagentResultFixture = `{"schema_version":1,"task_id":"task-1","status":"completed","summary":"safe result","summary_truncated":false,"truncation_reason":"","stop_reason":"completed","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"error":null}`

func TestOpenAISubagentResultMapsToUnpairedMarkedUserMessage(t *testing.T) {
	request, err := newOpenAIRequest(ChatRequest{Messages: []ModelMessage{{
		Role:    ModelMessageRoleSubagentResult,
		Content: safeText(providerSubagentResultFixture),
	}}}, "model")
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatal(err)
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("OpenAI subagent messages = %#v", body["messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok || message["role"] != "user" || message["content"] != SubagentResultMarker+"\n"+providerSubagentResultFixture {
		t.Fatalf("OpenAI subagent result = %#v", messages[0])
	}
	for _, forbidden := range []string{"tool_call_id", "tool_calls"} {
		if _, exists := message[forbidden]; exists {
			t.Fatalf("OpenAI subagent result paired to prior tool call: %#v", message)
		}
	}
}

func TestAnthropicSubagentResultMapsToSingleMarkedUserTextBlock(t *testing.T) {
	params, err := anthropicMessageParams("model", ChatRequest{Messages: []ModelMessage{{
		Role:    ModelMessageRoleSubagentResult,
		Content: safeText(providerSubagentResultFixture),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatal(err)
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("Anthropic subagent messages = %#v", body["messages"])
	}
	message := messages[0].(map[string]any)
	if message["role"] != "user" {
		t.Fatalf("Anthropic subagent role = %#v", message)
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("Anthropic subagent content = %#v", message["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != SubagentResultMarker+"\n"+providerSubagentResultFixture {
		t.Fatalf("Anthropic subagent block = %#v", block)
	}
	serialized := string(wire)
	for _, forbidden := range []string{"tool_use_id", "tool_result", "parent_ref"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("Anthropic subagent result leaked forbidden field %q: %s", forbidden, serialized)
		}
	}
}

func TestProviderAdaptersRejectUnknownMessageRoleInsteadOfDroppingIt(t *testing.T) {
	request := ChatRequest{Messages: []ModelMessage{{Role: ModelMessageRole("future"), Content: safeText("must not disappear")}}}
	if _, err := newOpenAIRequest(request, "model"); err == nil {
		t.Fatal("OpenAI adapter silently dropped unknown message role")
	}
	if _, err := anthropicMessageParams("model", request); err == nil {
		t.Fatal("Anthropic adapter silently dropped unknown message role")
	}
}
