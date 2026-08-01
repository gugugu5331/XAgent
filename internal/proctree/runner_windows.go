//go:build windows

package proctree

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsJobExitCode = 0x58414754

type windowsProcessCreator func(Request, windowsChildHandles) (windows.ProcessInformation, error)
type windowsProcessProtector func(Request, windows.Handle) error

type windowsRunner struct {
	options Options
	create  windowsProcessCreator
	protect windowsProcessProtector
}

type windowsChildHandles struct {
	stdin  windows.Handle
	stdout windows.Handle
	stderr windows.Handle
}

type windowsOwnedPipes struct {
	parent Pipes
	child  windowsChildHandles
}

func newWindowsRunner(options Options) (*windowsRunner, error) {
	return newWindowsRunnerWithProtection(options, createWindowsSuspendedProcess, func(Request, windows.Handle) error {
		return errors.New("proctree Windows protection is unavailable")
	})
}

func newWindowsRunnerWithProtection(options Options, create windowsProcessCreator, protect windowsProcessProtector) (*windowsRunner, error) {
	if options.Diagnostics == nil || options.CleanupTimeout > maxCleanupTimeout || create == nil || protect == nil {
		return nil, errors.New("proctree Windows runner options are invalid")
	}
	return &windowsRunner{options: options, create: create, protect: protect}, nil
}

func (r *windowsRunner) Start(ctx context.Context, request Request) (Process, error) {
	if r == nil || ctx == nil {
		return nil, newStartError(startCodeProcessStartFailed)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	validProtection := request.Protection.validForStart()
	if request.Mode != ProtectionRequired || !validProtection || request.WorkingDir == nil ||
		!request.Protection.containsReadRoot(request.WorkingDir) {
		if validProtection {
			_ = request.Protection.cleanupScratch()
		}
		return nil, newStartError(startCodeProtectedExecUnavailable)
	}
	pipes, err := createWindowsPipes()
	if err != nil {
		_ = request.Protection.cleanupScratch()
		return nil, newStartError(startCodeProcessStartFailed)
	}
	childClosed := false
	defer func() {
		if !childClosed {
			pipes.closeChild()
		}
	}()
	info, err := r.create(request, pipes.child)
	pipes.closeChild()
	childClosed = true
	if err != nil {
		pipes.closeParent()
		_ = request.Protection.cleanupScratch()
		var startErr *StartError
		if errors.As(err, &startErr) {
			return nil, newStartError(startErr.Code)
		}
		return nil, newStartError(startCodeProcessStartFailed)
	}
	job, err := createWindowsKillJob()
	if err != nil || windows.AssignProcessToJobObject(job, info.Process) != nil || r.protect(request, info.Process) != nil {
		terminateWindowsSuspended(info, job)
		pipes.closeParent()
		_ = request.Protection.cleanupScratch()
		return nil, newStartError(startCodeProtectedExecUnavailable)
	}
	select {
	case <-ctx.Done():
		terminateWindowsSuspended(info, job)
		pipes.closeParent()
		_ = request.Protection.cleanupScratch()
		return nil, ctx.Err()
	default:
	}
	previous, resumeErr := windows.ResumeThread(info.Thread)
	_ = windows.CloseHandle(info.Thread)
	if resumeErr != nil || previous != 1 {
		_ = windows.TerminateJobObject(job, windowsJobExitCode)
		_ = windows.TerminateProcess(info.Process, windowsJobExitCode)
		_, _ = windows.WaitForSingleObject(info.Process, windows.INFINITE)
		_ = windows.CloseHandle(info.Process)
		_ = windows.CloseHandle(job)
		pipes.closeParent()
		_ = request.Protection.cleanupScratch()
		return nil, newStartError(startCodeProtectedExecUnavailable)
	}
	controller := &windowsController{process: info.Process, job: job, protection: request.Protection, pipes: pipes.parent}
	process, err := newManagedProcess(controller, r.options)
	if err != nil {
		controller.ForceClose()
		controller.ClosePipes()
		_ = request.Protection.cleanupScratch()
		return nil, newStartError(startCodeProcessStartFailed)
	}
	return process, nil
}

func createWindowsPipes() (windowsOwnedPipes, error) {
	stdinChild, stdinParent, err := createWindowsPipe(true)
	if err != nil {
		return windowsOwnedPipes{}, err
	}
	stdoutChild, stdoutParent, err := createWindowsPipe(false)
	if err != nil {
		_ = windows.CloseHandle(stdinChild)
		_ = stdinParent.Close()
		return windowsOwnedPipes{}, err
	}
	stderrChild, stderrParent, err := createWindowsPipe(false)
	if err != nil {
		_ = windows.CloseHandle(stdinChild)
		_ = stdinParent.Close()
		_ = stdoutParent.Close()
		_ = windows.CloseHandle(stdoutChild)
		return windowsOwnedPipes{}, err
	}
	return windowsOwnedPipes{
		parent: Pipes{Stdin: stdinParent, Stdout: stdoutParent, Stderr: stderrParent},
		child:  windowsChildHandles{stdin: stdinChild, stdout: stdoutChild, stderr: stderrChild},
	}, nil
}

func createWindowsPipe(childReads bool) (windows.Handle, *os.File, error) {
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	var readHandle, writeHandle windows.Handle
	if err := windows.CreatePipe(&readHandle, &writeHandle, &attributes, 0); err != nil {
		return 0, nil, errors.New("proctree Windows pipe creation failed")
	}
	child, parent := writeHandle, readHandle
	if childReads {
		child, parent = readHandle, writeHandle
	}
	if err := windows.SetHandleInformation(parent, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		_ = windows.CloseHandle(readHandle)
		_ = windows.CloseHandle(writeHandle)
		return 0, nil, errors.New("proctree Windows pipe inheritance setup failed")
	}
	file := os.NewFile(uintptr(parent), "xagent-process-pipe")
	if file == nil {
		_ = windows.CloseHandle(readHandle)
		_ = windows.CloseHandle(writeHandle)
		return 0, nil, errors.New("proctree Windows pipe conversion failed")
	}
	return child, file, nil
}

func (p windowsOwnedPipes) closeChild() {
	_ = windows.CloseHandle(p.child.stdin)
	_ = windows.CloseHandle(p.child.stdout)
	_ = windows.CloseHandle(p.child.stderr)
}

func (p windowsOwnedPipes) closeParent() {
	_ = p.parent.Stdin.Close()
	_ = p.parent.Stdout.Close()
	_ = p.parent.Stderr.Close()
}

func createWindowsSuspendedProcess(request Request, handles windowsChildHandles) (windows.ProcessInformation, error) {
	executable, err := windows.UTF16PtrFromString(request.Executable)
	if err != nil {
		return windows.ProcessInformation{}, errors.New("proctree Windows executable is invalid")
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{request.Executable}, request.Args...)))
	if err != nil {
		return windows.ProcessInformation{}, errors.New("proctree Windows command line is invalid")
	}
	environment, err := windowsEnvironmentBlock(request.Env)
	if err != nil {
		return windows.ProcessInformation{}, err
	}
	attributeList, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return windows.ProcessInformation{}, errors.New("proctree Windows handle list creation failed")
	}
	defer attributeList.Delete()
	inherited := []windows.Handle{handles.stdin, handles.stdout, handles.stderr}
	if err := attributeList.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&inherited[0]), uintptr(len(inherited))*unsafe.Sizeof(inherited[0])); err != nil {
		return windows.ProcessInformation{}, errors.New("proctree Windows handle list setup failed")
	}
	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:        uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags:     windows.STARTF_USESTDHANDLES,
			StdInput:  handles.stdin,
			StdOutput: handles.stdout,
			StdErr:    handles.stderr,
		},
		ProcThreadAttributeList: attributeList.List(),
	}
	var info windows.ProcessInformation
	err = windows.CreateProcess(executable, commandLine, nil, nil, true,
		windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT,
		&environment[0], nil, &startup.StartupInfo, &info)
	if err != nil {
		return windows.ProcessInformation{}, errors.New("proctree Windows suspended process creation failed")
	}
	return info, nil
}

func windowsEnvironmentBlock(environment []string) ([]uint16, error) {
	block := make([]uint16, 0)
	for _, item := range environment {
		if item == "" || strings.ContainsRune(item, 0) || !strings.Contains(item, "=") {
			return nil, errors.New("proctree Windows environment is invalid")
		}
		block = append(block, utf16.Encode([]rune(item))...)
		block = append(block, 0)
	}
	if len(block) == 0 {
		block = append(block, 0)
	}
	return append(block, 0), nil
}

func createWindowsKillJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, errors.New("proctree Windows Job Object creation failed")
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, errors.New("proctree Windows Job Object setup failed")
	}
	return job, nil
}

func terminateWindowsSuspended(info windows.ProcessInformation, job windows.Handle) {
	if job != 0 {
		_ = windows.TerminateJobObject(job, windowsJobExitCode)
		_ = windows.CloseHandle(job)
	}
	if info.Process != 0 {
		_ = windows.TerminateProcess(info.Process, windowsJobExitCode)
	}
	if info.Thread != 0 {
		_ = windows.CloseHandle(info.Thread)
	}
	if info.Process != 0 {
		_, _ = windows.WaitForSingleObject(info.Process, windows.INFINITE)
		_ = windows.CloseHandle(info.Process)
	}
}

type windowsController struct {
	process    windows.Handle
	job        windows.Handle
	protection ProtectionPlan
	pipes      Pipes

	stdinOnce  sync.Once
	pipesOnce  sync.Once
	handleOnce sync.Once
}

func (c *windowsController) Pipes() Pipes         { return c.pipes }
func (c *windowsController) StopWrites()          { c.stdinOnce.Do(func() { _ = c.pipes.Stdin.Close() }) }
func (c *windowsController) TerminateTree() error { return c.terminate() }
func (c *windowsController) KillTree() error      { return c.terminate() }
func (c *windowsController) terminate() error {
	if err := windows.TerminateJobObject(c.job, windowsJobExitCode); err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return errors.New("proctree Windows Job Object termination failed")
	}
	return nil
}
func (c *windowsController) Wait() (Result, error) {
	if _, err := windows.WaitForSingleObject(c.process, windows.INFINITE); err != nil {
		return Result{}, errors.New("proctree Windows process wait failed")
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(c.process, &exitCode); err != nil {
		return Result{}, errors.New("proctree Windows exit code unavailable")
	}
	c.closeHandles()
	if err := c.protection.cleanupScratch(); err != nil {
		return Result{ExitCode: int(exitCode)}, errors.New("proctree Windows process wait failed")
	}
	return Result{ExitCode: int(exitCode)}, nil
}
func (c *windowsController) ClosePipes() {
	c.pipesOnce.Do(func() {
		c.stdinOnce.Do(func() { _ = c.pipes.Stdin.Close() })
		_ = c.pipes.Stdout.Close()
		_ = c.pipes.Stderr.Close()
	})
}
func (c *windowsController) ForceClose() { _ = c.terminate(); c.closeHandles() }
func (c *windowsController) closeHandles() {
	c.handleOnce.Do(func() {
		_ = windows.CloseHandle(c.process)
		_ = windows.CloseHandle(c.job)
	})
}
