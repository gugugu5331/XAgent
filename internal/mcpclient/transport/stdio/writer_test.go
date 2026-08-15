package stdio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcptransport "xagent/internal/mcpclient/transport"
	"xagent/internal/proctree"
)

func TestCancelledWriteDoesNotLeakGoroutine(t *testing.T) {
	t.Run("active blocked write cancellation returns and owner closes", func(t *testing.T) {
		stdin := newGatedStdin(4)
		closeRelease := make(chan struct{})
		process := newWriterTestProcess(stdin, closeRelease)
		created := newWriterTestTransport(t, process)
		t.Cleanup(func() {
			process.releaseClose()
			stdin.stopOwner()
			_ = created.Close(context.Background())
		})

		ctx, cancel := context.WithCancel(context.Background())
		sendResult := make(chan error, 1)
		go func() { sendResult <- created.Send(ctx, json.RawMessage(`{"id":1}`)) }()
		waitWriterSignal(t, stdin.entered, "blocked stdin Write")
		cancel()
		select {
		case err := <-sendResult:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled Send = %v, want context cancellation", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled Send waited for blocked owner cleanup")
		}
		waitWriterSignal(t, process.closeEntered, "Process.Close")
		if stdin.closeCalls.Load() != 0 {
			t.Fatal("writer directly closed borrowed stdin")
		}

		process.releaseClose()
		waitWriterSignal(t, created.writer.Done(), "writer shutdown")
		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if process.closeCalls.Load() != 1 || stdin.closeCalls.Load() != 0 || stdin.activeWrites() != 0 {
			t.Fatalf("process closes/borrowed closes/active writes = %d/%d/%d, want 1/0/0",
				process.closeCalls.Load(), stdin.closeCalls.Load(), stdin.activeWrites())
		}
		if err := created.Send(context.Background(), json.RawMessage(`{"id":2}`)); !errors.Is(err, mcptransport.ErrClosing) {
			t.Fatalf("Send after cancelled active write = %v, want closing", err)
		}
	})

	t.Run("concurrent sends use one writer and whole frames", func(t *testing.T) {
		const sends = 32
		stdin := newGatedStdin(sends + 1)
		process := newWriterTestProcess(stdin, nil)
		created := newWriterTestTransport(t, process)
		t.Cleanup(func() {
			stdin.stopOwner()
			_ = created.Close(context.Background())
		})

		start := make(chan struct{})
		results := make(chan error, sends)
		for index := 0; index < sends; index++ {
			index := index
			go func() {
				<-start
				results <- created.Send(context.Background(), json.RawMessage(fmt.Sprintf(`{"id":%d}`, index)))
			}()
		}
		close(start)
		for index := 0; index < sends; index++ {
			waitWriterSignal(t, stdin.entered, "serialized stdin Write")
			stdin.allowOne()
		}
		for index := 0; index < sends; index++ {
			select {
			case err := <-results:
				if err != nil {
					t.Fatalf("concurrent Send = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("concurrent Send did not converge")
			}
		}

		writes, maximum := stdin.snapshot()
		if len(writes) != sends || maximum != 1 {
			t.Fatalf("write count/max active = %d/%d, want %d/1", len(writes), maximum, sends)
		}
		seen := make(map[string]int, sends)
		for _, write := range writes {
			if len(write) == 0 || write[len(write)-1] != '\n' || strings.Count(string(write), "\n") != 1 ||
				!json.Valid(write[:len(write)-1]) {
				t.Fatalf("writer emitted invalid NDJSON frame %q", write)
			}
			seen[string(write)]++
		}
		for index := 0; index < sends; index++ {
			expected := fmt.Sprintf("{\"id\":%d}\n", index)
			if seen[expected] != 1 {
				t.Fatalf("frame %q write count = %d, want 1", expected, seen[expected])
			}
		}
		before := len(writes)
		if err := created.Send(context.Background(), json.RawMessage("{\n\"id\":99}")); !errors.Is(err, ErrWriteFailed) {
			t.Fatalf("multiline frame Send = %v, want fixed write failure", err)
		}
		if after, _ := stdin.snapshot(); len(after) != before {
			t.Fatal("invalid multiline frame reached stdin")
		}

		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitWriterSignal(t, created.writer.Done(), "idle writer shutdown")
		if process.closeCalls.Load() != 1 || stdin.closeCalls.Load() != 0 {
			t.Fatalf("process/borrowed closes = %d/%d, want 1/0", process.closeCalls.Load(), stdin.closeCalls.Load())
		}
	})

	t.Run("response budget does not reject an outbound request", func(t *testing.T) {
		stdin := newGatedStdin(1)
		process := newWriterTestProcess(stdin, nil)
		created := newWriterTestTransportWithResponseLimit(t, process, 1)
		t.Cleanup(func() {
			stdin.stopOwner()
			_ = created.Close(context.Background())
		})

		result := make(chan error, 1)
		go func() {
			result <- created.Send(context.Background(), json.RawMessage(`{"request":"larger-than-response-budget"}`))
		}()
		waitWriterSignal(t, stdin.entered, "outbound request with small response budget")
		stdin.allowOne()
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("outbound request with small response budget = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("outbound request with small response budget did not converge")
		}
	})

	t.Run("queued cancellations are skipped without aborting active write", func(t *testing.T) {
		stdin := newGatedStdin(2)
		var fatalCalls atomic.Int64
		worker, err := startWriterWorker(stdin, func() { fatalCalls.Add(1) })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			stdin.stopOwner()
			worker.Stop()
		})

		activeResult := make(chan error, 1)
		go func() { activeResult <- worker.Send(context.Background(), []byte(`{"active":true}`)) }()
		waitWriterSignal(t, stdin.entered, "active queued-cancellation Write")

		const waiting = writerInFlightCapacity + 8
		cancels := make([]context.CancelFunc, waiting)
		results := make(chan error, waiting)
		var started sync.WaitGroup
		started.Add(waiting)
		for index := 0; index < waiting; index++ {
			ctx, cancel := context.WithCancel(context.Background())
			cancels[index] = cancel
			go func(index int) {
				started.Done()
				results <- worker.Send(ctx, []byte(fmt.Sprintf(`{"queued":%d}`, index)))
			}(index)
		}
		started.Wait()
		waitWriterCondition(t, "full writer queue", func() bool { return len(worker.queue) == writerQueueCapacity })
		for _, cancel := range cancels {
			cancel()
		}
		for index := 0; index < waiting; index++ {
			select {
			case err := <-results:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("queued cancellation = %v, want context cancellation", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("queued cancellation did not converge")
			}
		}
		if fatalCalls.Load() != 0 {
			t.Fatal("queued-only cancellation aborted the shared writer")
		}

		stdin.allowOne()
		select {
		case err := <-activeResult:
			if err != nil {
				t.Fatalf("active write after queued cancellations = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("active write did not finish after queued cancellations")
		}
		waitWriterCondition(t, "cancelled queue drain", func() bool {
			return len(worker.queue) == 0 && len(worker.slots) == writerInFlightCapacity
		})
		worker.Stop()
		waitWriterSignal(t, worker.Done(), "queued-cancellation writer shutdown")
		writes, maximum := stdin.snapshot()
		if len(writes) != 1 || maximum != 1 || stdin.activeWrites() != 0 {
			t.Fatalf("writes/max/active after queued cancellation = %d/%d/%d, want 1/1/0",
				len(writes), maximum, stdin.activeWrites())
		}
	})

	t.Run("active cancellation aborts once and Stop preserves write success", func(t *testing.T) {
		cancelledStdin := newGatedStdin(1)
		var aborts atomic.Int64
		cancelled, err := startWriterWorker(cancelledStdin, func() {
			aborts.Add(1)
			cancelledStdin.stopOwner()
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancelledResult := make(chan error, 1)
		go func() { cancelledResult <- cancelled.Send(ctx, []byte(`{"cancelled":true}`)) }()
		waitWriterSignal(t, cancelledStdin.entered, "direct cancelled Write")
		cancel()
		select {
		case err := <-cancelledResult:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("direct active cancellation = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("direct active cancellation did not return")
		}
		waitWriterSignal(t, cancelled.Done(), "direct cancelled writer")
		if aborts.Load() != 1 {
			t.Fatalf("active cancellation abort callbacks = %d, want 1", aborts.Load())
		}

		successStdin := newGatedStdin(1)
		var successAborts atomic.Int64
		success, err := startWriterWorker(successStdin, func() { successAborts.Add(1) })
		if err != nil {
			t.Fatal(err)
		}
		successResult := make(chan error, 1)
		go func() { successResult <- success.Send(context.Background(), []byte(`{"success":true}`)) }()
		waitWriterSignal(t, successStdin.entered, "Stop-success Write")
		success.Stop()
		successStdin.allowOne()
		select {
		case err := <-successResult:
			if err != nil {
				t.Fatalf("successful Write racing Stop = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("successful Write racing Stop did not return")
		}
		waitWriterSignal(t, success.Done(), "Stop-success writer")
		if successAborts.Load() != 0 {
			t.Fatal("successful Write racing Stop triggered abort")
		}
	})

	t.Run("short writes complete and physical failure is sticky", func(t *testing.T) {
		short := &shortStdinWriter{maximum: 3}
		var shortFatal atomic.Int64
		worker, err := startWriterWorker(short, func() { shortFatal.Add(1) })
		if err != nil {
			t.Fatal(err)
		}
		shortCtx, cancelShort := context.WithTimeout(context.Background(), time.Second)
		defer cancelShort()
		if err := worker.Send(shortCtx, []byte(`{"short":true}`)); err != nil {
			t.Fatalf("short Write completion = %v", err)
		}
		worker.Stop()
		waitWriterSignal(t, worker.Done(), "short-write worker")
		if got := string(short.bytes()); got != "{\"short\":true}\n" || shortFatal.Load() != 0 {
			t.Fatalf("short Write output/fatal = %q/%d", got, shortFatal.Load())
		}

		const canary = "writer-physical-error-canary"
		failed := &failingStdinWriter{err: errors.New(canary)}
		var failedFatal atomic.Int64
		broken, err := startWriterWorker(failed, func() { failedFatal.Add(1) })
		if err != nil {
			t.Fatal(err)
		}
		failedCtx, cancelFailed := context.WithTimeout(context.Background(), time.Second)
		defer cancelFailed()
		writeErr := broken.Send(failedCtx, []byte(`{"fatal":true}`))
		if !errors.Is(writeErr, ErrWriteFailed) || strings.Contains(fmt.Sprintf("%+v", writeErr), canary) {
			t.Fatalf("physical write failure = %v, want fixed payload-free error", writeErr)
		}
		waitWriterSignal(t, broken.Done(), "failed writer")
		if err := broken.Send(context.Background(), []byte(`{"late":true}`)); !errors.Is(err, ErrWriteFailed) {
			t.Fatalf("Send after physical failure = %v, want sticky failure", err)
		}
		if failedFatal.Load() != 1 || failed.calls.Load() != 1 {
			t.Fatalf("fatal callbacks/writes = %d/%d, want 1/1", failedFatal.Load(), failed.calls.Load())
		}
	})

	t.Run("invalid Writer results fail with a fixed error", func(t *testing.T) {
		const canary = "invalid-writer-result-canary"
		frame := []byte("{}\n")
		tests := []struct {
			name  string
			write func([]byte) (int, error)
		}{
			{name: "zero progress", write: func([]byte) (int, error) { return 0, nil }},
			{name: "negative count", write: func([]byte) (int, error) { return -1, nil }},
			{name: "oversized count", write: func(data []byte) (int, error) { return len(data) + 1, nil }},
			{name: "complete count with error", write: func(data []byte) (int, error) {
				return len(data), errors.New(canary)
			}},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				calls := 0
				err := writeStdioFrame(writerFunc(func(data []byte) (int, error) {
					calls++
					return test.write(data)
				}), frame)
				if err != ErrWriteFailed || strings.Contains(fmt.Sprintf("%+v", err), canary) || calls != 1 {
					t.Fatalf("writeStdioFrame error/calls = %v/%d, want fixed payload-free failure/1", err, calls)
				}
			})
		}
	})

	t.Run("physical failure rejects an already queued request", func(t *testing.T) {
		stdin := newGatedFailWriter()
		var fatalCalls atomic.Int64
		worker, err := startWriterWorker(stdin, func() { fatalCalls.Add(1) })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			stdin.releaseFailure()
			worker.Stop()
		})

		activeResult := make(chan error, 1)
		queuedResult := make(chan error, 1)
		go func() { activeResult <- worker.Send(context.Background(), []byte(`{"active":true}`)) }()
		waitWriterSignal(t, stdin.entered, "failing active Write")
		go func() { queuedResult <- worker.Send(context.Background(), []byte(`{"queued":true}`)) }()
		waitWriterCondition(t, "request queued behind failing Write", func() bool {
			return len(worker.queue) == writerQueueCapacity
		})
		stdin.releaseFailure()

		for name, result := range map[string]<-chan error{
			"active": activeResult,
			"queued": queuedResult,
		} {
			select {
			case err := <-result:
				if err != ErrWriteFailed {
					t.Fatalf("%s request after physical failure = %v, want sticky failure", name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s request did not converge after physical failure", name)
			}
		}
		waitWriterSignal(t, worker.Done(), "queued-failure writer shutdown")
		if err := worker.Send(context.Background(), []byte(`{"late":true}`)); err != ErrWriteFailed {
			t.Fatalf("request after queued physical failure = %v, want sticky failure", err)
		}
		if fatalCalls.Load() != 1 || stdin.calls.Load() != 1 || len(worker.queue) != 0 ||
			len(worker.slots) != writerInFlightCapacity {
			t.Fatalf("fatal/writes/queue/slots = %d/%d/%d/%d, want 1/1/0/%d",
				fatalCalls.Load(), stdin.calls.Load(), len(worker.queue), len(worker.slots), writerInFlightCapacity)
		}
	})
}

func newWriterTestTransport(t *testing.T, process *writerTestProcess) *Transport {
	return newWriterTestTransportWithResponseLimit(t, process, 0)
}

func newWriterTestTransportWithResponseLimit(t *testing.T, process *writerTestProcess, maxResponseBytes int64) *Transport {
	t.Helper()
	workingDirectory, approvedPlan := stdioTestProtection(t)
	config := validStdioTestConfig(
		workingDirectory,
		&recordingRunner{process: process},
		&recordingPlanFactory{plan: approvedPlan},
	)
	config.MaxResponseBytes = maxResponseBytes
	created := newStdioTestTransport(t, config)
	if err := created.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return created
}

type gatedStdin struct {
	entered      chan struct{}
	permits      chan struct{}
	ownerStopped chan struct{}
	ownerOnce    sync.Once
	closeCalls   atomic.Int64

	mu        sync.Mutex
	writes    [][]byte
	active    int
	maxActive int
}

func newGatedStdin(events int) *gatedStdin {
	return &gatedStdin{
		entered: make(chan struct{}, events), permits: make(chan struct{}, events), ownerStopped: make(chan struct{}),
	}
}

func (writer *gatedStdin) Write(data []byte) (int, error) {
	copyOfData := append([]byte(nil), data...)
	writer.mu.Lock()
	writer.writes = append(writer.writes, copyOfData)
	writer.active++
	if writer.active > writer.maxActive {
		writer.maxActive = writer.active
	}
	writer.mu.Unlock()
	writer.entered <- struct{}{}

	var err error
	select {
	case <-writer.permits:
	case <-writer.ownerStopped:
		err = errors.New("owner stdin stopped")
	}
	writer.mu.Lock()
	writer.active--
	writer.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func (writer *gatedStdin) Close() error {
	writer.closeCalls.Add(1)
	return errors.New("borrowed stdin cannot be closed")
}

func (writer *gatedStdin) allowOne() { writer.permits <- struct{}{} }

func (writer *gatedStdin) stopOwner() { writer.ownerOnce.Do(func() { close(writer.ownerStopped) }) }

func (writer *gatedStdin) snapshot() ([][]byte, int) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writes := make([][]byte, len(writer.writes))
	for index := range writer.writes {
		writes[index] = append([]byte(nil), writer.writes[index]...)
	}
	return writes, writer.maxActive
}

func (writer *gatedStdin) activeWrites() int {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.active
}

type writerTestProcess struct {
	pipes        proctree.Pipes
	stdin        *gatedStdin
	closeRelease chan struct{}
	closeEntered chan struct{}
	closeCalls   atomic.Int64
	closeOnce    sync.Once
	releaseOnce  sync.Once
}

func newWriterTestProcess(stdin *gatedStdin, closeRelease chan struct{}) *writerTestProcess {
	return &writerTestProcess{
		pipes: proctree.Pipes{Stdin: stdin, Stdout: &borrowedReadSpy{}, Stderr: &borrowedReadSpy{}},
		stdin: stdin, closeRelease: closeRelease, closeEntered: make(chan struct{}),
	}
}

func (process *writerTestProcess) Pipes() proctree.Pipes { return process.pipes }

func (*writerTestProcess) CloseStdin() error { return nil }

func (*writerTestProcess) Wait(context.Context) (proctree.Result, error) {
	return proctree.Result{}, nil
}

func (*writerTestProcess) Terminate(context.Context) error { return nil }

func (process *writerTestProcess) Close(context.Context) error {
	process.closeCalls.Add(1)
	process.closeOnce.Do(func() { close(process.closeEntered) })
	if process.closeRelease != nil {
		<-process.closeRelease
	}
	process.stdin.stopOwner()
	return nil
}

func (process *writerTestProcess) releaseClose() {
	if process.closeRelease != nil {
		process.releaseOnce.Do(func() { close(process.closeRelease) })
	}
}

type shortStdinWriter struct {
	maximum int
	mu      sync.Mutex
	data    []byte
}

func (writer *shortStdinWriter) Write(data []byte) (int, error) {
	count := len(data)
	if count > writer.maximum {
		count = writer.maximum
	}
	writer.mu.Lock()
	writer.data = append(writer.data, data[:count]...)
	writer.mu.Unlock()
	return count, nil
}

func (writer *shortStdinWriter) bytes() []byte {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]byte(nil), writer.data...)
}

type failingStdinWriter struct {
	err   error
	calls atomic.Int64
}

type writerFunc func([]byte) (int, error)

func (write writerFunc) Write(data []byte) (int, error) { return write(data) }

type gatedFailWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int64
}

func newGatedFailWriter() *gatedFailWriter {
	return &gatedFailWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (writer *gatedFailWriter) Write([]byte) (int, error) {
	writer.calls.Add(1)
	writer.entered <- struct{}{}
	<-writer.release
	return 0, errors.New("injected physical failure")
}

func (writer *gatedFailWriter) releaseFailure() {
	writer.once.Do(func() { close(writer.release) })
}

func (writer *failingStdinWriter) Write(data []byte) (int, error) {
	writer.calls.Add(1)
	return len(data) / 2, writer.err
}

func waitWriterSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitWriterCondition(t *testing.T, name string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", name)
		}
		runtime.Gosched()
	}
}

var _ io.WriteCloser = (*gatedStdin)(nil)
