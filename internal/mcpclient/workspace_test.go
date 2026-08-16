package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"

	"xagent/internal/config"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/tool"
)

func TestWorkspaceHTTPDefaultsUnknownAndExactLocalTrustIsIndependent(t *testing.T) {
	remoteClaim := json.RawMessage(`{"x-xagent-workspace":{"mode":"independent"}}`)
	for _, test := range []struct {
		name    string
		trusted map[string]bool
		want    tool.WorkspaceMode
	}{
		{name: "remote claim only", want: tool.WorkspaceUnknown},
		{name: "different local server name", trusted: map[string]bool{"other": true}, want: tool.WorkspaceUnknown},
		{name: "exact local server name", trusted: map[string]bool{"server": true}, want: tool.WorkspaceIndependent},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"server": {Type: config.MCPTransportHTTP},
				"other":  {Type: config.MCPTransportHTTP},
			}}
			manager, err := newManager(cfg, ManagerOptions{IndependentHTTPServers: test.trusted}, managerTestDependencies(t, nil), managerSessionFactoryFunc(func(context.Context, string, config.MCPServerConfig, ManagerOptions) (managerSession, error) {
				return nil, nil
			}))
			if err != nil {
				t.Fatalf("new manager: %v", err)
			}
			server := &managedServer{name: "server", config: cfg.Servers["server"], configDigest: sha256.Sum256([]byte("server config"))}
			candidates, _ := manager.buildToolCandidates(server, []protocol.RemoteTool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: remoteClaim}}, map[string]bool{})
			if len(candidates) != 1 {
				t.Fatalf("candidate count = %d", len(candidates))
			}
			registration := candidates[0].(ToolAdapter).RegistrationOptions()
			if registration.Workspace.Mode != test.want {
				t.Fatalf("workspace mode = %q, want %q", registration.Workspace.Mode, test.want)
			}
		})
	}
}

func TestWorkspaceIndependentTrustRejectsStdioAndUnknownServer(t *testing.T) {
	for _, test := range []struct {
		name    string
		servers map[string]config.MCPServerConfig
		trusted map[string]bool
	}{
		{name: "stdio", servers: map[string]config.MCPServerConfig{"local": {Type: config.MCPTransportStdio}}, trusted: map[string]bool{"local": true}},
		{name: "unknown", servers: map[string]config.MCPServerConfig{"http": {Type: config.MCPTransportHTTP}}, trusted: map[string]bool{"missing": true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := newManager(config.MCPConfig{Servers: test.servers}, ManagerOptions{IndependentHTTPServers: test.trusted}, managerTestDependencies(t, nil), managerSessionFactoryFunc(func(context.Context, string, config.MCPServerConfig, ManagerOptions) (managerSession, error) {
				return nil, nil
			}))
			if err == nil {
				t.Fatal("invalid IndependentHTTPServers trust entry was accepted")
			}
		})
	}
}

func TestWorkspaceStdioAdapterIsFixed(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{"local": {Type: config.MCPTransportStdio}}}
	manager, err := newManager(cfg, ManagerOptions{}, managerTestDependencies(t, nil), managerSessionFactoryFunc(func(context.Context, string, config.MCPServerConfig, ManagerOptions) (managerSession, error) {
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	server := &managedServer{name: "local", config: cfg.Servers["local"], configDigest: sha256.Sum256([]byte("stdio config"))}
	candidates, _ := manager.buildToolCandidates(server, []protocol.RemoteTool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}, map[string]bool{})
	if got := candidates[0].(ToolAdapter).RegistrationOptions().Workspace.Mode; got != tool.WorkspaceFixed {
		t.Fatalf("stdio workspace mode = %q, want fixed", got)
	}
}

func TestWorkspaceIndependentTrustSnapshotIsDetached(t *testing.T) {
	trusted := map[string]bool{"server": true}
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{"server": {Type: config.MCPTransportHTTP}}}
	manager, err := newManager(cfg, ManagerOptions{IndependentHTTPServers: trusted}, managerTestDependencies(t, nil), managerSessionFactoryFunc(func(context.Context, string, config.MCPServerConfig, ManagerOptions) (managerSession, error) {
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	trusted["server"] = false
	server := &managedServer{name: "server", config: cfg.Servers["server"]}
	if got := manager.workspacePolicy(server).Mode; got != tool.WorkspaceIndependent {
		t.Fatalf("caller mutation changed trusted workspace snapshot: %q", got)
	}
}
