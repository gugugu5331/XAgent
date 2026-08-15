package stdio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	mcptransport "xagent/internal/mcpclient/transport"
	"xagent/internal/proctree"
	"xagent/internal/safefs"
)

func TestStartRequiresProtectedProcessAndBorrowedPipes(t *testing.T) {
	t.Run("protected request uses an approved plan and copied config", func(t *testing.T) {
		workingDirectory, approvedPlan := stdioTestProtection(t)
		stdin := &borrowedWriteSpy{}
		stdout := &borrowedReadSpy{}
		stderr := &borrowedReadSpy{}
		process := &recordingProcess{pipes: proctree.Pipes{Stdin: stdin, Stdout: stdout, Stderr: stderr}}
		runner := &recordingRunner{process: process}
		plans := &recordingPlanFactory{plan: approvedPlan}
		args := []string{"--stdio", "original"}
		environment := []string{"SAFE_MODE=1", "LANG=C"}

		created := newStdioTestTransport(t, Config{
			Executable:       "approved-mcp-server",
			Args:             args,
			Env:              environment,
			WorkingDirectory: workingDirectory,
			Runner:           runner,
			PlanFactory:      plans,
			Lifecycle:        mcptransport.Options{CleanupTimeout: time.Second, Diagnostics: stdioDiscardSink{}},
		})
		args[0] = "mutated"
		environment[0] = "UNSAFE=1"

		if err := created.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		request := runner.snapshotRequest()
		if runner.calls.Load() != 1 || plans.calls.Load() != 1 || process.pipesCalls.Load() != 1 {
			t.Fatalf("plan/runner/pipes calls = %d/%d/%d, want 1/1/1", plans.calls.Load(), runner.calls.Load(), process.pipesCalls.Load())
		}
		if request.Mode != proctree.ProtectionRequired || request.Executable != "approved-mcp-server" ||
			!reflect.DeepEqual(request.Args, []string{"--stdio", "original"}) ||
			!reflect.DeepEqual(request.Env, []string{"SAFE_MODE=1", "LANG=C"}) ||
			request.WorkingDir == nil || request.WorkingDir.Identity() != workingDirectory.Identity() ||
			!reflect.DeepEqual(request.Protection, approvedPlan) {
			t.Fatalf("protected start request = %#v", request)
		}
		if created.state.State() != mcptransport.TransportStateRunning {
			t.Fatalf("state after protected Start = %s, want running", created.state.State())
		}
		created.mu.Lock()
		gotProcess, gotStdin, gotStdout, gotStderr := created.process, created.stdin, created.stdout, created.stderr
		created.mu.Unlock()
		if gotProcess != process || gotStdin != stdin || gotStdout != stdout || gotStderr != stderr {
			t.Fatal("transport did not retain the single borrowed Process.Pipes snapshot")
		}

		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if process.closeCalls.Load() != 1 || stdin.closeCalls.Load() != 0 || stdout.closeCalls.Load() != 0 || stderr.closeCalls.Load() != 0 {
			t.Fatalf("process/borrowed closes = %d/%d/%d/%d, want 1/0/0/0", process.closeCalls.Load(), stdin.closeCalls.Load(), stdout.closeCalls.Load(), stderr.closeCalls.Load())
		}
	})

	t.Run("plan failure never reaches runner", func(t *testing.T) {
		workingDirectory, _ := stdioTestProtection(t)
		plans := &recordingPlanFactory{err: errors.New("plan unavailable")}
		runner := &recordingRunner{}
		created := newStdioTestTransport(t, validStdioTestConfig(workingDirectory, runner, plans))

		if err := created.Start(context.Background()); !errors.Is(err, ErrStartFailed) {
			t.Fatalf("Start after plan failure = %v, want safe start failure", err)
		}
		if runner.calls.Load() != 0 || created.state.State() != mcptransport.TransportStateFailed {
			t.Fatalf("runner calls/state = %d/%s, want 0/failed", runner.calls.Load(), created.state.State())
		}
		if err := created.Send(context.Background(), nil); !errors.Is(err, mcptransport.ErrNotRunning) {
			t.Fatalf("Send after failed Start = %v, want not running", err)
		}
		if _, err := created.Receive(context.Background()); !errors.Is(err, mcptransport.ErrNotRunning) {
			t.Fatalf("Receive after failed Start = %v, want not running", err)
		}
	})

	t.Run("StartError never publishes Running", func(t *testing.T) {
		workingDirectory, approvedPlan := stdioTestProtection(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		runner := &recordingRunner{
			err:     &proctree.StartError{Code: "protected_exec_unavailable", TargetStarted: false},
			entered: entered,
			release: release,
		}
		created := newStdioTestTransport(t, validStdioTestConfig(workingDirectory, runner, &recordingPlanFactory{plan: approvedPlan}))
		result := make(chan error, 1)
		go func() { result <- created.Start(context.Background()) }()
		waitStdioSignal(t, entered, "Runner.Start entry")
		if created.state.State() != mcptransport.TransportStateStarting {
			t.Fatalf("state while Runner.Start blocked = %s, want starting", created.state.State())
		}
		close(release)
		err := <-result
		var startErr *proctree.StartError
		if !errors.As(err, &startErr) || startErr.TargetStarted || startErr.Code != "protected_exec_unavailable" {
			t.Fatalf("Start error = %#v, want structured pre-target StartError", err)
		}
		if created.state.State() != mcptransport.TransportStateFailed {
			t.Fatalf("state after StartError = %s, want failed", created.state.State())
		}
	})

	t.Run("invalid borrowed pipes roll back only through Process", func(t *testing.T) {
		workingDirectory, approvedPlan := stdioTestProtection(t)
		stdin := &borrowedWriteSpy{}
		stdout := &borrowedReadSpy{}
		var typedNilStderr *borrowedReadSpy
		process := &recordingProcess{pipes: proctree.Pipes{Stdin: stdin, Stdout: stdout, Stderr: typedNilStderr}}
		created := newStdioTestTransport(t, validStdioTestConfig(
			workingDirectory,
			&recordingRunner{process: process},
			&recordingPlanFactory{plan: approvedPlan},
		))

		if err := created.Start(context.Background()); !errors.Is(err, ErrStartFailed) {
			t.Fatalf("Start with missing stderr = %v, want start failure", err)
		}
		if process.pipesCalls.Load() != 1 || process.closeCalls.Load() != 1 || stdin.closeCalls.Load() != 0 || stdout.closeCalls.Load() != 0 {
			t.Fatalf("pipes/process/borrowed closes = %d/%d/%d/%d, want 1/1/0/0", process.pipesCalls.Load(), process.closeCalls.Load(), stdin.closeCalls.Load(), stdout.closeCalls.Load())
		}
		if created.state.State() != mcptransport.TransportStateFailed {
			t.Fatalf("state after invalid pipes = %s, want failed", created.state.State())
		}
	})

	t.Run("created plan is handed to Runner even when caller becomes cancelled", func(t *testing.T) {
		workingDirectory, approvedPlan := stdioTestProtection(t)
		ctx, cancel := context.WithCancel(context.Background())
		process := &recordingProcess{pipes: completeRecordingPipes()}
		runner := &recordingRunner{process: process}
		plans := &cancelingPlanFactory{plan: approvedPlan, cancel: cancel}
		created := newStdioTestTransport(t, validStdioTestConfig(workingDirectory, runner, plans))

		if err := created.Start(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Start after post-plan cancellation = %v, want cancelled", err)
		}
		if plans.calls.Load() != 1 || runner.calls.Load() != 1 || process.closeCalls.Load() != 1 || process.pipesCalls.Load() != 0 {
			t.Fatalf("plan/runner/process-close/pipes calls = %d/%d/%d/%d, want 1/1/1/0", plans.calls.Load(), runner.calls.Load(), process.closeCalls.Load(), process.pipesCalls.Load())
		}
		if created.state.State() != mcptransport.TransportStateFailed {
			t.Fatalf("state after post-plan cancellation = %s, want failed", created.state.State())
		}
	})

	t.Run("process returned with an error is rolled back", func(t *testing.T) {
		workingDirectory, approvedPlan := stdioTestProtection(t)
		process := &recordingProcess{pipes: completeRecordingPipes()}
		created := newStdioTestTransport(t, validStdioTestConfig(
			workingDirectory,
			&recordingRunner{
				process: process,
				err:     &proctree.StartError{Code: "process_start_failed", TargetStarted: true},
			},
			&recordingPlanFactory{plan: approvedPlan},
		))

		err := created.Start(context.Background())
		var startErr *proctree.StartError
		if !errors.As(err, &startErr) || !startErr.TargetStarted {
			t.Fatalf("Start contract violation = %#v, want preserved StartError", err)
		}
		if process.closeCalls.Load() != 1 || process.pipesCalls.Load() != 0 || created.state.State() != mcptransport.TransportStateFailed {
			t.Fatalf("process close/pipes/state = %d/%d/%s, want 1/0/failed", process.closeCalls.Load(), process.pipesCalls.Load(), created.state.State())
		}
	})

	t.Run("Close cancels a blocked plan creation before target start", func(t *testing.T) {
		workingDirectory, _ := stdioTestProtection(t)
		entered := make(chan struct{})
		plans := &contextBlockingPlanFactory{entered: entered}
		runner := &recordingRunner{}
		created := newStdioTestTransport(t, validStdioTestConfig(workingDirectory, runner, plans))
		startResult := make(chan error, 1)
		go func() { startResult <- created.Start(context.Background()) }()
		waitStdioSignal(t, entered, "blocked plan creation")

		if err := created.Close(context.Background()); err != nil {
			t.Fatalf("Close with blocked plan factory = %v", err)
		}
		if err := <-startResult; !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked plan Start result = %v, want cancelled", err)
		}
		if runner.calls.Load() != 0 || created.state.State() != mcptransport.TransportStateClosed {
			t.Fatalf("runner calls/state = %d/%s, want 0/closed", runner.calls.Load(), created.state.State())
		}
	})

	t.Run("Close cancels a blocked protected Runner", func(t *testing.T) {
		workingDirectory, approvedPlan := stdioTestProtection(t)
		entered := make(chan struct{})
		runner := &contextBlockingRunner{entered: entered}
		created := newStdioTestTransport(t, validStdioTestConfig(
			workingDirectory,
			runner,
			&recordingPlanFactory{plan: approvedPlan},
		))
		startResult := make(chan error, 1)
		go func() { startResult <- created.Start(context.Background()) }()
		waitStdioSignal(t, entered, "blocked protected Runner")

		if err := created.Close(context.Background()); err != nil {
			t.Fatalf("Close with blocked Runner = %v", err)
		}
		if err := <-startResult; !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked Runner Start result = %v, want cancelled", err)
		}
		if runner.calls.Load() != 1 || created.state.State() != mcptransport.TransportStateClosed {
			t.Fatalf("runner calls/state = %d/%s, want 1/closed", runner.calls.Load(), created.state.State())
		}
	})

	t.Run("Process remains owner-visible while Pipes is blocked", func(t *testing.T) {
		workingDirectory, approvedPlan := stdioTestProtection(t)
		pipesEntered := make(chan struct{})
		pipesRelease := make(chan struct{})
		closeStarted := make(chan struct{})
		process := &recordingProcess{
			pipes:        completeRecordingPipes(),
			pipesEntered: pipesEntered,
			pipesRelease: pipesRelease,
			closeStarted: closeStarted,
		}
		config := validStdioTestConfig(
			workingDirectory,
			&recordingRunner{process: process},
			&recordingPlanFactory{plan: approvedPlan},
		)
		config.Lifecycle.CleanupTimeout = 20 * time.Millisecond
		created := newStdioTestTransport(t, config)
		startResult := make(chan error, 1)
		go func() { startResult <- created.Start(context.Background()) }()
		waitStdioSignal(t, pipesEntered, "blocked Process.Pipes")

		if err := created.Close(context.Background()); !errors.Is(err, mcptransport.ErrCleanupTimeout) {
			t.Fatalf("Close while Pipes blocked = %v, want hard cleanup timeout", err)
		}
		waitStdioSignal(t, closeStarted, "forced Process.Close while Pipes blocked")
		if process.closeCalls.Load() == 0 {
			t.Fatal("forceClose could not see the Process while Pipes was blocked")
		}
		close(pipesRelease)
		if err := <-startResult; !errors.Is(err, context.Canceled) && !errors.Is(err, mcptransport.ErrClosing) {
			t.Fatalf("blocked Pipes Start result = %v, want cancelled/closing", err)
		}
		if created.state.State() != mcptransport.TransportStateClosed {
			t.Fatalf("state after blocked Pipes release = %s, want closed", created.state.State())
		}
	})

	t.Run("late Process after Close hard deadline is reclaimed", func(t *testing.T) {
		workingDirectory, approvedPlan := stdioTestProtection(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		process := &recordingProcess{pipes: completeRecordingPipes()}
		runner := &recordingRunner{process: process, entered: entered, release: release}
		config := validStdioTestConfig(workingDirectory, runner, &recordingPlanFactory{plan: approvedPlan})
		config.Lifecycle.CleanupTimeout = 20 * time.Millisecond
		created := newStdioTestTransport(t, config)
		startResult := make(chan error, 1)
		go func() { startResult <- created.Start(context.Background()) }()
		waitStdioSignal(t, entered, "late Runner.Start entry")

		if err := created.Close(context.Background()); !errors.Is(err, mcptransport.ErrCleanupTimeout) {
			t.Fatalf("Close while Start blocked = %v, want hard cleanup timeout", err)
		}
		close(release)
		if err := <-startResult; !errors.Is(err, context.Canceled) && !errors.Is(err, mcptransport.ErrClosing) {
			t.Fatalf("late Start result = %v, want cancelled/closing", err)
		}
		if process.closeCalls.Load() != 1 || process.pipesCalls.Load() != 0 || created.state.State() != mcptransport.TransportStateClosed {
			t.Fatalf("late process close/pipes/state = %d/%d/%s, want 1/0/closed", process.closeCalls.Load(), process.pipesCalls.Load(), created.state.State())
		}
	})

	t.Run("surface exposes no bare process handles", func(t *testing.T) {
		for _, candidate := range []reflect.Type{reflect.TypeOf(Config{}), reflect.TypeOf(Transport{})} {
			for index := 0; index < candidate.NumField(); index++ {
				fieldType := candidate.Field(index).Type
				if forbiddenStdioProcessType(fieldType) {
					t.Fatalf("%s exposes forbidden process field %s", candidate, fieldType)
				}
			}
		}
	})
}

func TestStdioCloseAndStartupRollback(t *testing.T) {
	t.Run("framing failure joins the supervisor through Process ownership", func(t *testing.T) {
		process := newBurstExitProcess(proctree.Result{})
		workingDirectory, approvedPlan := stdioTestProtection(t)
		created := newStdioTestTransport(t, validStdioTestConfig(
			workingDirectory,
			&recordingRunner{process: process},
			&recordingPlanFactory{plan: approvedPlan},
		))
		created.maxResponseBytes = -1
		t.Cleanup(func() {
			process.abort()
			_ = created.Close(context.Background())
		})

		if err := created.Start(context.Background()); !errors.Is(err, ErrStartFailed) {
			t.Fatalf("Start with injected framing failure = %v, want safe start failure", err)
		}
		created.mu.Lock()
		supervisor, stderrDrain, writer := created.supervisor, created.stderrDrain, created.writer
		created.mu.Unlock()
		if supervisor == nil || stderrDrain != nil || writer != nil {
			t.Fatalf("startup worker boundary = supervisor:%v stderr:%v writer:%v", supervisor != nil, stderrDrain != nil, writer != nil)
		}
		waitStdioSignal(t, supervisor.Done(), "rolled-back supervisor")
		waitStdioSignal(t, created.receives.Done(), "rolled-back receive admission")
		if process.waitCalls.Load() != 1 || process.closeCalls.Load() != 1 || process.pipesCalls.Load() != 1 {
			t.Fatalf("rollback wait/close/pipes calls = %d/%d/%d, want 1/1/1",
				process.waitCalls.Load(), process.closeCalls.Load(), process.pipesCalls.Load())
		}
		if process.stdin.borrowedCloses.Load() != 0 || process.stdout.borrowedCloses.Load() != 0 || process.stderr.borrowedCloses.Load() != 0 {
			t.Fatal("startup rollback directly closed a borrowed pipe")
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if process.closeCalls.Load() != 1 || created.state.State() != mcptransport.TransportStateClosed {
			t.Fatalf("repeated cleanup close calls/state = %d/%s, want 1/closed", process.closeCalls.Load(), created.state.State())
		}
	})

	t.Run("Close during blocked Pipes continues after caller cancellation", func(t *testing.T) {
		workingDirectory, approvedPlan := stdioTestProtection(t)
		pipesEntered := make(chan struct{})
		pipesRelease := make(chan struct{})
		process := &recordingProcess{
			pipes:        completeRecordingPipes(),
			pipesEntered: pipesEntered,
			pipesRelease: pipesRelease,
		}
		created := newStdioTestTransport(t, validStdioTestConfig(
			workingDirectory,
			&recordingRunner{process: process},
			&recordingPlanFactory{plan: approvedPlan},
		))
		startResult := make(chan error, 1)
		go func() { startResult <- created.Start(context.Background()) }()
		waitStdioSignal(t, pipesEntered, "startup Process.Pipes")
		created.mu.Lock()
		supervisor := created.supervisor
		created.mu.Unlock()
		if supervisor == nil {
			t.Fatal("supervisor was not started before the blocking Pipes snapshot")
		}

		caller, cancel := context.WithCancel(context.Background())
		cancel()
		if err := created.Close(caller); !errors.Is(err, context.Canceled) {
			t.Fatalf("Close cancelled waiter = %v, want caller cancellation", err)
		}
		close(pipesRelease)
		if err := <-startResult; !errors.Is(err, context.Canceled) && !errors.Is(err, mcptransport.ErrClosing) {
			t.Fatalf("Start released after Close = %v, want cancelled/closing", err)
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatalf("background cleanup after cancelled waiter = %v", err)
		}
		waitStdioSignal(t, supervisor.Done(), "startup-close supervisor")
		waitStdioSignal(t, created.receives.Done(), "startup-close receive admission")
		if process.closeCalls.Load() != 1 || process.pipesCalls.Load() != 1 {
			t.Fatalf("startup-close process close/pipes calls = %d/%d, want 1/1", process.closeCalls.Load(), process.pipesCalls.Load())
		}
		if created.state.State() != mcptransport.TransportStateClosed {
			t.Fatalf("startup-close state = %s, want closed", created.state.State())
		}
	})

	t.Run("running Close rejects late receives and joins active work", func(t *testing.T) {
		process := newBurstExitProcess(proctree.Result{Cancelled: true})
		closeRelease := make(chan struct{})
		process.closeRelease = closeRelease
		workingDirectory, approvedPlan := stdioTestProtection(t)
		created := newStdioTestTransport(t, validStdioTestConfig(
			workingDirectory,
			&recordingRunner{process: process},
			&recordingPlanFactory{plan: approvedPlan},
		))
		var releaseOnce sync.Once
		releaseClose := func() { releaseOnce.Do(func() { close(closeRelease) }) }
		t.Cleanup(func() {
			releaseClose()
			process.abort()
			_ = created.Close(context.Background())
		})
		if err := created.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		created.mu.Lock()
		writer, stderrDrain, supervisor := created.writer, created.stderrDrain, created.supervisor
		created.mu.Unlock()

		receiveResult := make(chan error, 1)
		go func() {
			_, err := created.Receive(context.Background())
			receiveResult <- err
		}()
		waitStdioSignal(t, process.stdout.readEntered, "active stdout Receive")

		caller, cancel := context.WithCancel(context.Background())
		closeResult := make(chan error, 1)
		go func() { closeResult <- created.Close(caller) }()
		waitStdioSignal(t, process.closeEntered, "background Process.Close")
		cancel()
		if err := <-closeResult; !errors.Is(err, context.Canceled) {
			t.Fatalf("Close waiter cancellation = %v, want caller cancellation", err)
		}
		if _, err := created.Receive(context.Background()); !errors.Is(err, mcptransport.ErrClosing) {
			t.Fatalf("late Receive during cleanup = %v, want closing", err)
		}

		releaseClose()
		if err := <-receiveResult; !errors.Is(err, ErrFatalFraming) {
			t.Fatalf("active Receive after owner close = %v, want fatal framing", err)
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatalf("background Close convergence = %v", err)
		}
		waitStdioSignal(t, writer.Done(), "running-close writer")
		waitStdioSignal(t, stderrDrain.Done(), "running-close stderr")
		waitStdioSignal(t, supervisor.Done(), "running-close supervisor")
		waitStdioSignal(t, created.receives.Done(), "running-close receive admission")
		if process.waitCalls.Load() != 1 || process.closeCalls.Load() != 1 {
			t.Fatalf("running-close wait/close calls = %d/%d, want 1/1", process.waitCalls.Load(), process.closeCalls.Load())
		}
		if process.stdout.activeReads.Load() != 0 || process.stderr.activeReads.Load() != 0 {
			t.Fatalf("running-close active stdout/stderr reads = %d/%d, want 0/0",
				process.stdout.activeReads.Load(), process.stderr.activeReads.Load())
		}
		if process.stdin.borrowedCloses.Load() != 0 || process.stdout.borrowedCloses.Load() != 0 || process.stderr.borrowedCloses.Load() != 0 {
			t.Fatal("running Close directly closed a borrowed pipe")
		}
	})
}

func TestBurstThenExit(t *testing.T) {
	const responseCount = 64
	expectedResult := proctree.Result{ExitCode: 23}
	process := newBurstExitProcess(expectedResult)
	sink := &supervisorDiagnosticSink{}
	workingDirectory, approvedPlan := stdioTestProtection(t)
	config := validStdioTestConfig(
		workingDirectory,
		&recordingRunner{process: process},
		&recordingPlanFactory{plan: approvedPlan},
	)
	config.Lifecycle.Diagnostics = sink
	created := newStdioTestTransport(t, config)
	t.Cleanup(func() {
		process.abort()
		_ = created.Close(context.Background())
	})
	if err := created.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitStdioSignal(t, process.waitEntered, "burst supervisor Process.Wait")

	created.mu.Lock()
	writer := created.writer
	stderrDrain := created.stderrDrain
	supervisor := created.supervisor
	created.mu.Unlock()
	if writer == nil || stderrDrain == nil || supervisor == nil {
		t.Fatal("burst transport did not publish every I/O worker")
	}

	var burst bytes.Buffer
	expectedFrames := make([]string, responseCount)
	for index := range responseCount {
		expectedFrames[index] = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"sequence":%d}}`, index+1, index)
		burst.WriteString(expectedFrames[index])
		burst.WriteByte('\n')
	}

	type receiveResult struct {
		frames      []string
		terminalErr error
	}
	received := make(chan receiveResult, 1)
	go func() {
		frames := make([]string, 0, responseCount)
		for range responseCount {
			event, err := created.Receive(context.Background())
			if err != nil {
				received <- receiveResult{frames: frames, terminalErr: err}
				return
			}
			frames = append(frames, string(event.Frame))
		}
		_, terminalErr := created.Receive(context.Background())
		received <- receiveResult{frames: frames, terminalErr: terminalErr}
	}()

	published := make(chan error, 1)
	go func() { published <- process.publish(burst.Bytes()) }()

	var observation receiveResult
	select {
	case observation = <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out draining burst responses and terminal EOF")
	}
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("publish burst responses: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out publishing burst responses")
	}
	if len(observation.frames) != len(expectedFrames) {
		t.Fatalf("burst response count = %d, want %d; terminal=%v", len(observation.frames), len(expectedFrames), observation.terminalErr)
	}
	for index := range expectedFrames {
		if observation.frames[index] != expectedFrames[index] {
			t.Fatalf("burst response %d = %q, want %q", index, observation.frames[index], expectedFrames[index])
		}
	}
	if observation.terminalErr == nil {
		t.Fatal("burst process exit did not produce a terminal Receive error")
	}

	result, waitErr := supervisor.Wait(context.Background())
	if result != expectedResult || waitErr != nil {
		t.Fatalf("supervisor terminal outcome = %#v/%v, want %#v/nil", result, waitErr, expectedResult)
	}
	waitStdioSignal(t, supervisor.Done(), "burst supervisor diagnostics")
	diagnostics := sink.snapshot()
	if len(diagnostics) != 1 || diagnostics[0].Code != "mcp_stdio_process_exited" {
		t.Fatalf("burst exit diagnostics = %#v, want one fixed process-exit record", diagnostics)
	}

	const closeCallers = 16
	closeResults := make(chan error, closeCallers)
	var closeStarted sync.WaitGroup
	closeStarted.Add(closeCallers)
	for range closeCallers {
		go func() {
			closeStarted.Done()
			closeResults <- created.Close(context.Background())
		}()
	}
	closeStarted.Wait()
	for range closeCallers {
		select {
		case err := <-closeResults:
			if err != nil {
				t.Fatalf("concurrent burst Close = %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for concurrent burst Close")
		}
	}

	waitStdioSignal(t, writer.Done(), "burst writer completion")
	waitStdioSignal(t, stderrDrain.Done(), "burst stderr completion")
	waitStdioSignal(t, created.receives.Done(), "burst receiver completion")
	if process.waitCalls.Load() != 1 || process.closeCalls.Load() != 1 || process.pipesCalls.Load() != 1 {
		t.Fatalf("burst process wait/close/pipes calls = %d/%d/%d, want 1/1/1",
			process.waitCalls.Load(), process.closeCalls.Load(), process.pipesCalls.Load())
	}
	if process.closeStdinCalls.Load() != 0 || process.terminateCalls.Load() != 0 {
		t.Fatalf("burst process CloseStdin/Terminate calls = %d/%d, want 0/0",
			process.closeStdinCalls.Load(), process.terminateCalls.Load())
	}
	if process.stdin.borrowedCloses.Load() != 0 || process.stdout.borrowedCloses.Load() != 0 || process.stderr.borrowedCloses.Load() != 0 {
		t.Fatalf("burst borrowed stdin/stdout/stderr closes = %d/%d/%d, want 0/0/0",
			process.stdin.borrowedCloses.Load(), process.stdout.borrowedCloses.Load(), process.stderr.borrowedCloses.Load())
	}
	if process.stdin.ownerCloses.Load() != 1 || process.stdout.ownerCloses.Load() != 1 || process.stderr.ownerCloses.Load() != 1 {
		t.Fatalf("burst owner stdin/stdout/stderr closes = %d/%d/%d, want 1/1/1",
			process.stdin.ownerCloses.Load(), process.stdout.ownerCloses.Load(), process.stderr.ownerCloses.Load())
	}
	if process.stdout.activeReads.Load() != 0 || process.stderr.activeReads.Load() != 0 {
		t.Fatalf("burst active stdout/stderr reads = %d/%d, want 0/0",
			process.stdout.activeReads.Load(), process.stderr.activeReads.Load())
	}
	if created.state.State() != mcptransport.TransportStateClosed {
		t.Fatalf("burst transport state = %s, want closed", created.state.State())
	}
	select {
	case _, open := <-writer.queue:
		if !open {
			t.Fatal("burst cleanup closed the shared writer queue")
		}
		t.Fatal("burst cleanup retained an unexpected queued write")
	default:
	}
}

func validStdioTestConfig(workingDirectory *safefs.Root, runner proctree.Runner, plans proctree.ProtectionPlanFactory) Config {
	return Config{
		Executable:       "approved-mcp-server",
		Env:              []string{"SAFE_MODE=1"},
		WorkingDirectory: workingDirectory,
		Runner:           runner,
		PlanFactory:      plans,
		Lifecycle:        mcptransport.Options{CleanupTimeout: time.Second, Diagnostics: stdioDiscardSink{}},
	}
}

func newStdioTestTransport(t *testing.T, config Config) *Transport {
	t.Helper()
	created, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func stdioTestProtection(t *testing.T) (*safefs.Root, proctree.ProtectionPlan) {
	t.Helper()
	working := bootstrapStdioTestRoot(t)
	scratch := bootstrapStdioTestRoot(t)
	plan, err := proctree.NewProtectionPlan([]*safefs.Root{working}, scratch)
	if err != nil {
		t.Fatal(err)
	}
	return working, plan
}

func bootstrapStdioTestRoot(t *testing.T) *safefs.Root {
	t.Helper()
	opened, err := safefs.Bootstrap(t.TempDir(), safefs.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Root.Close() })
	return opened.Root
}

func forbiddenStdioProcessType(candidate reflect.Type) bool {
	for _, forbidden := range []reflect.Type{
		reflect.TypeOf(exec.Cmd{}), reflect.TypeOf((*exec.Cmd)(nil)),
		reflect.TypeOf(os.Process{}), reflect.TypeOf((*os.Process)(nil)),
		reflect.TypeOf(os.File{}), reflect.TypeOf((*os.File)(nil)),
	} {
		if candidate == forbidden {
			return true
		}
	}
	return false
}

type recordingPlanFactory struct {
	plan  proctree.ProtectionPlan
	err   error
	calls atomic.Int64
}

type contextBlockingPlanFactory struct {
	entered chan struct{}
	once    sync.Once
}

func (factory *contextBlockingPlanFactory) Create(ctx context.Context) (proctree.ProtectionPlan, error) {
	factory.once.Do(func() { close(factory.entered) })
	<-ctx.Done()
	return proctree.ProtectionPlan{}, ctx.Err()
}

type cancelingPlanFactory struct {
	plan   proctree.ProtectionPlan
	cancel context.CancelFunc
	calls  atomic.Int64
}

func (factory *cancelingPlanFactory) Create(context.Context) (proctree.ProtectionPlan, error) {
	factory.calls.Add(1)
	factory.cancel()
	return factory.plan, nil
}

func (factory *recordingPlanFactory) Create(context.Context) (proctree.ProtectionPlan, error) {
	factory.calls.Add(1)
	return factory.plan, factory.err
}

type recordingRunner struct {
	process proctree.Process
	err     error
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64

	mu      sync.Mutex
	request proctree.Request
	once    sync.Once
}

type contextBlockingRunner struct {
	entered chan struct{}
	once    sync.Once
	calls   atomic.Int64
}

func (runner *contextBlockingRunner) Start(ctx context.Context, _ proctree.Request) (proctree.Process, error) {
	runner.calls.Add(1)
	runner.once.Do(func() { close(runner.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (runner *recordingRunner) Start(_ context.Context, request proctree.Request) (proctree.Process, error) {
	runner.calls.Add(1)
	runner.mu.Lock()
	runner.request = request
	runner.mu.Unlock()
	if runner.entered != nil {
		runner.once.Do(func() { close(runner.entered) })
	}
	if runner.release != nil {
		<-runner.release
	}
	return runner.process, runner.err
}

func (runner *recordingRunner) snapshotRequest() proctree.Request {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.request
}

type recordingProcess struct {
	pipes        proctree.Pipes
	pipesCalls   atomic.Int64
	closeCalls   atomic.Int64
	pipesEntered chan struct{}
	pipesRelease chan struct{}
	pipesOnce    sync.Once
	closeStarted chan struct{}
	closeOnce    sync.Once
}

type burstExitProcess struct {
	pipes  proctree.Pipes
	stdin  *burstBorrowedWriter
	stdout *burstBorrowedReader
	stderr *burstBorrowedReader

	stdoutChild  *io.PipeWriter
	stderrChild  *io.PipeWriter
	result       proctree.Result
	waitEntered  chan struct{}
	closeEntered chan struct{}
	closeRelease <-chan struct{}
	terminal     chan struct{}

	waitCalls        atomic.Int64
	closeCalls       atomic.Int64
	pipesCalls       atomic.Int64
	closeStdinCalls  atomic.Int64
	terminateCalls   atomic.Int64
	waitEnteredOnce  sync.Once
	closeEnteredOnce sync.Once
	exitOnce         sync.Once
	closeOnce        sync.Once
	publishErr       error
}

func newBurstExitProcess(result proctree.Result) *burstExitProcess {
	stdoutOwner, stdoutChild := io.Pipe()
	stderrOwner, stderrChild := io.Pipe()
	stdin := &burstBorrowedWriter{}
	stdout := &burstBorrowedReader{owner: stdoutOwner, readEntered: make(chan struct{})}
	stderr := &burstBorrowedReader{owner: stderrOwner, readEntered: make(chan struct{})}
	return &burstExitProcess{
		pipes: proctree.Pipes{
			Stdin:  stdin,
			Stdout: stdout,
			Stderr: stderr,
		},
		stdin: stdin, stdout: stdout, stderr: stderr,
		stdoutChild: stdoutChild, stderrChild: stderrChild,
		result: result, waitEntered: make(chan struct{}), closeEntered: make(chan struct{}), terminal: make(chan struct{}),
	}
}

func (process *burstExitProcess) Pipes() proctree.Pipes {
	process.pipesCalls.Add(1)
	return process.pipes
}

func (process *burstExitProcess) CloseStdin() error {
	process.closeStdinCalls.Add(1)
	return nil
}

func (process *burstExitProcess) Wait(ctx context.Context) (proctree.Result, error) {
	process.waitCalls.Add(1)
	process.waitEnteredOnce.Do(func() { close(process.waitEntered) })
	select {
	case <-process.terminal:
		return process.result, nil
	case <-ctx.Done():
		return proctree.Result{}, ctx.Err()
	}
}

func (process *burstExitProcess) Terminate(context.Context) error {
	process.terminateCalls.Add(1)
	return nil
}

func (process *burstExitProcess) Close(ctx context.Context) error {
	process.closeCalls.Add(1)
	process.closeEnteredOnce.Do(func() { close(process.closeEntered) })
	if process.closeRelease != nil {
		select {
		case <-process.closeRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	process.closeOnce.Do(func() {
		process.abort()
		process.stdin.closeOwner()
		process.stdout.closeOwner()
		process.stderr.closeOwner()
	})
	select {
	case <-process.terminal:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (process *burstExitProcess) publish(payload []byte) error {
	process.exitOnce.Do(func() {
		if _, err := process.stdoutChild.Write(payload); err != nil {
			process.publishErr = err
		}
		if err := process.stdoutChild.Close(); err != nil && process.publishErr == nil {
			process.publishErr = err
		}
		if err := process.stderrChild.Close(); err != nil && process.publishErr == nil {
			process.publishErr = err
		}
		close(process.terminal)
	})
	return process.publishErr
}

func (process *burstExitProcess) abort() {
	process.exitOnce.Do(func() {
		_ = process.stdoutChild.Close()
		_ = process.stderrChild.Close()
		close(process.terminal)
	})
}

type burstBorrowedWriter struct {
	borrowedCloses atomic.Int64
	ownerCloses    atomic.Int64
	stopped        atomic.Bool
}

func (writer *burstBorrowedWriter) Write(data []byte) (int, error) {
	if writer.stopped.Load() {
		return 0, errors.New("burst owner stdin is closed")
	}
	return len(data), nil
}

func (writer *burstBorrowedWriter) Close() error {
	writer.borrowedCloses.Add(1)
	return errors.New("burst borrowed stdin cannot be closed")
}

func (writer *burstBorrowedWriter) closeOwner() {
	writer.ownerCloses.Add(1)
	writer.stopped.Store(true)
}

type burstBorrowedReader struct {
	owner          *io.PipeReader
	borrowedCloses atomic.Int64
	ownerCloses    atomic.Int64
	activeReads    atomic.Int64
	readEntered    chan struct{}
	readOnce       sync.Once
}

func (reader *burstBorrowedReader) Read(buffer []byte) (int, error) {
	reader.activeReads.Add(1)
	defer reader.activeReads.Add(-1)
	reader.readOnce.Do(func() { close(reader.readEntered) })
	return reader.owner.Read(buffer)
}

func (reader *burstBorrowedReader) Close() error {
	reader.borrowedCloses.Add(1)
	return errors.New("burst borrowed reader cannot be closed")
}

func (reader *burstBorrowedReader) closeOwner() {
	reader.ownerCloses.Add(1)
	_ = reader.owner.Close()
}

func completeRecordingPipes() proctree.Pipes {
	return proctree.Pipes{Stdin: &borrowedWriteSpy{}, Stdout: &borrowedReadSpy{}, Stderr: &borrowedReadSpy{}}
}

func (process *recordingProcess) Pipes() proctree.Pipes {
	process.pipesCalls.Add(1)
	if process.pipesEntered != nil {
		process.pipesOnce.Do(func() { close(process.pipesEntered) })
	}
	if process.pipesRelease != nil {
		<-process.pipesRelease
	}
	return process.pipes
}

func (*recordingProcess) CloseStdin() error { return nil }

func (*recordingProcess) Wait(context.Context) (proctree.Result, error) {
	return proctree.Result{}, nil
}

func (*recordingProcess) Terminate(context.Context) error { return nil }

func (process *recordingProcess) Close(context.Context) error {
	process.closeCalls.Add(1)
	if process.closeStarted != nil {
		process.closeOnce.Do(func() { close(process.closeStarted) })
	}
	return nil
}

type borrowedWriteSpy struct {
	closeCalls atomic.Int64
}

func (*borrowedWriteSpy) Write(data []byte) (int, error) { return len(data), nil }

func (pipe *borrowedWriteSpy) Close() error {
	pipe.closeCalls.Add(1)
	return errors.New("borrowed pipe cannot be closed")
}

type borrowedReadSpy struct {
	closeCalls atomic.Int64
}

func (*borrowedReadSpy) Read([]byte) (int, error) { return 0, io.EOF }

func (pipe *borrowedReadSpy) Close() error {
	pipe.closeCalls.Add(1)
	return errors.New("borrowed pipe cannot be closed")
}

type stdioDiscardSink struct{}

func (stdioDiscardSink) Add(diagnostics.SanitizeInput) {}

func waitStdioSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
