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

func TestPipeOwnership(t *testing.T) {
	controller := newFakeController()
	process, err := newManagedProcess(controller, Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}})
	if err != nil {
		t.Fatal("create managed process failed")
	}
	first := process.Pipes()
	second := process.Pipes()
	if first.Stdin != second.Stdin || first.Stdout != second.Stdout || first.Stderr != second.Stderr {
		t.Fatal("Pipes returned different borrowed handles")
	}
	if first.Stdin == controller.ownerPipes.Stdin || first.Stdout == controller.ownerPipes.Stdout || first.Stderr == controller.ownerPipes.Stderr {
		t.Fatal("Pipes exposed an owner handle")
	}
	if !errors.Is(first.Stdin.Close(), errBorrowedPipeClose) ||
		!errors.Is(first.Stdout.Close(), errBorrowedPipeClose) ||
		!errors.Is(first.Stderr.Close(), errBorrowedPipeClose) {
		t.Fatal("borrowed pipe close did not fail safely")
	}
	if controller.ownerCloseCount() != 0 {
		t.Fatal("borrowed pipe close reached an owner handle")
	}
	controller.finish(Result{}, nil)
	if err := process.Close(context.Background()); err != nil {
		t.Fatal("process close failed")
	}
	if controller.ownerCloseCount() != 3 {
		t.Fatal("process did not close all owner handles exactly once")
	}
}

func TestProcessCloseStdinSignalsEOFWithoutClosingBorrowedPipes(t *testing.T) {
	controller := newFakeController()
	controller.closeStdinOnStop = true
	process, err := newManagedProcess(controller, Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}})
	if err != nil {
		t.Fatal("create managed process failed")
	}
	if !errors.Is(process.Pipes().Stdin.Close(), errBorrowedPipeClose) {
		t.Fatal("borrowed stdin close reached its owner")
	}
	if err := process.CloseStdin(); err != nil {
		t.Fatal("close process stdin failed")
	}
	if err := process.CloseStdin(); err != nil {
		t.Fatal("repeated close process stdin failed")
	}
	if controller.stdin.closeCount() != 1 || controller.stdout.closeCount() != 0 || controller.stderr.closeCount() != 0 {
		t.Fatal("CloseStdin did not close only the owner stdin exactly once")
	}
	if _, err := process.Pipes().Stdin.Write(nil); !errors.Is(err, errPipeStopped) {
		t.Fatal("CloseStdin accepted a new borrowed write")
	}
	controller.finish(Result{}, nil)
	if err := process.Close(context.Background()); err != nil {
		t.Fatal("close process after stdin EOF failed")
	}
	if controller.ownerCloseCount() != 3 {
		t.Fatal("terminal cleanup did not close every owner pipe exactly once")
	}
}

func TestBlockedWriteCloseOrder(t *testing.T) {
	controller := newFakeController()
	controller.finishOnTerminate = true
	process, err := newManagedProcess(controller, Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}})
	if err != nil {
		t.Fatal("create managed process failed")
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := process.Pipes().Stdin.Write([]byte("blocked"))
		controller.record("write_done")
		writeDone <- err
	}()
	select {
	case <-controller.stdin.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write did not block in owner pipe")
	}
	if err := process.Close(context.Background()); err != nil {
		t.Fatal("process close failed")
	}
	select {
	case err := <-writeDone:
		if !errors.Is(err, errOwnerPipeClosed) {
			t.Fatal("blocked write did not end through owner pipe close")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked I/O did not finish after process close")
	}
	if _, err := process.Pipes().Stdin.Write(nil); !errors.Is(err, errPipeStopped) {
		t.Fatal("new write was accepted after close began")
	}
	controller.assertOrder(t, "stop_writes", "terminate", "reap", "close_pipes", "write_done")
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
	ownerPipes Pipes
	stdin      *blockingWriteCloser
	stdout     *trackedReadCloser
	stderr     *trackedReadCloser
	stdinOnce  sync.Once

	mu                sync.Mutex
	events            []string
	finishOnTerminate bool

	waitCalls        int
	killCalls        int
	forceCalls       int
	closePipeCalls   int
	closeStdinOnStop bool
}

func newFakeController() *fakeController {
	stdin := newBlockingWriteCloser()
	stdout := &trackedReadCloser{reader: &emptyReader{}}
	stderr := &trackedReadCloser{reader: &emptyReader{}}
	return &fakeController{
		waitGate: make(chan waitResult, 1),
		ownerPipes: Pipes{
			Stdin:  stdin,
			Stdout: stdout,
			Stderr: stderr,
		},
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
	}
}

func (c *fakeController) Pipes() Pipes {
	return c.ownerPipes
}

func (c *fakeController) StopWrites() {
	c.record("stop_writes")
	if c.closeStdinOnStop {
		c.closeStdin()
	}
}

func (c *fakeController) TerminateTree() error {
	c.record("terminate")
	if c.finishOnTerminate {
		c.finish(Result{}, nil)
	}
	return nil
}

func (c *fakeController) KillTree() error {
	c.killCalls++
	return nil
}

func (c *fakeController) Wait() (Result, error) {
	c.waitCalls++
	result := <-c.waitGate
	c.record("reap")
	return result.result, result.err
}

func (c *fakeController) ClosePipes() {
	c.closePipeCalls++
	c.record("close_pipes")
	c.closeStdin()
	_ = c.ownerPipes.Stdout.Close()
	_ = c.ownerPipes.Stderr.Close()
}

func (c *fakeController) closeStdin() {
	c.stdinOnce.Do(func() { _ = c.ownerPipes.Stdin.Close() })
}

func (c *fakeController) ForceClose() {
	c.forceCalls++
	c.finish(Result{}, errProcessCleanupTimeout)
}

func (c *fakeController) finish(result Result, err error) {
	c.finishOnce.Do(func() { c.waitGate <- waitResult{result: result, err: err} })
}

func (c *fakeController) record(event string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *fakeController) ownerCloseCount() int {
	return c.stdin.closeCount() + c.stdout.closeCount() + c.stderr.closeCount()
}

func (c *fakeController) assertOrder(t *testing.T, expected ...string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	position := 0
	for _, event := range c.events {
		if position < len(expected) && event == expected[position] {
			position++
		}
	}
	if position != len(expected) {
		t.Fatalf("close order mismatch: got %v, want subsequence %v", c.events, expected)
	}
}

var errOwnerPipeClosed = errors.New("owner pipe closed")

type blockingWriteCloser struct {
	writeStarted chan struct{}
	closed       chan struct{}
	startOnce    sync.Once
	closeOnce    sync.Once
	mu           sync.Mutex
	closes       int
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{writeStarted: make(chan struct{}), closed: make(chan struct{})}
}

func (w *blockingWriteCloser) Write([]byte) (int, error) {
	w.startOnce.Do(func() { close(w.writeStarted) })
	<-w.closed
	return 0, errOwnerPipeClosed
}

func (w *blockingWriteCloser) Close() error {
	w.mu.Lock()
	w.closes++
	w.mu.Unlock()
	w.closeOnce.Do(func() { close(w.closed) })
	return nil
}

func (w *blockingWriteCloser) closeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closes
}

type trackedReadCloser struct {
	reader io.Reader
	mu     sync.Mutex
	closes int
}

func (r *trackedReadCloser) Read(data []byte) (int, error) { return r.reader.Read(data) }

func (r *trackedReadCloser) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closes++
	return nil
}

func (r *trackedReadCloser) closeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closes
}

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
