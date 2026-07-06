package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"xagent/internal/config"
	"xagent/internal/tool"
)

const (
	DefaultMaxTools           = 128
	DefaultMaxPages           = 32
	DefaultMaxConcurrentCalls = 4
	DefaultMaxResponseBytes   = 1024 * 1024
	DefaultMCPTimeout         = 30 * time.Second
)

type ServerState string

const (
	ServerDisabled    ServerState = "disabled"
	ServerConfigured  ServerState = "configured"
	ServerStarting    ServerState = "starting"
	ServerInitialized ServerState = "initialized"
	ServerListed      ServerState = "listed"
	ServerReady       ServerState = "ready"
	ServerFailed      ServerState = "failed"
	ServerClosed      ServerState = "closed"
)

type ManagerOptions struct {
	MaxTools           int
	MaxPages           int
	MaxConcurrentCalls int
	MaxResponseBytes   int64
	DefaultTimeout     time.Duration
}

type Manager struct {
	config  config.MCPConfig
	options ManagerOptions

	mu          sync.Mutex
	servers     map[string]*managedServer
	tools       []tool.Tool
	byName      map[string]*managedTool
	diagnostics []Diagnostic
	closed      bool
}

type managedServer struct {
	name      string
	config    config.MCPServerConfig
	state     ServerState
	transport closeableTransport
	client    *ProtocolClient
	semaphore chan struct{}
}

type managedTool struct {
	registeredName string
	serverName     string
	remoteName     string
}

type closeableTransport interface {
	Transport
	Close(ctx context.Context) error
}

type Diagnostic struct {
	Server  string
	Message string
}

type StatusSummary struct {
	Configured  int
	Ready       int
	Failed      int
	Disabled    int
	Closed      int
	Diagnostics []Diagnostic
}

func (s StatusSummary) String() string {
	if s.Configured == 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("%d ready", s.Ready)}
	if s.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", s.Failed))
	}
	if s.Disabled > 0 {
		parts = append(parts, fmt.Sprintf("%d disabled", s.Disabled))
	}
	if s.Closed > 0 && s.Closed == s.Configured {
		parts = append(parts, "closed")
	}
	if len(s.Diagnostics) > 0 {
		last := s.Diagnostics[len(s.Diagnostics)-1]
		message := last.Message
		if last.Server != "" {
			message = last.Server + ": " + message
		}
		parts = append(parts, message)
	}
	return strings.Join(parts, ", ")
}

func NewManager(cfg config.MCPConfig, options ManagerOptions) *Manager {
	if options.MaxTools <= 0 {
		options.MaxTools = DefaultMaxTools
	}
	if options.MaxPages <= 0 {
		options.MaxPages = DefaultMaxPages
	}
	if options.MaxConcurrentCalls <= 0 {
		options.MaxConcurrentCalls = DefaultMaxConcurrentCalls
	}
	if options.MaxResponseBytes <= 0 {
		options.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if options.DefaultTimeout <= 0 {
		options.DefaultTimeout = DefaultMCPTimeout
	}
	manager := &Manager{config: cfg, options: options, servers: map[string]*managedServer{}, byName: map[string]*managedTool{}}
	return manager
}

func (m *Manager) Start(ctx context.Context) {
	usedNames := map[string]bool{"Read": true, "Write": true, "Edit": true, "Bash": true, "Glob": true, "Grep": true}
	for name, serverConfig := range m.config.Servers {
		server := &managedServer{name: name, config: serverConfig, state: ServerConfigured, semaphore: make(chan struct{}, m.options.MaxConcurrentCalls)}
		m.servers[name] = server
		if serverConfig.Disabled {
			server.state = ServerDisabled
			continue
		}
		if serverConfig.Type == config.MCPTransportStdio && serverConfig.Source == "project" {
			server.state = ServerDisabled
			m.addDiagnostic(name, "project stdio MCP server is disabled until explicitly trusted; source=project; fields=command,args,env")
			continue
		}
		m.startServer(ctx, server, usedNames)
	}
}

func (m *Manager) Tools() []tool.Tool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]tool.Tool(nil), m.tools...)
}

func (m *Manager) Diagnostics() []Diagnostic {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Diagnostic(nil), m.diagnostics...)
}

func (m *Manager) Summary() StatusSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	summary := StatusSummary{Diagnostics: append([]Diagnostic(nil), m.diagnostics...)}
	for _, server := range m.servers {
		summary.Configured++
		switch server.state {
		case ServerReady:
			summary.Ready++
		case ServerFailed:
			summary.Failed++
		case ServerDisabled:
			summary.Disabled++
		case ServerClosed:
			summary.Closed++
		}
	}
	return summary
}

func (m *Manager) StatusLine() string {
	return m.Summary().String()
}

func (m *Manager) CallTool(ctx context.Context, registeredName string, arguments map[string]any) (CallToolResult, error) {
	m.mu.Lock()
	toolMeta := m.byName[registeredName]
	server := (*managedServer)(nil)
	if toolMeta != nil {
		server = m.servers[toolMeta.serverName]
	}
	m.mu.Unlock()
	if toolMeta == nil || server == nil || server.client == nil {
		return CallToolResult{}, fmt.Errorf("mcp tool %s is not available", registeredName)
	}

	select {
	case server.semaphore <- struct{}{}:
		defer func() { <-server.semaphore }()
	case <-ctx.Done():
		return CallToolResult{}, ctx.Err()
	}
	return server.client.CallTool(ctx, toolMeta.remoteName, arguments)
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	servers := make([]*managedServer, 0, len(m.servers))
	for _, server := range m.servers {
		servers = append(servers, server)
	}
	m.mu.Unlock()

	var closeErr error
	for _, server := range servers {
		if server.transport == nil {
			server.state = ServerClosed
			continue
		}
		if err := server.transport.Close(ctx); err != nil && closeErr == nil {
			closeErr = err
		}
		server.state = ServerClosed
	}
	return closeErr
}

func (m *Manager) startServer(ctx context.Context, server *managedServer, usedNames map[string]bool) {
	server.state = ServerStarting
	transport, err := m.createTransport(server.config)
	if err != nil {
		server.state = ServerFailed
		m.addDiagnostic(server.name, err.Error())
		return
	}
	server.transport = transport
	if starter, ok := transport.(interface{ Start(context.Context) error }); ok {
		if err := starter.Start(ctx); err != nil {
			server.state = ServerFailed
			m.addDiagnostic(server.name, err.Error())
			return
		}
	}
	client := NewProtocolClient(transport)
	client.Start(ctx)
	server.client = client

	initCtx, cancel := context.WithTimeout(ctx, m.timeoutFor(server.config))
	defer cancel()
	if _, err := client.Initialize(initCtx); err != nil {
		server.state = ServerFailed
		m.addDiagnostic(server.name, err.Error())
		return
	}
	server.state = ServerInitialized
	tools, err := client.ListTools(initCtx, m.options.MaxPages, m.options.MaxTools)
	if err != nil {
		server.state = ServerFailed
		m.addDiagnostic(server.name, err.Error())
		return
	}
	server.state = ServerListed
	m.registerTools(server, tools, usedNames)
	server.state = ServerReady
}

func (m *Manager) createTransport(server config.MCPServerConfig) (closeableTransport, error) {
	switch server.Type {
	case config.MCPTransportStdio:
		return NewStdioTransport(StdioConfig{Command: server.Command, Args: server.Args, Env: server.Env, MaxResponseBytes: m.options.MaxResponseBytes}), nil
	case config.MCPTransportHTTP:
		return NewClosableHTTPTransport(HTTPConfig{URL: server.URL, Headers: server.Headers, MaxResponseBytes: m.options.MaxResponseBytes})
	default:
		return nil, fmt.Errorf("unsupported MCP transport %q", server.Type)
	}
}

func (m *Manager) registerTools(server *managedServer, remoteTools []RemoteTool, usedNames map[string]bool) {
	for _, remoteTool := range remoteTools {
		if remoteTool.Name == "" {
			m.addDiagnostic(server.name, "skipped MCP tool with empty name")
			continue
		}
		if !validObjectSchema(remoteTool.InputSchema) {
			m.addDiagnostic(server.name, "skipped MCP tool with invalid inputSchema: "+remoteTool.Name)
			continue
		}
		identity := RegisteredToolName(server.name, remoteTool.Name, usedNames)
		description := remoteTool.Description
		if description == "" {
			description = remoteTool.Title
		}
		adapter := NewToolAdapter(identity.RegisteredName, identity.ServerName, identity.RemoteToolName, description, tool.Schema{Raw: remoteTool.InputSchema}, m)
		m.tools = append(m.tools, adapter)
		m.byName[identity.RegisteredName] = &managedTool{registeredName: identity.RegisteredName, serverName: server.name, remoteName: remoteTool.Name}
	}
}

func (m *Manager) timeoutFor(server config.MCPServerConfig) time.Duration {
	if server.TimeoutMS > 0 {
		return time.Duration(server.TimeoutMS) * time.Millisecond
	}
	if m.config.DefaultTimeoutMS > 0 {
		return time.Duration(m.config.DefaultTimeoutMS) * time.Millisecond
	}
	return m.options.DefaultTimeout
}

func (m *Manager) addDiagnostic(server string, message string) {
	m.mu.Lock()
	m.diagnostics = append(m.diagnostics, Diagnostic{Server: server, Message: SanitizeMetadata(RedactText(message), 2048)})
	m.mu.Unlock()
}

func validObjectSchema(raw json.RawMessage) bool {
	var schema struct {
		Type string `json:"type"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &schema) != nil {
		return false
	}
	return schema.Type == "object"
}
