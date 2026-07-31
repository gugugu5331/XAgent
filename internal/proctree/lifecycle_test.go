package proctree

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/safefs"
)

func TestProtectionPlanRejectsZeroValue(t *testing.T) {
	if (ProtectionPlan{}).valid() {
		t.Fatal("zero protection plan was valid")
	}
	if _, err := NewProtectionPlan(nil, nil); err == nil {
		t.Fatal("empty protection plan was accepted")
	}
	project := bootstrapTestRoot(t)
	defer project.Close()
	scratch := bootstrapTestRoot(t)
	plan, err := NewProtectionPlan([]*safefs.Root{project}, scratch)
	if err != nil || !plan.valid() {
		t.Fatal("opened roots did not produce a valid protection plan")
	}
	if err := scratch.Close(); err != nil {
		t.Fatal("close scratch failed")
	}
	if plan.valid() {
		t.Fatal("plan remained valid after its scratch root closed")
	}
}

func TestStartErrorNeverClaimsTargetStarted(t *testing.T) {
	for _, code := range []string{"protected_exec_unavailable", "process_start_failed", ""} {
		err := newStartError(code)
		if err.TargetStarted {
			t.Fatal("pre-exec start error claimed target was started")
		}
		if err.Error() == "" {
			t.Fatal("start error had an empty safe summary")
		}
	}
}

func TestProcessCloseIsIdempotentAndContinuesAfterWaiterTimeout(t *testing.T) {
	controller := newFakeController()
	sink := &recordingSink{}
	process, err := newManagedProcess(controller, Options{CleanupTimeout: time.Second, Diagnostics: sink})
	if err != nil {
		t.Fatal("create managed process failed")
	}
	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if _, err := process.Wait(waitCtx); !errors.Is(err, context.Canceled) {
		t.Fatal("caller wait cancellation was not returned")
	}
	closeCtx, cancelClose := context.WithCancel(context.Background())
	cancelClose()
	if err := process.Close(closeCtx); !errors.Is(err, context.Canceled) {
		t.Fatal("caller close cancellation was not returned")
	}
	controller.finish(Result{ExitCode: 0}, nil)
	if err := process.Close(context.Background()); err != nil {
		t.Fatal("background cleanup did not continue after caller timeout")
	}
	if err := process.Close(context.Background()); err != nil {
		t.Fatal("repeated close changed the final result")
	}
	if controller.waitCalls != 1 || sink.count() != 0 {
		t.Fatal("wait ownership or clean close diagnostics were incorrect")
	}
}

func TestProcessCleanupTimeoutForcesCloseAndDiagnosesOnce(t *testing.T) {
	controller := newFakeController()
	sink := &recordingSink{}
	process, err := newManagedProcess(controller, Options{CleanupTimeout: 20 * time.Millisecond, Diagnostics: sink})
	if err != nil {
		t.Fatal("create managed process failed")
	}
	first := process.Close(context.Background())
	second := process.Close(context.Background())
	if first == nil || second == nil || first.Error() != second.Error() {
		t.Fatal("cleanup timeout did not return one stable final result")
	}
	if controller.forceCalls != 1 || controller.killCalls != 1 || controller.closePipeCalls != 1 {
		t.Fatal("cleanup timeout did not execute the forced close state machine once")
	}
	if sink.count() != 1 {
		t.Fatal("cleanup timeout diagnostic count was not exactly one")
	}
}

func bootstrapTestRoot(t *testing.T) *safefs.Root {
	t.Helper()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "anchor"), nil, 0o600); err != nil {
		t.Fatal("create root anchor failed")
	}
	result, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap test root failed")
	}
	return result.Root
}

type recordingSink struct {
	mu    sync.Mutex
	items []diagnostics.SanitizeInput
}

func (s *recordingSink) Add(input diagnostics.SanitizeInput) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, input)
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

type fakeController struct {
	finishOnce sync.Once
	waitGate   chan waitResult

	waitCalls      int
	killCalls      int
	forceCalls     int
	closePipeCalls int
}

func newFakeController() *fakeController {
	return &fakeController{waitGate: make(chan waitResult, 1)}
}

func (c *fakeController) Pipes() Pipes {
	return Pipes{Stdin: nopWriteCloser{Writer: io.Discard}, Stdout: io.NopCloser(&emptyReader{}), Stderr: io.NopCloser(&emptyReader{})}
}

func (c *fakeController) StopWrites() {}

func (c *fakeController) TerminateTree() error { return nil }

func (c *fakeController) KillTree() error {
	c.killCalls++
	return nil
}

func (c *fakeController) Wait() (Result, error) {
	c.waitCalls++
	result := <-c.waitGate
	return result.result, result.err
}

func (c *fakeController) ClosePipes() { c.closePipeCalls++ }

func (c *fakeController) ForceClose() {
	c.forceCalls++
	c.finish(Result{}, errProcessCleanupTimeout)
}

func (c *fakeController) finish(result Result, err error) {
	c.finishOnce.Do(func() { c.waitGate <- waitResult{result: result, err: err} })
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
