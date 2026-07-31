package proctree

import (
	"context"
	"errors"
	"sync"
	"time"

	"xagent/internal/diagnostics"
)

const maxCleanupTimeout = 2 * time.Second

var errProcessCleanupTimeout = errors.New("proctree cleanup exceeded its hard deadline")

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
	timeout    time.Duration
	sink       diagnostics.BoundedSink

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
	process := &managedProcess{
		controller: controller,
		pipes:      controller.Pipes(),
		timeout:    timeout,
		sink:       options.Diagnostics,
		waitDone:   make(chan struct{}),
		closeDone:  make(chan struct{}),
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
	p.controller.StopWrites()
	_ = p.controller.TerminateTree()
	deadline := time.NewTimer(p.timeout)
	defer deadline.Stop()
	softWait := time.NewTimer(p.timeout / 2)
	defer softWait.Stop()

	select {
	case <-p.waitDone:
		p.controller.ClosePipes()
		p.closeErr = p.wait.err
		return
	case <-softWait.C:
		_ = p.controller.KillTree()
	}

	select {
	case <-p.waitDone:
		p.controller.ClosePipes()
		p.closeErr = p.wait.err
	case <-deadline.C:
		p.controller.ForceClose()
		p.controller.ClosePipes()
		p.sink.Add(diagnostics.SanitizeInput{
			Code:     "process_cleanup_timeout",
			Source:   "proctree",
			Severity: diagnostics.SeverityError,
			Err:      errProcessCleanupTimeout,
		})
		p.closeErr = errProcessCleanupTimeout
	}
}
