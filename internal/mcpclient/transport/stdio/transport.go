package stdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"xagent/internal/diagnostics"
	mcptransport "xagent/internal/mcpclient/transport"
	"xagent/internal/proctree"
	"xagent/internal/safefs"
)

var (
	ErrInvalidConfig      = errors.New("MCP stdio transport configuration is invalid")
	ErrStartFailed        = errors.New("MCP stdio protected process start failed")
	ErrFramingUnavailable = errors.New("MCP stdio framing is not available")
)

// Transport owns one protected Process. Its three streams are deliberately
// retained as narrowed borrowed interfaces, so only Process can close their
// owner handles. Framing, stderr draining, the single writer, process
// supervision, and complete Close convergence are all bounded here.
type Transport struct {
	runner           proctree.Runner
	planFactory      proctree.ProtectionPlanFactory
	executable       string
	args             []string
	env              []string
	workingDirectory *safefs.Root
	maxResponseBytes int64
	cleanupTimeout   time.Duration
	diagnostics      diagnostics.BoundedSink
	state            *mcptransport.StateMachine
	startupContext   context.Context
	cancelStartup    context.CancelFunc
	receives         *receiveAdmission
	processCloseDone chan struct{}
	processCloseOnce sync.Once
	processCloseErr  error

	mu          sync.Mutex
	process     proctree.Process
	stdin       io.Writer
	stdout      io.Reader
	stderr      io.Reader
	framing     *newlineDecoder
	stderrDrain *stderrWorker
	writer      *writerWorker
	supervisor  *stdioSupervisor
	captures    *mcptransport.OutputCaptureBindings
}

var _ mcptransport.Transport = (*Transport)(nil)

func New(config Config) (*Transport, error) {
	resolved, err := resolveConfig(config)
	if err != nil {
		return nil, err
	}
	startupContext, cancelStartup := context.WithCancel(context.Background())
	created := &Transport{
		runner:           resolved.runner,
		planFactory:      resolved.planFactory,
		executable:       resolved.executable,
		args:             resolved.args,
		env:              resolved.env,
		workingDirectory: resolved.workingDirectory,
		maxResponseBytes: resolved.maxResponseBytes,
		cleanupTimeout:   resolved.cleanupTimeout,
		diagnostics:      resolved.lifecycle.Diagnostics,
		startupContext:   startupContext,
		cancelStartup:    cancelStartup,
		receives:         newReceiveAdmission(),
		processCloseDone: make(chan struct{}),
		captures:         mcptransport.NewOutputCaptureBindings(),
	}
	machine, err := mcptransport.NewStateMachine(resolved.lifecycle, created.cleanup, created.forceClose)
	if err != nil {
		cancelStartup()
		return nil, ErrInvalidConfig
	}
	created.state = machine
	return created, nil
}

// BindOutputCapture establishes the destination before the protected process
// can write the matching response to stdout.
func (transport *Transport) BindOutputCapture(frame json.RawMessage, destination io.Writer) error {
	if transport == nil || transport.captures == nil {
		return mcptransport.ErrOutputCaptureBinding
	}
	return transport.captures.Bind(frame, destination)
}

func (transport *Transport) UnbindOutputCapture(frame json.RawMessage) {
	if transport != nil && transport.captures != nil {
		transport.captures.Remove(frame)
	}
}

func (transport *Transport) Start(ctx context.Context) error {
	if transport == nil || transport.state == nil || isNilInterface(transport.runner) || isNilInterface(transport.planFactory) {
		return ErrInvalidConfig
	}
	startupContext, cancelStartup := transport.startupRequestContext(ctx)
	defer cancelStartup()
	return transport.state.Start(startupContext, transport.startProtectedProcess)
}

func (transport *Transport) startProtectedProcess(ctx context.Context) error {
	plan, err := transport.planFactory.Create(ctx)
	if err != nil {
		return normalizeStartError(err)
	}

	process, startErr := transport.runner.Start(ctx, proctree.Request{
		Executable: transport.executable,
		Args:       append([]string(nil), transport.args...),
		WorkingDir: transport.workingDirectory,
		Env:        append([]string(nil), transport.env...),
		Mode:       proctree.ProtectionRequired,
		Protection: plan,
	})
	if !isNilInterface(process) {
		transport.ownProcess(process)
	}
	if startErr != nil {
		transport.rollbackProcess(process)
		return normalizeStartError(startErr)
	}
	if isNilInterface(process) {
		return ErrStartFailed
	}
	supervisor, err := startStdioSupervisor(process, transport.diagnostics)
	if err != nil {
		transport.rollbackProcess(process)
		return ErrStartFailed
	}
	transport.trackSupervisor(supervisor)
	if err := transport.startupStatus(ctx); err != nil {
		transport.rollbackProcess(process)
		return err
	}

	pipes := process.Pipes()
	if err := transport.startupStatus(ctx); err != nil {
		transport.rollbackProcess(process)
		return err
	}
	if isNilInterface(pipes.Stdin) || isNilInterface(pipes.Stdout) || isNilInterface(pipes.Stderr) {
		transport.rollbackProcess(process)
		return ErrStartFailed
	}
	framing, err := newNewlineDecoder(pipes.Stdout, transport.maxResponseBytes)
	if err != nil {
		transport.rollbackProcess(process)
		return ErrStartFailed
	}
	if err := transport.startupStatus(ctx); err != nil {
		transport.rollbackProcess(process)
		return err
	}
	stderrDrain, err := startStderrWorker(pipes.Stderr, transport.diagnostics)
	if err != nil {
		transport.rollbackProcess(process)
		return ErrStartFailed
	}
	transport.trackStderr(stderrDrain)
	if err := transport.startupStatus(ctx); err != nil {
		transport.rollbackProcess(process)
		return err
	}
	writer, err := startWriterWorker(pipes.Stdin, transport.requestClose)
	if err != nil {
		transport.rollbackProcess(process)
		return ErrStartFailed
	}
	transport.trackWriter(writer)
	if err := transport.startupStatus(ctx); err != nil {
		transport.rollbackProcess(process)
		return err
	}
	transport.mu.Lock()
	transport.stdin = pipes.Stdin
	transport.stdout = pipes.Stdout
	transport.stderr = pipes.Stderr
	transport.framing = framing
	transport.mu.Unlock()
	if err := transport.startupStatus(ctx); err != nil {
		transport.rollbackProcess(process)
		return err
	}
	return nil
}

func (transport *Transport) Send(ctx context.Context, frame json.RawMessage) error {
	if transport == nil || transport.state == nil {
		return ErrInvalidConfig
	}
	if err := transport.state.RequireRunning(); err != nil {
		return err
	}
	if len(frame) == 0 || !utf8.Valid(frame) ||
		bytes.IndexAny(frame, "\r\n") >= 0 || !json.Valid(frame) {
		return ErrWriteFailed
	}
	transport.mu.Lock()
	writer := transport.writer
	transport.mu.Unlock()
	if writer == nil {
		return ErrWriterUnavailable
	}
	return writer.Send(ctx, frame)
}

func (transport *Transport) Receive(ctx context.Context) (mcptransport.TransportEvent, error) {
	if transport == nil || transport.state == nil {
		return mcptransport.TransportEvent{}, ErrInvalidConfig
	}
	if err := transport.state.RequireRunning(); err != nil {
		return mcptransport.TransportEvent{}, err
	}
	if transport.receives == nil || !transport.receives.Begin() {
		return mcptransport.TransportEvent{}, mcptransport.ErrClosing
	}
	defer transport.receives.End()
	transport.mu.Lock()
	framing := transport.framing
	transport.mu.Unlock()
	if framing == nil {
		return mcptransport.TransportEvent{}, ErrFramingUnavailable
	}
	if transport.captures != nil && transport.captures.HasBindings() {
		captured, err := framing.NextCaptured(ctx, transport.captures.TakeID)
		if err != nil {
			return mcptransport.TransportEvent{}, err
		}
		return mcptransport.TransportEvent{Captured: &captured, CapturedBytes: framing.lastCapturedBytes()}, nil
	}
	frame, err := framing.Next(ctx)
	if err != nil {
		return mcptransport.TransportEvent{}, err
	}
	return mcptransport.TransportEvent{Frame: frame}, nil
}

func (transport *Transport) Close(ctx context.Context) error {
	if transport == nil || transport.state == nil {
		return nil
	}
	transport.stopStartup()
	return transport.state.Close(ctx)
}

func (transport *Transport) cleanup(ctx context.Context) error {
	transport.stopStartup()
	transport.stopReceives()
	transport.stopWriter()
	process := transport.currentProcess()
	closeErr := transport.closeOwnedProcess(ctx, process)
	waitErr := transport.waitForWorkers(ctx)
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if closeErr != nil {
		return closeErr
	}
	return waitErr
}

func (transport *Transport) forceClose() {
	transport.stopStartup()
	transport.stopReceives()
	transport.stopWriter()
	process := transport.currentProcess()
	if isNilInterface(process) {
		return
	}
	transport.startOwnedProcessClose(process)
}

func (transport *Transport) currentProcess() proctree.Process {
	if transport == nil {
		return nil
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.process
}

func (transport *Transport) ownProcess(process proctree.Process) {
	if transport == nil || isNilInterface(process) {
		return
	}
	transport.mu.Lock()
	if isNilInterface(transport.process) {
		transport.process = process
	}
	transport.mu.Unlock()
}

func (transport *Transport) stopStartup() {
	if transport != nil && transport.cancelStartup != nil {
		transport.cancelStartup()
	}
}

func (transport *Transport) startupRequestContext(caller context.Context) (context.Context, context.CancelFunc) {
	if caller == nil {
		caller = context.Background()
	}
	requestContext, cancel := context.WithCancel(caller)
	if transport == nil || transport.startupContext == nil {
		cancel()
		return requestContext, cancel
	}
	stopOwnerCancellation := context.AfterFunc(transport.startupContext, cancel)
	return requestContext, func() {
		stopOwnerCancellation()
		cancel()
	}
}

func (transport *Transport) rollbackProcess(process proctree.Process) {
	transport.stopReceives()
	transport.stopWriter()
	if isNilInterface(process) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), transport.cleanupDuration())
	defer cancel()
	_ = transport.closeOwnedProcess(ctx, process)
	_ = transport.waitForWorkers(ctx)
}

func (transport *Transport) stopWriter() {
	if transport == nil {
		return
	}
	transport.mu.Lock()
	writer := transport.writer
	transport.mu.Unlock()
	if writer != nil {
		writer.Stop()
	}
}

func (transport *Transport) stopReceives() {
	if transport != nil && transport.receives != nil {
		transport.receives.Stop()
	}
}

func (transport *Transport) trackSupervisor(supervisor *stdioSupervisor) {
	if transport == nil || supervisor == nil {
		return
	}
	transport.mu.Lock()
	transport.supervisor = supervisor
	transport.mu.Unlock()
}

func (transport *Transport) trackStderr(worker *stderrWorker) {
	if transport == nil || worker == nil {
		return
	}
	transport.mu.Lock()
	transport.stderrDrain = worker
	transport.mu.Unlock()
}

func (transport *Transport) trackWriter(worker *writerWorker) {
	if transport == nil || worker == nil {
		return
	}
	transport.mu.Lock()
	transport.writer = worker
	transport.mu.Unlock()
}

func (transport *Transport) startupStatus(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if transport == nil || transport.state == nil || transport.state.State() != mcptransport.TransportStateStarting {
		return mcptransport.ErrClosing
	}
	return nil
}

func (transport *Transport) startOwnedProcessClose(process proctree.Process) <-chan struct{} {
	if transport == nil || isNilInterface(process) {
		return nil
	}
	transport.processCloseOnce.Do(func() {
		go func() {
			transport.processCloseErr = process.Close(context.Background())
			close(transport.processCloseDone)
		}()
	})
	return transport.processCloseDone
}

func (transport *Transport) closeOwnedProcess(ctx context.Context, process proctree.Process) error {
	done := transport.startOwnedProcessClose(process)
	if done == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-done:
		return transport.processCloseErr
	default:
	}
	select {
	case <-done:
		return transport.processCloseErr
	case <-ctx.Done():
		select {
		case <-done:
			return transport.processCloseErr
		default:
			return ctx.Err()
		}
	}
}

func (transport *Transport) waitForWorkers(ctx context.Context) error {
	if transport == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	transport.mu.Lock()
	writer := transport.writer
	stderrDrain := transport.stderrDrain
	supervisor := transport.supervisor
	transport.mu.Unlock()

	var waits []<-chan struct{}
	if writer != nil {
		waits = append(waits, writer.Done())
	}
	if transport.receives != nil {
		waits = append(waits, transport.receives.Done())
	}
	if stderrDrain != nil {
		waits = append(waits, stderrDrain.Done())
	}
	if supervisor != nil {
		waits = append(waits, supervisor.Done())
	}
	for _, done := range waits {
		if err := waitForStdioDone(ctx, done); err != nil {
			return err
		}
	}
	return nil
}

func waitForStdioDone(ctx context.Context, done <-chan struct{}) error {
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

func (transport *Transport) requestClose() {
	if transport == nil || transport.state == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = transport.Close(ctx)
}

func (transport *Transport) cleanupDuration() time.Duration {
	if transport != nil && transport.cleanupTimeout > 0 {
		return transport.cleanupTimeout
	}
	return mcptransport.MaxCleanupTimeout
}

type receiveAdmission struct {
	mu        sync.Mutex
	accepting bool
	active    int
	done      chan struct{}
	doneOnce  sync.Once
}

func newReceiveAdmission() *receiveAdmission {
	return &receiveAdmission{accepting: true, done: make(chan struct{})}
}

func (admission *receiveAdmission) Begin() bool {
	if admission == nil {
		return false
	}
	admission.mu.Lock()
	defer admission.mu.Unlock()
	if !admission.accepting {
		return false
	}
	admission.active++
	return true
}

func (admission *receiveAdmission) End() {
	if admission == nil {
		return
	}
	admission.mu.Lock()
	if admission.active > 0 {
		admission.active--
	}
	completed := !admission.accepting && admission.active == 0
	admission.mu.Unlock()
	if completed {
		admission.doneOnce.Do(func() { close(admission.done) })
	}
}

func (admission *receiveAdmission) Stop() {
	if admission == nil {
		return
	}
	admission.mu.Lock()
	admission.accepting = false
	completed := admission.active == 0
	admission.mu.Unlock()
	if completed {
		admission.doneOnce.Do(func() { close(admission.done) })
	}
}

func (admission *receiveAdmission) Done() <-chan struct{} {
	if admission == nil {
		return nil
	}
	return admission.done
}

func normalizeStartError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var startErr *proctree.StartError
	if errors.As(err, &startErr) && startErr != nil {
		if startErr.Code != "protected_exec_unavailable" && startErr.Code != "process_start_failed" {
			return ErrStartFailed
		}
		return &proctree.StartError{Code: startErr.Code, TargetStarted: startErr.TargetStarted}
	}
	return ErrStartFailed
}
