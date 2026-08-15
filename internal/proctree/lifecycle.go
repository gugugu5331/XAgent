package proctree

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"xagent/internal/diagnostics"
)

const maxCleanupTimeout = 2 * time.Second

var errProcessCleanupTimeout = errors.New("proctree cleanup exceeded its hard deadline")

var (
	errBorrowedPipeClose = errors.New("proctree pipe is borrowed")
	errPipeStopped       = errors.New("proctree pipe is stopped")
)

type waitResult struct {
	result Result
	err    error
}

type lifecycleController interface {
	Pipes() Pipes
	StopWrites()
	TerminateTree() error
	KillTree() error
	Wait() (Result, error)
	ClosePipes()
	ForceClose()
}

type managedProcess struct {
	controller lifecycleController
	pipes      Pipes
	pipeState  *pipeBorrowState
	timeout    time.Duration
	sink       diagnostics.BoundedSink
	stdinOnce  sync.Once

	waitDone chan struct{}
	wait     waitResult

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func newManagedProcess(controller lifecycleController, options Options) (*managedProcess, error) {
	if controller == nil || options.Diagnostics == nil {
		return nil, errors.New("proctree process options are invalid")
	}
	timeout := options.CleanupTimeout
	if timeout <= 0 {
		timeout = maxCleanupTimeout
	}
	if timeout > maxCleanupTimeout {
		return nil, errors.New("proctree cleanup timeout exceeds hard limit")
	}
	ownedPipes := controller.Pipes()
	if ownedPipes.Stdin == nil || ownedPipes.Stdout == nil || ownedPipes.Stderr == nil {
		return nil, errors.New("proctree process pipes are invalid")
	}
	pipeState := newPipeBorrowState()
	process := &managedProcess{
		controller: controller,
		pipes: Pipes{
			Stdin:  &borrowedWriter{owner: ownedPipes.Stdin, state: pipeState},
			Stdout: &borrowedReader{owner: ownedPipes.Stdout, state: pipeState},
			Stderr: &borrowedReader{owner: ownedPipes.Stderr, state: pipeState},
		},
		pipeState: pipeState,
		timeout:   timeout,
		sink:      options.Diagnostics,
		waitDone:  make(chan struct{}),
		closeDone: make(chan struct{}),
	}
	go process.ownWait()
	return process, nil
}

func (p *managedProcess) ownWait() {
	p.wait.result, p.wait.err = p.controller.Wait()
	if p.wait.err != nil {
		p.wait.err = errors.New("proctree process wait failed")
	}
	close(p.waitDone)
}

func (p *managedProcess) Pipes() Pipes {
	if p == nil {
		return Pipes{}
	}
	return p.pipes
}

// CloseStdin atomically prevents new borrowed writes and closes the owner
// handle exactly once so the child observes EOF. Borrowed pipe Close methods
// remain unable to close any owner handle.
func (p *managedProcess) CloseStdin() error {
	if p == nil || p.controller == nil || p.pipeState == nil {
		return errors.New("proctree stdin is unavailable")
	}
	p.stdinOnce.Do(func() {
		p.pipeState.stopWrites()
		p.controller.StopWrites()
	})
	return nil
}

func (p *managedProcess) Wait(ctx context.Context) (Result, error) {
	if p == nil || ctx == nil {
		return Result{}, errors.New("proctree wait request is invalid")
	}
	select {
	case <-p.waitDone:
		return p.wait.result, p.wait.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func (p *managedProcess) Terminate(ctx context.Context) error {
	if p == nil || ctx == nil {
		return errors.New("proctree terminate request is invalid")
	}
	select {
	case <-p.waitDone:
		return p.wait.err
	default:
	}
	if err := p.controller.TerminateTree(); err != nil {
		return errors.New("proctree terminate failed")
	}
	_, err := p.Wait(ctx)
	return err
}

func (p *managedProcess) Close(ctx context.Context) error {
	if p == nil || ctx == nil {
		return errors.New("proctree close request is invalid")
	}
	p.closeOnce.Do(func() {
		go p.cleanup()
	})
	select {
	case <-p.closeDone:
		return p.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *managedProcess) cleanup() {
	defer close(p.closeDone)
	_ = p.CloseStdin()
	deadline := time.NewTimer(p.timeout)
	defer deadline.Stop()
	select {
	case <-p.waitDone:
		p.closeAfterReap(deadline.C)
		return
	default:
	}
	_ = p.controller.TerminateTree()
	softWait := time.NewTimer(p.timeout / 2)
	defer softWait.Stop()

	select {
	case <-p.waitDone:
		p.closeAfterReap(deadline.C)
		return
	case <-softWait.C:
		_ = p.controller.KillTree()
	}

	select {
	case <-p.waitDone:
		p.closeAfterReap(deadline.C)
	case <-deadline.C:
		p.forceClose(false)
	}
}

func (p *managedProcess) closeAfterReap(deadline <-chan time.Time) {
	idle := p.pipeState.stopAll()
	p.controller.ClosePipes()
	select {
	case <-idle:
		p.closeErr = p.wait.err
	case <-deadline:
		p.forceClose(true)
	}
}

func (p *managedProcess) forceClose(pipesClosed bool) {
	p.controller.ForceClose()
	if !pipesClosed {
		p.pipeState.stopAll()
		p.controller.ClosePipes()
	}
	p.sink.Add(diagnostics.SanitizeInput{
		Code:     "process_cleanup_timeout",
		Source:   "proctree",
		Severity: diagnostics.SeverityError,
		Err:      errProcessCleanupTimeout,
	})
	p.closeErr = errProcessCleanupTimeout
}

type pipeBorrowState struct {
	mu sync.Mutex

	writesStopped bool
	readsStopped  bool
	active        int
	idle          chan struct{}
	idleOnce      sync.Once
}

func newPipeBorrowState() *pipeBorrowState {
	return &pipeBorrowState{idle: make(chan struct{})}
}

func (s *pipeBorrowState) beginWrite() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writesStopped {
		return false
	}
	s.active++
	return true
}

func (s *pipeBorrowState) beginRead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readsStopped {
		return false
	}
	s.active++
	return true
}

func (s *pipeBorrowState) end() {
	s.mu.Lock()
	s.active--
	shouldClose := s.readsStopped && s.writesStopped && s.active == 0
	s.mu.Unlock()
	if shouldClose {
		s.idleOnce.Do(func() { close(s.idle) })
	}
}

func (s *pipeBorrowState) stopWrites() {
	s.mu.Lock()
	s.writesStopped = true
	s.mu.Unlock()
}

func (s *pipeBorrowState) stopAll() <-chan struct{} {
	s.mu.Lock()
	s.writesStopped = true
	s.readsStopped = true
	shouldClose := s.active == 0
	s.mu.Unlock()
	if shouldClose {
		s.idleOnce.Do(func() { close(s.idle) })
	}
	return s.idle
}

type borrowedWriter struct {
	owner io.Writer
	state *pipeBorrowState
}

func (w *borrowedWriter) Write(data []byte) (int, error) {
	if w == nil || w.owner == nil || w.state == nil || !w.state.beginWrite() {
		return 0, errPipeStopped
	}
	defer w.state.end()
	return w.owner.Write(data)
}

func (*borrowedWriter) Close() error {
	return errBorrowedPipeClose
}

type borrowedReader struct {
	owner io.Reader
	state *pipeBorrowState
}

func (r *borrowedReader) Read(data []byte) (int, error) {
	if r == nil || r.owner == nil || r.state == nil || !r.state.beginRead() {
		return 0, errPipeStopped
	}
	defer r.state.end()
	return r.owner.Read(data)
}

func (*borrowedReader) Close() error {
	return errBorrowedPipeClose
}
