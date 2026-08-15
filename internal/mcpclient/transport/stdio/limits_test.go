package stdio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"xagent/internal/budget"
	mcptransport "xagent/internal/mcpclient/transport"
	"xagent/internal/proctree"
)

func TestOversizedFrameFailsSharedTransport(t *testing.T) {
	t.Run("exact raw cap is accepted before parsing", func(t *testing.T) {
		const limit int64 = 64
		line := exactJSONFrameLine(t, int(limit), "accepted")
		reader := newFrameChunkReader(line, 5)
		created, _, _ := newFramingTestTransport(t, reader, limit)

		event, err := created.Receive(context.Background())
		if err != nil {
			t.Fatalf("Receive exact-cap frame: %v", err)
		}
		if got, want := string(event.Frame), string(line[:len(line)-1]); got != want {
			t.Fatalf("exact-cap frame = %q, want %q", got, want)
		}
		if used := created.framing.limits.raw.counter.Snapshot().Used(budget.Bytes); used != limit {
			t.Fatalf("raw bytes used = %d, want %d", used, limit)
		}
		if used := created.framing.limits.events.counter.Snapshot().Used(budget.Items); used != 1 {
			t.Fatalf("events used = %d, want 1", used)
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cap plus one is fatal sticky and owner closed", func(t *testing.T) {
		const limit int64 = 64
		const canary = "oversized-frame-secret-canary"
		line := exactJSONFrameLine(t, int(limit+1), canary)
		suffix := []byte(`{"jsonrpc":"2.0","id":2,"result":{}}` + "\n")
		reader := newFrameChunkReader(append(append([]byte(nil), line...), suffix...), 7)
		created, process, pipes := newFramingTestTransport(t, reader, limit)

		event, receiveErr := created.Receive(context.Background())
		assertFatalFrameLimit(t, event.Frame, receiveErr, limit, limit+1, canary)
		before := reader.snapshot()
		if before.bytes != limit+1 || before.bytes >= int64(len(line)+len(suffix)) {
			t.Fatalf("bytes read at fatal = %d, want %d and before suffix", before.bytes, limit+1)
		}
		if state := created.state.State(); state != mcptransport.TransportStateRunning {
			t.Fatalf("fatal Receive changed transport state to %s; Connection must own failure", state)
		}
		if process.closeCalls.Load() != 0 {
			t.Fatalf("fatal Receive closed Process %d times before Connection ownership", process.closeCalls.Load())
		}

		secondEvent, secondErr := created.Receive(context.Background())
		assertFatalFrameLimit(t, secondEvent.Frame, secondErr, limit, limit+1, canary)
		if after := reader.snapshot(); after != before {
			t.Fatalf("sticky fatal read stdout again: before=%#v after=%#v", before, after)
		}

		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if process.closeCalls.Load() != 1 || pipes.stdin.closeCalls.Load() != 0 ||
			pipes.stdout.snapshot().closes != 0 || pipes.stderr.closeCalls.Load() != 0 {
			t.Fatalf("process/stdin/stdout/stderr closes = %d/%d/%d/%d, want 1/0/0/0",
				process.closeCalls.Load(), pipes.stdin.closeCalls.Load(), pipes.stdout.snapshot().closes, pipes.stderr.closeCalls.Load())
		}
	})

	t.Run("fragmented unknown length stops before remote suffix", func(t *testing.T) {
		const limit int64 = 64
		const canary = "unread-remote-suffix-canary"
		payload := []byte(strings.Repeat("x", 4*int(stdioFrameReadBufferBytes)) + canary + "\n")
		reader := newFrameChunkReader(payload, 3)
		created, _, _ := newFramingTestTransport(t, reader, limit)

		event, err := created.Receive(context.Background())
		assertFatalFrameLimitExceeded(t, event.Frame, err, limit, canary)
		snapshot := reader.snapshot()
		canaryOffset := int64(bytes.Index(payload, []byte(canary)))
		if snapshot.bytes > stdioFrameReadBufferBytes+3 || snapshot.bytes >= canaryOffset {
			t.Fatalf("fragmented fatal read through remote suffix: bytes=%d canary-offset=%d",
				snapshot.bytes, canaryOffset)
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("greedy reader prefetch stays within one fixed buffer", func(t *testing.T) {
		const limit = stdioFrameReadBufferBytes
		const canary = "greedy-reader-unread-canary"
		payload := []byte(strings.Repeat("x", 3*int(stdioFrameReadBufferBytes)) + canary + "\n")
		reader := newFrameChunkReader(payload, 0)
		created, _, _ := newFramingTestTransport(t, reader, limit)

		event, err := created.Receive(context.Background())
		assertFatalFrameLimitExceeded(t, event.Frame, err, limit, canary)
		snapshot := reader.snapshot()
		canaryOffset := int64(bytes.Index(payload, []byte(canary)))
		if snapshot.bytes > limit+stdioFrameReadBufferBytes || snapshot.bytes >= canaryOffset {
			t.Fatalf("greedy reader exceeded bounded prefetch: bytes=%d bound=%d canary-offset=%d",
				snapshot.bytes, limit+stdioFrameReadBufferBytes, canaryOffset)
		}
		before := reader.snapshot()
		_, _ = created.Receive(context.Background())
		if after := reader.snapshot(); after != before {
			t.Fatalf("greedy sticky fatal read again: before=%#v after=%#v", before, after)
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("multiple frames share cumulative byte budget", func(t *testing.T) {
		const limit int64 = 96
		first := exactJSONFrameLine(t, 60, "first")
		second := exactJSONFrameLine(t, 60, "second")
		reader := newFrameChunkReader(append(append([]byte(nil), first...), second...), len(first))
		created, _, _ := newFramingTestTransport(t, reader, limit)

		if event, err := created.Receive(context.Background()); err != nil || string(event.Frame) != string(first[:len(first)-1]) {
			t.Fatalf("first cumulative frame = %q/%v", event.Frame, err)
		}
		event, err := created.Receive(context.Background())
		var limitErr *budget.LimitError
		if len(event.Frame) != 0 || !errors.Is(err, ErrFatalFraming) || !errors.As(err, &limitErr) {
			t.Fatalf("second cumulative frame = %q/%T %v, want fatal byte limit", event.Frame, err, err)
		}
		if limitErr.Scope != string(budget.MCPMaxResponseBytes) || limitErr.Dimension != budget.Bytes ||
			limitErr.Limit != limit || limitErr.Observed != int64(len(first)+len(second)) {
			t.Fatalf("cumulative limit metadata = %#v", limitErr)
		}
		if used := created.framing.limits.raw.counter.Snapshot().Used(budget.Bytes); used != int64(len(first)) {
			t.Fatalf("failed cumulative reservation changed used bytes to %d", used)
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("event count is cumulative and failed reservation is stable", func(t *testing.T) {
		first := exactJSONFrameLine(t, 40, "one")
		second := exactJSONFrameLine(t, 40, "two")
		reader := newFrameChunkReader(append(append([]byte(nil), first...), second...), len(first))
		created, _, _ := newFramingTestTransport(t, reader, 128)
		for index, expected := range [][]byte{first[:len(first)-1], second[:len(second)-1]} {
			event, err := created.Receive(context.Background())
			if err != nil || !bytes.Equal(event.Frame, expected) {
				t.Fatalf("successful event %d = %q/%v", index+1, event.Frame, err)
			}
		}
		if used := created.framing.limits.events.counter.Snapshot().Used(budget.Items); used != 2 {
			t.Fatalf("decoder event count = %d, want 2", used)
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}

		limits, err := newFramingLimits(2)
		if err != nil {
			t.Fatal(err)
		}
		for index := 0; index < 2; index++ {
			if err := limits.consumeEvent(); err != nil {
				t.Fatalf("consume event %d: %v", index+1, err)
			}
		}
		err = limits.consumeEvent()
		var limitErr *budget.LimitError
		if !errors.As(err, &limitErr) || limitErr.Scope != string(budget.MCPMaxResponseBytes) ||
			limitErr.Dimension != budget.Items || limitErr.Limit != 2 || limitErr.Observed != 3 {
			t.Fatalf("event limit error = %#v/%v", limitErr, err)
		}
		if used := limits.events.counter.Snapshot().Used(budget.Items); used != 2 {
			t.Fatalf("failed event reservation changed used count to %d", used)
		}
	})

	t.Run("malformed complete frame consumes event before sticky fatal", func(t *testing.T) {
		const limit int64 = 256
		const canary = "malformed-frame-secret-canary"
		malformed := []byte(`{"value":"` + canary + `"` + "\n")
		valid := []byte(`{"jsonrpc":"2.0","id":3,"result":{}}` + "\n")
		reader := newFrameChunkReader(append(append([]byte(nil), malformed...), valid...), len(malformed))
		created, _, _ := newFramingTestTransport(t, reader, limit)

		event, err := created.Receive(context.Background())
		assertFatalWithoutPayload(t, event.Frame, err, canary)
		if used := created.framing.limits.events.counter.Snapshot().Used(budget.Items); used != 1 {
			t.Fatalf("malformed frame event count = %d, want 1", used)
		}
		before := reader.snapshot()
		if _, err := created.Receive(context.Background()); !errors.Is(err, ErrFatalFraming) {
			t.Fatalf("second malformed Receive = %v, want sticky fatal", err)
		}
		if after := reader.snapshot(); after != before {
			t.Fatalf("malformed sticky fatal read valid suffix: before=%#v after=%#v", before, after)
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("invalid UTF-8 is a fatal malformed frame", func(t *testing.T) {
		line := append([]byte(`{"value":"`), 0xff)
		line = append(line, []byte(`"}`+"\n")...)
		reader := newFrameChunkReader(line, 2)
		created, _, _ := newFramingTestTransport(t, reader, 64)

		event, err := created.Receive(context.Background())
		if len(event.Frame) != 0 || !errors.Is(err, ErrFatalFraming) || !errors.Is(err, errFrameMalformed) {
			t.Fatalf("invalid UTF-8 frame = %q/%T %v, want malformed fatal", event.Frame, err, err)
		}
		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unterminated and clean EOF are fatal", func(t *testing.T) {
		for _, scenario := range []struct {
			name string
			data []byte
		}{
			{name: "unterminated", data: []byte(`{"jsonrpc":"2.0"}`)},
			{name: "clean EOF"},
		} {
			t.Run(scenario.name, func(t *testing.T) {
				reader := newFrameChunkReader(scenario.data, 3)
				created, _, _ := newFramingTestTransport(t, reader, 64)
				event, err := created.Receive(context.Background())
				if len(event.Frame) != 0 || !errors.Is(err, ErrFatalFraming) {
					t.Fatalf("EOF frame = %q/%v, want fatal", event.Frame, err)
				}
				before := reader.snapshot()
				_, _ = created.Receive(context.Background())
				if after := reader.snapshot(); after != before {
					t.Fatalf("EOF sticky fatal read again: before=%#v after=%#v", before, after)
				}
				if err := created.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
}

func newFramingTestTransport(t *testing.T, stdout *frameChunkReader, limit int64) (*Transport, *recordingProcess, framingBorrowedPipes) {
	t.Helper()
	workingDirectory, approvedPlan := stdioTestProtection(t)
	pipes := framingBorrowedPipes{stdin: &borrowedWriteSpy{}, stdout: stdout, stderr: &borrowedReadSpy{}}
	process := &recordingProcess{pipes: proctree.Pipes{Stdin: pipes.stdin, Stdout: stdout, Stderr: pipes.stderr}}
	config := validStdioTestConfig(
		workingDirectory,
		&recordingRunner{process: process},
		&recordingPlanFactory{plan: approvedPlan},
	)
	config.MaxResponseBytes = limit
	created := newStdioTestTransport(t, config)
	if err := created.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = created.Close(context.Background()) })
	return created, process, pipes
}

type framingBorrowedPipes struct {
	stdin  *borrowedWriteSpy
	stdout *frameChunkReader
	stderr *borrowedReadSpy
}

func exactJSONFrameLine(t *testing.T, totalBytes int, label string) []byte {
	t.Helper()
	prefix := `{"value":"` + label + `:`
	suffix := `"}` + "\n"
	padding := totalBytes - len(prefix) - len(suffix)
	if padding < 0 {
		t.Fatalf("frame size %d is too small for label %q", totalBytes, label)
	}
	return []byte(prefix + strings.Repeat("x", padding) + suffix)
}

func assertFatalFrameLimit(t *testing.T, frame []byte, err error, limit, observed int64, canary string) {
	t.Helper()
	if len(frame) != 0 || !errors.Is(err, ErrFatalFraming) {
		t.Fatalf("oversized frame = %q/%T %v, want fatal framing error", frame, err, err)
	}
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) || limitErr.Scope != string(budget.MCPMaxResponseBytes) ||
		limitErr.Dimension != budget.Bytes || limitErr.Limit != limit || limitErr.Observed != observed {
		t.Fatalf("oversized frame limit metadata = %#v", limitErr)
	}
	assertFatalWithoutPayload(t, frame, err, canary)
}

func assertFatalFrameLimitExceeded(t *testing.T, frame []byte, err error, limit int64, canary string) {
	t.Helper()
	if len(frame) != 0 || !errors.Is(err, ErrFatalFraming) {
		t.Fatalf("oversized frame = %q/%T %v, want fatal framing error", frame, err, err)
	}
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) || limitErr.Scope != string(budget.MCPMaxResponseBytes) ||
		limitErr.Dimension != budget.Bytes || limitErr.Limit != limit || limitErr.Observed <= limit {
		t.Fatalf("oversized frame limit metadata = %#v", limitErr)
	}
	assertFatalWithoutPayload(t, frame, err, canary)
}

func assertFatalWithoutPayload(t *testing.T, frame []byte, err error, canary string) {
	t.Helper()
	if len(frame) != 0 || !errors.Is(err, ErrFatalFraming) {
		t.Fatalf("fatal frame = %q/%T %v", frame, err, err)
	}
	for _, rendered := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(rendered, canary) {
			t.Fatalf("fatal framing error exposed payload in %q", rendered)
		}
	}
}

type frameReaderSnapshot struct {
	reads  int64
	bytes  int64
	offset int
	closes int64
}

type frameChunkReader struct {
	mu       sync.Mutex
	data     []byte
	offset   int
	maxChunk int
	reads    int64
	bytes    int64
	closes   int64
}

func newFrameChunkReader(data []byte, maxChunk int) *frameChunkReader {
	return &frameChunkReader{data: append([]byte(nil), data...), maxChunk: maxChunk}
}

func (reader *frameChunkReader) Read(buffer []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.reads++
	if reader.offset == len(reader.data) {
		return 0, io.EOF
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
	reader.bytes += int64(count)
	return count, nil
}

func (reader *frameChunkReader) Close() error {
	reader.mu.Lock()
	reader.closes++
	reader.mu.Unlock()
	return errors.New("borrowed stdout cannot be closed")
}

func (reader *frameChunkReader) snapshot() frameReaderSnapshot {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return frameReaderSnapshot{reads: reader.reads, bytes: reader.bytes, offset: reader.offset, closes: reader.closes}
}
