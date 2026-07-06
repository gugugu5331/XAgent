package mcpclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/tool"
)

func TestMCPHTTPToolRegistersAndExecutesThroughToolExecutor(t *testing.T) {
	server := fakeManagerHTTPServer(t, []RemoteTool{{Name: "echo", Description: "Echo tool", InputSchema: []byte(`{"type":"object"}`)}})
	defer server.Close()

	registry, manager, root := registryWithManager(t, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"http": {Type: config.MCPTransportHTTP, URL: server.URL},
	}})
	defer closeManager(t, manager)

	mcpTool, ok := registry.Get("mcp__http__echo")
	if !ok {
		t.Fatalf("expected MCP tool to be registered")
	}
	if mcpTool.Risk() != tool.RiskDangerous {
		t.Fatalf("expected MCP tool to be dangerous, got %s", mcpTool.Risk())
	}

	executor := tool.NewExecutor(registry, root, time.Second, 4096)
	result := executor.Execute(context.Background(), tool.Call{ID: "mcp", Name: "mcp__http__echo", ArgumentsJSON: `{"message":"hello"}`})
	if result.Status != tool.StatusSuccess || result.Content != "hello" {
		t.Fatalf("unexpected MCP tool result: %#v", result)
	}

	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("built-in still works"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := executor.Execute(context.Background(), tool.Call{ID: "read", Name: "Read", ArgumentsJSON: `{"path":"note.txt"}`})
	if read.Status != tool.StatusSuccess || read.Content != "built-in still works" {
		t.Fatalf("built-in tool stopped working after MCP registration: %#v", read)
	}
}

func TestMCPStdioToolRegistersAndExecutesThroughToolExecutor(t *testing.T) {
	server := buildFakeStdioServer(t)
	registry, manager, root := registryWithManager(t, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"stdio": {Type: config.MCPTransportStdio, Command: server, Args: []string{"mcp"}},
	}})
	defer closeManager(t, manager)

	if _, ok := registry.Get("mcp__stdio__echo"); !ok {
		t.Fatalf("expected stdio MCP echo tool to be registered")
	}
	executor := tool.NewExecutor(registry, root, time.Second, 4096)
	success := executor.Execute(context.Background(), tool.Call{ID: "echo", Name: "mcp__stdio__echo", ArgumentsJSON: `{"message":"hello"}`})
	if success.Status != tool.StatusSuccess || success.Content != "hello" {
		t.Fatalf("unexpected stdio MCP success result: %#v", success)
	}

	failure := executor.Execute(context.Background(), tool.Call{ID: "fail", Name: "mcp__stdio__fail", ArgumentsJSON: `{}`})
	if failure.Status != tool.StatusError || failure.Error == nil || !failure.Error.Recoverable {
		t.Fatalf("expected stdio MCP isError to become recoverable tool error: %#v", failure)
	}
}

func registryWithManager(t *testing.T, cfg config.MCPConfig) (*tool.Registry, *Manager, string) {
	t.Helper()
	root := t.TempDir()
	registry, err := tool.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, ManagerOptions{DefaultTimeout: 2 * time.Second})
	manager.Start(context.Background())
	for _, mcpTool := range manager.Tools() {
		if err := registry.Register(mcpTool); err != nil {
			t.Fatal(err)
		}
	}
	return registry, manager, root
}

func closeManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
