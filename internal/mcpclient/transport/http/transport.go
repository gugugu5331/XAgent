package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	stdhttp "net/http"
	"sync"

	"xagent/internal/mcpclient/protocol"
	mcptransport "xagent/internal/mcpclient/transport"
	"xagent/internal/netpolicy"
)

var (
	ErrInvalidConfig       = errors.New("MCP HTTP transport configuration is invalid")
	ErrRequestFailed       = errors.New("MCP HTTP request failed")
	ErrResponseUnavailable = errors.New("MCP HTTP response is unavailable")
	ErrResponseUnsupported = errors.New("MCP HTTP response handling is not available")
)

// Transport owns exactly one netpolicy Client created for its sealed endpoint.
// JSON responses use an independent bounded reader for each call. SSE responses
// are consumed asynchronously under cumulative stream and protocol-error budgets.
type Transport struct {
	client            netpolicy.Client
	target            string
	headers           stdhttp.Header
	version           string
	maxResponseBytes  int64
	maxProtocolErrors int64
	state             *mcptransport.StateMachine

	results    chan transportReceiveResult
	stop       chan struct{}
	ioContext  context.Context
	cancelIO   context.CancelFunc
	bodies     *bodyRegistry
	sseWorkers *sseWorkerGroup
	session    *remoteSession
	captures   *mcptransport.OutputCaptureBindings

	stopOnce        sync.Once
	closeClientOnce sync.Once
}

var _ mcptransport.Transport = (*Transport)(nil)

func New(config Config) (*Transport, error) {
	resolved, err := resolveConfig(config)
	if err != nil {
		return nil, err
	}
	client, clientErr := resolved.factory.New(resolved.endpoint, resolved.clientOptions)
	if clientErr != nil || client == nil {
		if client != nil {
			client.CloseIdleConnections()
		}
		return nil, ErrInvalidConfig
	}
	ioContext, cancelIO := context.WithCancel(context.Background())
	created := &Transport{
		client:            client,
		target:            resolved.target,
		headers:           resolved.headers.Clone(),
		version:           resolved.protocolVersion,
		maxResponseBytes:  resolved.maxResponseBytes,
		maxProtocolErrors: resolved.maxProtocolErrors,
		results:           make(chan transportReceiveResult),
		stop:              make(chan struct{}),
		ioContext:         ioContext,
		cancelIO:          cancelIO,
		bodies:            newBodyRegistry(),
		sseWorkers:        newSSEWorkerGroup(),
		session:           newRemoteSession(),
		captures:          mcptransport.NewOutputCaptureBindings(),
	}
	machine, err := mcptransport.NewStateMachine(resolved.lifecycle, created.cleanup, created.forceClose)
	if err != nil {
		created.stopLocalIO()
		created.closeClient()
		return nil, ErrInvalidConfig
	}
	created.state = machine
	return created, nil
}

// BindOutputCapture is called by the managed connection before Send.  It
// establishes the writer-to-request association before the HTTP body is read.
func (transport *Transport) BindOutputCapture(frame json.RawMessage, destination io.Writer) error {
	if transport == nil || transport.captures == nil {
		return mcptransport.ErrOutputCaptureBinding
	}
	return transport.captures.Bind(frame, destination)
}

func (transport *Transport) UnbindOutputCapture(frame json.RawMessage) {
	if transport != nil && transport.captures != nil {
		transport.captures.Remove(frame)
	}
}

func (transport *Transport) Start(ctx context.Context) error {
	if transport == nil || transport.state == nil || transport.client == nil {
		return ErrInvalidConfig
	}
	return transport.state.Start(ctx, func(context.Context) error { return nil })
}

func (transport *Transport) Send(ctx context.Context, frame json.RawMessage) error {
	if transport == nil || transport.state == nil {
		return ErrInvalidConfig
	}
	if err := transport.state.RequireRunning(); err != nil {
		return err
	}
	if len(frame) == 0 || !json.Valid(frame) {
		return ErrRequestFailed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestContext, cancelRequest := transport.requestContext(ctx)
	sseOwnsRequest := false
	defer func() {
		if !sseOwnsRequest {
			cancelRequest()
		}
	}()
	request, err := stdhttp.NewRequestWithContext(requestContext, stdhttp.MethodPost, transport.target, bytes.NewReader(cloneFrame(frame)))
	if err != nil {
		return ErrRequestFailed
	}
	request.Header = transport.headers.Clone()
	request.Header.Set(headerContentType, contentTypeJSON)
	request.Header.Set(headerAccept, acceptMCP)
	if transport.version != "" {
		request.Header.Set(headerProtocolVersion, transport.version)
	}
	if sessionID := transport.session.current(); sessionID != "" {
		request.Header.Set(headerSessionID, sessionID)
	}

	response, err := transport.client.Do(request)
	if response != nil && response.Body != nil {
		tracked, trackErr := transport.bodies.register(response.Body)
		if trackErr != nil {
			return ErrRequestFailed
		}
		response.Body = tracked
	}
	if err != nil {
		closeHTTPResponse(response)
		return ErrRequestFailed
	}
	if response == nil {
		return ErrResponseUnavailable
	}
	transport.session.capture(response.Header.Get(headerSessionID))
	if response.StatusCode == stdhttp.StatusAccepted {
		if err := closeHTTPResponse(response); err != nil {
			return ErrRequestFailed
		}
		return nil
	}
	if response.StatusCode < stdhttp.StatusOK || response.StatusCode >= stdhttp.StatusMultipleChoices || response.Body == nil {
		if err := closeHTTPResponse(response); err != nil {
			return ErrRequestFailed
		}
		return ErrResponseUnsupported
	}
	contentType := response.Header.Get(headerContentType)
	if isSSEContentType(contentType) {
		if transport.startSSEWorker(response, cancelRequest) {
			sseOwnsRequest = true
			return nil
		}
		_ = closeHTTPResponse(response)
		return ErrRequestFailed
	}
	if !isJSONContentType(contentType) {
		if err := closeHTTPResponse(response); err != nil {
			return ErrRequestFailed
		}
		return ErrResponseUnsupported
	}

	hasCaptureBinding := transport.captures != nil && transport.captures.HasBindings()
	destination, bindingErr := transport.captures.TakeRequest(frame)
	if hasCaptureBinding && (bindingErr != nil || destination == nil) {
		_ = closeHTTPResponse(response)
		return ErrRequestFailed
	}
	var captured *protocol.CapturedCallToolResponse
	var capturedBytes int64
	var responseFrame json.RawMessage
	var readErr error
	if bindingErr == nil && destination != nil {
		decoded, bytesRead, decodeErr := readCapturedJSONResponse(response.Body, transport.maxResponseBytes, destination)
		if decodeErr == nil {
			captured = &decoded
			capturedBytes = bytesRead
		} else {
			readErr = decodeErr
		}
	} else {
		responseFrame, readErr = readJSONResponse(response.Body, transport.maxResponseBytes)
	}
	closeErr := closeHTTPResponse(response)
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return ErrRequestFailed
	}
	select {
	case transport.results <- transportReceiveResult{event: mcptransport.TransportEvent{Frame: responseFrame, Captured: captured, CapturedBytes: capturedBytes}}:
		return nil
	case <-transport.stop:
		return ErrRequestFailed
	case <-requestContext.Done():
		return ErrRequestFailed
	}
}

func (transport *Transport) Receive(ctx context.Context) (mcptransport.TransportEvent, error) {
	if transport == nil || transport.state == nil {
		return mcptransport.TransportEvent{}, ErrInvalidConfig
	}
	if err := transport.state.RequireRunning(); err != nil {
		return mcptransport.TransportEvent{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case result := <-transport.results:
		if result.err != nil {
			return mcptransport.TransportEvent{}, result.err
		}
		// Preserve the captured response metadata produced by the incremental
		// JSON reader.  Dropping Captured here makes the connection fall back
		// to decoding an empty raw frame, even though the operation-local
		// writer already received the result content.
		event := result.event
		event.Frame = cloneFrame(event.Frame)
		return event, nil
	case <-transport.stop:
		return mcptransport.TransportEvent{}, mcptransport.ErrClosing
	case <-ctx.Done():
		return mcptransport.TransportEvent{}, ctx.Err()
	}
}

func (transport *Transport) Close(ctx context.Context) error {
	if transport == nil || transport.state == nil {
		return nil
	}
	return transport.state.Close(ctx)
}

func (transport *Transport) cleanup(ctx context.Context) error {
	transport.stopLocalIO()
	transport.sseWorkers.stop()
	bodyErr := transport.bodies.Close()
	workerErr := transport.sseWorkers.wait(ctx)
	transport.closeClient()
	if bodyErr != nil {
		return bodyErr
	}
	return workerErr
}

func (transport *Transport) forceClose() {
	transport.stopLocalIO()
	transport.sseWorkers.stop()
	// cleanup has already started body closure. Do not wait on body/client
	// sync.Once values here: a hostile Close implementation must not prevent
	// the lifecycle state machine from publishing its bounded final result.
	go transport.closeClient()
}

func (transport *Transport) stopLocalIO() {
	transport.stopOnce.Do(func() {
		if transport.cancelIO != nil {
			transport.cancelIO()
		}
		close(transport.stop)
	})
}

func (transport *Transport) requestContext(caller context.Context) (context.Context, context.CancelFunc) {
	if caller == nil {
		caller = context.Background()
	}
	requestContext, cancel := context.WithCancel(caller)
	if transport == nil || transport.ioContext == nil {
		cancel()
		return requestContext, cancel
	}
	stopOwnerCancellation := context.AfterFunc(transport.ioContext, cancel)
	return requestContext, func() {
		stopOwnerCancellation()
		cancel()
	}
}

func (transport *Transport) closeClient() {
	transport.closeClientOnce.Do(func() {
		if transport.client != nil {
			transport.client.CloseIdleConnections()
		}
	})
}

func closeHTTPResponse(response *stdhttp.Response) error {
	if response == nil || response.Body == nil {
		return nil
	}
	return response.Body.Close()
}
