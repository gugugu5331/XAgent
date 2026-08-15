package hook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/proctree"
	"xagent/internal/redact"
	"xagent/internal/safefs"
)

func TestCommandHookUsesProcessTreeAndBoundedCapture(t *testing.T) {
	t.Run("missing protected runtime fails closed", func(t *testing.T) {
		request := CommandRequest{Command: "must-not-start", ProjectRoot: t.TempDir(), EventJSON: []byte(`{}`)}
		if _, err := ((*ShellCommandRunner)(nil)).Run(context.Background(), request); err == nil || err.Error() != "protected command runtime is unavailable" {
			t.Fatalf("nil protected runtime error = %v", err)
		}
		if _, err := (&ShellCommandRunner{}).Run(context.Background(), request); err == nil || err.Error() != "protected command runtime is unavailable" {
			t.Fatalf("zero protected runtime error = %v", err)
		}
	})

	t.Run("protected request and pipes", func(t *testing.T) {
		process := newStaticCommandProcess("bounded stdout", "bounded stderr", proctree.Result{})
		runner := &commandTestRunner{process: process, started: make(chan struct{})}
		plans := &commandTestPlanFactory{}
		root := t.TempDir()
		command := newProtectedCommandTestRunner(t, root, runner, plans)
		payload := []byte(`{"event":"tool_before"}`)
		result, err := command.Run(context.Background(), CommandRequest{
			Command:     "echo protected",
			ProjectRoot: root,
			Environment: map[string]string{"ONLY_THIS": "yes"},
			EventJSON:   payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		if string(result.Stdout) != "bounded stdout" || string(result.Stderr) != "bounded stderr" {
			t.Fatalf("bounded capture = %#v", result)
		}
		request := runner.lastRequest()
		if request.Mode != proctree.ProtectionRequired || request.Executable == "" || len(request.Args) < 2 || request.Args[len(request.Args)-1] != "echo protected" {
			t.Fatalf("unprotected command request: %#v", request)
		}
		if request.WorkingDir == nil || request.WorkingDir.Identity() == (safefs.Identity{}) || plans.count() != 1 {
			t.Fatal("command did not use the approved working root and plan factory")
		}
		if got := process.stdinBytes(); !bytes.Equal(got, payload) || process.stdinCloseCount() != 1 {
			t.Fatalf("command stdin = %q closes=%d", got, process.stdinCloseCount())
		}
		if process.closeCount() != 1 || process.waitCount() != 1 {
			t.Fatal("command process did not settle through its proctree owner")
		}
	})

	t.Run("hard output limit closes producer", func(t *testing.T) {
		process := newStaticCommandProcess("12345", "", proctree.Result{})
		runner := &commandTestRunner{process: process}
		limits := DefaultLimits()
		limits.CommandStdoutBytes = 4
		root := t.TempDir()
		command := newProtectedCommandTestRunner(t, root, runner, &commandTestPlanFactory{})
		command.Limits = limits
		result, err := command.Run(context.Background(), CommandRequest{Command: "large", ProjectRoot: root, EventJSON: []byte(`{}`)})
		if err == nil || err.Error() != "command output exceeds limit" || len(result.Stdout) != 0 || len(result.Stderr) != 0 {
			t.Fatalf("hard-limit result = %#v err=%v", result, err)
		}
		if process.closeCount() != 1 || process.waitCount() != 1 {
			t.Fatal("hard-limit command was not terminated and reaped")
		}
	})

	t.Run("request cancellation uses independent cleanup", func(t *testing.T) {
		process := newBlockingCommandProcess()
		runner := &commandTestRunner{process: process, started: make(chan struct{})}
		root := t.TempDir()
		command := newProtectedCommandTestRunner(t, root, runner, &commandTestPlanFactory{})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := command.Run(ctx, CommandRequest{Command: "blocking", ProjectRoot: root, EventJSON: []byte(`{}`)})
			done <- err
		}()
		select {
		case <-runner.started:
		case <-time.After(time.Second):
			t.Fatal("command did not cross protected Start")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel error = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled command did not finish cleanup")
		}
		if !process.wasClosed() || !process.wasReaped() || process.stdinCloseCount() != 1 {
			t.Fatal("cancelled command did not close stdin and reap its process tree")
		}
	})
}

func TestCommandEnvironment(t *testing.T) {
	t.Setenv("HOOK_FORBIDDEN_MARKER", "leak")
	root := t.TempDir()
	process := newStaticCommandProcess("", "", proctree.Result{})
	runner := &commandTestRunner{process: process}
	command := newProtectedCommandTestRunner(t, root, runner, &commandTestPlanFactory{})
	if _, err := command.Run(context.Background(), CommandRequest{
		Command: "environment", ProjectRoot: root, Environment: map[string]string{"ONLY_THIS": "yes"}, EventJSON: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	environment := runner.lastRequest().Env
	if !containsCommandEnvironment(environment, "PWD="+root) || !containsCommandEnvironment(environment, "ONLY_THIS=yes") {
		t.Fatalf("controlled environment = %#v", environment)
	}
	for _, item := range environment {
		if strings.HasPrefix(item, "HOOK_FORBIDDEN_MARKER=") {
			t.Fatal("forbidden parent environment was inherited")
		}
	}
}

func TestCommandCWDAndStdin(t *testing.T) {
	root := t.TempDir()
	payload := []byte(`{"event":"turn_start"}`)
	process := newStaticCommandProcess("", "", proctree.Result{})
	runner := &commandTestRunner{process: process}
	command := newProtectedCommandTestRunner(t, root, runner, &commandTestPlanFactory{})
	if _, err := command.Run(context.Background(), CommandRequest{Command: "stdin", ProjectRoot: root, EventJSON: payload}); err != nil {
		t.Fatal(err)
	}
	request := runner.lastRequest()
	if request.WorkingDir == nil || !bytes.Equal(process.stdinBytes(), payload) {
		t.Fatal("working directory or EventJSON was not delivered through proctree")
	}
}

func TestCommandOutputHardLimit(t *testing.T) {
	root := t.TempDir()
	limits := DefaultLimits()
	limits.CommandStdoutBytes = 16
	for _, item := range []struct {
		output  string
		wantErr bool
	}{{output: "1234567890123456"}, {output: "12345678901234567", wantErr: true}} {
		process := newStaticCommandProcess(item.output, "", proctree.Result{})
		command := newProtectedCommandTestRunner(t, root, &commandTestRunner{process: process}, &commandTestPlanFactory{})
		command.Limits = limits
		_, err := command.Run(context.Background(), CommandRequest{Command: "limit", ProjectRoot: root, EventJSON: []byte(`{}`)})
		if (err != nil) != item.wantErr {
			t.Fatalf("output bytes=%d err=%v", len(item.output), err)
		}
	}
}

func TestCommandCancel(t *testing.T) {
	root := t.TempDir()
	process := newBlockingCommandProcess()
	runner := &commandTestRunner{process: process, started: make(chan struct{})}
	command := newProtectedCommandTestRunner(t, root, runner, &commandTestPlanFactory{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := command.Run(ctx, CommandRequest{Command: "cancel", ProjectRoot: root, EventJSON: []byte(`{}`)})
		done <- err
	}()
	<-runner.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if !process.wasClosed() || !process.wasReaped() {
		t.Fatal("cancel did not delegate descendant cleanup to proctree")
	}
}

func TestCommandProcessGroupSignals(t *testing.T) {
	TestCommandCancel(t)
}

func TestCommandDetachedDescendantCannotHoldPipesOpen(t *testing.T) {
	root := t.TempDir()
	process := newBlockingCommandProcess()
	runner := &commandTestRunner{process: process, started: make(chan struct{})}
	command := newProtectedCommandTestRunner(t, root, runner, &commandTestPlanFactory{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := command.Run(ctx, CommandRequest{Command: "detached", ProjectRoot: root, EventJSON: nil})
		done <- err
	}()
	<-runner.started
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("proctree Close did not release inherited command pipes")
	}
}

func TestCommandRedactsOutput(t *testing.T) {
	const marker = "command-output-private-marker"
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(marker)
	root := t.TempDir()
	process := newStaticCommandProcess(marker, marker, proctree.Result{})
	command := newProtectedCommandTestRunner(t, root, &commandTestRunner{process: process}, &commandTestPlanFactory{})
	command.Redactor = runtimeRedactor
	result, err := command.Run(context.Background(), CommandRequest{Command: "redact", ProjectRoot: root, EventJSON: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.Stdout)+string(result.Stderr), marker) {
		t.Fatal("expanded private value leaked through command output")
	}
}

func TestCommandRunnerIOFailure(t *testing.T) {
	root := t.TempDir()
	for _, stream := range []string{"stdin", "stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			process := newStaticCommandProcess("stdout", "stderr", proctree.Result{})
			process.failStream(stream)
			command := newProtectedCommandTestRunner(t, root, &commandTestRunner{process: process}, &commandTestPlanFactory{})
			result, err := command.Run(context.Background(), CommandRequest{Command: "io-failure", ProjectRoot: root, EventJSON: []byte(`{"event":"test"}`)})
			if err == nil || err.Error() != "command I/O failed" || len(result.Stdout) != 0 || len(result.Stderr) != 0 {
				t.Fatalf("I/O result = %#v err=%v", result, err)
			}
			if process.closeCount() != 1 || process.waitCount() != 1 {
				t.Fatal("I/O failure did not settle the process owner")
			}
		})
	}
}

func TestCommandExecutionFailure(t *testing.T) {
	root := t.TempDir()
	for _, item := range []struct {
		name      string
		startErr  error
		exitCode  int
		wantError string
	}{
		{name: "start error", startErr: errors.New("private start detail"), wantError: "command start failed"},
		{name: "non-zero exit", exitCode: 7, wantError: "command failed"},
	} {
		t.Run(item.name, func(t *testing.T) {
			process := newStaticCommandProcess("private stdout", "private stderr", proctree.Result{ExitCode: item.exitCode})
			runner := &commandTestRunner{process: process, startErr: item.startErr}
			command := newProtectedCommandTestRunner(t, root, runner, &commandTestPlanFactory{})
			result, err := command.Run(context.Background(), CommandRequest{Command: "failure", ProjectRoot: root, EventJSON: []byte(`{}`)})
			if err == nil || err.Error() != item.wantError || len(result.Stdout) != 0 || len(result.Stderr) != 0 {
				t.Fatalf("execution result = %#v err=%v", result, err)
			}
		})
	}
}

func TestCommandDecisionIntegration(t *testing.T) {
	root := t.TempDir()
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret("private-decision-reason")
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: runtimeRedactor.Text})
	process := newStaticCommandProcess(`{"decision":"deny","reason":"private-decision-reason"}`, "", proctree.Result{})
	command := newProtectedCommandTestRunner(t, root, &commandTestRunner{process: process}, &commandTestPlanFactory{})
	command.Redactor = runtimeRedactor
	rule := commandRule(EventToolBefore, 1, "decision", true, false, false)
	engine, err := NewEngine(newSnapshot([]Rule{rule}), EngineOptions{ProjectRoot: root, CommandRunner: command, LegacyDiagnostics: collector, Redactor: runtimeRedactor})
	if err != nil {
		t.Fatal(err)
	}
	decision := engine.BeforeTool(context.Background(), ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault}, NewToolInput("c", "Read", map[string]any{}))
	if !decision.IsDeny() || decision.Reason() != "[redacted]" || len(collector.List()) != 0 {
		t.Fatalf("command decision = %#v diagnostics=%#v", decision, collector.List())
	}
}

func newProtectedCommandTestRunner(t *testing.T, root string, runner proctree.Runner, plans proctree.ProtectionPlanFactory) *ShellCommandRunner {
	t.Helper()
	opened, err := safefs.Bootstrap(root, safefs.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := opened.Root.Close(); err != nil {
			t.Error(err)
		}
	})
	return &ShellCommandRunner{
		Runner: runner, Plans: plans, WorkingDirectory: opened.Root, ProjectRoot: root, JoinGrace: time.Second,
	}
}

func containsCommandEnvironment(environment []string, want string) bool {
	for _, item := range environment {
		if item == want {
			return true
		}
	}
	return false
}

type commandTestPlanFactory struct {
	mu      sync.Mutex
	creates int
}

func (f *commandTestPlanFactory) Create(context.Context) (proctree.ProtectionPlan, error) {
	f.mu.Lock()
	f.creates++
	f.mu.Unlock()
	return proctree.ProtectionPlan{}, nil
}

func (f *commandTestPlanFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

type commandTestRunner struct {
	mu       sync.Mutex
	process  proctree.Process
	startErr error
	request  proctree.Request
	started  chan struct{}
	once     sync.Once
}

func (r *commandTestRunner) Start(_ context.Context, request proctree.Request) (proctree.Process, error) {
	r.mu.Lock()
	r.request = request
	r.mu.Unlock()
	if r.started != nil {
		r.once.Do(func() { close(r.started) })
	}
	return r.process, r.startErr
}

func (r *commandTestRunner) lastRequest() proctree.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.request
}

type commandTestWriteCloser struct {
	mu       sync.Mutex
	data     bytes.Buffer
	closed   int
	writeErr error
}

func (w *commandTestWriteCloser) Write(data []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.Write(data)
}

func (w *commandTestWriteCloser) Close() error {
	w.mu.Lock()
	w.closed++
	w.mu.Unlock()
	return nil
}

func (w *commandTestWriteCloser) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.data.Bytes()...)
}

func (w *commandTestWriteCloser) closeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

type commandErrorReadCloser struct{ err error }

func (r commandErrorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (commandErrorReadCloser) Close() error               { return nil }

type staticCommandProcess struct {
	mu         sync.Mutex
	pipes      proctree.Pipes
	stdin      *commandTestWriteCloser
	result     proctree.Result
	waitErr    error
	waits      int
	closes     int
	stdinOnce  sync.Once
	stdoutData string
	stderrData string
}

func newStaticCommandProcess(stdout, stderr string, result proctree.Result) *staticCommandProcess {
	stdin := &commandTestWriteCloser{}
	process := &staticCommandProcess{stdin: stdin, result: result, stdoutData: stdout, stderrData: stderr}
	process.resetPipes()
	return process
}

func (p *staticCommandProcess) resetPipes() {
	p.pipes = proctree.Pipes{
		Stdin:  stdinBorrow{owner: p.stdin},
		Stdout: io.NopCloser(strings.NewReader(p.stdoutData)),
		Stderr: io.NopCloser(strings.NewReader(p.stderrData)),
	}
}

func (p *staticCommandProcess) failStream(stream string) {
	switch stream {
	case "stdin":
		p.stdin.writeErr = errors.New("private stdin failure")
	case "stdout":
		p.pipes.Stdout = commandErrorReadCloser{err: errors.New("private stdout failure")}
	case "stderr":
		p.pipes.Stderr = commandErrorReadCloser{err: errors.New("private stderr failure")}
	}
}

func (p *staticCommandProcess) Pipes() proctree.Pipes { return p.pipes }
func (p *staticCommandProcess) CloseStdin() error {
	p.stdinOnce.Do(func() { _ = p.stdin.Close() })
	return nil
}
func (p *staticCommandProcess) Wait(context.Context) (proctree.Result, error) {
	p.mu.Lock()
	p.waits++
	p.mu.Unlock()
	return p.result, p.waitErr
}
func (p *staticCommandProcess) Terminate(ctx context.Context) error { return p.Close(ctx) }
func (p *staticCommandProcess) Close(context.Context) error {
	p.mu.Lock()
	p.closes++
	p.mu.Unlock()
	return nil
}
func (p *staticCommandProcess) stdinBytes() []byte   { return p.stdin.bytes() }
func (p *staticCommandProcess) stdinCloseCount() int { return p.stdin.closeCount() }
func (p *staticCommandProcess) closeCount() int      { p.mu.Lock(); defer p.mu.Unlock(); return p.closes }
func (p *staticCommandProcess) waitCount() int       { p.mu.Lock(); defer p.mu.Unlock(); return p.waits }

// stdinBorrow models the proctree borrowed pipe: Close is not used by Hook;
// only Process.CloseStdin reaches the owner.
type stdinBorrow struct{ owner *commandTestWriteCloser }

func (w stdinBorrow) Write(data []byte) (int, error) { return w.owner.Write(data) }
func (stdinBorrow) Close() error                     { return errors.New("borrowed close refused") }

type blockingCommandProcess struct {
	pipes     proctree.Pipes
	stdin     *commandTestWriteCloser
	stdout    *io.PipeWriter
	stderr    *io.PipeWriter
	done      chan struct{}
	closeOnce sync.Once
	stdinOnce sync.Once
	mu        sync.Mutex
	closed    bool
	reaped    bool
}

func newBlockingCommandProcess() *blockingCommandProcess {
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	stdin := &commandTestWriteCloser{}
	return &blockingCommandProcess{
		pipes: proctree.Pipes{Stdin: stdinBorrow{owner: stdin}, Stdout: stdoutReader, Stderr: stderrReader},
		stdin: stdin, stdout: stdoutWriter, stderr: stderrWriter, done: make(chan struct{}),
	}
}

func (p *blockingCommandProcess) Pipes() proctree.Pipes { return p.pipes }
func (p *blockingCommandProcess) CloseStdin() error {
	p.stdinOnce.Do(func() { _ = p.stdin.Close() })
	return nil
}
func (p *blockingCommandProcess) Wait(ctx context.Context) (proctree.Result, error) {
	select {
	case <-p.done:
		p.mu.Lock()
		p.reaped = true
		p.mu.Unlock()
		return proctree.Result{Cancelled: true}, nil
	case <-ctx.Done():
		return proctree.Result{}, ctx.Err()
	}
}
func (p *blockingCommandProcess) Terminate(ctx context.Context) error { return p.Close(ctx) }
func (p *blockingCommandProcess) Close(context.Context) error {
	p.closeOnce.Do(func() {
		_ = p.stdout.Close()
		_ = p.stderr.Close()
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.done)
	})
	return nil
}
func (p *blockingCommandProcess) wasClosed() bool      { p.mu.Lock(); defer p.mu.Unlock(); return p.closed }
func (p *blockingCommandProcess) wasReaped() bool      { p.mu.Lock(); defer p.mu.Unlock(); return p.reaped }
func (p *blockingCommandProcess) stdinCloseCount() int { return p.stdin.closeCount() }

var _ proctree.Runner = (*commandTestRunner)(nil)
var _ proctree.Process = (*staticCommandProcess)(nil)
var _ proctree.Process = (*blockingCommandProcess)(nil)
