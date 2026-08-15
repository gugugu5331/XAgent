package stdio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"xagent/internal/diagnostics"
	"xagent/internal/proctree"
	"xagent/internal/redact"
)

func TestStderrCanaryDoesNotReachDiagnostics(t *testing.T) {
	const canary = "XAGENT_MCP_SECRET_CANARY_20260802_7F31"
	bodyLine := "stderr-body=" + canary
	authorizationLine := "Authorization: Bearer " + canary
	apiKeyLine := "api_key=" + canary
	terminalMessage := "terminal reader failure " + canary
	firstHalf := canary[:len(canary)/2]
	secondHalf := canary[len(canary)/2:]

	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(canary)
	sink := newStderrTestSinkWithRedactor(t, redactor, 2)
	observer := &stderrObservingSink{
		next: sink,
		forbidden: []string{
			canary,
			firstHalf,
			secondHalf,
			bodyLine,
			authorizationLine,
			apiKeyLine,
			terminalMessage,
		},
	}
	data := []byte(strings.Join([]string{
		canary,
		bodyLine,
		authorizationLine,
		apiKeyLine,
	}, "\n"))
	reader := &chunkedStderrReader{
		data:     data,
		maxChunk: len(canary) / 2,
		terminal: errors.New(terminalMessage),
	}

	worker, err := startStderrWorker(reader, observer)
	if err != nil {
		t.Fatal("start stderr canary worker failed")
	}
	waitStdioSignal(t, worker.Done(), "stderr canary drain")
	if observer.rejected.Load() {
		t.Fatal("stderr worker passed canary material to diagnostics sink")
	}

	snapshot := sink.Snapshot()
	items := snapshot.Items()
	if len(items) != 2 {
		t.Fatalf("stderr canary diagnostic item count = %d, want 2", len(items))
	}
	for _, item := range items {
		rendered := fmt.Sprintf("%s %s %s %s", item.Diagnostic.Code, item.Diagnostic.Source,
			item.Diagnostic.Hint, item.Diagnostic.Message.Text())
		for _, forbidden := range observer.forbidden {
			if strings.Contains(rendered, forbidden) {
				t.Fatal("stderr diagnostic snapshot leaked canary material")
			}
		}
	}
}

func TestStderrCanaryTestOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("locate stderr canary test executable")
	}
	command := exec.Command(executable,
		"-test.run=^TestStderrCanaryDoesNotReachDiagnostics$",
		"-test.count=1",
		"-test.v",
	)
	output, childErr := command.CombinedOutput()
	value := string(output)
	const canary = "XAGENT_MCP_SECRET_CANARY_20260802_7F31"
	fragments := []string{
		canary,
		canary[:len(canary)/2],
		canary[len(canary)/2:],
	}
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			t.Fatal("stderr canary appeared in child test output")
		}
	}
	if childErr != nil {
		t.Fatal("stderr canary child test failed")
	}
}

func TestStderrFloodDoesNotBlockOrLeak(t *testing.T) {
	t.Run("blocked stderr tail does not block stdout and joins at EOF", func(t *testing.T) {
		const stderrBytes = 2 * 1024 * 1024
		stderr := newGatedStderrReader(stderrBytes, stderrReadBufferBytes, stderrReadBufferBytes)
		stdout := newFrameChunkReader([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`+"\n"), 7)
		stdin := &borrowedWriteSpy{}
		process := &recordingProcess{pipes: proctree.Pipes{Stdin: stdin, Stdout: stdout, Stderr: stderr}}
		workingDirectory, approvedPlan := stdioTestProtection(t)
		sink := newStderrTestSink(t, 4)
		config := validStdioTestConfig(
			workingDirectory,
			&recordingRunner{process: process},
			&recordingPlanFactory{plan: approvedPlan},
		)
		config.Lifecycle.Diagnostics = sink
		created := newStdioTestTransport(t, config)
		t.Cleanup(func() {
			stderr.releaseTail()
			_ = created.Close(context.Background())
		})

		startResult := make(chan error, 1)
		go func() { startResult <- created.Start(context.Background()) }()
		waitStdioSignal(t, stderr.blocked, "stderr drain blocked tail")
		select {
		case err := <-startResult:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("Transport.Start blocked on stderr drain")
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		event, err := created.Receive(ctx)
		if err != nil || string(event.Frame) != `{"jsonrpc":"2.0","id":1,"result":{}}` {
			t.Fatalf("Receive while stderr tail blocked = %q/%v", event.Frame, err)
		}

		stderr.releaseTail()
		waitStdioSignal(t, created.stderrDrain.Done(), "stderr EOF")
		if read := stderr.bytesRead(); read != stderrBytes {
			t.Fatalf("stderr bytes read = %d, want %d", read, stderrBytes)
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if process.closeCalls.Load() != 1 || stdin.closeCalls.Load() != 0 ||
			stdout.snapshot().closes != 0 || stderr.closeCalls.Load() != 0 {
			t.Fatalf("process/borrowed closes = %d/%d/%d/%d, want 1/0/0/0",
				process.closeCalls.Load(), stdin.closeCalls.Load(), stdout.snapshot().closes, stderr.closeCalls.Load())
		}
	})

	t.Run("bounded safe summary aggregates without accepting raw", func(t *testing.T) {
		const canary = "stderr-runtime-secret-canary-7f31"
		const staticSecret = "Authorization: Bearer stderr-static-secret"
		redactor := redact.NewRuntimeRedactor()
		redactor.RegisterSecret(canary)
		sink := newStderrTestSinkWithRedactor(t, redactor, 4)
		observer := &stderrObservingSink{
			next:      sink,
			forbidden: []string{canary, canary[:len(canary)/2], canary[len(canary)/2:], staticSecret, "stderr-static-secret"},
		}
		data := append([]byte(canary+"\n"+staticSecret+"\n\x1b[31m"), 0xff, 0xfe)
		data = append(data, []byte(strings.Repeat("stderr-tail-", 128))...)
		reader := &chunkedStderrReader{
			data:     data,
			maxChunk: len(canary) / 2,
			terminal: errors.New("reader failure " + canary),
		}

		worker, err := startStderrWorker(reader, observer)
		if err != nil {
			t.Fatal(err)
		}
		waitStdioSignal(t, worker.Done(), "safe stderr drain")
		second, err := startStderrWorker(&chunkedStderrReader{data: []byte("second stderr occurrence"), terminal: io.EOF}, observer)
		if err != nil {
			t.Fatal(err)
		}
		waitStdioSignal(t, second.Done(), "second safe stderr drain")
		if observer.rejected.Load() {
			t.Fatal("stderr worker passed raw stderr or reader error to diagnostics sink")
		}

		snapshot := sink.Snapshot()
		items := snapshot.Items()
		if len(items) != 2 {
			t.Fatalf("stderr diagnostic items = %d, want observed and read-failure summaries", len(items))
		}
		if items[0].Count != 2 || items[0].Diagnostic.Hint != "content_omitted" ||
			items[1].Count != 1 || items[1].Diagnostic.Hint != "read_failed" {
			t.Fatalf("stderr aggregate counts/hints = %#v", items)
		}
		if snapshot.Bytes() <= 0 || snapshot.Bytes() > 512 {
			t.Fatalf("stderr diagnostic bytes = %d, want bounded positive bytes", snapshot.Bytes())
		}
		for _, item := range items {
			rendered := fmt.Sprintf("%s %s %s %s", item.Diagnostic.Code, item.Diagnostic.Source,
				item.Diagnostic.Hint, item.Diagnostic.Message.Text())
			if !utf8.ValidString(rendered) || strings.ContainsAny(rendered, "\x1b\x00") {
				t.Fatal("stderr diagnostic is not terminal-safe UTF-8")
			}
			for _, secret := range observer.forbidden {
				if strings.Contains(rendered, secret) {
					t.Fatal("stderr diagnostic leaked secret fragment")
				}
			}
		}
	})

	t.Run("saturated sink drops summaries but keeps draining", func(t *testing.T) {
		sink := newStderrTestSink(t, 1)
		sink.Add(diagnostics.SanitizeInput{Code: "preexisting", Err: errors.New("safe")})
		const stderrBytes = 3 * 1024 * 1024
		reader := newGatedStderrReader(stderrBytes, stderrReadBufferBytes, -1)
		worker, err := startStderrWorker(reader, sink)
		if err != nil {
			t.Fatal(err)
		}
		waitStdioSignal(t, worker.Done(), "saturated stderr drain")
		if read := reader.bytesRead(); read != stderrBytes {
			t.Fatalf("stderr bytes read after sink saturation = %d, want %d", read, stderrBytes)
		}
		snapshot := sink.Snapshot()
		if len(snapshot.Items()) != 1 || snapshot.Items()[0].Diagnostic.Code != "preexisting" || snapshot.Dropped() == 0 {
			t.Fatalf("saturated stderr snapshot = items %#v dropped %d", snapshot.Items(), snapshot.Dropped())
		}
		if reader.closeCalls.Load() != 0 {
			t.Fatalf("stderr worker closed borrowed reader %d times", reader.closeCalls.Load())
		}
	})

	t.Run("empty reads and invalid counts terminate with a safe failure", func(t *testing.T) {
		for _, scenario := range []struct {
			name   string
			reader io.Reader
		}{
			{name: "empty reads", reader: emptyStderrReader{}},
			{name: "invalid count", reader: invalidCountStderrReader{}},
			{name: "negative count", reader: negativeCountStderrReader{}},
		} {
			t.Run(scenario.name, func(t *testing.T) {
				sink := newStderrTestSink(t, 2)
				worker, err := startStderrWorker(scenario.reader, sink)
				if err != nil {
					t.Fatal(err)
				}
				waitStdioSignal(t, worker.Done(), scenario.name)
				items := sink.Snapshot().Items()
				if len(items) != 1 || items[0].Diagnostic.Hint != "read_failed" || items[0].Count != 1 {
					t.Fatalf("%s diagnostics = %#v", scenario.name, items)
				}
			})
		}
	})

	t.Run("EOF contracts terminate without false read failures", func(t *testing.T) {
		emptySink := newStderrTestSink(t, 2)
		empty, err := startStderrWorker(strings.NewReader(""), emptySink)
		if err != nil {
			t.Fatal(err)
		}
		waitStdioSignal(t, empty.Done(), "empty stderr EOF")
		if items := emptySink.Snapshot().Items(); len(items) != 0 {
			t.Fatalf("empty stderr diagnostics = %#v, want none", items)
		}

		partialSink := newStderrTestSink(t, 2)
		partial, err := startStderrWorker(&dataAndEOFStderrReader{data: []byte("stderr")}, partialSink)
		if err != nil {
			t.Fatal(err)
		}
		waitStdioSignal(t, partial.Done(), "data and stderr EOF")
		items := partialSink.Snapshot().Items()
		if len(items) != 1 || items[0].Diagnostic.Hint != "content_omitted" || items[0].Count != 1 {
			t.Fatalf("data plus EOF diagnostics = %#v", items)
		}
	})
}

func newStderrTestSink(t *testing.T, maxItems int64) *diagnostics.Sink {
	t.Helper()
	return newStderrTestSinkWithRedactor(t, redact.NewRuntimeRedactor(), maxItems)
}

func newStderrTestSinkWithRedactor(t *testing.T, redactor *redact.RuntimeRedactor, maxItems int64) *diagnostics.Sink {
	t.Helper()
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      maxItems,
		MaxItemBytes:  256,
		MaxTotalBytes: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sink
}

type stderrObservingSink struct {
	next      diagnostics.BoundedSink
	forbidden []string
	rejected  atomic.Bool
}

func (sink *stderrObservingSink) Add(input diagnostics.SanitizeInput) {
	candidates := []string{input.Code, input.Source, input.Hint, string(input.Severity)}
	if input.Err != nil {
		candidates = append(candidates, input.Err.Error())
	}
	for _, candidate := range candidates {
		for _, forbidden := range sink.forbidden {
			if strings.Contains(candidate, forbidden) {
				sink.rejected.Store(true)
			}
		}
	}
	sink.next.Add(input)
}

type gatedStderrReader struct {
	total       int64
	maxChunk    int
	blockAt     int64
	blocked     chan struct{}
	release     chan struct{}
	blockOnce   sync.Once
	releaseOnce sync.Once
	closeCalls  atomic.Int64

	mu     sync.Mutex
	offset int64
}

func newGatedStderrReader(total int64, maxChunk int, blockAt int64) *gatedStderrReader {
	return &gatedStderrReader{
		total: total, maxChunk: maxChunk, blockAt: blockAt,
		blocked: make(chan struct{}), release: make(chan struct{}),
	}
}

func (reader *gatedStderrReader) Read(buffer []byte) (int, error) {
	reader.mu.Lock()
	if reader.offset >= reader.total {
		reader.mu.Unlock()
		return 0, io.EOF
	}
	shouldBlock := reader.blockAt >= 0 && reader.offset >= reader.blockAt
	if shouldBlock {
		reader.blockAt = -1
		reader.blockOnce.Do(func() { close(reader.blocked) })
		reader.mu.Unlock()
		<-reader.release
		reader.mu.Lock()
	}
	remaining := reader.total - reader.offset
	count := int64(len(buffer))
	if reader.maxChunk > 0 && count > int64(reader.maxChunk) {
		count = int64(reader.maxChunk)
	}
	if count > remaining {
		count = remaining
	}
	for index := int64(0); index < count; index++ {
		buffer[index] = byte('a' + (reader.offset+index)%26)
	}
	reader.offset += count
	reader.mu.Unlock()
	return int(count), nil
}

func (reader *gatedStderrReader) Close() error {
	reader.closeCalls.Add(1)
	return errors.New("borrowed stderr cannot be closed")
}

func (reader *gatedStderrReader) releaseTail() {
	reader.releaseOnce.Do(func() { close(reader.release) })
}

func (reader *gatedStderrReader) bytesRead() int64 {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.offset
}

type chunkedStderrReader struct {
	data     []byte
	offset   int
	maxChunk int
	terminal error
}

func (reader *chunkedStderrReader) Read(buffer []byte) (int, error) {
	if reader.offset == len(reader.data) {
		return 0, reader.terminal
	}
	count := len(reader.data) - reader.offset
	if reader.maxChunk > 0 && count > reader.maxChunk {
		count = reader.maxChunk
	}
	if count > len(buffer) {
		count = len(buffer)
	}
	copy(buffer, reader.data[reader.offset:reader.offset+count])
	reader.offset += count
	return count, nil
}

type emptyStderrReader struct{}

func (emptyStderrReader) Read([]byte) (int, error) { return 0, nil }

type invalidCountStderrReader struct{}

func (invalidCountStderrReader) Read(buffer []byte) (int, error) { return len(buffer) + 1, nil }

type negativeCountStderrReader struct{}

func (negativeCountStderrReader) Read([]byte) (int, error) { return -1, nil }

type dataAndEOFStderrReader struct {
	data []byte
}

func (reader *dataAndEOFStderrReader) Read(buffer []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, io.EOF
	}
	read := copy(buffer, reader.data)
	reader.data = nil
	return read, io.EOF
}
