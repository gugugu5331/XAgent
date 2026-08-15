package mcpclient

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"xagent/internal/budget"
	"xagent/internal/diagnostics"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/mcpclient/transport"
)

var (
	errManagedConnectionOptions        = errors.New("MCP connection options are invalid")
	errManagedConnectionAlreadyStarted = errors.New("MCP connection was already started")
	errManagedConnectionNotRunning     = errors.New("MCP connection is not running")
	errManagedConnectionStartFailed    = errors.New("MCP connection failed to start")
	errManagedConnectionFailed         = errors.New("MCP connection failed")
	errManagedConnectionClosed         = errors.New("MCP connection is closed")
	errManagedConnectionCloseFailed    = errors.New("MCP connection cleanup failed")
	errManagedConnectionCleanupTimeout = errors.New("MCP connection cleanup exceeded its hard deadline")
	errManagedConnectionUnknownID      = errors.New("MCP response id is unknown or no longer pending")
)

const maxManagedConnectionCleanupTimeout = 2 * time.Second

const (
	diagnosticMCPConnectionFailed    = "mcp_connection_failed"
	diagnosticMCPConnectionTimeout   = "mcp_connection_cleanup_timeout"
	diagnosticMCPUnknownResponseID   = "mcp_connection_unknown_response_id"
	diagnosticMCPConnectionSource    = "mcp.connection"
	diagnosticMCPResponseIgnoredHint = "response_ignored"
	diagnosticMCPTransportClosedHint = "transport_closed"
)

type managedConnectionState uint8

const (
	managedConnectionStateNew managedConnectionState = iota
	managedConnectionStateStarting
	managedConnectionStateRunning
	managedConnectionStateFailed
	managedConnectionStateClosing
	managedConnectionStateClosed
)

type managedConnectionOptions struct {
	Transport      transport.Transport
	FrameCounter   *budget.Counter
	Diagnostics    diagnostics.BoundedSink
	CleanupTimeout time.Duration
}

// managedConnection is the staged replacement for the legacy Connection.
// It remains internal until the atomic MCP entry-point migration in T2.69.
type managedConnection struct {
	transport    transport.Transport
	frameCounter *budget.Counter
	diagnostics  diagnostics.BoundedSink
	pending      *pendingTable
	timeout      time.Duration

	mu     sync.Mutex
	state  managedConnectionState
	nextID atomic.Int64

	receiveDone     chan struct{}
	receiveDoneOnce sync.Once

	transportCloseOnce    sync.Once
	transportCloseStarted chan struct{}
	transportCloseDone    chan struct{}
	transportCloseErr     error

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func newManagedConnection(options managedConnectionOptions) (*managedConnection, error) {
	if options.Transport == nil || options.FrameCounter == nil || options.Diagnostics == nil {
		return nil, errManagedConnectionOptions
	}
	timeout := options.CleanupTimeout
	if timeout < 0 || timeout > maxManagedConnectionCleanupTimeout {
		return nil, errManagedConnectionOptions
	}
	if timeout == 0 {
		timeout = maxManagedConnectionCleanupTimeout
	}
	return &managedConnection{
		transport:             options.Transport,
		frameCounter:          options.FrameCounter,
		diagnostics:           options.Diagnostics,
		pending:               newPendingTable(),
		timeout:               timeout,
		state:                 managedConnectionStateNew,
		receiveDone:           make(chan struct{}),
		transportCloseStarted: make(chan struct{}),
		transportCloseDone:    make(chan struct{}),
		closeDone:             make(chan struct{}),
	}, nil
}

// Start owns the only receive goroutine. The starting state prevents a second
// caller from invoking Transport.Start while the first start is in progress.
func (connection *managedConnection) Start(ctx context.Context) error {
	if connection == nil {
		return errManagedConnectionOptions
	}
	if ctx == nil {
		ctx = context.Background()
	}

	connection.mu.Lock()
	if connection.state != managedConnectionStateNew {
		connection.mu.Unlock()
		return errManagedConnectionAlreadyStarted
	}
	connection.state = managedConnectionStateStarting
	connection.mu.Unlock()

	if err := connection.transport.Start(ctx); err != nil {
		connection.fail()
		connection.closeReceiveDone()
		return errManagedConnectionStartFailed
	}

	connection.mu.Lock()
	if connection.state != managedConnectionStateStarting {
		connection.mu.Unlock()
		connection.closeReceiveDone()
		return errManagedConnectionClosed
	}
	connection.state = managedConnectionStateRunning
	connection.mu.Unlock()
	go connection.receiveLoop(ctx)
	return nil
}

// registerRequest performs the Running check and pending publication under
// the same connection lock used by failure. A failure cannot take all pending
// and then lose a race to a newly registered orphan.
func (connection *managedConnection) registerRequest(method string) (protocol.RPCID, *pendingCall, error) {
	if connection == nil {
		return protocol.RPCID{}, nil, errManagedConnectionNotRunning
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.state != managedConnectionStateRunning {
		return protocol.RPCID{}, nil, errManagedConnectionNotRunning
	}
	id := protocol.NumberID(connection.nextID.Add(1))
	call, err := connection.pending.registerPending(id, method)
	if err != nil {
		return protocol.RPCID{}, nil, err
	}
	return id, call, nil
}

// waitForOutcome makes cancellation participate in the same takePending race
// as response, send failure, and connection termination. If another path has
// already taken the pending call, cancellation waits for that winner to
// publish the sole outcome instead of returning a competing result.
func (connection *managedConnection) waitForOutcome(ctx context.Context, id protocol.RPCID, call *pendingCall) pendingOutcome {
	if connection == nil || call == nil {
		return pendingOutcome{err: errManagedConnectionNotRunning}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case outcome := <-call.outcome:
		return outcome
	case <-ctx.Done():
		if winner, ok := connection.pending.takePending(id); ok {
			winner.deliver(pendingOutcome{err: ctx.Err()})
		}
		return <-call.outcome
	}
}

func (connection *managedConnection) receiveLoop(ctx context.Context) {
	defer connection.closeReceiveDone()
	for {
		event, err := connection.transport.Receive(ctx)
		if err != nil {
			connection.fail()
			return
		}

		if event.Captured != nil {
			if event.CapturedBytes <= 0 || connection.frameCounter.Consume(budget.Bytes, event.CapturedBytes) != nil {
				connection.fail()
				return
			}
			response := protocol.RPCResponse{JSONRPC: event.Captured.JSONRPC, ID: event.Captured.ID}
			if event.Captured.HasError {
				response.Error = &protocol.RPCError{}
			}
			if err := protocol.ValidateRPCResponse(response); err != nil {
				connection.fail()
				return
			}
			connection.completeResponse(response, event.Captured)
			continue
		}
		var response protocol.RPCResponse
		if err := protocol.Decode(connection.frameCounter, event.Frame, &response); err != nil {
			connection.fail()
			return
		}
		if err := protocol.ValidateRPCResponse(response); err != nil {
			connection.fail()
			return
		}
		connection.completeResponse(response, nil)
	}
}

func (connection *managedConnection) completeResponse(response protocol.RPCResponse, captured *protocol.CapturedCallToolResponse) {
	call, ok := connection.pending.takePending(response.ID)
	if !ok {
		connection.diagnostics.Add(diagnostics.SanitizeInput{
			Code:     diagnosticMCPUnknownResponseID,
			Source:   diagnosticMCPConnectionSource,
			Hint:     diagnosticMCPResponseIgnoredHint,
			Severity: diagnostics.SeverityWarning,
			Err:      errManagedConnectionUnknownID,
		})
		return
	}
	call.deliver(pendingOutcome{response: response, captured: captured})
}

// fail has exactly one state winner. It transfers every pending call while
// still holding the state lock, then completes calls and closes the transport
// without retaining or reporting the raw receive/decode error.
func (connection *managedConnection) fail() bool {
	connection.mu.Lock()
	switch connection.state {
	case managedConnectionStateStarting, managedConnectionStateRunning:
		connection.state = managedConnectionStateFailed
	default:
		connection.mu.Unlock()
		return false
	}
	calls := connection.pending.takeAllPending()
	connection.mu.Unlock()

	for _, call := range calls {
		call.deliver(pendingOutcome{err: errManagedConnectionFailed})
	}
	connection.diagnostics.Add(diagnostics.SanitizeInput{
		Code:     diagnosticMCPConnectionFailed,
		Source:   diagnosticMCPConnectionSource,
		Hint:     diagnosticMCPTransportClosedHint,
		Severity: diagnostics.SeverityError,
		Err:      errManagedConnectionFailed,
	})
	connection.startTransportClose(context.Background())
	return true
}

// Close starts one background cleanup with its own hard deadline. A caller's
// context controls only how long that caller waits for the stable final result.
func (connection *managedConnection) Close(ctx context.Context) error {
	if connection == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	connection.closeOnce.Do(func() {
		connection.mu.Lock()
		noReceiveLoop := connection.state == managedConnectionStateNew
		connection.state = managedConnectionStateClosing
		calls := connection.pending.takeAllPending()
		connection.mu.Unlock()

		for _, call := range calls {
			call.deliver(pendingOutcome{err: errManagedConnectionClosed})
		}
		if noReceiveLoop {
			connection.closeReceiveDone()
		}
		go connection.runClose()
	})

	select {
	case <-connection.closeDone:
		return connection.closeErr
	default:
	}
	select {
	case <-connection.closeDone:
		return connection.closeErr
	case <-ctx.Done():
		select {
		case <-connection.closeDone:
			return connection.closeErr
		default:
			return ctx.Err()
		}
	}
}

func (connection *managedConnection) runClose() {
	defer close(connection.closeDone)
	ctx, cancel := context.WithTimeout(context.Background(), connection.timeout)
	defer cancel()

	connection.startTransportClose(ctx)
	var finalErr error
	select {
	case <-connection.transportCloseDone:
		if errors.Is(connection.transportCloseErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			connection.finishCloseTimeout()
			return
		}
		if connection.transportCloseErr != nil {
			finalErr = errManagedConnectionCloseFailed
		}
	case <-ctx.Done():
		connection.finishCloseTimeout()
		return
	}

	select {
	case <-connection.receiveDone:
		connection.finishClose(finalErr)
	case <-ctx.Done():
		connection.finishCloseTimeout()
	}
}

func (connection *managedConnection) startTransportClose(ctx context.Context) {
	connection.transportCloseOnce.Do(func() {
		go func() {
			close(connection.transportCloseStarted)
			connection.transportCloseErr = connection.transport.Close(ctx)
			close(connection.transportCloseDone)
		}()
	})
	<-connection.transportCloseStarted
}

func (connection *managedConnection) finishClose(err error) {
	connection.mu.Lock()
	connection.closeErr = err
	connection.state = managedConnectionStateClosed
	connection.mu.Unlock()
}

func (connection *managedConnection) finishCloseTimeout() {
	connection.diagnostics.Add(diagnostics.SanitizeInput{
		Code:     diagnosticMCPConnectionTimeout,
		Source:   diagnosticMCPConnectionSource,
		Severity: diagnostics.SeverityError,
		Err:      errManagedConnectionCleanupTimeout,
	})
	connection.finishClose(errManagedConnectionCleanupTimeout)
}

func (connection *managedConnection) closeReceiveDone() {
	connection.receiveDoneOnce.Do(func() {
		close(connection.receiveDone)
	})
}

func (connection *managedConnection) currentState() managedConnectionState {
	if connection == nil {
		return managedConnectionStateClosed
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.state
}

func (connection *managedConnection) pendingCount() int {
	if connection == nil {
		return 0
	}
	return connection.pending.count()
}
