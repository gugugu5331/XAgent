package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"xagent/internal/budget"
	"xagent/internal/diagnostics"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/mcpclient/transport"
)

var (
	errServerSessionOptions        = errors.New("MCP server session options are invalid")
	errServerSessionAlreadyStarted = errors.New("MCP server session was already started")
	errServerSessionStartFailed    = errors.New("MCP server session startup failed")
	errServerSessionProtocol       = errors.New("MCP server session protocol validation failed")
	errManagedConnectionSendFailed = errors.New("MCP connection send failed")
	errManagedConnectionRPCFailed  = errors.New("MCP remote request failed")
	errServerSessionCloseFailed    = errors.New("MCP server session cleanup failed")
	errServerSessionCleanupTimeout = errors.New("MCP server session cleanup exceeded its hard deadline")
	errRemoteSessionCleanupFailed  = errors.New("MCP remote session cleanup failed")
)

const (
	diagnosticMCPRemoteSessionCleanupFailed = "mcp_remote_session_cleanup_failed"
	diagnosticMCPServerSessionTimeout       = "mcp_server_session_cleanup_timeout"
	diagnosticMCPServerSessionSource        = "mcp.server_session"
)

// RemoteSessionCloser is implemented only by transports whose remote protocol
// has an explicit session-termination operation.
type RemoteSessionCloser interface {
	CloseRemote(ctx context.Context) error
}

type serverSessionState uint8

const (
	serverSessionStateNew serverSessionState = iota
	serverSessionStateStarting
	serverSessionStateInitializing
	serverSessionStateDiscovering
	serverSessionStateRunning
	serverSessionStateFailed
	serverSessionStateClosing
	serverSessionStateClosed
)

type serverSessionOptions struct {
	Transport        transport.Transport
	Diagnostics      diagnostics.BoundedSink
	CleanupTimeout   time.Duration
	MaxResponseBytes int64
	MaxPages         int64
	MaxTools         int64
	ClientInfo       protocol.ClientInfo
}

type serverSession struct {
	connection  *managedConnection
	limits      *sessionDiscoveryLimits
	clientInfo  protocol.ClientInfo
	diagnostics diagnostics.BoundedSink
	timeout     time.Duration

	mu              sync.Mutex
	state           serverSessionState
	protocolVersion string
	toolsSupported  bool
	tools           []protocol.RemoteTool

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

type serverSessionSnapshot struct {
	State           serverSessionState
	ProtocolVersion string
	ToolsSupported  bool
	Tools           []protocol.RemoteTool
}

func newServerSession(options serverSessionOptions) (*serverSession, error) {
	if options.Transport == nil || options.Diagnostics == nil {
		return nil, errServerSessionOptions
	}
	limits, err := newSessionDiscoveryLimits(options.MaxResponseBytes, options.MaxPages, options.MaxTools)
	if err != nil {
		return nil, errServerSessionOptions
	}
	connection, err := newManagedConnection(managedConnectionOptions{
		Transport:      options.Transport,
		FrameCounter:   limits.frames.counter,
		Diagnostics:    options.Diagnostics,
		CleanupTimeout: options.CleanupTimeout,
	})
	if err != nil {
		return nil, errServerSessionOptions
	}
	clientInfo := options.ClientInfo
	if clientInfo.Name == "" {
		clientInfo.Name = "xagent"
	}
	if clientInfo.Version == "" {
		clientInfo.Version = "0.1.0"
	}
	timeout := options.CleanupTimeout
	if timeout == 0 {
		timeout = transport.MaxCleanupTimeout
	}
	return &serverSession{
		connection:  connection,
		limits:      limits,
		clientInfo:  clientInfo,
		diagnostics: options.Diagnostics,
		timeout:     timeout,
		state:       serverSessionStateNew,
		closeDone:   make(chan struct{}),
	}, nil
}

func (session *serverSession) Start(ctx context.Context) error {
	if session == nil || session.connection == nil {
		return errServerSessionOptions
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session.mu.Lock()
	if session.state != serverSessionStateNew {
		session.mu.Unlock()
		return errServerSessionAlreadyStarted
	}
	session.state = serverSessionStateStarting
	session.mu.Unlock()

	if err := session.connection.Start(ctx); err != nil {
		session.rollbackStartup()
		return errServerSessionStartFailed
	}
	session.setStartupState(serverSessionStateInitializing)

	initialize := protocol.InitializeRequest{
		ProtocolVersion: protocol.SupportedProtocolVersion,
		ClientInfo:      session.clientInfo,
		Capabilities:    map[string]any{},
	}
	var initialized protocol.InitializeResult
	if err := session.connection.request(ctx, "initialize", initialize, &initialized); err != nil {
		session.rollbackStartup()
		return errServerSessionStartFailed
	}
	if initialized.ProtocolVersion != protocol.SupportedProtocolVersion {
		session.rollbackStartup()
		return errServerSessionProtocol
	}
	if err := session.connection.notify(ctx, "notifications/initialized", nil); err != nil {
		session.rollbackStartup()
		return errServerSessionStartFailed
	}

	session.setStartupState(serverSessionStateDiscovering)
	_, toolsSupported := initialized.Capabilities["tools"]
	discovered := []protocol.RemoteTool{}
	if toolsSupported {
		var err error
		discovered, err = session.discoverTools(ctx)
		if err != nil {
			session.rollbackStartup()
			return err
		}
	}

	session.mu.Lock()
	session.protocolVersion = initialized.ProtocolVersion
	session.toolsSupported = toolsSupported
	session.tools = cloneSessionTools(discovered)
	session.state = serverSessionStateRunning
	session.mu.Unlock()
	return nil
}

func (session *serverSession) discoverTools(ctx context.Context) ([]protocol.RemoteTool, error) {
	discovered := []protocol.RemoteTool{}
	seenCursors := make(map[string]struct{})
	cursor := ""
	for {
		if err := session.limits.pages.consume(1); err != nil {
			return nil, err
		}
		var page protocol.ListToolsResult
		if err := session.connection.request(ctx, "tools/list", protocol.ListToolsRequest{Cursor: cursor}, &page); err != nil {
			return nil, errServerSessionStartFailed
		}
		if err := session.limits.tools.consume(int64(len(page.Tools))); err != nil {
			return nil, err
		}
		discovered = append(discovered, cloneSessionTools(page.Tools)...)
		if page.NextCursor == "" {
			return discovered, nil
		}
		if _, exists := seenCursors[page.NextCursor]; exists {
			return nil, errServerSessionProtocol
		}
		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
}

func (session *serverSession) rollbackStartup() {
	session.mu.Lock()
	session.protocolVersion = ""
	session.toolsSupported = false
	session.tools = nil
	session.state = serverSessionStateFailed
	session.mu.Unlock()
	_ = session.connection.Close(context.Background())
}

func (session *serverSession) setStartupState(state serverSessionState) {
	session.mu.Lock()
	session.state = state
	session.mu.Unlock()
}

func (session *serverSession) Snapshot() serverSessionSnapshot {
	if session == nil {
		return serverSessionSnapshot{State: serverSessionStateClosed}
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	snapshot := serverSessionSnapshot{State: session.state}
	if session.state != serverSessionStateRunning {
		return snapshot
	}
	snapshot.ProtocolVersion = session.protocolVersion
	snapshot.ToolsSupported = session.toolsSupported
	snapshot.Tools = cloneSessionTools(session.tools)
	return snapshot
}

// CallTool dispatches one tools/call request only after the session has
// completed initialization and discovery. The returned value is the protocol
// package's decoded DTO; legacy root-package protocol types never cross this
// boundary.
func (session *serverSession) CallTool(
	ctx context.Context,
	remoteName string,
	arguments map[string]any,
) (protocol.CallToolResult, error) {
	if session == nil || session.connection == nil || remoteName == "" {
		return protocol.CallToolResult{}, errManagerToolUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session.mu.Lock()
	running := session.state == serverSessionStateRunning
	session.mu.Unlock()
	if !running {
		return protocol.CallToolResult{}, errManagerToolUnavailable
	}

	var result protocol.CallToolResult
	request := protocol.CallToolRequest{
		Name:      remoteName,
		Arguments: cloneMCPArguments(arguments),
	}
	if err := session.connection.request(ctx, "tools/call", request, &result); err != nil {
		return protocol.CallToolResult{}, err
	}
	return result, nil
}

// CallToolCaptured is the production MCP output path.  The destination is
// supplied before dispatch and receives decoded user content before the
// complete CallToolResult DTO is published to the caller.
func (session *serverSession) CallToolCaptured(
	ctx context.Context,
	remoteName string,
	arguments map[string]any,
	destination io.Writer,
) (protocol.CallToolResult, error) {
	if destination == nil {
		return protocol.CallToolResult{}, errManagerToolUnavailable
	}
	if session == nil || session.connection == nil || remoteName == "" {
		return protocol.CallToolResult{}, errManagerToolUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session.mu.Lock()
	running := session.state == serverSessionStateRunning
	session.mu.Unlock()
	if !running {
		return protocol.CallToolResult{}, errManagerToolUnavailable
	}
	var result protocol.CallToolResult
	request := protocol.CallToolRequest{Name: remoteName, Arguments: cloneMCPArguments(arguments)}
	if err := session.connection.requestCaptured(ctx, "tools/call", request, &result, destination); err != nil {
		return protocol.CallToolResult{}, err
	}
	return result, nil
}

// Close starts one background cleanup that first attempts protocol-level
// session termination and then always starts Connection's local cleanup. The
// caller context limits only this invocation's wait.
func (session *serverSession) Close(ctx context.Context) error {
	if session == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session.closeOnce.Do(func() {
		session.mu.Lock()
		session.state = serverSessionStateClosing
		session.protocolVersion = ""
		session.toolsSupported = false
		session.tools = nil
		session.mu.Unlock()
		go session.runClose()
	})

	select {
	case <-session.closeDone:
		return session.closeErr
	default:
	}
	select {
	case <-session.closeDone:
		return session.closeErr
	case <-ctx.Done():
		select {
		case <-session.closeDone:
			return session.closeErr
		default:
			return ctx.Err()
		}
	}
}

func (session *serverSession) runClose() {
	defer close(session.closeDone)
	ctx, cancel := context.WithTimeout(context.Background(), session.timeout)
	defer cancel()

	remoteErr := session.closeRemoteIfAvailable(ctx)
	if remoteErr != nil && !errors.Is(remoteErr, context.DeadlineExceeded) {
		session.diagnostics.Add(diagnostics.SanitizeInput{
			Code:     diagnosticMCPRemoteSessionCleanupFailed,
			Source:   diagnosticMCPServerSessionSource,
			Severity: diagnostics.SeverityWarning,
			Err:      errRemoteSessionCleanupFailed,
		})
	}

	localErr := session.connection.Close(ctx)
	if errors.Is(remoteErr, context.DeadlineExceeded) || errors.Is(localErr, context.DeadlineExceeded) ||
		errors.Is(ctx.Err(), context.DeadlineExceeded) {
		session.diagnostics.Add(diagnostics.SanitizeInput{
			Code:     diagnosticMCPServerSessionTimeout,
			Source:   diagnosticMCPServerSessionSource,
			Severity: diagnostics.SeverityError,
			Err:      errServerSessionCleanupTimeout,
		})
		session.finishClose(errServerSessionCleanupTimeout)
		return
	}
	if localErr != nil {
		session.finishClose(errServerSessionCloseFailed)
		return
	}
	session.finishClose(nil)
}

func (session *serverSession) closeRemoteIfAvailable(ctx context.Context) error {
	if session == nil || session.connection == nil {
		return nil
	}
	connection := session.connection
	connection.mu.Lock()
	available := connection.state == managedConnectionStateRunning
	closer, supported := connection.transport.(RemoteSessionCloser)
	connection.mu.Unlock()
	if !available || !supported {
		return nil
	}
	return closer.CloseRemote(ctx)
}

func (session *serverSession) finishClose(err error) {
	session.mu.Lock()
	session.closeErr = err
	session.state = serverSessionStateClosed
	session.mu.Unlock()
}

func cloneSessionTools(source []protocol.RemoteTool) []protocol.RemoteTool {
	if source == nil {
		return nil
	}
	cloned := make([]protocol.RemoteTool, len(source))
	for index, tool := range source {
		cloned[index] = tool
		cloned[index].InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
		cloned[index].OutputSchema = append(json.RawMessage(nil), tool.OutputSchema...)
		cloned[index].Annotations = append(json.RawMessage(nil), tool.Annotations...)
		cloned[index].Meta = append(json.RawMessage(nil), tool.Meta...)
	}
	return cloned
}

func (connection *managedConnection) request(ctx context.Context, method string, params any, target any) error {
	return connection.requestWithCapture(ctx, method, params, target, nil)
}

func (connection *managedConnection) requestCaptured(ctx context.Context, method string, params any, target any, destination io.Writer) error {
	if destination == nil {
		return errManagedConnectionSendFailed
	}
	return connection.requestWithCapture(ctx, method, params, target, destination)
}

func (connection *managedConnection) requestWithCapture(ctx context.Context, method string, params any, target any, destination io.Writer) error {
	paramsFrame, err := encodeSessionParams(params)
	if err != nil {
		return errManagedConnectionSendFailed
	}
	id, call, err := connection.registerRequest(method)
	if err != nil {
		return err
	}
	requestFrame, err := protocol.Encode(protocol.RPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  paramsFrame,
	})
	if err != nil {
		connection.completeRequestFailure(id, errManagedConnectionSendFailed)
		return connection.waitForOutcome(ctx, id, call).err
	}
	if destination != nil {
		binder, ok := connection.transport.(transport.OutputCaptureBinder)
		if !ok || binder.BindOutputCapture(requestFrame, destination) != nil {
			connection.completeRequestFailure(id, errManagedConnectionSendFailed)
			return connection.waitForOutcome(ctx, id, call).err
		}
		if unbinder, ok := connection.transport.(transport.OutputCaptureUnbinder); ok {
			defer unbinder.UnbindOutputCapture(requestFrame)
		}
	}
	if err := connection.transport.Send(ctx, requestFrame); err != nil {
		connection.completeRequestFailure(id, errManagedConnectionSendFailed)
	}
	outcome := connection.waitForOutcome(ctx, id, call)
	if outcome.err != nil {
		return outcome.err
	}
	if outcome.response.Error != nil {
		return errManagedConnectionRPCFailed
	}
	if target == nil {
		return nil
	}
	if destination != nil {
		resultTarget, ok := target.(*protocol.CallToolResult)
		if !ok || resultTarget == nil {
			return errServerSessionProtocol
		}
		if outcome.captured != nil {
			*resultTarget = outcome.captured.Result
			return nil
		}
		return errServerSessionProtocol
	}
	if err := json.Unmarshal(outcome.response.Result, target); err != nil {
		return errServerSessionProtocol
	}
	return nil
}

func (connection *managedConnection) notify(ctx context.Context, method string, params any) error {
	paramsFrame, err := encodeSessionParams(params)
	if err != nil {
		return errManagedConnectionSendFailed
	}
	frame, err := protocol.Encode(protocol.RPCNotification{JSONRPC: "2.0", Method: method, Params: paramsFrame})
	if err != nil {
		return errManagedConnectionSendFailed
	}
	connection.mu.Lock()
	running := connection.state == managedConnectionStateRunning
	connection.mu.Unlock()
	if !running {
		return errManagedConnectionNotRunning
	}
	if err := connection.transport.Send(ctx, frame); err != nil {
		return errManagedConnectionSendFailed
	}
	return nil
}

func (connection *managedConnection) completeRequestFailure(id protocol.RPCID, failure error) {
	if call, ok := connection.pending.takePending(id); ok {
		call.deliver(pendingOutcome{err: failure})
	}
}

func encodeSessionParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return protocol.Encode(params)
}

type sessionDiscoveryLimits struct {
	frames *sessionScopedCounter
	pages  *sessionScopedCounter
	tools  *sessionScopedCounter
}

type sessionScopedCounter struct {
	scope     budget.Scope
	dimension budget.Dimension
	counter   *budget.Counter
}

func newSessionDiscoveryLimits(maxBytes, maxPages, maxTools int64) (*sessionDiscoveryLimits, error) {
	frames, err := newSessionScopedCounter(budget.MCPMaxResponseBytes, maxBytes)
	if err != nil {
		return nil, err
	}
	pages, err := newSessionScopedCounter(budget.MCPMaxPages, maxPages)
	if err != nil {
		return nil, err
	}
	tools, err := newSessionScopedCounter(budget.MCPMaxTools, maxTools)
	if err != nil {
		return nil, err
	}
	return &sessionDiscoveryLimits{frames: frames, pages: pages, tools: tools}, nil
}

func newSessionScopedCounter(scope budget.Scope, configured int64) (*sessionScopedCounter, error) {
	spec, ok := sessionBudgetSpec(scope)
	if !ok {
		return nil, errServerSessionOptions
	}
	var candidate *int64
	if configured != 0 {
		candidate = &configured
	}
	effectiveValue, err := spec.Resolve(candidate)
	if err != nil {
		return nil, err
	}
	effective, err := budget.NewLimits(budget.Limit{Dimension: spec.Dimension, Value: effectiveValue})
	if err != nil {
		return nil, err
	}
	hard, err := budget.NewLimits(budget.Limit{Dimension: spec.Dimension, Value: spec.HardCap})
	if err != nil {
		return nil, err
	}
	counter, err := budget.NewCounter(effective, hard)
	if err != nil {
		return nil, err
	}
	return &sessionScopedCounter{scope: scope, dimension: spec.Dimension, counter: counter}, nil
}

func sessionBudgetSpec(scope budget.Scope) (budget.Spec, bool) {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope == scope {
			return spec, true
		}
	}
	return budget.Spec{}, false
}

func (counter *sessionScopedCounter) consume(amount int64) error {
	if counter == nil || counter.counter == nil {
		return errServerSessionOptions
	}
	err := counter.counter.Consume(counter.dimension, amount)
	if err == nil {
		return nil
	}
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) {
		return err
	}
	return &budget.LimitError{
		Scope:     string(counter.scope),
		Dimension: limitErr.Dimension,
		Limit:     limitErr.Limit,
		Observed:  limitErr.Observed,
	}
}
