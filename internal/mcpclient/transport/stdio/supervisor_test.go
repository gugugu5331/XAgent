package stdio

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/proctree"
)

func TestSupervisorObservesSingleProcessResult(t *testing.T) {
	t.Run("one Wait result is broadcast to every observer", func(t *testing.T) {
		const observers = 48
		expected := proctree.Result{ExitCode: 23, Cancelled: true, TimedOut: true}
		process := newGatedSupervisorProcess(expected, nil)
		t.Cleanup(process.releaseWait)
		sink := &supervisorDiagnosticSink{}
		supervisor, err := startStdioSupervisor(process, sink)
		if err != nil {
			t.Fatal(err)
		}
		waitStdioSignal(t, process.entered, "supervisor Process.Wait")
		if process.waitCalls.Load() != 1 {
			t.Fatalf("Process.Wait calls before release = %d, want 1", process.waitCalls.Load())
		}
		if _, _, ready := supervisor.Result(); ready {
			t.Fatal("supervisor published a process result before Wait completed")
		}

		cancelledContext, cancel := context.WithCancel(context.Background())
		cancel()
		if result, err := supervisor.Wait(cancelledContext); !errors.Is(err, context.Canceled) || result != (proctree.Result{}) {
			t.Fatal("cancelled observer did not return only its context error")
		}
		if process.waitCalls.Load() != 1 {
			t.Fatal("observer cancellation started another Process.Wait")
		}

		type observed struct {
			result proctree.Result
			err    error
		}
		observations := make(chan observed, observers)
		resultObservations := make(chan observed, observers)
		stopPolling := make(chan struct{})
		var stopPollingOnce sync.Once
		t.Cleanup(func() { stopPollingOnce.Do(func() { close(stopPolling) }) })
		var started sync.WaitGroup
		started.Add(observers * 2)
		for index := 0; index < observers; index++ {
			go func() {
				started.Done()
				result, err := supervisor.Wait(context.Background())
				observations <- observed{result: result, err: err}
			}()
			go func() {
				started.Done()
				for {
					select {
					case <-stopPolling:
						return
					default:
					}
					result, err, ready := supervisor.Result()
					if ready {
						resultObservations <- observed{result: result, err: err}
						return
					}
					runtime.Gosched()
				}
			}()
		}
		started.Wait()
		process.releaseWait()

		for index := 0; index < observers; index++ {
			select {
			case observation := <-observations:
				if observation.result != expected || observation.err != nil {
					t.Fatal("observer did not receive the immutable process result")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("process result broadcast did not converge")
			}
		}
		for index := 0; index < observers; index++ {
			select {
			case observation := <-resultObservations:
				if observation.result != expected || observation.err != nil {
					t.Fatal("concurrent Result observer saw a partial process outcome")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("non-blocking process result observers did not converge")
			}
		}
		stopPollingOnce.Do(func() { close(stopPolling) })
		waitStdioSignal(t, supervisor.Done(), "supervisor completion")
		if process.waitCalls.Load() != 1 {
			t.Fatalf("Process.Wait calls after broadcast = %d, want 1", process.waitCalls.Load())
		}
		result, resultErr, ready := supervisor.Result()
		if !ready || result != expected || resultErr != nil {
			t.Fatal("non-blocking observer did not receive the final process result")
		}
		if result, err := supervisor.Wait(cancelledContext); result != expected || err != nil {
			t.Fatal("published process result did not win over an already cancelled observer")
		}
		recorded := sink.snapshot()
		if len(recorded) != 1 || recorded[0].Code != "mcp_stdio_process_exited" ||
			recorded[0].Source != "mcp.transport.stdio" || recorded[0].Severity != diagnostics.SeverityWarning ||
			recorded[0].Err != errSupervisedProcessExited {
			t.Fatal("abnormal process result did not produce one fixed diagnostic")
		}
	})

	t.Run("Wait error and result remain one payload-safe terminal pair", func(t *testing.T) {
		const canary = "supervisor-private-wait-error-canary"
		expectedResult := proctree.Result{ExitCode: -1}
		expectedErr := errors.New(canary)
		process := newGatedSupervisorProcess(expectedResult, expectedErr)
		t.Cleanup(process.releaseWait)
		sink := &supervisorDiagnosticSink{}
		supervisor, err := startStdioSupervisor(process, sink)
		if err != nil {
			t.Fatal(err)
		}
		waitStdioSignal(t, process.entered, "failing supervisor Process.Wait")
		process.releaseWait()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, waitErr := supervisor.Wait(ctx)
		if result != expectedResult || waitErr != ErrProcessWaitFailed || errors.Is(waitErr, expectedErr) ||
			strings.Contains(waitErr.Error(), canary) {
			t.Fatal("supervisor did not retain the result with a fixed wait failure")
		}
		result, waitErr, ready := supervisor.Result()
		if !ready || result != expectedResult || waitErr != ErrProcessWaitFailed || process.waitCalls.Load() != 1 {
			t.Fatal("repeated supervisor observation changed the terminal pair")
		}
		waitStdioSignal(t, supervisor.Done(), "failing supervisor diagnostic completion")
		recorded := sink.snapshot()
		if len(recorded) != 1 || recorded[0].Code != "mcp_stdio_process_wait_failed" ||
			recorded[0].Err != ErrProcessWaitFailed || strings.Contains(recorded[0].Err.Error(), canary) {
			t.Fatal("Process.Wait failure diagnostic retained private error text")
		}
	})

	t.Run("observer cancellation can race completion without changing the outcome", func(t *testing.T) {
		const observers = 32
		expected := proctree.Result{ExitCode: 17}
		process := newGatedSupervisorProcess(expected, nil)
		t.Cleanup(process.releaseWait)
		supervisor, err := startStdioSupervisor(process, &supervisorDiagnosticSink{})
		if err != nil {
			t.Fatal(err)
		}
		waitStdioSignal(t, process.entered, "racing supervisor Process.Wait")

		type observed struct {
			result proctree.Result
			err    error
		}
		results := make(chan observed, observers)
		cancels := make([]context.CancelFunc, observers)
		var started sync.WaitGroup
		started.Add(observers)
		for index := 0; index < observers; index++ {
			ctx, cancel := context.WithCancel(context.Background())
			cancels[index] = cancel
			go func() {
				started.Done()
				result, err := supervisor.Wait(ctx)
				results <- observed{result: result, err: err}
			}()
		}
		started.Wait()
		go process.releaseWait()
		for _, cancel := range cancels {
			cancel()
		}

		for index := 0; index < observers; index++ {
			select {
			case observed := <-results:
				completed := observed.result == expected && observed.err == nil
				cancelled := observed.result == (proctree.Result{}) && errors.Is(observed.err, context.Canceled)
				if !completed && !cancelled {
					t.Fatal("completion race returned neither process outcome nor caller cancellation")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("completion and cancellation race did not converge")
			}
		}
		waitStdioSignal(t, supervisor.Done(), "racing supervisor completion")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if result, err := supervisor.Wait(ctx); result != expected || err != nil || process.waitCalls.Load() != 1 {
			t.Fatal("completion race corrupted the final process outcome")
		}
	})

	t.Run("clean exit is stable without failure diagnostics", func(t *testing.T) {
		process := newGatedSupervisorProcess(proctree.Result{}, nil)
		t.Cleanup(process.releaseWait)
		sink := &supervisorDiagnosticSink{}
		supervisor, err := startStdioSupervisor(process, sink)
		if err != nil {
			t.Fatal(err)
		}
		waitStdioSignal(t, process.entered, "clean supervisor Process.Wait")
		process.releaseWait()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if result, err := supervisor.Wait(ctx); result != (proctree.Result{}) || err != nil {
			t.Fatal("clean process exit changed during supervision")
		}
		waitStdioSignal(t, supervisor.Done(), "clean supervisor completion")
		if process.waitCalls.Load() != 1 || len(sink.snapshot()) != 0 {
			t.Fatal("clean process exit repeated Wait or produced a failure diagnostic")
		}
	})

	t.Run("blocking diagnostics do not hide a published process outcome", func(t *testing.T) {
		expected := proctree.Result{ExitCode: 9}
		process := newGatedSupervisorProcess(expected, nil)
		t.Cleanup(process.releaseWait)
		sink := newBlockingSupervisorSink()
		t.Cleanup(sink.releaseAdd)
		supervisor, err := startStdioSupervisor(process, sink)
		if err != nil {
			t.Fatal(err)
		}
		waitStdioSignal(t, process.entered, "blocking-diagnostic Process.Wait")
		process.releaseWait()
		waitStdioSignal(t, sink.entered, "blocking diagnostic Add")

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if result, err := supervisor.Wait(ctx); result != expected || err != nil {
			t.Fatal("blocking diagnostic hid the published process outcome")
		}
		if result, err, ready := supervisor.Result(); !ready || result != expected || err != nil {
			t.Fatal("blocking diagnostic hid the non-blocking process outcome")
		}
		select {
		case <-supervisor.Done():
			t.Fatal("supervisor completed while its diagnostic was still blocked")
		default:
		}
		sink.releaseAdd()
		waitStdioSignal(t, supervisor.Done(), "blocking diagnostic completion")
	})

	t.Run("invalid dependencies fail before starting a goroutine", func(t *testing.T) {
		process := newGatedSupervisorProcess(proctree.Result{}, nil)
		if _, err := startStdioSupervisor(nil, &supervisorDiagnosticSink{}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("nil Process was accepted")
		}
		if _, err := startStdioSupervisor(process, nil); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("nil diagnostics sink was accepted")
		}
		var typedProcess *gatedSupervisorProcess
		if _, err := startStdioSupervisor(typedProcess, &supervisorDiagnosticSink{}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("typed nil Process was accepted")
		}
		var typedSink *supervisorDiagnosticSink
		if _, err := startStdioSupervisor(process, typedSink); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("typed nil diagnostics sink was accepted")
		}
		var missing *stdioSupervisor
		if _, err := missing.Wait(context.Background()); !errors.Is(err, ErrSupervisorUnavailable) {
			t.Fatal("nil supervisor Wait did not fail safely")
		}
		if _, err, ready := missing.Result(); !ready || !errors.Is(err, ErrSupervisorUnavailable) {
			t.Fatal("nil supervisor Result did not fail safely")
		}
		if process.waitCalls.Load() != 0 {
			t.Fatal("invalid supervisor construction called Process.Wait")
		}
	})
}

type gatedSupervisorProcess struct {
	result    proctree.Result
	err       error
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
	releaseMu sync.Once
	waitCalls atomic.Int64
}

func newGatedSupervisorProcess(result proctree.Result, err error) *gatedSupervisorProcess {
	return &gatedSupervisorProcess{result: result, err: err, entered: make(chan struct{}), release: make(chan struct{})}
}

func (*gatedSupervisorProcess) Pipes() proctree.Pipes {
	panic("supervisor must not inspect borrowed pipes")
}

func (*gatedSupervisorProcess) CloseStdin() error { panic("supervisor must not close stdin") }

func (process *gatedSupervisorProcess) Wait(ctx context.Context) (proctree.Result, error) {
	process.waitCalls.Add(1)
	process.enterOnce.Do(func() { close(process.entered) })
	select {
	case <-process.release:
		return process.result, process.err
	case <-ctx.Done():
		return proctree.Result{}, ctx.Err()
	}
}

func (*gatedSupervisorProcess) Terminate(context.Context) error {
	panic("supervisor must not terminate the process")
}

func (*gatedSupervisorProcess) Close(context.Context) error {
	panic("supervisor must not close the process")
}

func (process *gatedSupervisorProcess) releaseWait() {
	process.releaseMu.Do(func() { close(process.release) })
}

type supervisorDiagnosticSink struct {
	mu     sync.Mutex
	inputs []diagnostics.SanitizeInput
}

type blockingSupervisorSink struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newBlockingSupervisorSink() *blockingSupervisorSink {
	return &blockingSupervisorSink{entered: make(chan struct{}), release: make(chan struct{})}
}

func (sink *blockingSupervisorSink) Add(diagnostics.SanitizeInput) {
	sink.enteredOnce.Do(func() { close(sink.entered) })
	<-sink.release
}

func (sink *blockingSupervisorSink) releaseAdd() {
	sink.releaseOnce.Do(func() { close(sink.release) })
}

func (sink *supervisorDiagnosticSink) Add(input diagnostics.SanitizeInput) {
	sink.mu.Lock()
	sink.inputs = append(sink.inputs, input)
	sink.mu.Unlock()
}

func (sink *supervisorDiagnosticSink) snapshot() []diagnostics.SanitizeInput {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]diagnostics.SanitizeInput(nil), sink.inputs...)
}

var _ proctree.Process = (*gatedSupervisorProcess)(nil)
var _ diagnostics.BoundedSink = (*supervisorDiagnosticSink)(nil)
var _ diagnostics.BoundedSink = (*blockingSupervisorSink)(nil)
