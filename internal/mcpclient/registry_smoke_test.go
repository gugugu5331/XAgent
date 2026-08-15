package mcpclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/tool"
)

func TestMCPHTTPToolRegistersAndExecutesThroughToolExecutor(t *testing.T) {
	server := fakeManagerHTTPServer(t, []protocol.RemoteTool{{Name: "echo", Description: "Echo tool", InputSchema: []byte(`{"type":"object"}`)}})
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
	result := executeAuthorizedTool(t, executor, tool.Call{ID: "mcp", Name: "mcp__http__echo", ArgumentsJSON: `{"message":"hello"}`})
	if result.Status != tool.StatusSuccess || result.Content != "hello" {
		t.Fatalf("unexpected MCP tool result: %#v", result)
	}

	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("built-in still works"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := executeAuthorizedTool(t, executor, tool.Call{ID: "read", Name: "Read", ArgumentsJSON: `{"path":"note.txt"}`})
	if read.Status != tool.StatusSuccess || read.Content != "built-in still works" {
		t.Fatalf("built-in tool stopped working after MCP registration: %#v", read)
	}
}

func TestMCPStdioToolRegistersAndExecutesThroughToolExecutor(t *testing.T) {
	session := &registryStdioSession{}
	registry, manager, root := registryWithManagerFactory(t, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"stdio": {Type: config.MCPTransportStdio, Command: "/safe/fake-mcp"},
	}}, managerSessionFactoryFunc(func(context.Context, string, config.MCPServerConfig, ManagerOptions) (managerSession, error) {
		return session, nil
	}))
	defer closeManager(t, manager)

	if _, ok := registry.Get("mcp__stdio__echo"); !ok {
		t.Fatalf("expected stdio MCP echo tool to be registered")
	}
	executor := tool.NewExecutor(registry, root, time.Second, 4096)
	success := executeAuthorizedTool(t, executor, tool.Call{ID: "echo", Name: "mcp__stdio__echo", ArgumentsJSON: `{"message":"hello"}`})
	if success.Status != tool.StatusSuccess || success.Content != "hello" {
		t.Fatalf("unexpected stdio MCP success result: %#v", success)
	}

	failure := executeAuthorizedTool(t, executor, tool.Call{ID: "fail", Name: "mcp__stdio__fail", ArgumentsJSON: `{}`})
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
	dependencies := managerTestDependencies(t, nil)
	for _, server := range cfg.Servers {
		switch server.Type {
		case config.MCPTransportHTTP:
			dependencies = managerHTTPTestDependencies(t)
		case config.MCPTransportStdio:
			dependencies = managerStdioTestDependencies(t, filepath.Dir(server.Command))
		}
	}
	manager, err := NewManager(cfg, ManagerOptions{DefaultTimeout: 2 * time.Second}, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, mcpTool := range manager.Tools() {
		registration, ok := mcpTool.(interface {
			RegistrationOptions() tool.RegistrationOptions
		})
		if !ok {
			t.Fatal("MCP tool does not expose bound registration options")
		}
		if err := registry.RegisterWithOptions(mcpTool, registration.RegistrationOptions()); err != nil {
			t.Fatal(err)
		}
	}
	return registry, manager, root
}

func registryWithManagerFactory(
	t *testing.T,
	cfg config.MCPConfig,
	factory managerSessionFactory,
) (*tool.Registry, *Manager, string) {
	t.Helper()
	root := t.TempDir()
	registry, err := tool.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	manager := newManagerWithFactoryForTest(t, cfg, ManagerOptions{DefaultTimeout: 2 * time.Second}, factory, nil)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	registerManagerTools(t, registry, manager)
	return registry, manager, root
}

func registerManagerTools(t *testing.T, registry *tool.Registry, manager *Manager) {
	t.Helper()
	for _, mcpTool := range manager.Tools() {
		registration, ok := mcpTool.(interface {
			RegistrationOptions() tool.RegistrationOptions
		})
		if !ok {
			t.Fatal("MCP tool does not expose bound registration options")
		}
		if err := registry.RegisterWithOptions(mcpTool, registration.RegistrationOptions()); err != nil {
			t.Fatal(err)
		}
	}
}

type registryStdioSession struct{}

func (*registryStdioSession) Start(context.Context) error { return nil }

func (*registryStdioSession) Snapshot() serverSessionSnapshot {
	return serverSessionSnapshot{
		State:          serverSessionStateRunning,
		ToolsSupported: true,
		Tools: []protocol.RemoteTool{
			{Name: "echo", InputSchema: []byte(`{"type":"object"}`)},
			{Name: "fail", InputSchema: []byte(`{"type":"object"}`)},
		},
	}
}

func (*registryStdioSession) CallTool(
	_ context.Context,
	remoteName string,
	arguments map[string]any,
) (protocol.CallToolResult, error) {
	if remoteName == "fail" {
		return protocol.CallToolResult{IsError: true, Content: []protocol.ContentBlock{{Type: "text", Text: "failed"}}}, nil
	}
	message, _ := arguments["message"].(string)
	return protocol.CallToolResult{Content: []protocol.ContentBlock{{Type: "text", Text: message}}}, nil
}

func (*registryStdioSession) Close(context.Context) error { return nil }

func closeManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
