package mcpclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/tool"
)

func TestMCPSecretCanaryDoesNotReachToolHistoryOrDiagnostics(t *testing.T) {
	canary := "CANARY_MCP_SECRET_SHOULD_NOT_APPEAR"
	server := fakeManagerHTTPServer(t, []RemoteTool{{Name: "echo", Description: "Echo tool", InputSchema: json.RawMessage(`{"type":"object"}`)}})
	defer server.Close()

	registry, manager, root := registryWithManager(t, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"http": {Type: config.MCPTransportHTTP, URL: server.URL},
	}})
	defer closeManager(t, manager)
	executor := tool.NewExecutor(registry, root, time.Second, 4096)
	result := executor.Execute(context.Background(), tool.Call{ID: "mcp", Name: "mcp__http__echo", ArgumentsJSON: `{"api_key":"` + canary + `","message":"hello"}`})
	if result.Status != tool.StatusSuccess {
		t.Fatalf("unexpected MCP result: %#v", result)
	}
	joinedDiagnostics, _ := json.Marshal(manager.Diagnostics())
	if strings.Contains(string(joinedDiagnostics), canary) {
		t.Fatalf("diagnostics leaked canary: %s", joinedDiagnostics)
	}
	redacted := RedactArguments(map[string]any{"api_key": canary, "message": "hello"})
	redactedJSON, _ := json.Marshal(redacted)
	if strings.Contains(string(redactedJSON), canary) {
		t.Fatalf("redacted arguments leaked canary: %s", redactedJSON)
	}
}
