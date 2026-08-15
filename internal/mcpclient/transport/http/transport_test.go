package http

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	stdhttp "net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/mcpclient/transport"
	"xagent/internal/netpolicy"
)

func TestHTTPTransportRequiresGuardedClient(t *testing.T) {
	endpoint := testMCPEndpoint(t)
	client := &recordingClient{}
	factory := &recordingClientFactory{client: client}
	options := netpolicy.ClientOptions{
		Timeout:          3 * time.Second,
		SensitiveHeaders: []string{"X-Explicit-Secret"},
		TrustedRoots:     x509.NewCertPool(),
	}
	config := Config{
		Endpoint:        endpoint,
		ClientFactory:   factory,
		ClientOptions:   options,
		Headers:         map[string]string{"Authorization": "Bearer test", "X-MCP-Secret": "value"},
		ProtocolVersion: "2025-06-18",
		Lifecycle:       transport.Options{Diagnostics: &discardSink{}},
	}

	if _, err := New(Config{Endpoint: endpoint, Lifecycle: config.Lifecycle}); err == nil {
		t.Fatal("HTTP transport accepted a missing ClientFactory")
	}
	if _, err := New(Config{ClientFactory: factory, Lifecycle: config.Lifecycle}); err == nil {
		t.Fatal("HTTP transport accepted an unverified endpoint")
	}
	if got := factory.newCalls.Load(); got != 0 {
		t.Fatalf("invalid configuration reached ClientFactory.New %d times", got)
	}

	configType := reflect.TypeOf(Config{})
	clientInterface := reflect.TypeOf((*netpolicy.Client)(nil)).Elem()
	httpClient := reflect.TypeOf((*stdhttp.Client)(nil))
	roundTripper := reflect.TypeOf((*stdhttp.RoundTripper)(nil)).Elem()
	for index := 0; index < configType.NumField(); index++ {
		field := configType.Field(index)
		if field.Type == httpClient || field.Type.Implements(clientInterface) || field.Type.Implements(roundTripper) {
			t.Fatalf("Config exposes network client/transport injection field %s", field.Name)
		}
	}

	created, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if factory.newCalls.Load() != 1 {
		t.Fatalf("ClientFactory.New calls = %d, want 1", factory.newCalls.Load())
	}
	if factory.endpoint.Origin != endpoint.Origin || factory.endpoint.Purpose != netpolicy.PurposeMCP {
		t.Fatalf("factory received the wrong endpoint: %#v", factory.endpoint)
	}
	if factory.options.Timeout != options.Timeout || factory.options.TrustedRoots != options.TrustedRoots {
		t.Fatalf("factory received the wrong fixed options: %#v", factory.options)
	}
	wantSensitive := map[string]bool{
		"Authorization":     true,
		"X-Explicit-Secret": true,
		"X-Mcp-Secret":      true,
	}
	for _, name := range factory.options.SensitiveHeaders {
		delete(wantSensitive, stdhttp.CanonicalHeaderKey(name))
	}
	if len(wantSensitive) != 0 {
		t.Fatalf("configured MCP headers were not scoped as sensitive: %#v", factory.options.SensitiveHeaders)
	}

	// The request target must be detached from caller-owned Endpoint.URL.
	endpoint.URL.Path = "/mutated-after-construction"
	if err := created.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		t.Fatal(err)
	}
	if client.lastRequest == nil || client.lastRequest.URL.Path != "/mcp" || client.lastRequest.Header.Get("Authorization") != "Bearer test" {
		t.Fatalf("Send bypassed the frozen endpoint/headers: %#v", client.lastRequest)
	}
	if client.sdkCalls.Load() != 0 || client.doCalls.Load() != 1 {
		t.Fatalf("SDKHTTPClient/Do calls = %d/%d, want 0/1", client.sdkCalls.Load(), client.doCalls.Load())
	}
	if err := created.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPTransportOwnsAndClosesClientOnce(t *testing.T) {
	events := &orderedEvents{}
	client := &recordingClient{events: events}
	factory := &recordingClientFactory{client: client}
	created, err := New(Config{
		Endpoint:      testMCPEndpoint(t),
		ClientFactory: factory,
		Lifecycle:     transport.Options{Diagnostics: &discardSink{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
			t.Fatal(err)
		}
	}

	const closers = 32
	results := make(chan error, closers)
	var waiters sync.WaitGroup
	for range closers {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			results <- created.Close(context.Background())
		}()
	}
	waiters.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent Close returned %v", err)
		}
	}
	if factory.newCalls.Load() != 1 || client.doCalls.Load() != 2 || client.closeCalls.Load() != 1 || client.sdkCalls.Load() != 0 {
		t.Fatalf("factory/Do/CloseIdle/SDK calls = %d/%d/%d/%d", factory.newCalls.Load(), client.doCalls.Load(), client.closeCalls.Load(), client.sdkCalls.Load())
	}
	if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"late"}`)); err == nil {
		t.Fatal("Send succeeded after Close")
	}
	if client.doCalls.Load() != 2 {
		t.Fatal("post-close Send reached the owned client")
	}

	gotEvents := events.snapshot()
	wantEvents := []string{"do", "body-close", "do", "body-close", "client-close"}
	if !reflect.DeepEqual(gotEvents, wantEvents) {
		t.Fatalf("resource close order = %#v, want %#v", gotEvents, wantEvents)
	}
}

func TestHTTPTransportCloseLifecycle(t *testing.T) {
	t.Run("local close leaves remote session cleanup explicit", func(t *testing.T) {
		client := &remoteSessionClient{responses: []*stdhttp.Response{
			remoteSessionResponse(stdhttp.StatusAccepted, "session-still-live"),
		}}
		created := newLifecycleHTTPTransport(t, client, time.Second, &httpLifecycleSink{})
		if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"initialize"}`)); err != nil {
			t.Fatal(err)
		}
		if got := created.session.current(); got != "session-still-live" {
			t.Fatalf("captured session id = %q, want live session", got)
		}

		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		requests := client.snapshot()
		if len(requests) != 1 || requests[0].Method != stdhttp.MethodPost {
			t.Fatalf("requests after local Close = %#v, want only the original POST", requests)
		}
		if client.closeCalls.Load() != 1 {
			t.Fatalf("local client closes = %d, want 1", client.closeCalls.Load())
		}
	})

	t.Run("SSE request stays live until local close", func(t *testing.T) {
		client := newRequestContextSSEClient()
		created := newLifecycleHTTPTransport(t, client, time.Second, &httpLifecycleSink{})
		if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"stream"}`)); err != nil {
			t.Fatal(err)
		}
		body := waitRequestContextSSEBody(t, client.bodyReady)
		waitHTTPSignal(t, body.readStarted, "request-context SSE body read")
		if err := body.requestContext.Err(); err != nil {
			t.Fatalf("SSE request context after Send = %v, want live", err)
		}

		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitHTTPSignal(t, body.requestCancelled, "SSE request context cancellation")
		waitHTTPSignal(t, body.closeDone, "request-context SSE body close")
		if !errors.Is(body.requestContext.Err(), context.Canceled) {
			t.Fatalf("SSE request context after Close = %v, want cancelled", body.requestContext.Err())
		}
		if body.closeCalls.Load() != 1 || client.closeCalls.Load() != 1 || client.doCalls.Load() != 1 {
			t.Fatalf("body/client/Do calls = %d/%d/%d, want 1/1/1", body.closeCalls.Load(), client.closeCalls.Load(), client.doCalls.Load())
		}
		if active := created.bodies.activeCount(); active != 0 {
			t.Fatalf("active response bodies after Close = %d, want 0", active)
		}
	})

	t.Run("local close cancels active request and rejects new I/O", func(t *testing.T) {
		client := newCancelAwareHTTPClient()
		created := newLifecycleHTTPTransport(t, client, time.Second, &httpLifecycleSink{})
		result := make(chan error, 1)
		go func() {
			result <- created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"blocking"}`))
		}()
		waitHTTPSignal(t, client.requestStarted, "active HTTP request")

		if err := created.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			if !errors.Is(err, ErrRequestFailed) {
				t.Fatalf("cancelled Send = %v, want bounded request failure", err)
			}
		case <-time.After(time.Second):
			t.Fatal("active HTTP request was not cancelled by Close")
		}
		if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"late"}`)); !errors.Is(err, transport.ErrClosing) {
			t.Fatalf("post-close Send = %v, want closing", err)
		}
		if client.doCalls.Load() != 1 || client.closeCalls.Load() != 1 {
			t.Fatalf("Do/client close calls = %d/%d, want 1/1", client.doCalls.Load(), client.closeCalls.Load())
		}
	})

	t.Run("caller timeout only stops waiting", func(t *testing.T) {
		body := newBlockingLifecycleBody()
		client := newLifecycleSSEClient(body)
		sink := &httpLifecycleSink{}
		created := newLifecycleHTTPTransport(t, client, time.Second, sink)
		if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"stream"}`)); err != nil {
			t.Fatal(err)
		}
		waitHTTPSignal(t, body.readStarted, "SSE body read")

		waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancelWait()
		firstResult := make(chan error, 1)
		go func() { firstResult <- created.Close(waitCtx) }()
		waitHTTPSignal(t, body.closeStarted, "SSE body close")
		select {
		case err := <-firstResult:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("first Close = %v, want caller deadline", err)
			}
		case <-time.After(time.Second):
			t.Fatal("caller-limited Close did not return")
		}
		if created.state.State() != transport.TransportStateClosing {
			t.Fatalf("state after caller timeout = %s, want closing", created.state.State())
		}
		if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"late"}`)); !errors.Is(err, transport.ErrClosing) {
			t.Fatalf("Send during cleanup = %v, want closing", err)
		}

		close(body.releaseClose)
		if err := created.Close(context.Background()); err != nil {
			t.Fatalf("stable final Close: %v", err)
		}
		cancelled, cancelNow := context.WithCancel(context.Background())
		cancelNow()
		if err := created.Close(cancelled); err != nil {
			t.Fatalf("completed cleanup did not beat cancelled waiter: %v", err)
		}
		if body.closeCalls.Load() != 1 || client.closeCalls.Load() != 1 || client.doCalls.Load() != 1 {
			t.Fatalf("body/client/Do calls = %d/%d/%d, want 1/1/1", body.closeCalls.Load(), client.closeCalls.Load(), client.doCalls.Load())
		}
		if sink.countCode("mcp_transport_cleanup_timeout") != 0 {
			t.Fatal("caller timeout was cached as an internal cleanup timeout")
		}
	})

	t.Run("hard timeout forces a stable bounded result", func(t *testing.T) {
		body := newBlockingLifecycleBody()
		client := newLifecycleSSEClient(body)
		sink := &httpLifecycleSink{}
		created := newLifecycleHTTPTransport(t, client, 20*time.Millisecond, sink)
		if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"stream"}`)); err != nil {
			t.Fatal(err)
		}
		waitHTTPSignal(t, body.readStarted, "hard-timeout SSE read")

		first := created.Close(context.Background())
		second := created.Close(context.Background())
		if !errors.Is(first, transport.ErrCleanupTimeout) || !errors.Is(second, transport.ErrCleanupTimeout) {
			t.Fatalf("hard-timeout results = %v / %v", first, second)
		}
		waitHTTPSignal(t, client.clientClosed, "forced client close")
		if created.state.State() != transport.TransportStateClosed {
			t.Fatalf("hard-timeout state = %s, want closed", created.state.State())
		}
		if sink.countCode("mcp_transport_cleanup_timeout") != 1 {
			t.Fatalf("hard-timeout diagnostics = %#v, want one", sink.snapshot())
		}
		if body.closeCalls.Load() != 1 || client.closeCalls.Load() != 1 || client.doCalls.Load() != 1 {
			t.Fatalf("hard-timeout body/client/Do calls = %d/%d/%d", body.closeCalls.Load(), client.closeCalls.Load(), client.doCalls.Load())
		}

		close(body.releaseClose)
		waitHTTPSignal(t, body.closeDone, "blocked body close release")
	})
}

func newLifecycleHTTPTransport(t *testing.T, client netpolicy.Client, timeout time.Duration, sink diagnostics.BoundedSink) *Transport {
	t.Helper()
	created, err := New(Config{
		Endpoint:      testMCPEndpoint(t),
		ClientFactory: &recordingClientFactory{client: client},
		Lifecycle:     transport.Options{CleanupTimeout: timeout, Diagnostics: sink},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return created
}

func testMCPEndpoint(t *testing.T) netpolicy.Endpoint {
	t.Helper()
	endpoint, err := netpolicy.NewPolicy().ValidateInitial(context.Background(), "http://127.0.0.1:43117/mcp", netpolicy.PurposeMCP)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

type recordingClientFactory struct {
	client   netpolicy.Client
	err      error
	endpoint netpolicy.Endpoint
	options  netpolicy.ClientOptions
	newCalls atomic.Int64
}

func (factory *recordingClientFactory) New(endpoint netpolicy.Endpoint, options netpolicy.ClientOptions) (netpolicy.Client, error) {
	factory.newCalls.Add(1)
	factory.endpoint = endpoint
	factory.options = options
	return factory.client, factory.err
}

type recordingClient struct {
	lastRequest *stdhttp.Request
	events      *orderedEvents
	doCalls     atomic.Int64
	sdkCalls    atomic.Int64
	closeCalls  atomic.Int64
}

func (client *recordingClient) Do(request *stdhttp.Request) (*stdhttp.Response, error) {
	client.doCalls.Add(1)
	client.lastRequest = request.Clone(request.Context())
	if client.events != nil {
		client.events.add("do")
	}
	return &stdhttp.Response{
		StatusCode: stdhttp.StatusAccepted,
		Header:     make(stdhttp.Header),
		Body:       &eventBody{Reader: strings.NewReader(""), events: client.events},
		Request:    request,
	}, nil
}

func (client *recordingClient) SDKHTTPClient() *stdhttp.Client {
	client.sdkCalls.Add(1)
	return nil
}

func (client *recordingClient) CloseIdleConnections() {
	client.closeCalls.Add(1)
	if client.events != nil {
		client.events.add("client-close")
	}
}

type eventBody struct {
	io.Reader
	events *orderedEvents
}

func (body *eventBody) Close() error {
	if body.events != nil {
		body.events.add("body-close")
	}
	return nil
}

type orderedEvents struct {
	mu     sync.Mutex
	events []string
}

func (events *orderedEvents) add(event string) {
	events.mu.Lock()
	events.events = append(events.events, event)
	events.mu.Unlock()
}

func (events *orderedEvents) snapshot() []string {
	events.mu.Lock()
	defer events.mu.Unlock()
	return append([]string(nil), events.events...)
}

type discardSink struct{}

func (*discardSink) Add(diagnostics.SanitizeInput) {}

type cancelAwareHTTPClient struct {
	requestStarted chan struct{}
	clientClosed   chan struct{}
	requestOnce    sync.Once
	clientOnce     sync.Once
	doCalls        atomic.Int64
	closeCalls     atomic.Int64
}

func newCancelAwareHTTPClient() *cancelAwareHTTPClient {
	return &cancelAwareHTTPClient{requestStarted: make(chan struct{}), clientClosed: make(chan struct{})}
}

func (client *cancelAwareHTTPClient) Do(request *stdhttp.Request) (*stdhttp.Response, error) {
	client.doCalls.Add(1)
	client.requestOnce.Do(func() { close(client.requestStarted) })
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func (*cancelAwareHTTPClient) SDKHTTPClient() *stdhttp.Client { return nil }

func (client *cancelAwareHTTPClient) CloseIdleConnections() {
	client.closeCalls.Add(1)
	client.clientOnce.Do(func() { close(client.clientClosed) })
}

type lifecycleSSEClient struct {
	body         io.ReadCloser
	clientClosed chan struct{}
	clientOnce   sync.Once
	doCalls      atomic.Int64
	closeCalls   atomic.Int64
}

type requestContextSSEClient struct {
	bodyReady   chan *requestContextSSEBody
	doCalls     atomic.Int64
	closeCalls  atomic.Int64
	clientClose sync.Once
}

func newRequestContextSSEClient() *requestContextSSEClient {
	return &requestContextSSEClient{bodyReady: make(chan *requestContextSSEBody, 1)}
}

func (client *requestContextSSEClient) Do(request *stdhttp.Request) (*stdhttp.Response, error) {
	client.doCalls.Add(1)
	body := newRequestContextSSEBody(request.Context())
	client.bodyReady <- body
	return &stdhttp.Response{
		StatusCode: stdhttp.StatusOK,
		Header:     stdhttp.Header{headerContentType: []string{contentTypeEventStream}},
		Body:       body,
		Request:    request,
	}, nil
}

func (*requestContextSSEClient) SDKHTTPClient() *stdhttp.Client { return nil }

func (client *requestContextSSEClient) CloseIdleConnections() {
	client.clientClose.Do(func() { client.closeCalls.Add(1) })
}

type requestContextSSEBody struct {
	requestContext   context.Context
	readStarted      chan struct{}
	requestCancelled chan struct{}
	closeDone        chan struct{}
	readOnce         sync.Once
	cancelOnce       sync.Once
	closeOnce        sync.Once
	closeCalls       atomic.Int64
}

func newRequestContextSSEBody(requestContext context.Context) *requestContextSSEBody {
	return &requestContextSSEBody{
		requestContext:   requestContext,
		readStarted:      make(chan struct{}),
		requestCancelled: make(chan struct{}),
		closeDone:        make(chan struct{}),
	}
}

func (body *requestContextSSEBody) Read([]byte) (int, error) {
	body.readOnce.Do(func() { close(body.readStarted) })
	<-body.requestContext.Done()
	body.cancelOnce.Do(func() { close(body.requestCancelled) })
	return 0, body.requestContext.Err()
}

func (body *requestContextSSEBody) Close() error {
	body.closeOnce.Do(func() {
		body.closeCalls.Add(1)
		close(body.closeDone)
	})
	return nil
}

func waitRequestContextSSEBody(t *testing.T, bodies <-chan *requestContextSSEBody) *requestContextSSEBody {
	t.Helper()
	select {
	case body := <-bodies:
		return body
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for request-context SSE body")
		return nil
	}
}

func newLifecycleSSEClient(body io.ReadCloser) *lifecycleSSEClient {
	return &lifecycleSSEClient{body: body, clientClosed: make(chan struct{})}
}

func (client *lifecycleSSEClient) Do(request *stdhttp.Request) (*stdhttp.Response, error) {
	client.doCalls.Add(1)
	return &stdhttp.Response{
		StatusCode: stdhttp.StatusOK,
		Header:     stdhttp.Header{headerContentType: []string{contentTypeEventStream}},
		Body:       client.body,
		Request:    request,
	}, nil
}

func (*lifecycleSSEClient) SDKHTTPClient() *stdhttp.Client { return nil }

func (client *lifecycleSSEClient) CloseIdleConnections() {
	client.closeCalls.Add(1)
	client.clientOnce.Do(func() { close(client.clientClosed) })
}

type blockingLifecycleBody struct {
	readStarted  chan struct{}
	closeStarted chan struct{}
	releaseClose chan struct{}
	closed       chan struct{}
	closeDone    chan struct{}
	readOnce     sync.Once
	startOnce    sync.Once
	closedOnce   sync.Once
	closeCalls   atomic.Int64
}

func newBlockingLifecycleBody() *blockingLifecycleBody {
	return &blockingLifecycleBody{
		readStarted:  make(chan struct{}),
		closeStarted: make(chan struct{}),
		releaseClose: make(chan struct{}),
		closed:       make(chan struct{}),
		closeDone:    make(chan struct{}),
	}
}

func (body *blockingLifecycleBody) Read([]byte) (int, error) {
	body.readOnce.Do(func() { close(body.readStarted) })
	<-body.closed
	return 0, io.EOF
}

func (body *blockingLifecycleBody) Close() error {
	body.closeCalls.Add(1)
	body.startOnce.Do(func() { close(body.closeStarted) })
	<-body.releaseClose
	body.closedOnce.Do(func() {
		close(body.closed)
		close(body.closeDone)
	})
	return nil
}

type httpLifecycleSink struct {
	mu    sync.Mutex
	items []diagnostics.SanitizeInput
}

func (sink *httpLifecycleSink) Add(input diagnostics.SanitizeInput) {
	sink.mu.Lock()
	sink.items = append(sink.items, input)
	sink.mu.Unlock()
}

func (sink *httpLifecycleSink) countCode(code string) int {
	count := 0
	for _, input := range sink.snapshot() {
		if input.Code == code {
			count++
		}
	}
	return count
}

func (sink *httpLifecycleSink) snapshot() []diagnostics.SanitizeInput {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]diagnostics.SanitizeInput(nil), sink.items...)
}

func waitHTTPSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
