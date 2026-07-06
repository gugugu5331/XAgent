package mcpclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"xagent/internal/tool"
)

func TestToolAdapterConvertsSuccessResult(t *testing.T) {
	caller := &fakeToolCaller{result: CallToolResult{
		Content: []ContentBlock{
			{Type: "text", Text: "hello"},
			{Type: "image", Raw: []byte(`{"type":"image","data":"..."}`)},
		},
		StructuredContent: map[string]any{"value": "structured"},
	}}
	adapter := NewToolAdapter("mcp__server__tool", "server", "tool", "description", tool.Schema{}, caller)
	result := adapter.Execute(context.Background(), tool.Input{Name: adapter.Name(), CallID: "1", Arguments: map[string]any{"q": "x"}})
	if result.Status != tool.StatusSuccess || result.Content != "hello\n[non-text MCP content omitted]" {
		t.Fatalf("unexpected success result: %#v", result)
	}
	if result.Data["structuredContent"] == nil || result.Data["content"] == nil {
		t.Fatalf("expected structured and non-text data: %#v", result.Data)
	}
	if caller.registeredName != "mcp__server__tool" || caller.arguments["q"] != "x" {
		t.Fatalf("unexpected call: %#v", caller)
	}
}

func TestToolAdapterRedactsSecretsFromResult(t *testing.T) {
	canary := "CANARY_SHOULD_NOT_LEAK"
	caller := &fakeToolCaller{result: CallToolResult{
		Content:           []ContentBlock{{Type: "text", Text: "token=" + canary}, {Type: "resource", Raw: []byte(`{"text":"authorization:` + canary + `"}`)}},
		StructuredContent: map[string]any{"message": "secret=" + canary},
		IsError:           true,
	}}
	adapter := NewToolAdapter("mcp__server__tool", "server", "tool", "", tool.Schema{}, caller)
	result := adapter.Execute(context.Background(), tool.Input{Name: adapter.Name(), CallID: "1"})
	combined := result.Content + result.Error.Message
	if strings.Contains(combined, canary) {
		t.Fatalf("MCP result leaked canary in content/error: %#v", result)
	}
	data, _ := json.Marshal(result.Data)
	if strings.Contains(string(data), canary) {
		t.Fatalf("MCP result leaked canary in data: %s", data)
	}
}

func TestToolAdapterConvertsIsErrorAndRPCErrors(t *testing.T) {
	adapter := NewToolAdapter("mcp__server__tool", "server", "tool", "", tool.Schema{}, &fakeToolCaller{result: CallToolResult{IsError: true, Content: []ContentBlock{{Type: "text", Text: "failed"}}}})
	result := adapter.Execute(context.Background(), tool.Input{Name: adapter.Name(), CallID: "1"})
	if result.Status != tool.StatusError || result.Error == nil || !result.Error.Recoverable || result.Content != "failed" {
		t.Fatalf("unexpected isError conversion: %#v", result)
	}

	adapter = NewToolAdapter("mcp__server__tool", "server", "tool", "", tool.Schema{}, &fakeToolCaller{err: &RPCError{Code: -32000, Message: "boom"}})
	result = adapter.Execute(context.Background(), tool.Input{Name: adapter.Name(), CallID: "2"})
	if result.Status != tool.StatusError || result.Error == nil || !result.Error.Recoverable {
		t.Fatalf("unexpected RPC error conversion: %#v", result)
	}
}

func TestToolAdapterConvertsTimeout(t *testing.T) {
	adapter := NewToolAdapter("mcp__server__tool", "server", "tool", "", tool.Schema{}, &fakeToolCaller{err: context.DeadlineExceeded})
	result := adapter.Execute(context.Background(), tool.Input{Name: adapter.Name(), CallID: "1"})
	if result.Status != tool.StatusTimeout || result.Error == nil || result.Error.Code != tool.ErrTimeout {
		t.Fatalf("unexpected timeout conversion: %#v", result)
	}
}

func TestToolAdapterMetadataAndRisk(t *testing.T) {
	schema := tool.Schema{Raw: []byte(`{"type":"object"}`)}
	adapter := NewToolAdapter("mcp__server__tool", "server", "tool", "desc\n\x1b[31m", schema, &fakeToolCaller{})
	if adapter.Risk() != tool.RiskDangerous {
		t.Fatalf("expected dangerous risk")
	}
	if adapter.Schema().Raw == nil {
		t.Fatalf("expected raw schema")
	}
	if adapter.Description() == "" || adapter.Description() == "desc\n\x1b[31m" {
		t.Fatalf("description not sanitized: %q", adapter.Description())
	}
}

type fakeToolCaller struct {
	registeredName string
	arguments      map[string]any
	result         CallToolResult
	err            error
}

func (f *fakeToolCaller) CallTool(ctx context.Context, registeredName string, arguments map[string]any) (CallToolResult, error) {
	f.registeredName = registeredName
	f.arguments = arguments
	if f.err != nil {
		return CallToolResult{}, f.err
	}
	return f.result, nil
}
