package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"xagent/internal/config"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/tool"
)

func TestManagerStartsServersAndRegistersTools(t *testing.T) {
	server := fakeManagerHTTPServer(t, []protocol.RemoteTool{{Name: "echo", Description: "Echo tool", InputSchema: json.RawMessage(`{"type":"object"}`)}})
	defer server.Close()
	manager, err := NewManager(config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"good/server": {Type: config.MCPTransportHTTP, URL: server.URL},
		"disabled":    {Disabled: true},
		"bad":         {Type: config.MCPTransportHTTP, URL: "http://example.invalid/mcp"},
	}}, ManagerOptions{}, managerHTTPTestDependencies(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tools := manager.Tools()
	if len(tools) != 1 {
		t.Fatalf("expected one registered tool, got %d diagnostics=%#v", len(tools), manager.Diagnostics())
	}
	if tools[0].Name() != "mcp__good_server__echo" || tools[0].Risk() != tool.RiskDangerous {
		t.Fatalf("unexpected tool: %s %s", tools[0].Name(), tools[0].Risk())
	}
	result, err := manager.CallTool(context.Background(), tools[0].Name(), map[string]any{"message": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hi" {
		t.Fatalf("unexpected call result: %#v", result)
	}
	if len(manager.Diagnostics()) == 0 {
		t.Fatal("expected diagnostic for bad server")
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("second close should be idempotent: %v", err)
	}
}

func TestManagerCallsOriginalRemoteToolName(t *testing.T) {
	seenToolName := ""
	server := fakeManagerHTTPServerWithCallObserver(t, []protocol.RemoteTool{{Name: "remote tool/name", InputSchema: json.RawMessage(`{"type":"object"}`)}}, func(params protocol.CallToolRequest) {
		seenToolName = params.Name
	})
	defer server.Close()
	manager, err := NewManager(config.MCPConfig{Servers: map[string]config.MCPServerConfig{"server": {Type: config.MCPTransportHTTP, URL: server.URL}}}, ManagerOptions{}, managerHTTPTestDependencies(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tools := manager.Tools()
	if len(tools) != 1 || tools[0].Name() == "mcp__server__remote tool/name" {
		t.Fatalf("expected sanitized registered name, got %#v", tools)
	}
	if _, err := manager.CallTool(context.Background(), tools[0].Name(), nil); err != nil {
		t.Fatal(err)
	}
	if seenToolName != "remote tool/name" {
		t.Fatalf("tools/call used sanitized name instead of original: %q", seenToolName)
	}
}

func TestManagerDisablesProjectStdioUntilTrusted(t *testing.T) {
	manager, err := NewManager(config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"local": {Type: config.MCPTransportStdio, Command: "echo", Source: "project"},
	}}, ManagerOptions{}, managerTestDependencies(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.Tools()) != 0 {
		t.Fatalf("project stdio should not register tools before trust")
	}
	summary := manager.Summary()
	if summary.Disabled != 1 || len(summary.Diagnostics) != 1 {
		t.Fatalf("expected disabled diagnostic, got %#v", summary)
	}
}

func TestManagerSkipsInvalidRemoteTools(t *testing.T) {
	server := fakeManagerHTTPServer(t, []protocol.RemoteTool{
		{Name: "", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "bad_schema", InputSchema: json.RawMessage(`{"type":"string"}`)},
		{Name: "ok", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})
	defer server.Close()
	manager, err := NewManager(config.MCPConfig{Servers: map[string]config.MCPServerConfig{"server": {Type: config.MCPTransportHTTP, URL: server.URL}}}, ManagerOptions{}, managerHTTPTestDependencies(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.Tools()) != 1 || manager.Tools()[0].Name() != "mcp__server__ok" {
		t.Fatalf("unexpected tools: %#v", manager.Tools())
	}
	if len(manager.Diagnostics()) != 2 {
		t.Fatalf("expected diagnostics for skipped tools, got %#v", manager.Diagnostics())
	}
}

func fakeManagerHTTPServer(t *testing.T, remoteTools []protocol.RemoteTool) *httptest.Server {
	t.Helper()
	return fakeManagerHTTPServerWithCallObserver(t, remoteTools, nil)
}

func fakeManagerHTTPServerWithCallObserver(t *testing.T, remoteTools []protocol.RemoteTool, observeCall func(protocol.CallToolRequest)) *httptest.Server {
	t.Helper()
	initialized := false
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch message.Method {
		case "initialize":
			writeManagerRPCResult(t, w, message.ID, map[string]any{"protocolVersion": protocol.SupportedProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake"}})
		case "notifications/initialized":
			initialized = true
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if !initialized {
				t.Fatal("tools/list before initialized")
			}
			writeManagerRPCResult(t, w, message.ID, protocol.ListToolsResult{Tools: remoteTools})
		case "tools/call":
			var params protocol.CallToolRequest
			if err := json.Unmarshal(message.Params, &params); err != nil {
				t.Fatal(err)
			}
			if observeCall != nil {
				observeCall(params)
			}
			text, _ := params.Arguments["message"].(string)
			writeManagerRPCResult(t, w, message.ID, protocol.CallToolResult{Content: []protocol.ContentBlock{{Type: "text", Text: text}}})
		default:
			t.Fatalf("unexpected method %s", message.Method)
		}
	}))
}

func writeManagerRPCResult(t *testing.T, writer http.ResponseWriter, id any, result any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
		t.Fatal(err)
	}
}
