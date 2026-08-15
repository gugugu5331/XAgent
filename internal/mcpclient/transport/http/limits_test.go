package http

import (
	"context"
	"errors"
	"io"
	stdhttp "net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/budget"
	mcptransport "xagent/internal/mcpclient/transport"
	"xagent/internal/netpolicy"
)

func TestChunkedJSONLimitIsPerCall(t *testing.T) {
	const limit int64 = 64
	const canary = "chunked-json-secret-canary"
	oversizedPayload := []byte(`{"jsonrpc":"2.0","id":1,"result":"` + canary + strings.Repeat("x", 128) + `"}`)
	validPayload := []byte(`{"jsonrpc":"2.0","id":3,"result":{}}`)

	unknownLength := newChunkedResponseBody(oversizedPayload, 7)
	forgedSmallLength := newChunkedResponseBody(oversizedPayload, 5)
	forgedLargeLength := newChunkedResponseBody(validPayload, 4)
	client := &scriptedJSONClient{responses: []*stdhttp.Response{
		jsonResponse(unknownLength, -1),
		jsonResponse(forgedSmallLength, 1),
		jsonResponse(forgedLargeLength, limit+1024),
	}}
	created, err := New(Config{
		Endpoint:         testMCPEndpoint(t),
		ClientFactory:    &recordingClientFactory{client: client},
		MaxResponseBytes: limit,
		Lifecycle:        mcptransport.Options{Diagnostics: &discardSink{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	for index, body := range []*chunkedResponseBody{unknownLength, forgedSmallLength} {
		err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		var limitErr *budget.LimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("over-limit call %d error = %T %v, want budget LimitError", index, err, err)
		}
		if limitErr.Scope != string(budget.MCPMaxResponseBytes) || limitErr.Dimension != budget.Bytes ||
			limitErr.Limit != limit || limitErr.Observed <= limit {
			t.Fatalf("over-limit call %d metadata = %#v", index, limitErr)
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("over-limit call %d exposed response payload", index)
		}
		if body.closes.Load() != 1 {
			t.Fatalf("over-limit call %d body closes = %d, want 1", index, body.closes.Load())
		}
		if got := body.bytesRead.Load(); got <= limit || got >= int64(len(oversizedPayload)) {
			t.Fatalf("over-limit call %d bytes read = %d, want early stop just beyond %d", index, got, limit)
		}
		if state := created.state.State(); state != mcptransport.TransportStateRunning {
			t.Fatalf("over-limit call %d changed transport state to %s", index, state)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	received := make(chan struct {
		frame string
		err   error
	}, 1)
	go func() {
		event, receiveErr := created.Receive(ctx)
		received <- struct {
			frame string
			err   error
		}{frame: string(event.Frame), err: receiveErr}
	}()
	if err := created.Send(ctx, []byte(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)); err != nil {
		t.Fatalf("valid call after independent limit failures: %v", err)
	}
	result := <-received
	if result.err != nil || result.frame != string(validPayload) {
		t.Fatalf("valid response after limit failures = %q/%v", result.frame, result.err)
	}
	if forgedLargeLength.closes.Load() != 1 {
		t.Fatalf("valid forged-large Content-Length body closes = %d, want 1", forgedLargeLength.closes.Load())
	}
	if err := created.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.closeCalls.Load() != 1 {
		t.Fatalf("client closes = %d, want 1", client.closeCalls.Load())
	}
}

func TestChunkedSSEUsesCumulativeBudget(t *testing.T) {
	const limit int64 = 72
	const canary = "chunked-sse-secret-canary"
	firstFrame := `{"jsonrpc":"2.0","id":1,"result":{}}`
	stream := []byte("data: " + firstFrame + "\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":\"" + canary + strings.Repeat("x", 128) + "\"}\n\n")
	body := newChunkedResponseBody(stream, 5)
	client := &scriptedJSONClient{responses: []*stdhttp.Response{sseResponse(body)}}
	created := newLimitedHTTPTransport(t, client, limit, 4)

	if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("start SSE response: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := created.Receive(ctx)
	if err != nil || string(event.Frame) != firstFrame {
		t.Fatalf("first SSE frame = %q/%v", event.Frame, err)
	}
	_, err = created.Receive(ctx)
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("cumulative SSE error = %T %v, want budget LimitError", err, err)
	}
	if limitErr.Scope != string(budget.MCPMaxResponseBytes) || limitErr.Dimension != budget.Bytes ||
		limitErr.Limit != limit || limitErr.Observed <= limit {
		t.Fatalf("cumulative SSE metadata = %#v", limitErr)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatal("cumulative SSE error exposed payload")
	}
	if body.closes.Load() != 1 {
		t.Fatalf("over-limit SSE body closes = %d, want 1", body.closes.Load())
	}
	if got := body.bytesRead.Load(); got <= limit || got >= int64(len(stream)) {
		t.Fatalf("over-limit SSE bytes read = %d, want early bounded stop", got)
	}

	limits, err := newSSELimits(limit, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := limits.checkEventBytes(limit + 1); !isLimitError(err, budget.MCPMaxResponseBytes, budget.Bytes, limit, limit+1) {
		t.Fatalf("single-event limit = %v", err)
	}
	for range limit {
		if err := limits.consumeEventCount(); err != nil {
			t.Fatalf("consume allowed SSE event count: %v", err)
		}
	}
	if err := limits.consumeEventCount(); !isLimitError(err, budget.MCPMaxResponseBytes, budget.Items, limit, limit+1) {
		t.Fatalf("SSE event-count limit = %v", err)
	}

	if err := created.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.closeCalls.Load() != 1 {
		t.Fatalf("SSE client closes = %d, want 1", client.closeCalls.Load())
	}
}

func TestMaliciousProtocolErrorsAreBounded(t *testing.T) {
	const errorLimit int64 = 2
	const canary = "malicious-sse-protocol-canary"
	validSuffix := `data: {"jsonrpc":"2.0","id":9,"result":{}}` + "\n\n"
	stream := []byte(
		"data: \"" + canary + "-1\n\n" +
			"data: \"" + canary + "-2\n\n" +
			"data: \"" + canary + "-3\n\n" +
			validSuffix,
	)
	body := newChunkedResponseBody(stream, 3)
	client := &scriptedJSONClient{responses: []*stdhttp.Response{sseResponse(body)}}
	created := newLimitedHTTPTransport(t, client, 512, errorLimit)

	if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","id":9,"method":"ping"}`)); err != nil {
		t.Fatalf("start malicious SSE response: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, err := created.Receive(ctx)
	var limitErr *budget.LimitError
	if len(event.Frame) != 0 || !errors.As(err, &limitErr) {
		t.Fatalf("malicious SSE result = %q/%T %v, want fatal budget error", event.Frame, err, err)
	}
	if limitErr.Scope != string(budget.MCPMaxProtocolErrors) || limitErr.Dimension != budget.ProtocolErrors ||
		limitErr.Limit != errorLimit || limitErr.Observed != errorLimit+1 {
		t.Fatalf("protocol-error metadata = %#v", limitErr)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatal("protocol-error limit exposed payload")
	}
	if body.closes.Load() != 1 {
		t.Fatalf("malicious SSE body closes = %d, want 1", body.closes.Load())
	}
	if got := body.bytesRead.Load(); got >= int64(len(stream)) {
		t.Fatalf("malicious SSE consumed valid suffix: bytes=%d total=%d", got, len(stream))
	}
	if state := created.state.State(); state != mcptransport.TransportStateRunning {
		t.Fatalf("Receive fatal bypassed Connection ownership and changed Transport state to %s", state)
	}

	if err := created.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.closeCalls.Load() != 1 {
		t.Fatalf("malicious SSE client closes = %d, want 1", client.closeCalls.Load())
	}
}

func newLimitedHTTPTransport(
	t *testing.T,
	client netpolicy.Client,
	maxResponseBytes int64,
	maxProtocolErrors int64,
) *Transport {
	t.Helper()
	created, err := New(Config{
		Endpoint:          testMCPEndpoint(t),
		ClientFactory:     &recordingClientFactory{client: client},
		MaxResponseBytes:  maxResponseBytes,
		MaxProtocolErrors: maxProtocolErrors,
		Lifecycle:         mcptransport.Options{Diagnostics: &discardSink{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return created
}

func isLimitError(err error, scope budget.Scope, dimension budget.Dimension, limit, observed int64) bool {
	var limitErr *budget.LimitError
	return errors.As(err, &limitErr) && limitErr.Scope == string(scope) && limitErr.Dimension == dimension &&
		limitErr.Limit == limit && limitErr.Observed == observed
}

func jsonResponse(body io.ReadCloser, contentLength int64) *stdhttp.Response {
	return &stdhttp.Response{
		StatusCode:    stdhttp.StatusOK,
		Header:        stdhttp.Header{headerContentType: []string{contentTypeJSON}},
		Body:          body,
		ContentLength: contentLength,
	}
}

func sseResponse(body io.ReadCloser) *stdhttp.Response {
	return &stdhttp.Response{
		StatusCode:    stdhttp.StatusOK,
		Header:        stdhttp.Header{headerContentType: []string{contentTypeEventStream}},
		Body:          body,
		ContentLength: -1,
	}
}

type scriptedJSONClient struct {
	mu         sync.Mutex
	responses  []*stdhttp.Response
	next       int
	closeCalls atomic.Int64
}

func (client *scriptedJSONClient) Do(request *stdhttp.Request) (*stdhttp.Response, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.next >= len(client.responses) {
		return nil, errors.New("scripted response exhausted")
	}
	response := client.responses[client.next]
	client.next++
	response.Request = request
	return response, nil
}

func (*scriptedJSONClient) SDKHTTPClient() *stdhttp.Client { return nil }

func (client *scriptedJSONClient) CloseIdleConnections() { client.closeCalls.Add(1) }

var _ netpolicy.Client = (*scriptedJSONClient)(nil)

type chunkedResponseBody struct {
	mu       sync.Mutex
	data     []byte
	offset   int
	maxChunk int
	closed   bool

	bytesRead atomic.Int64
	closes    atomic.Int64
}

func newChunkedResponseBody(data []byte, maxChunk int) *chunkedResponseBody {
	return &chunkedResponseBody{data: append([]byte(nil), data...), maxChunk: maxChunk}
}

func (body *chunkedResponseBody) Read(buffer []byte) (int, error) {
	body.mu.Lock()
	defer body.mu.Unlock()
	if body.closed {
		return 0, io.ErrClosedPipe
	}
	if body.offset == len(body.data) {
		return 0, io.EOF
	}
	count := len(body.data) - body.offset
	if count > body.maxChunk {
		count = body.maxChunk
	}
	if count > len(buffer) {
		count = len(buffer)
	}
	copy(buffer, body.data[body.offset:body.offset+count])
	body.offset += count
	body.bytesRead.Add(int64(count))
	return count, nil
}

func (body *chunkedResponseBody) Close() error {
	body.mu.Lock()
	body.closed = true
	body.mu.Unlock()
	body.closes.Add(1)
	return nil
}
