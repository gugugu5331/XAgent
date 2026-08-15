package stdio

import (
	"context"
	"errors"

	"xagent/internal/diagnostics"
	"xagent/internal/proctree"
)

var (
	ErrSupervisorUnavailable = errors.New("MCP stdio process supervisor is unavailable")
	ErrProcessWaitFailed     = errors.New("MCP stdio process wait failed")
)

var errSupervisedProcessExited = errors.New("MCP stdio process exited unsuccessfully")

type processOutcome struct {
	result proctree.Result
	err    error
}

// stdioSupervisor is the only stdio-level observer that calls Process.Wait.
// Process remains the owner of the OS wait/reap operation and all process,
// pipe, and protection resources.
type stdioSupervisor struct {
	process     proctree.Process
	diagnostics diagnostics.BoundedSink
	ready       chan struct{}
	done        chan struct{}
	outcome     processOutcome
}

func startStdioSupervisor(process proctree.Process, sink diagnostics.BoundedSink) (*stdioSupervisor, error) {
	if isNilInterface(process) || isNilInterface(sink) {
		return nil, ErrInvalidConfig
	}
	supervisor := &stdioSupervisor{
		process:     process,
		diagnostics: sink,
		ready:       make(chan struct{}),
		done:        make(chan struct{}),
	}
	go supervisor.run()
	return supervisor, nil
}

func (supervisor *stdioSupervisor) run() {
	result, err := supervisor.process.Wait(context.Background())
	if err != nil {
		err = ErrProcessWaitFailed
	}
	supervisor.outcome = processOutcome{result: result, err: err}
	close(supervisor.ready)
	supervisor.report(supervisor.outcome)
	close(supervisor.done)
}

// Wait observes the immutable process outcome. The caller context limits only
// this observation and never cancels the supervisor's sole Process.Wait call.
func (supervisor *stdioSupervisor) Wait(ctx context.Context) (proctree.Result, error) {
	if supervisor == nil {
		return proctree.Result{}, ErrSupervisorUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-supervisor.ready:
		return supervisor.outcome.result, supervisor.outcome.err
	default:
	}
	select {
	case <-supervisor.ready:
		return supervisor.outcome.result, supervisor.outcome.err
	case <-ctx.Done():
		select {
		case <-supervisor.ready:
			return supervisor.outcome.result, supervisor.outcome.err
		default:
			return proctree.Result{}, ctx.Err()
		}
	}
}

// Result performs a non-blocking observation for receive paths. The returned
// outcome is valid only when ready is true.
func (supervisor *stdioSupervisor) Result() (result proctree.Result, err error, ready bool) {
	if supervisor == nil {
		return proctree.Result{}, ErrSupervisorUnavailable, true
	}
	select {
	case <-supervisor.ready:
		return supervisor.outcome.result, supervisor.outcome.err, true
	default:
		return proctree.Result{}, nil, false
	}
}

func (supervisor *stdioSupervisor) Done() <-chan struct{} {
	if supervisor == nil {
		return nil
	}
	return supervisor.done
}

func (supervisor *stdioSupervisor) report(outcome processOutcome) {
	if supervisor == nil || isNilInterface(supervisor.diagnostics) {
		return
	}
	if outcome.err != nil {
		supervisor.diagnostics.Add(diagnostics.SanitizeInput{
			Code:     "mcp_stdio_process_wait_failed",
			Source:   "mcp.transport.stdio",
			Severity: diagnostics.SeverityError,
			Err:      ErrProcessWaitFailed,
		})
		return
	}
	if outcome.result.ExitCode == 0 && !outcome.result.Cancelled && !outcome.result.TimedOut {
		return
	}
	supervisor.diagnostics.Add(diagnostics.SanitizeInput{
		Code:     "mcp_stdio_process_exited",
		Source:   "mcp.transport.stdio",
		Severity: diagnostics.SeverityWarning,
		Err:      errSupervisedProcessExited,
	})
}
