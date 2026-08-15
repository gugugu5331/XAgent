package transport

import (
	"context"
	"errors"
	"sync"
	"time"

	"xagent/internal/diagnostics"
)

var (
	ErrInvalidTransition = errors.New("invalid MCP transport state transition")
	ErrNotRunning        = errors.New("MCP transport is not running")
	ErrClosing           = errors.New("MCP transport is closing")
	ErrCleanupTimeout    = errors.New("MCP transport cleanup exceeded its hard deadline")
)

type TransportState uint8

const (
	TransportStateNew TransportState = iota
	TransportStateStarting
	TransportStateRunning
	TransportStateFailed
	TransportStateClosing
	TransportStateClosed
)

func (state TransportState) String() string {
	switch state {
	case TransportStateNew:
		return "new"
	case TransportStateStarting:
		return "starting"
	case TransportStateRunning:
		return "running"
	case TransportStateFailed:
		return "failed"
	case TransportStateClosing:
		return "closing"
	case TransportStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

type StartFunc func(context.Context) error
type CleanupFunc func(context.Context) error
type ForceCloseFunc func()

// StateMachine coordinates logical transport state and one asynchronous local
// cleanup. Close callers only control how long they wait; cleanup always uses
// its own background-derived hard deadline.
type StateMachine struct {
	mu          sync.Mutex
	state       TransportState
	startDone   chan struct{}
	timeout     time.Duration
	diagnostics diagnostics.BoundedSink
	cleanup     CleanupFunc
	forceClose  ForceCloseFunc

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func NewStateMachine(options Options, cleanup CleanupFunc, forceClose ForceCloseFunc) (*StateMachine, error) {
	resolved, err := resolveOptions(options)
	if err != nil {
		return nil, err
	}
	if cleanup == nil || forceClose == nil {
		return nil, errors.New("MCP transport cleanup ownership is invalid")
	}
	return &StateMachine{
		state:       TransportStateNew,
		timeout:     resolved.cleanupTimeout,
		diagnostics: resolved.diagnostics,
		cleanup:     cleanup,
		forceClose:  forceClose,
		closeDone:   make(chan struct{}),
	}, nil
}

func (machine *StateMachine) State() TransportState {
	if machine == nil {
		return TransportStateClosed
	}
	machine.mu.Lock()
	defer machine.mu.Unlock()
	return machine.state
}

// Start permits exactly one start attempt. A concurrent Close waits for that
// attempt inside the independent cleanup deadline before reclaiming resources.
func (machine *StateMachine) Start(ctx context.Context, start StartFunc) error {
	if machine == nil || start == nil {
		return ErrInvalidTransition
	}
	if ctx == nil {
		ctx = context.Background()
	}

	machine.mu.Lock()
	if machine.state != TransportStateNew {
		machine.mu.Unlock()
		return ErrInvalidTransition
	}
	machine.state = TransportStateStarting
	machine.startDone = make(chan struct{})
	startDone := machine.startDone
	machine.mu.Unlock()

	err := start(ctx)

	machine.mu.Lock()
	switch machine.state {
	case TransportStateStarting:
		if err != nil {
			machine.state = TransportStateFailed
		} else {
			machine.state = TransportStateRunning
		}
	case TransportStateClosing, TransportStateClosed:
		if err == nil {
			err = ErrClosing
		}
	default:
		if err == nil {
			err = ErrInvalidTransition
		}
	}
	close(startDone)
	machine.mu.Unlock()
	return err
}

// RequireRunning is the common Send/Receive admission check. Closing is
// reported distinctly so no new I/O begins after Close wins the state lock.
func (machine *StateMachine) RequireRunning() error {
	if machine == nil {
		return ErrNotRunning
	}
	machine.mu.Lock()
	defer machine.mu.Unlock()
	switch machine.state {
	case TransportStateRunning:
		return nil
	case TransportStateClosing, TransportStateClosed:
		return ErrClosing
	default:
		return ErrNotRunning
	}
}

// MarkFailed records an unrecoverable framing or physical I/O failure. The
// failure value itself remains with the caller and is not retained by state.
func (machine *StateMachine) MarkFailed() error {
	if machine == nil {
		return ErrInvalidTransition
	}
	machine.mu.Lock()
	defer machine.mu.Unlock()
	if machine.state != TransportStateRunning {
		return ErrInvalidTransition
	}
	machine.state = TransportStateFailed
	return nil
}

func (machine *StateMachine) Close(ctx context.Context) error {
	if machine == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	machine.closeOnce.Do(func() {
		machine.mu.Lock()
		if machine.state != TransportStateClosed {
			machine.state = TransportStateClosing
		}
		machine.mu.Unlock()
		go machine.runCleanup()
	})

	select {
	case <-machine.closeDone:
		return machine.closeErr
	default:
	}
	select {
	case <-machine.closeDone:
		return machine.closeErr
	case <-ctx.Done():
		select {
		case <-machine.closeDone:
			return machine.closeErr
		default:
			return ctx.Err()
		}
	}
}

func (machine *StateMachine) runCleanup() {
	defer close(machine.closeDone)
	ctx, cancel := context.WithTimeout(context.Background(), machine.timeout)
	defer cancel()

	machine.mu.Lock()
	startDone := machine.startDone
	machine.mu.Unlock()
	if startDone != nil {
		select {
		case <-startDone:
		case <-ctx.Done():
			machine.finishCleanupTimeout()
			return
		}
	}

	result := make(chan error, 1)
	go func() {
		result <- machine.cleanup(ctx)
	}()

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			machine.finishCleanup(err)
			return
		}
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			machine.finishCleanup(ctx.Err())
			return
		}
	}
	machine.finishCleanupTimeout()
}

func (machine *StateMachine) finishCleanup(err error) {
	machine.mu.Lock()
	machine.closeErr = err
	machine.state = TransportStateClosed
	machine.mu.Unlock()
}

func (machine *StateMachine) finishCleanupTimeout() {
	machine.forceClose()
	machine.diagnostics.Add(diagnostics.SanitizeInput{
		Code:     "mcp_transport_cleanup_timeout",
		Source:   "mcp.transport",
		Severity: diagnostics.SeverityError,
		Err:      ErrCleanupTimeout,
	})
	machine.finishCleanup(ErrCleanupTimeout)
}
