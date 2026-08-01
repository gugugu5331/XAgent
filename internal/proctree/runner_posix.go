//go:build darwin || linux

package proctree

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
)

type posixCommandFactory func(Request) (*exec.Cmd, error)

type posixRunner struct {
	options Options
	build   posixCommandFactory
}

func newPOSIXRunner(options Options, build posixCommandFactory) (*posixRunner, error) {
	if options.Diagnostics == nil || options.CleanupTimeout > maxCleanupTimeout || build == nil {
		return nil, errors.New("proctree POSIX runner options are invalid")
	}
	return &posixRunner{options: options, build: build}, nil
}

func (r *posixRunner) Start(ctx context.Context, request Request) (Process, error) {
	if r == nil || ctx == nil {
		return nil, newStartError(startCodeProcessStartFailed)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	validProtection := request.Protection.validForStart()
	if request.Mode != ProtectionRequired || !validProtection ||
		request.WorkingDir == nil || !request.Protection.containsReadRoot(request.WorkingDir) {
		if validProtection {
			_ = request.Protection.cleanupScratch()
		}
		return nil, newStartError(startCodeProtectedExecUnavailable)
	}
	command, err := r.build(request)
	if err != nil || command == nil {
		_ = request.Protection.cleanupScratch()
		var startErr *StartError
		if errors.As(err, &startErr) {
			return nil, newStartError(startErr.Code)
		}
		return nil, newStartError(startCodeProtectedExecUnavailable)
	}
	return startPOSIXCommand(command, request.Protection, r.options)
}

func startPOSIXCommand(command *exec.Cmd, protection ProtectionPlan, options Options) (Process, error) {
	if command == nil {
		_ = protection.cleanupScratch()
		return nil, newStartError(startCodeProcessStartFailed)
	}
	ownershipTransferred := false
	defer func() {
		if !ownershipTransferred {
			_ = protection.cleanupScratch()
		}
	}()
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, newStartError(startCodeProcessStartFailed)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, newStartError(startCodeProcessStartFailed)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, newStartError(startCodeProcessStartFailed)
	}
	closePipes := func() {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
	}
	if err := command.Start(); err != nil {
		closePipes()
		return nil, newStartError(startCodeProcessStartFailed)
	}
	controller := &posixController{
		command:    command,
		pid:        command.Process.Pid,
		protection: protection,
		pipes:      Pipes{Stdin: stdin, Stdout: stdout, Stderr: stderr},
	}
	process, err := newManagedProcess(controller, options)
	if err != nil {
		_ = controller.KillTree()
		_, _ = controller.wait()
		controller.ClosePipes()
		return nil, newStartError(startCodeProcessStartFailed)
	}
	ownershipTransferred = true
	return process, nil
}

type posixController struct {
	command    *exec.Cmd
	pid        int
	protection ProtectionPlan
	pipes      Pipes

	stdinOnce sync.Once
	pipesOnce sync.Once
	waitDone  atomic.Bool
}

func (c *posixController) Pipes() Pipes {
	if c == nil {
		return Pipes{}
	}
	return c.pipes
}

func (c *posixController) StopWrites() {
	if c == nil {
		return
	}
	c.stdinOnce.Do(func() { _ = c.pipes.Stdin.Close() })
}

func (c *posixController) TerminateTree() error {
	if c == nil {
		return errors.New("proctree POSIX process is unavailable")
	}
	if c.waitDone.Load() {
		return nil
	}
	return signalPOSIXGroup(c.pid, syscall.SIGTERM)
}

func (c *posixController) KillTree() error {
	if c == nil {
		return errors.New("proctree POSIX process is unavailable")
	}
	if c.waitDone.Load() {
		return nil
	}
	return signalPOSIXGroup(c.pid, syscall.SIGKILL)
}

func (c *posixController) Wait() (Result, error) {
	return c.wait()
}

func (c *posixController) wait() (Result, error) {
	if c == nil || c.command == nil {
		return Result{}, errors.New("proctree POSIX wait failed")
	}
	waitErr := c.command.Wait()
	_ = signalPOSIXGroup(c.pid, syscall.SIGKILL)
	c.waitDone.Store(true)
	result := Result{ExitCode: -1}
	if c.command.ProcessState != nil {
		result.ExitCode = c.command.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		waitErr = errors.New("proctree POSIX wait failed")
	} else {
		waitErr = nil
	}
	cleanupErr := c.protection.cleanupScratch()
	if waitErr != nil || cleanupErr != nil {
		return result, errors.New("proctree POSIX wait failed")
	}
	return result, nil
}

func (c *posixController) ClosePipes() {
	if c == nil {
		return
	}
	c.pipesOnce.Do(func() {
		c.stdinOnce.Do(func() { _ = c.pipes.Stdin.Close() })
		_ = c.pipes.Stdout.Close()
		_ = c.pipes.Stderr.Close()
	})
}

func (c *posixController) ForceClose() {
	if c != nil {
		_ = c.KillTree()
	}
}

func signalPOSIXGroup(pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return errors.New("proctree POSIX process group is invalid")
	}
	if err := syscall.Kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return errors.New("proctree POSIX process group signal failed")
	}
	return nil
}
