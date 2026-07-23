//go:build unix

package hook

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

const (
	commandHelperModeEnv       = "XAGENT_COMMAND_HELPER_MODE"
	commandHelperParentReady   = "XAGENT_COMMAND_PARENT_READY"
	commandHelperChildReady    = "XAGENT_COMMAND_CHILD_READY"
	commandHelperParentTERM    = "XAGENT_COMMAND_PARENT_TERM"
	commandHelperChildTERM     = "XAGENT_COMMAND_CHILD_TERM"
	commandHelperHeartbeat     = "XAGENT_COMMAND_CHILD_HEARTBEAT"
	commandHelperDetachedReady = "XAGENT_COMMAND_DETACHED_READY"
	commandHelperDetachedStop  = "XAGENT_COMMAND_DETACHED_STOP"
	commandHelperDetachedExit  = "XAGENT_COMMAND_DETACHED_EXIT"
)

func TestCommandEnvironment(t *testing.T) {
	t.Setenv("HOOK_FORBIDDEN_CANARY", "leak")
	root := t.TempDir()
	runner := &ShellCommandRunner{}
	result, err := runner.Run(context.Background(), CommandRequest{Command: `printf '%s|%s|%s' "$PWD" "$ONLY_THIS" "${HOOK_FORBIDDEN_CANARY-unset}"`, ProjectRoot: root, Environment: map[string]string{"ONLY_THIS": "yes"}, EventJSON: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(result.Stdout); got != root+"|yes|unset" {
		t.Fatalf("environment = %q", got)
	}
	for _, item := range controlledEnvironment(root, nil) {
		if strings.HasPrefix(item, "HOOK_FORBIDDEN_CANARY=") {
			t.Fatal("forbidden env inherited")
		}
	}
}

func TestCommandCWDAndStdin(t *testing.T) {
	root := t.TempDir()
	payload := []byte(`{"event":"turn_start"}`)
	runner := &ShellCommandRunner{}
	result, err := runner.Run(context.Background(), CommandRequest{Command: `pwd; cat`, ProjectRoot: root, EventJSON: payload})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(result.Stdout); got != root+"\n"+string(payload) {
		t.Fatalf("output = %q", got)
	}
}

func TestCommandOutputHardLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.CommandStdoutBytes = 16
	runner := &ShellCommandRunner{Limits: limits}
	if _, err := runner.Run(context.Background(), CommandRequest{Command: `printf '1234567890123456'`, ProjectRoot: t.TempDir(), EventJSON: []byte(`{}`)}); err != nil {
		t.Fatalf("limit failed: %v", err)
	}
	if _, err := runner.Run(context.Background(), CommandRequest{Command: `printf '12345678901234567'`, ProjectRoot: t.TempDir(), EventJSON: []byte(`{}`)}); err == nil {
		t.Fatal("limit+1 accepted")
	}
}

func TestCommandCancel(t *testing.T) {
	runner := &ShellCommandRunner{TerminateGrace: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runner.Run(ctx, CommandRequest{Command: `sleep 10`, ProjectRoot: t.TempDir(), EventJSON: []byte(`{}`)}); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestCommandProcessGroupSignals(t *testing.T) {
	root := t.TempDir()
	parentReadyPath := filepath.Join(root, "parent.ready")
	childReadyPath := filepath.Join(root, "child.ready")
	parentTERMPath := filepath.Join(root, "parent.term")
	childTERMPath := filepath.Join(root, "child.term")
	heartbeatPath := filepath.Join(root, "child.heartbeat")
	command := `exec "$TEST_BINARY" -test.run=^TestCommandSignalHelper$ -test.count=1`
	runner := &ShellCommandRunner{TerminateGrace: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, CommandRequest{
			Command:     command,
			ProjectRoot: root,
			Environment: map[string]string{
				"TEST_BINARY":            os.Args[0],
				commandHelperModeEnv:     "group-parent",
				commandHelperParentReady: parentReadyPath,
				commandHelperChildReady:  childReadyPath,
				commandHelperParentTERM:  parentTERMPath,
				commandHelperChildTERM:   childTERMPath,
				commandHelperHeartbeat:   heartbeatPath,
			},
			EventJSON: []byte(`{}`),
		})
		done <- err
	}()
	waitForCommandTestFile(t, parentReadyPath, 2*time.Second)
	waitForCommandTestFile(t, childReadyPath, 2*time.Second)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context cancellation", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("command runner did not join cancelled process group")
	}
	waitForCommandTestFile(t, parentTERMPath, 2*time.Second)
	waitForCommandTestFile(t, childTERMPath, 2*time.Second)
	time.Sleep(50 * time.Millisecond)
	before, err := os.Stat(heartbeatPath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.Stat(heartbeatPath)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() {
		t.Fatalf("process-group child kept running after TERM→KILL→join: heartbeat %d -> %d", before.Size(), after.Size())
	}
}

func TestCommandDetachedDescendantCannotHoldPipesOpen(t *testing.T) {
	root := t.TempDir()
	readyPath := filepath.Join(root, "detached.ready")
	stopPath := filepath.Join(root, "detached.stop")
	exitPath := filepath.Join(root, "detached.exit")
	t.Cleanup(func() { _ = os.WriteFile(stopPath, []byte("stop"), 0o600) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	runner := &ShellCommandRunner{TerminateGrace: 50 * time.Millisecond, JoinGrace: 500 * time.Millisecond}
	go func() {
		_, err := runner.Run(ctx, CommandRequest{
			Command:     `exec "$TEST_BINARY" -test.run=^TestCommandSignalHelper$ -test.count=1`,
			ProjectRoot: root,
			Environment: map[string]string{
				"TEST_BINARY":              os.Args[0],
				commandHelperModeEnv:       "detach-parent",
				commandHelperDetachedReady: readyPath,
				commandHelperDetachedStop:  stopPath,
				commandHelperDetachedExit:  exitPath,
			},
			EventJSON: []byte(`{}`),
		})
		done <- err
	}()
	waitForCommandTestFile(t, readyPath, 2*time.Second)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		// Release the detached helper so a broken runner can settle before the
		// test fails instead of leaving a process behind.
		_ = os.WriteFile(stopPath, []byte("stop"), 0o600)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("detached descendant kept inherited command pipes open")
	}
	if err := os.WriteFile(stopPath, []byte("stop"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForCommandTestFile(t, exitPath, 2*time.Second)
}

func TestCommandSignalHelper(t *testing.T) {
	mode := os.Getenv(commandHelperModeEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "group-parent":
		term := commandHelperTERMChannel()
		child := exec.Command(os.Args[0], "-test.run=^TestCommandSignalHelper$", "-test.count=1")
		child.Env = commandHelperEnvironment("group-child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		waitForCommandTestFile(t, os.Getenv(commandHelperChildReady), 2*time.Second)
		writeCommandHelperFile(t, os.Getenv(commandHelperParentReady), "ready")
		<-term
		writeCommandHelperFile(t, os.Getenv(commandHelperParentTERM), "term")
		for {
			time.Sleep(time.Second)
		}
	case "group-child":
		term := commandHelperTERMChannel()
		heartbeat, err := os.OpenFile(os.Getenv(commandHelperHeartbeat), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := heartbeat.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		writeCommandHelperFile(t, os.Getenv(commandHelperChildReady), "ready")
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		observedTERM := false
		for {
			select {
			case <-term:
				if !observedTERM {
					writeCommandHelperFile(t, os.Getenv(commandHelperChildTERM), "term")
					observedTERM = true
				}
			case <-ticker.C:
				if _, err := heartbeat.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
			}
		}
	case "detach-parent":
		child := exec.Command(os.Args[0], "-test.run=^TestCommandSignalHelper$", "-test.count=1")
		child.Env = commandHelperEnvironment("detached-child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Second)
		}
	case "detached-child":
		writeCommandHelperFile(t, os.Getenv(commandHelperDetachedReady), "ready")
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(os.Getenv(commandHelperDetachedStop)); err == nil {
				writeCommandHelperFile(t, os.Getenv(commandHelperDetachedExit), "exit")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("detached helper timed out")
	default:
		t.Fatalf("unknown command helper mode %q", mode)
	}
}

func commandHelperTERMChannel() <-chan os.Signal {
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	return term
}

func commandHelperEnvironment(mode string) []string {
	prefix := commandHelperModeEnv + "="
	environment := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, prefix) {
			environment = append(environment, item)
		}
	}
	return append(environment, commandHelperModeEnv+"="+mode)
}

func writeCommandHelperFile(t *testing.T, path, value string) {
	t.Helper()
	if path == "" {
		t.Fatal("missing command helper path")
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForCommandTestFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCommandRedactsOutput(t *testing.T) {
	const secret = "command-component-canary-12345"
	runtimeRedactor := redact.NewRuntimeRedactor()
	environment, err := compileEnv(map[string]string{"AUTHORIZATION": "Bearer ${API_TOKEN}"}, DefaultLimits(), func(name string) (string, bool) {
		return secret, name == "API_TOKEN"
	}, runtimeRedactor)
	if err != nil {
		t.Fatal(err)
	}
	runner := &ShellCommandRunner{Redactor: runtimeRedactor}
	result, err := runner.Run(context.Background(), CommandRequest{Command: `printf 'command-component-canary-12345'; printf 'command-component-canary-12345' >&2`, ProjectRoot: t.TempDir(), Environment: environment, EventJSON: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.Stdout)+string(result.Stderr), secret) {
		t.Fatalf("bare expanded secret leaked through command output: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
}

type trackedWriteCloser struct {
	io.WriteCloser
	fault   error
	tripped *atomic.Bool
	closed  *atomic.Int32
	once    sync.Once
}

func (w *trackedWriteCloser) Write(data []byte) (int, error) {
	if w.fault != nil {
		w.tripped.Store(true)
		return 0, w.fault
	}
	return w.WriteCloser.Write(data)
}

func (w *trackedWriteCloser) Close() error {
	w.once.Do(func() { w.closed.Add(1) })
	return w.WriteCloser.Close()
}

type trackedReadCloser struct {
	io.ReadCloser
	fault   error
	tripped *atomic.Bool
	closed  *atomic.Int32
	once    sync.Once
}

func (r *trackedReadCloser) Read(data []byte) (int, error) {
	if r.fault != nil {
		r.tripped.Store(true)
		return 0, r.fault
	}
	return r.ReadCloser.Read(data)
}

func (r *trackedReadCloser) Close() error {
	r.once.Do(func() { r.closed.Add(1) })
	return r.ReadCloser.Close()
}

type faultingCommandPipeFactory struct {
	stream  string
	fault   error
	tripped atomic.Bool
	closed  atomic.Int32
}

func (f *faultingCommandPipeFactory) stdin(cmd *exec.Cmd) (io.WriteCloser, error) {
	pipe, err := (execCommandPipeFactory{}).stdin(cmd)
	if err != nil {
		return nil, err
	}
	var fault error
	if f.stream == "stdin" {
		fault = f.fault
	}
	return &trackedWriteCloser{WriteCloser: pipe, fault: fault, tripped: &f.tripped, closed: &f.closed}, nil
}

func (f *faultingCommandPipeFactory) stdout(cmd *exec.Cmd) (io.ReadCloser, error) {
	pipe, err := (execCommandPipeFactory{}).stdout(cmd)
	if err != nil {
		return nil, err
	}
	var fault error
	if f.stream == "stdout" {
		fault = f.fault
	}
	return &trackedReadCloser{ReadCloser: pipe, fault: fault, tripped: &f.tripped, closed: &f.closed}, nil
}

func (f *faultingCommandPipeFactory) stderr(cmd *exec.Cmd) (io.ReadCloser, error) {
	pipe, err := (execCommandPipeFactory{}).stderr(cmd)
	if err != nil {
		return nil, err
	}
	var fault error
	if f.stream == "stderr" {
		fault = f.fault
	}
	return &trackedReadCloser{ReadCloser: pipe, fault: fault, tripped: &f.tripped, closed: &f.closed}, nil
}

func TestCommandRunnerIOFailure(t *testing.T) {
	const canary = "raw-command-io-error-canary"
	for _, item := range []struct {
		stream  string
		command string
	}{
		{stream: "stdin", command: `cat >/dev/null`},
		{stream: "stdout", command: `printf 'stdout'; cat >/dev/null`},
		{stream: "stderr", command: `printf 'stderr' >&2; cat >/dev/null`},
	} {
		t.Run(item.stream, func(t *testing.T) {
			factory := &faultingCommandPipeFactory{stream: item.stream, fault: errors.New(canary + "-" + item.stream)}
			runner := &ShellCommandRunner{TerminateGrace: time.Millisecond, pipeFactory: factory}
			result, err := runner.Run(context.Background(), CommandRequest{Command: item.command, ProjectRoot: t.TempDir(), EventJSON: []byte(`{"event":"test"}`)})
			if err == nil || err.Error() != "command I/O failed" {
				t.Fatalf("error = %v", err)
			}
			if len(result.Stdout) != 0 || len(result.Stderr) != 0 || strings.Contains(err.Error(), canary) {
				t.Fatalf("unsafe result = %#v, %v", result, err)
			}
			if !factory.tripped.Load() {
				t.Fatalf("%s fault was not exercised", item.stream)
			}
			if got := factory.closed.Load(); got != 3 {
				t.Fatalf("closed pipes = %d, want 3", got)
			}
		})
	}
}

func TestCommandExecutionFailure(t *testing.T) {
	cases := []struct {
		name      string
		project   func(*testing.T) string
		command   string
		canaries  []string
		wantError string
	}{
		{
			name: "start error",
			project: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "start-error-secret-canary")
			},
			command:   `printf 'unreachable-command-secret-canary'`,
			canaries:  []string{"start-error-secret-canary", "unreachable-command-secret-canary"},
			wantError: "command start failed",
		},
		{
			name:      "non-zero exit",
			project:   func(t *testing.T) string { return t.TempDir() },
			command:   `printf 'nonzero-stdout-secret-canary'; printf 'nonzero-stderr-secret-canary' >&2; exit 7`,
			canaries:  []string{"nonzero-stdout-secret-canary", "nonzero-stderr-secret-canary"},
			wantError: "command failed",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			projectRoot := item.project(t)
			runtimeRedactor := redact.NewRuntimeRedactor()
			for _, canary := range item.canaries {
				runtimeRedactor.RegisterSecret(canary)
			}
			runner := &ShellCommandRunner{Redactor: runtimeRedactor, TerminateGrace: time.Millisecond}
			result, err := runner.Run(context.Background(), CommandRequest{Command: item.command, ProjectRoot: projectRoot, EventJSON: []byte(`{}`)})
			if err == nil || err.Error() != item.wantError {
				t.Fatalf("runner error = %v", err)
			}
			if len(result.Stdout) != 0 || len(result.Stderr) != 0 {
				t.Fatalf("failure returned output: %#v", result)
			}
			for _, canary := range item.canaries {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("runner error leaked %q: %v", canary, err)
				}
			}

			collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: runtimeRedactor.Text})
			rule := commandRule(EventToolBefore, 1, item.command, true, false, false)
			engine, engineErr := NewEngine(newSnapshot([]Rule{rule}), EngineOptions{ProjectRoot: projectRoot, CommandRunner: runner, Diagnostics: collector, Redactor: runtimeRedactor})
			if engineErr != nil {
				t.Fatal(engineErr)
			}
			decision := engine.BeforeTool(context.Background(), ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault}, ToolInput{CallID: "c", Name: "Read", Arguments: map[string]any{}})
			if decision.IsDeny() {
				t.Fatalf("execution failure produced deny: %#v", decision)
			}
			diagnosticItems := collector.List()
			if len(diagnosticItems) != 1 || diagnosticItems[0].Code != DiagnosticCommandFailed {
				t.Fatalf("diagnostics = %#v", diagnosticItems)
			}
			for _, diagnostic := range diagnosticItems {
				for _, canary := range item.canaries {
					if strings.Contains(diagnostic.Text(), canary) {
						t.Fatalf("diagnostic leaked %q: %s", canary, diagnostic.Text())
					}
				}
			}
		})
	}
}

func TestCommandDecisionIntegration(t *testing.T) {
	const stdoutCanary = "command-stdout-decision-secret"
	const stderrCanary = "command-stderr-decision-secret"
	cases := []struct {
		name            string
		command         string
		wantDeny        bool
		wantReason      string
		wantDiagnostics int
	}{
		{name: "exact allow", command: `printf '%s' '{"decision":"allow"}'; printf '%s' 'command-stderr-decision-secret' >&2`},
		{name: "exact deny", command: `printf '%s' '{"decision":"deny","reason":"command-stdout-decision-secret"}'; printf '%s' 'command-stderr-decision-secret' >&2`, wantDeny: true, wantReason: "[redacted]"},
		{name: "extra stdout", command: `printf '%s%s' '{"decision":"allow"}' 'command-stdout-decision-secret'; printf '%s' 'command-stderr-decision-secret' >&2`, wantDiagnostics: 1},
		{name: "stderr only pseudo decision", command: `printf '%s' '{"decision":"deny","reason":"command-stderr-decision-secret"}' >&2`, wantDiagnostics: 1},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			runtimeRedactor := redact.NewRuntimeRedactor()
			runtimeRedactor.RegisterSecret(stdoutCanary)
			runtimeRedactor.RegisterSecret(stderrCanary)
			collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: runtimeRedactor.Text})
			runner := &ShellCommandRunner{Redactor: runtimeRedactor, TerminateGrace: time.Millisecond}
			rule := commandRule(EventToolBefore, 1, item.command, true, false, false)
			engine, err := NewEngine(newSnapshot([]Rule{rule}), EngineOptions{ProjectRoot: t.TempDir(), CommandRunner: runner, Diagnostics: collector, Redactor: runtimeRedactor})
			if err != nil {
				t.Fatal(err)
			}
			decision := engine.BeforeTool(context.Background(), ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault}, ToolInput{CallID: "c", Name: "Read", Arguments: map[string]any{}})
			if decision.IsDeny() != item.wantDeny || decision.Reason != item.wantReason {
				t.Fatalf("decision = %#v", decision)
			}
			items := collector.List()
			if len(items) != item.wantDiagnostics {
				t.Fatalf("diagnostics = %#v", items)
			}
			for _, diagnostic := range items {
				text := diagnostic.Text()
				if strings.Contains(text, stdoutCanary) || strings.Contains(text, stderrCanary) {
					t.Fatalf("decision stream leaked: %s", text)
				}
			}
		})
	}
}
