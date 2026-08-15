package http

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

func TestReadCapturedJSONResponseWritesBeforeBodyTail(t *testing.T) {
	const text = "http-wire-first-canary"
	reader := &gatedHTTPReader{prefix: []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"` + text + `"}]`), tail: []byte(`}}`), release: make(chan struct{})}
	output := newSynchronizedCaptureBuffer()
	done := make(chan error, 1)
	go func() {
		decoded, _, err := readCapturedJSONResponse(reader, 1024, output)
		if err == nil && (decoded.Result.Content[0].Text != "" || len(decoded.Result.Content[0].Raw) != 0) {
			done <- io.ErrUnexpectedEOF
			return
		}
		done <- err
	}()
	select {
	case <-output.firstWrite:
	case <-time.After(time.Second):
		t.Fatalf("HTTP capture did not precede body tail: %q", output.String())
	}
	if captured := output.String(); captured != text {
		t.Fatalf("HTTP capture wrote unexpected content before body tail: %q", captured)
	}
	select {
	case err := <-done:
		t.Fatalf("HTTP DTO completed before tail: %v", err)
	default:
	}
	reader.tailOnce.Do(func() { close(reader.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if captured := output.String(); captured != text {
		t.Fatalf("HTTP capture changed after body tail: %q", captured)
	}
}

// synchronizedCaptureBuffer gives this test an explicit hand-off between the
// decoder goroutine and the assertion goroutine. The production capture API
// deliberately owns its writer from one decoder goroutine; the test only
// needs to make the observation safe while it proves the write happens before
// the reader's gated tail is released.
type synchronizedCaptureBuffer struct {
	mu         sync.Mutex
	buffer     bytes.Buffer
	firstWrite chan struct{}
	writeOnce  sync.Once
}

func newSynchronizedCaptureBuffer() *synchronizedCaptureBuffer {
	return &synchronizedCaptureBuffer{firstWrite: make(chan struct{})}
}

func (buffer *synchronizedCaptureBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	count, err := buffer.buffer.Write(value)
	buffer.mu.Unlock()
	if count > 0 {
		buffer.writeOnce.Do(func() { close(buffer.firstWrite) })
	}
	return count, err
}

func (buffer *synchronizedCaptureBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

type gatedHTTPReader struct {
	prefix, tail []byte
	release      chan struct{}
	tailOnce     sync.Once
	stage        int
}

func (reader *gatedHTTPReader) Read(destination []byte) (int, error) {
	if reader.stage == 0 {
		reader.stage++
		return copy(destination, reader.prefix), nil
	}
	if reader.stage == 1 {
		<-reader.release
		reader.stage++
		return copy(destination, reader.tail), nil
	}
	return 0, io.EOF
}
