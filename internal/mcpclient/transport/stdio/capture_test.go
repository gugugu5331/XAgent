package stdio

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"xagent/internal/mcpclient/protocol"
)

func TestNextCapturedWritesBeforeStdioFrameTail(t *testing.T) {
	const text = "stdio-wire-first-canary"
	reader := &gatedStdioReader{prefix: []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"` + text + `"}]`), tail: []byte(`}}` + "\n"), release: make(chan struct{})}
	decoder, err := newNewlineDecoder(reader, 256)
	if err != nil {
		t.Fatal(err)
	}
	output := newSynchronizedStdioCaptureBuffer()
	done := make(chan error, 1)
	go func() {
		_, err := decoder.NextCaptured(context.Background(), func(id protocol.RPCID) (io.Writer, bool) {
			return output, id.IsNumber()
		})
		done <- err
	}()
	select {
	case <-output.firstWrite:
	case <-time.After(5 * time.Second):
		t.Fatalf("stdio capture did not precede frame tail: %q", output.String())
	}
	if captured := output.String(); captured != text {
		t.Fatalf("stdio capture wrote unexpected content before frame tail: %q", captured)
	}
	select {
	case err := <-done:
		t.Fatalf("stdio DTO completed before tail: %v", err)
	default:
	}
	reader.tailOnce.Do(func() { close(reader.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type synchronizedStdioCaptureBuffer struct {
	mu         sync.Mutex
	buffer     bytes.Buffer
	firstWrite chan struct{}
	writeOnce  sync.Once
}

func newSynchronizedStdioCaptureBuffer() *synchronizedStdioCaptureBuffer {
	return &synchronizedStdioCaptureBuffer{firstWrite: make(chan struct{})}
}

func (buffer *synchronizedStdioCaptureBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	count, err := buffer.buffer.Write(value)
	buffer.mu.Unlock()
	if count > 0 {
		buffer.writeOnce.Do(func() { close(buffer.firstWrite) })
	}
	return count, err
}

func (buffer *synchronizedStdioCaptureBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

type gatedStdioReader struct {
	prefix, tail []byte
	release      chan struct{}
	tailOnce     sync.Once
	stage        int
}

func (reader *gatedStdioReader) Read(destination []byte) (int, error) {
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
