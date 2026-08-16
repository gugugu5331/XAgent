package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

const (
	DefaultMaxTools           = 128
	DefaultMaxPages           = 32
	DefaultMaxConcurrentCalls = 4
	DefaultMaxResponseBytes   = 1024 * 1024
	DefaultMaxProtocolErrors  = 32
	DefaultMCPTimeout         = 30 * time.Second
	managerCleanupTimeout     = 2 * time.Second

	diagnosticMCPManagerCleanupTimeout = "mcp_manager_cleanup_timeout"
	diagnosticMCPManagerSource         = "mcp.manager"
)

var (
	errManagerToolUnavailable  = errors.New("MCP manager tool is not available")
	errManagerCloseFailed      = errors.New("MCP manager cleanup failed")
	errManagerCleanupTimeout   = errors.New("MCP manager cleanup exceeded its hard deadline")
	errManagerAlreadyStarted   = errors.New("MCP manager was already started")
	errManagerStartInterrupted = errors.New("MCP manager startup was interrupted")
	errManagerConfigDigest     = errors.New("MCP server configuration digest failed")
	errManagerWorkspacePolicy  = errors.New("MCP workspace policy is invalid")
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

type ManagerState uint8

const (
	ManagerStateNew ManagerState = iota
	ManagerStateStarting
	ManagerStateRunning
	ManagerStateClosing
	ManagerStateClosed
)

type ManagerOptions struct {
	MaxTools           int
	MaxPages           int
	MaxConcurrentCalls int
	MaxResponseBytes   int64
	MaxProtocolErrors  int64
	DefaultTimeout     time.Duration
	CleanupTimeout     time.Duration
	// IndependentHTTPServers is a local trusted allowlist. Remote MCP
	// annotations and server-returned metadata cannot modify it.
	IndependentHTTPServers map[string]bool
	// Diagnostics is the bounded C7 sink propagated unchanged to every
	// Manager-owned Session, Connection and Transport. ManagerDependencies
	// retains the compatibility input used by callers from before T4.27; when
	// both are provided they must identify the same owner.
	Diagnostics diagnostics.BoundedSink
}

type Manager struct {
	config          config.MCPConfig
	options         ManagerOptions
	sessionFactory  managerSessionFactory
	cleanupTimeout  time.Duration
	diagnosticSink  diagnostics.BoundedSink
	runtimeRedactor *redact.RuntimeRedactor
	resultFactory   *tool.ResultFactory
	capture         func(context.Context, artifact.Metadata) (*tool.Capture, error)

	mu          sync.Mutex
	state       ManagerState
	servers     map[string]*managedServer
	tools       []tool.Tool
	byName      map[string]*managedTool
	diagnostics []Diagnostic
	leases      *leaseGate
	runContext  context.Context
	cancelRun   context.CancelFunc

	startDone     chan struct{}
	startDoneOnce sync.Once
	closeOnce     sync.Once
	closeDone     chan struct{}
	closeErr      error
}

type managedServer struct {
	name         string
	config       config.MCPServerConfig
	state        ServerState
	session      managerSession
	semaphore    chan struct{}
	configDigest [32]byte

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

type managedTool struct {
	registeredName string
	serverName     string
	remoteName     string
}

type Diagnostic struct {
	Server  string
	Code    string
	Source  string
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

type ManagerSnapshot struct {
	State ManagerState
	Tools []tool.Tool
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

func NewManager(cfg config.MCPConfig, options ManagerOptions, dependencies ManagerDependencies) (*Manager, error) {
	options = resolveManagerOptions(cfg, options)
	if options.Diagnostics == nil {
		options.Diagnostics = dependencies.Diagnostics
	} else if dependencies.Diagnostics == nil {
		dependencies.Diagnostics = options.Diagnostics
	} else if !sameManagerDiagnosticSink(options.Diagnostics, dependencies.Diagnostics) {
		return nil, errManagerDependencies
	}
	if err := validateManagerDependencies(cfg, dependencies); err != nil {
		return nil, err
	}
	return newManager(cfg, options, dependencies, secureManagerSessionFactory{dependencies: dependencies})
}

func newManager(
	cfg config.MCPConfig,
	options ManagerOptions,
	dependencies ManagerDependencies,
	sessionFactory managerSessionFactory,
) (*Manager, error) {
	if options.Diagnostics == nil {
		options.Diagnostics = dependencies.Diagnostics
	}
	if dependencyMissing(options.Diagnostics) || dependencies.RuntimeRedactor == nil ||
		dependencies.ResultFactory == nil || sessionFactory == nil {
		return nil, errManagerDependencies
	}
	dependencies.Diagnostics = options.Diagnostics
	options = resolveManagerOptions(cfg, options)
	if err := validateIndependentHTTPServers(cfg, options.IndependentHTTPServers); err != nil {
		return nil, err
	}
	options.IndependentHTTPServers = cloneBoolMap(options.IndependentHTTPServers)
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
	if options.MaxProtocolErrors <= 0 {
		options.MaxProtocolErrors = DefaultMaxProtocolErrors
	}
	if options.DefaultTimeout <= 0 {
		options.DefaultTimeout = DefaultMCPTimeout
	}
	runContext, cancelRun := context.WithCancel(context.Background())
	cleanupTimeout := options.CleanupTimeout
	if cleanupTimeout <= 0 || cleanupTimeout > managerCleanupTimeout {
		cleanupTimeout = managerCleanupTimeout
	}
	options.CleanupTimeout = cleanupTimeout
	return &Manager{
		config:          cloneManagerConfig(cfg),
		options:         options,
		sessionFactory:  sessionFactory,
		cleanupTimeout:  cleanupTimeout,
		diagnosticSink:  options.Diagnostics,
		runtimeRedactor: dependencies.RuntimeRedactor,
		resultFactory:   dependencies.ResultFactory,
		capture:         dependencies.Capture,
		state:           ManagerStateNew,
		servers:         make(map[string]*managedServer),
		byName:          make(map[string]*managedTool),
		leases:          newLeaseGate(),
		runContext:      runContext,
		cancelRun:       cancelRun,
		startDone:       make(chan struct{}),
		closeDone:       make(chan struct{}),
	}, nil
}

// Start publishes one atomic tool snapshot after all configured sessions have
// reached a terminal startup state. Individual server failures remain visible
// through Summary and Diagnostics and do not fail the whole manager.
func (manager *Manager) Start(ctx context.Context) error {
	if manager == nil {
		return errManagerDependencies
	}
	if ctx == nil {
		ctx = context.Background()
	}
	manager.mu.Lock()
	if manager.state != ManagerStateNew {
		manager.mu.Unlock()
		return errManagerAlreadyStarted
	}
	manager.state = ManagerStateStarting
	manager.mu.Unlock()
	defer manager.startDoneOnce.Do(func() { close(manager.startDone) })

	startupContext, stopCallerCancellation := manager.startupContext(ctx)
	defer stopCallerCancellation()

	serverNames := make([]string, 0, len(manager.config.Servers))
	for name := range manager.config.Servers {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)

	usedNames := map[string]bool{"Read": true, "Write": true, "Edit": true, "Bash": true, "Glob": true, "Grep": true}
	candidateTools := make([]tool.Tool, 0)
	candidateRoutes := make(map[string]*managedTool)
	candidateServers := make([]*managedServer, 0)

	for _, name := range serverNames {
		if !manager.starting() {
			break
		}
		serverConfig := manager.config.Servers[name]
		server := &managedServer{
			name: name, config: serverConfig, state: ServerConfigured,
			semaphore: make(chan struct{}, manager.options.MaxConcurrentCalls),
			closeDone: make(chan struct{}),
		}
		manager.mu.Lock()
		if manager.state != ManagerStateStarting {
			manager.mu.Unlock()
			break
		}
		manager.servers[name] = server
		manager.mu.Unlock()

		if serverConfig.Disabled {
			manager.setServerState(server, ServerDisabled)
			continue
		}
		if serverConfig.Type == config.MCPTransportStdio && serverConfig.Source == "project" {
			manager.setServerState(server, ServerDisabled)
			manager.addDiagnostic(name, "project stdio MCP server is disabled until explicitly trusted; source=project; fields=command,args,env")
			continue
		}

		manager.setServerState(server, ServerStarting)
		sessionOptions := manager.options
		sessionOptions.DefaultTimeout = manager.timeoutFor(serverConfig)
		digest, digestErr := serverConfigDigest(serverConfig, sessionOptions)
		if digestErr != nil {
			manager.failServer(server, digestErr)
			continue
		}
		server.configDigest = digest
		session, err := manager.sessionFactory.New(startupContext, name, serverConfig, sessionOptions)
		if session == nil {
			manager.failServer(server, err)
			continue
		}
		manager.mu.Lock()
		managerState := manager.state
		if managerState == ManagerStateStarting || managerState == ManagerStateClosing {
			server.session = session
		}
		manager.mu.Unlock()
		if managerState == ManagerStateClosed {
			manager.closeDetachedSession(session)
			break
		}
		if err != nil {
			manager.failServer(server, err)
			manager.rollbackServer(server)
			continue
		}
		if managerState != ManagerStateStarting {
			break
		}

		if err := session.Start(startupContext); err != nil {
			manager.failServer(server, err)
			manager.rollbackServer(server)
			continue
		}
		snapshot := session.Snapshot()
		if snapshot.State != serverSessionStateRunning {
			manager.failServer(server, errManagerToolUnavailable)
			manager.rollbackServer(server)
			continue
		}

		tools, routes := manager.buildToolCandidates(server, snapshot.Tools, usedNames)
		candidateTools = append(candidateTools, tools...)
		candidateServers = append(candidateServers, server)
		for registeredName, route := range routes {
			candidateRoutes[registeredName] = route
		}
	}

	manager.mu.Lock()
	finalState := manager.state
	if manager.state == ManagerStateStarting {
		for _, server := range candidateServers {
			server.state = ServerReady
		}
		manager.tools = candidateTools
		manager.byName = candidateRoutes
		manager.state = ManagerStateRunning
		finalState = ManagerStateRunning
	}
	manager.mu.Unlock()
	if finalState == ManagerStateRunning {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errManagerStartInterrupted
}

func (manager *Manager) Snapshot() ManagerSnapshot {
	if manager == nil {
		return ManagerSnapshot{State: ManagerStateClosed}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	snapshot := ManagerSnapshot{State: manager.state}
	if manager.state == ManagerStateRunning {
		snapshot.Tools = append([]tool.Tool(nil), manager.tools...)
	}
	return snapshot
}

func (manager *Manager) Tools() []tool.Tool {
	return manager.Snapshot().Tools
}

func (manager *Manager) Diagnostics() []Diagnostic {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]Diagnostic(nil), manager.diagnostics...)
}

func (manager *Manager) Summary() StatusSummary {
	if manager == nil {
		return StatusSummary{}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.summaryLocked()
}

func (manager *Manager) summaryLocked() StatusSummary {
	summary := StatusSummary{Diagnostics: append([]Diagnostic(nil), manager.diagnostics...)}
	for _, server := range manager.servers {
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

func (manager *Manager) StatusLine() string {
	return manager.Summary().String()
}

func (manager *Manager) CallTool(ctx context.Context, registeredName string, arguments map[string]any) (protocol.CallToolResult, error) {
	return manager.callTool(ctx, registeredName, arguments, nil)
}

// CallToolCaptured forwards an operation-local Capture writer into the
// session before the complete CallToolResult DTO is published.
func (manager *Manager) CallToolCaptured(ctx context.Context, registeredName string, arguments map[string]any, destination io.Writer) (protocol.CallToolResult, error) {
	if destination == nil {
		return protocol.CallToolResult{}, errManagerToolUnavailable
	}
	return manager.callTool(ctx, registeredName, arguments, destination)
}

func (manager *Manager) callTool(ctx context.Context, registeredName string, arguments map[string]any, destination io.Writer) (protocol.CallToolResult, error) {
	if manager == nil {
		return protocol.CallToolResult{}, errManagerToolUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	manager.mu.Lock()
	if manager.state != ManagerStateRunning {
		manager.mu.Unlock()
		return protocol.CallToolResult{}, errManagerToolUnavailable
	}
	toolMeta := manager.byName[registeredName]
	var server *managedServer
	if toolMeta != nil {
		server = manager.servers[toolMeta.serverName]
	}
	if toolMeta == nil || server == nil || server.state != ServerReady || server.session == nil {
		manager.mu.Unlock()
		return protocol.CallToolResult{}, errManagerToolUnavailable
	}
	lease, err := manager.leases.Acquire()
	if err != nil {
		manager.mu.Unlock()
		return protocol.CallToolResult{}, errManagerToolUnavailable
	}
	ownerContext := manager.runContext
	manager.mu.Unlock()
	defer lease.Release()

	callContext, cancelCall := context.WithCancel(ctx)
	stopOwnerCancellation := context.AfterFunc(ownerContext, cancelCall)
	defer func() {
		stopOwnerCancellation()
		cancelCall()
	}()

	select {
	case server.semaphore <- struct{}{}:
		defer func() { <-server.semaphore }()
	case <-callContext.Done():
		return protocol.CallToolResult{}, callContext.Err()
	}
	if err := callContext.Err(); err != nil {
		return protocol.CallToolResult{}, err
	}
	if destination != nil {
		captured, ok := server.session.(capturedManagerSession)
		if !ok {
			return protocol.CallToolResult{}, errManagerToolUnavailable
		}
		return captured.CallToolCaptured(callContext, toolMeta.remoteName, arguments, destination)
	}
	return server.session.CallTool(callContext, toolMeta.remoteName, arguments)
}

// Close rejects new leases synchronously, then runs the only Manager cleanup
// under a background-derived hard deadline. The caller context limits only
// this invocation's wait and is never cached as the final result.
func (manager *Manager) Close(ctx context.Context) error {
	if manager == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	manager.closeOnce.Do(func() {
		manager.mu.Lock()
		wasNew := manager.state == ManagerStateNew
		manager.state = ManagerStateClosing
		manager.tools = nil
		manager.byName = make(map[string]*managedTool)
		leaseDone := manager.leases.BeginClose()
		manager.cancelRun()
		if wasNew {
			manager.startDoneOnce.Do(func() { close(manager.startDone) })
		}
		manager.mu.Unlock()
		go manager.runClose(leaseDone)
	})

	select {
	case <-manager.closeDone:
		return manager.closeErr
	default:
	}
	select {
	case <-manager.closeDone:
		return manager.closeErr
	case <-ctx.Done():
		select {
		case <-manager.closeDone:
			return manager.closeErr
		default:
			return ctx.Err()
		}
	}
}

func (manager *Manager) runClose(leaseDone <-chan struct{}) {
	defer close(manager.closeDone)
	ctx, cancel := context.WithTimeout(context.Background(), manager.cleanupDuration())
	defer cancel()

	if err := waitManagerSignal(ctx, manager.startDone); err != nil {
		manager.finishCleanupTimeout()
		return
	}
	servers := manager.serverSnapshot()
	for _, server := range servers {
		manager.startServerClose(ctx, server)
	}
	var closeErr error
	for _, server := range servers {
		if err := manager.waitServerClose(ctx, server); err != nil && closeErr == nil {
			closeErr = errManagerCloseFailed
		}
	}
	if err := waitManagerSignal(ctx, leaseDone); err != nil {
		manager.finishCleanupTimeout()
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		manager.finishCleanupTimeout()
		return
	}
	manager.finishClose(closeErr)
}

func (manager *Manager) finishCleanupTimeout() {
	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	for _, server := range manager.serverSnapshot() {
		manager.startServerClose(cancelledContext, server)
	}
	if manager.diagnosticSink != nil {
		manager.diagnosticSink.Add(diagnostics.SanitizeInput{
			Code:     diagnosticMCPManagerCleanupTimeout,
			Source:   diagnosticMCPManagerSource,
			Severity: diagnostics.SeverityError,
			Err:      errManagerCleanupTimeout,
		})
	}
	manager.mu.Lock()
	manager.diagnostics = append(manager.diagnostics, Diagnostic{
		Code: diagnosticMCPManagerCleanupTimeout, Source: diagnosticMCPManagerSource,
		Message: errManagerCleanupTimeout.Error(),
	})
	manager.mu.Unlock()
	manager.finishClose(errManagerCleanupTimeout)
}

func (manager *Manager) finishClose(err error) {
	manager.mu.Lock()
	manager.closeErr = err
	manager.state = ManagerStateClosed
	manager.tools = nil
	manager.byName = make(map[string]*managedTool)
	for _, server := range manager.servers {
		server.state = ServerClosed
	}
	manager.mu.Unlock()
}

func (manager *Manager) buildToolCandidates(
	server *managedServer,
	remoteTools []protocol.RemoteTool,
	usedNames map[string]bool,
) ([]tool.Tool, map[string]*managedTool) {
	tools := make([]tool.Tool, 0, len(remoteTools))
	routes := make(map[string]*managedTool, len(remoteTools))
	for _, remoteTool := range remoteTools {
		if remoteTool.Name == "" {
			manager.addDiagnostic(server.name, "skipped MCP tool with empty name")
			continue
		}
		if !validObjectSchema(remoteTool.InputSchema) {
			manager.addDiagnostic(server.name, "skipped MCP tool with invalid inputSchema: "+remoteTool.Name)
			continue
		}
		identity := RegisteredToolName(server.name, remoteTool.Name, usedNames)
		description := remoteTool.Description
		if description == "" {
			description = remoteTool.Title
		}
		adapterOptions := RemoteToolAdapterOptions{
			RegisteredName:     identity.RegisteredName,
			ServerName:         identity.ServerName,
			RemoteName:         identity.RemoteToolName,
			Description:        description,
			Schema:             tool.Schema{Raw: append(json.RawMessage(nil), remoteTool.InputSchema...)},
			ServerConfigDigest: server.configDigest,
			RemoteAnnotations:  append(json.RawMessage(nil), remoteTool.Annotations...),
			Workspace:          manager.workspacePolicy(server),
			Caller:             manager,
			ResultFactory:      manager.resultFactory,
			Capture:            manager.capture,
		}
		var adapter ToolAdapter
		var err error
		if manager.capture == nil {
			// Pre-T4.29a compatibility for the legacy production assembly. The
			// candidate assembly always injects Capture and cannot enter here.
			adapter, err = NewLegacyRemoteToolAdapter(adapterOptions)
		} else {
			adapter, err = NewRemoteToolAdapter(adapterOptions)
		}
		if err != nil {
			manager.addDiagnostic(server.name, "skipped MCP tool with invalid registration metadata")
			continue
		}
		tools = append(tools, adapter)
		routes[identity.RegisteredName] = &managedTool{
			registeredName: identity.RegisteredName,
			serverName:     server.name,
			remoteName:     remoteTool.Name,
		}
	}
	return tools, routes
}

func (manager *Manager) workspacePolicy(server *managedServer) tool.WorkspacePolicy {
	if server == nil {
		return tool.WorkspacePolicy{}
	}
	switch server.config.Type {
	case config.MCPTransportStdio:
		return tool.WorkspacePolicy{Mode: tool.WorkspaceFixed}
	case config.MCPTransportHTTP:
		if manager != nil && manager.options.IndependentHTTPServers[server.name] {
			return tool.WorkspacePolicy{Mode: tool.WorkspaceIndependent}
		}
	}
	return tool.WorkspacePolicy{}
}

func validateIndependentHTTPServers(cfg config.MCPConfig, trusted map[string]bool) error {
	for name, independent := range trusted {
		if !independent {
			continue
		}
		server, ok := cfg.Servers[name]
		if !ok || server.Type != config.MCPTransportHTTP {
			return errManagerWorkspacePolicy
		}
	}
	return nil
}

func cloneBoolMap(source map[string]bool) map[string]bool {
	if source == nil {
		return nil
	}
	cloned := make(map[string]bool, len(source))
	for name, value := range source {
		cloned[name] = value
	}
	return cloned
}

func (manager *Manager) startupContext(caller context.Context) (context.Context, func()) {
	startupContext, cancelStartup := context.WithCancel(manager.runContext)
	stopCallerCancellation := context.AfterFunc(caller, cancelStartup)
	return startupContext, func() { stopCallerCancellation() }
}

func (manager *Manager) starting() bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.state == ManagerStateStarting
}

func (manager *Manager) setServerState(server *managedServer, state ServerState) {
	manager.mu.Lock()
	if server != nil {
		server.state = state
	}
	manager.mu.Unlock()
}

func (manager *Manager) failServer(server *managedServer, _ error) {
	manager.setServerState(server, ServerFailed)
	manager.addDiagnostic(server.name, "MCP server session failed")
}

func (manager *Manager) addDiagnostic(server string, message string) {
	safeMessage := message
	if manager.runtimeRedactor != nil {
		safeMessage = manager.runtimeRedactor.Text(message)
	}
	manager.mu.Lock()
	manager.diagnostics = append(manager.diagnostics, Diagnostic{
		Server: server, Message: SanitizeMetadata(safeMessage, 2048),
	})
	manager.mu.Unlock()
}

func (manager *Manager) serverSnapshot() []*managedServer {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	servers := make([]*managedServer, 0, len(manager.servers))
	for _, server := range manager.servers {
		servers = append(servers, server)
	}
	sort.Slice(servers, func(left, right int) bool { return servers[left].name < servers[right].name })
	return servers
}

func (manager *Manager) startServerClose(ctx context.Context, server *managedServer) {
	if server == nil {
		return
	}
	manager.mu.Lock()
	session := server.session
	manager.mu.Unlock()
	if session == nil {
		if server != nil {
			server.closeOnce.Do(func() { close(server.closeDone) })
		}
		return
	}
	server.closeOnce.Do(func() {
		go func() {
			server.closeErr = session.Close(ctx)
			close(server.closeDone)
		}()
	})
}

func (manager *Manager) rollbackServer(server *managedServer) {
	ctx, cancel := context.WithTimeout(context.Background(), manager.cleanupDuration())
	defer cancel()
	manager.startServerClose(ctx, server)
	_ = manager.waitServerClose(ctx, server)
}

func (manager *Manager) waitServerClose(ctx context.Context, server *managedServer) error {
	if server == nil {
		return nil
	}
	if err := waitManagerSignal(ctx, server.closeDone); err != nil {
		return err
	}
	return server.closeErr
}

func (manager *Manager) closeDetachedSession(session managerSession) {
	if session == nil {
		return
	}
	go func() { _ = session.Close(context.Background()) }()
}

func (manager *Manager) cleanupDuration() time.Duration {
	if manager != nil && manager.cleanupTimeout > 0 && manager.cleanupTimeout <= managerCleanupTimeout {
		return manager.cleanupTimeout
	}
	return managerCleanupTimeout
}

func waitManagerSignal(ctx context.Context, done <-chan struct{}) error {
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		select {
		case <-done:
			return nil
		default:
			return ctx.Err()
		}
	}
}

func (manager *Manager) timeoutFor(server config.MCPServerConfig) time.Duration {
	if server.TimeoutMS > 0 {
		return time.Duration(server.TimeoutMS) * time.Millisecond
	}
	if manager.config.DefaultTimeoutMS > 0 {
		return time.Duration(manager.config.DefaultTimeoutMS) * time.Millisecond
	}
	return manager.options.DefaultTimeout
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

func resolveManagerOptions(cfg config.MCPConfig, options ManagerOptions) ManagerOptions {
	if options.MaxTools <= 0 && cfg.MaxTools > 0 {
		options.MaxTools = int(cfg.MaxTools)
	}
	if options.MaxPages <= 0 && cfg.MaxPages > 0 {
		options.MaxPages = int(cfg.MaxPages)
	}
	if options.MaxResponseBytes <= 0 && cfg.MaxResponseBytes > 0 {
		options.MaxResponseBytes = cfg.MaxResponseBytes
	}
	if options.MaxProtocolErrors <= 0 && cfg.MaxProtocolErrors > 0 {
		options.MaxProtocolErrors = cfg.MaxProtocolErrors
	}
	return options
}

func sameManagerDiagnosticSink(first, second diagnostics.BoundedSink) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	left := reflect.ValueOf(first)
	right := reflect.ValueOf(second)
	if left.Type() != right.Type() {
		return false
	}
	switch left.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice:
		return left.Pointer() == right.Pointer()
	default:
		return left.Type().Comparable() && left.Interface() == right.Interface()
	}
}

func cloneManagerConfig(source config.MCPConfig) config.MCPConfig {
	cloned := source
	cloned.Diagnostics = append([]config.MCPDiagnostic(nil), source.Diagnostics...)
	if source.Servers == nil {
		return cloned
	}
	cloned.Servers = make(map[string]config.MCPServerConfig, len(source.Servers))
	for name, server := range source.Servers {
		server.Args = append([]string(nil), server.Args...)
		server.Env = cloneStringMap(server.Env)
		server.Headers = cloneStringMap(server.Headers)
		cloned.Servers[name] = server
	}
	return cloned
}

type serverConfigDigestEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type serverConfigDigestInput struct {
	Version            string                    `json:"version"`
	Disabled           bool                      `json:"disabled"`
	Type               string                    `json:"type"`
	Command            string                    `json:"command"`
	Args               []string                  `json:"args"`
	Environment        []serverConfigDigestEntry `json:"environment"`
	URL                string                    `json:"url"`
	Headers            []serverConfigDigestEntry `json:"headers"`
	TimeoutNanoseconds int64                     `json:"timeout_nanoseconds"`
	Source             string                    `json:"source"`
	MaxResponseBytes   int64                     `json:"max_response_bytes"`
	MaxTools           int                       `json:"max_tools"`
	MaxPages           int                       `json:"max_pages"`
	MaxProtocolErrors  int64                     `json:"max_protocol_errors"`
}

func serverConfigDigest(server config.MCPServerConfig, options ManagerOptions) ([32]byte, error) {
	environment, err := canonicalDigestEntries(server.Env, canonicalEnvironmentName)
	if err != nil {
		return [32]byte{}, errManagerConfigDigest
	}
	headers, err := canonicalDigestEntries(server.Headers, canonicalHeaderName)
	if err != nil {
		return [32]byte{}, errManagerConfigDigest
	}
	encoded, err := json.Marshal(serverConfigDigestInput{
		Version:            "xagent.mcp.server-config.v1",
		Disabled:           server.Disabled,
		Type:               server.Type,
		Command:            server.Command,
		Args:               append([]string(nil), server.Args...),
		Environment:        environment,
		URL:                server.URL,
		Headers:            headers,
		TimeoutNanoseconds: options.DefaultTimeout.Nanoseconds(),
		Source:             server.Source,
		MaxResponseBytes:   options.MaxResponseBytes,
		MaxTools:           options.MaxTools,
		MaxPages:           options.MaxPages,
		MaxProtocolErrors:  options.MaxProtocolErrors,
	})
	if err != nil {
		return [32]byte{}, errManagerConfigDigest
	}
	digest := sha256.Sum256(encoded)
	if !validServerConfigDigest(digest) {
		return [32]byte{}, errManagerConfigDigest
	}
	return digest, nil
}

func canonicalDigestEntries(
	source map[string]string,
	canonicalize func(string) string,
) ([]serverConfigDigestEntry, error) {
	if len(source) == 0 {
		return []serverConfigDigestEntry{}, nil
	}
	byName := make(map[string]string, len(source))
	for name, value := range source {
		canonical := canonicalize(name)
		if canonical == "" || strings.IndexByte(name, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
			return nil, errManagerConfigDigest
		}
		if _, duplicate := byName[canonical]; duplicate {
			return nil, errManagerConfigDigest
		}
		byName[canonical] = value
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]serverConfigDigestEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, serverConfigDigestEntry{Name: name, Value: byName[name]})
	}
	return entries, nil
}
