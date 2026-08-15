package http

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	stdhttp "net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"xagent/internal/budget"
	mcptransport "xagent/internal/mcpclient/transport"
)

var (
	ErrSSEStreamRead   = errors.New("MCP HTTP SSE stream could not be read")
	ErrSSEStreamClosed = errors.New("MCP HTTP SSE stream ended")
)

const sseReadBufferBytes = 32 * 1024

type transportReceiveResult struct {
	event mcptransport.TransportEvent
	err   error
}

// sseLimits owns all cumulative counters for one SSE response. The configured
// response-byte limit is also the per-event byte ceiling and the maximum event
// count, expressed with distinct dimensions so every reservation is explicit.
type sseLimits struct {
	raw            *sseCounter
	events         *sseCounter
	protocolErrors *sseCounter
	eventBytes     int64
}

type sseCounter struct {
	scope     budget.Scope
	dimension budget.Dimension
	counter   *budget.Counter
}

func resolveSSEProtocolErrorLimit(configured int64) (int64, error) {
	spec, ok := sseBudgetSpec(budget.MCPMaxProtocolErrors)
	if !ok {
		return 0, ErrInvalidConfig
	}
	if configured == 0 {
		return spec.Resolve(nil)
	}
	return spec.Resolve(&configured)
}

func newSSELimits(maxResponseBytes, maxProtocolErrors int64) (*sseLimits, error) {
	responseSpec, ok := sseBudgetSpec(budget.MCPMaxResponseBytes)
	if !ok {
		return nil, ErrInvalidConfig
	}
	protocolSpec, ok := sseBudgetSpec(budget.MCPMaxProtocolErrors)
	if !ok {
		return nil, ErrInvalidConfig
	}
	raw, err := newSSECounter(
		budget.MCPMaxResponseBytes,
		budget.Bytes,
		maxResponseBytes,
		responseSpec.HardCap,
	)
	if err != nil {
		return nil, err
	}
	events, err := newSSECounter(
		budget.MCPMaxResponseBytes,
		budget.Items,
		maxResponseBytes,
		responseSpec.HardCap,
	)
	if err != nil {
		return nil, err
	}
	protocolErrors, err := newSSECounter(
		budget.MCPMaxProtocolErrors,
		budget.ProtocolErrors,
		maxProtocolErrors,
		protocolSpec.HardCap,
	)
	if err != nil {
		return nil, err
	}
	return &sseLimits{
		raw:            raw,
		events:         events,
		protocolErrors: protocolErrors,
		eventBytes:     maxResponseBytes,
	}, nil
}

func sseBudgetSpec(scope budget.Scope) (budget.Spec, bool) {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope == scope {
			return spec, true
		}
	}
	return budget.Spec{}, false
}

func newSSECounter(scope budget.Scope, dimension budget.Dimension, effectiveValue, hardValue int64) (*sseCounter, error) {
	effective, err := budget.NewLimits(budget.Limit{Dimension: dimension, Value: effectiveValue})
	if err != nil {
		return nil, err
	}
	hard, err := budget.NewLimits(budget.Limit{Dimension: dimension, Value: hardValue})
	if err != nil {
		return nil, err
	}
	counter, err := budget.NewCounter(effective, hard)
	if err != nil {
		return nil, err
	}
	return &sseCounter{scope: scope, dimension: dimension, counter: counter}, nil
}

func (limits *sseLimits) consumeRaw(amount int64) error {
	return limits.consume(limits.raw, amount)
}

func (limits *sseLimits) checkEventBytes(amount int64) error {
	if limits == nil || limits.eventBytes <= 0 || amount < 0 {
		return ErrInvalidConfig
	}
	if amount > limits.eventBytes {
		return &budget.LimitError{
			Scope:     string(budget.MCPMaxResponseBytes),
			Dimension: budget.Bytes,
			Limit:     limits.eventBytes,
			Observed:  amount,
		}
	}
	return nil
}

func (limits *sseLimits) consumeEventCount() error {
	return limits.consume(limits.events, 1)
}

func (limits *sseLimits) consumeProtocolError() error {
	return limits.consume(limits.protocolErrors, 1)
}

func (limits *sseLimits) consume(counter *sseCounter, amount int64) error {
	if limits == nil || counter == nil || counter.counter == nil {
		return ErrInvalidConfig
	}
	err := counter.counter.Consume(counter.dimension, amount)
	if err == nil {
		return nil
	}
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) {
		return ErrSSEStreamRead
	}
	return &budget.LimitError{
		Scope:     string(counter.scope),
		Dimension: limitErr.Dimension,
		Limit:     limitErr.Limit,
		Observed:  limitErr.Observed,
	}
}

type sseDecoder struct {
	reader *bufio.Reader
	limits *sseLimits

	data       json.RawMessage
	eventBytes int64
	touched    bool
	hasData    bool
	malformed  bool
	eof        bool
}

func newSSEDecoder(reader io.Reader, limits *sseLimits) (*sseDecoder, error) {
	if reader == nil || limits == nil {
		return nil, ErrInvalidConfig
	}
	return &sseDecoder{
		reader: bufio.NewReaderSize(reader, sseReadBufferBytes),
		limits: limits,
	}, nil
}

// Next skips resynchronizable malformed events until it can return a complete
// JSON frame. Limits, physical read failures, and stream EOF are fatal.
func (decoder *sseDecoder) Next() (json.RawMessage, error) {
	if decoder == nil || decoder.reader == nil || decoder.limits == nil {
		return nil, ErrInvalidConfig
	}
	if decoder.eof {
		return nil, ErrSSEStreamClosed
	}

	for {
		line, readErr := decoder.readLine()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			var limitErr *budget.LimitError
			if errors.As(readErr, &limitErr) {
				return nil, readErr
			}
			return nil, ErrSSEStreamRead
		}
		if len(line) > 0 {
			decoder.processLine(line)
		} else if readErr == nil {
			frame, emitted, dispatchErr := decoder.dispatch()
			if dispatchErr != nil {
				return nil, dispatchErr
			}
			if emitted {
				return frame, nil
			}
		}

		if errors.Is(readErr, io.EOF) {
			decoder.eof = true
			if decoder.touched {
				frame, emitted, dispatchErr := decoder.dispatch()
				if dispatchErr != nil {
					return nil, dispatchErr
				}
				if emitted {
					return frame, nil
				}
			}
			return nil, ErrSSEStreamClosed
		}
	}
}

// readLine uses ReadSlice so a peer-controlled line is split by a fixed-size
// buffer. Each fragment consumes raw and per-event budget before line growth.
func (decoder *sseDecoder) readLine() ([]byte, error) {
	var line []byte
	for {
		fragment, err := decoder.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if consumeErr := decoder.consumeFragment(len(fragment)); consumeErr != nil {
				return nil, consumeErr
			}
			line = append(line, fragment...)
		}

		switch {
		case err == nil:
			line = line[:len(line)-1]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return line, io.EOF
		default:
			return nil, ErrSSEStreamRead
		}
	}
}

func (decoder *sseDecoder) consumeFragment(size int) error {
	amount := int64(size)
	if err := decoder.limits.consumeRaw(amount); err != nil {
		return err
	}
	observed := int64(math.MaxInt64)
	if amount <= math.MaxInt64-decoder.eventBytes {
		observed = decoder.eventBytes + amount
	}
	if err := decoder.limits.checkEventBytes(observed); err != nil {
		return err
	}
	decoder.eventBytes = observed
	return nil
}

func (decoder *sseDecoder) processLine(line []byte) {
	if len(line) == 0 || line[0] == ':' {
		return
	}
	if !utf8.Valid(line) {
		decoder.touched = true
		decoder.malformed = true
		return
	}
	field, value, found := bytes.Cut(line, []byte{':'})
	if !found {
		value = nil
	}
	value = bytes.TrimPrefix(value, []byte{' '})
	switch string(field) {
	case "event":
		decoder.touched = true
	case "data":
		decoder.touched = true
		if decoder.hasData {
			decoder.data = append(decoder.data, '\n')
		}
		decoder.data = append(decoder.data, value...)
		decoder.hasData = true
	}
}

func (decoder *sseDecoder) dispatch() (json.RawMessage, bool, error) {
	if !decoder.touched {
		decoder.resetEvent()
		return nil, false, nil
	}
	if err := decoder.limits.consumeEventCount(); err != nil {
		decoder.resetEvent()
		return nil, false, err
	}

	malformed := decoder.malformed || !decoder.hasData || len(decoder.data) == 0 || !json.Valid(decoder.data)
	if malformed {
		decoder.resetEvent()
		if err := decoder.limits.consumeProtocolError(); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	frame := append(json.RawMessage(nil), decoder.data...)
	decoder.resetEvent()
	return frame, true, nil
}

func (decoder *sseDecoder) resetEvent() {
	decoder.data = nil
	decoder.eventBytes = 0
	decoder.touched = false
	decoder.hasData = false
	decoder.malformed = false
}

func isSSEContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, contentTypeEventStream)
}

type sseWorkerGroup struct {
	mu         sync.Mutex
	accepting  bool
	active     int
	done       chan struct{}
	doneClosed bool
}

func newSSEWorkerGroup() *sseWorkerGroup {
	return &sseWorkerGroup{accepting: true, done: make(chan struct{})}
}

func (group *sseWorkerGroup) start(run func()) bool {
	if group == nil || run == nil {
		return false
	}
	group.mu.Lock()
	if !group.accepting {
		group.mu.Unlock()
		return false
	}
	group.active++
	group.mu.Unlock()
	go func() {
		defer group.finish()
		run()
	}()
	return true
}

func (group *sseWorkerGroup) finish() {
	group.mu.Lock()
	if group.active > 0 {
		group.active--
	}
	group.closeDoneIfIdle()
	group.mu.Unlock()
}

func (group *sseWorkerGroup) stop() {
	if group == nil {
		return
	}
	group.mu.Lock()
	group.accepting = false
	group.closeDoneIfIdle()
	group.mu.Unlock()
}

func (group *sseWorkerGroup) wait(ctx context.Context) error {
	if group == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	group.stop()
	select {
	case <-group.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (group *sseWorkerGroup) closeDoneIfIdle() {
	if !group.accepting && group.active == 0 && !group.doneClosed {
		group.doneClosed = true
		close(group.done)
	}
}

func (transport *Transport) startSSEWorker(response *stdhttp.Response, cancelRequest context.CancelFunc) bool {
	if transport == nil || response == nil || response.Body == nil || transport.sseWorkers == nil || cancelRequest == nil {
		return false
	}
	return transport.sseWorkers.start(func() {
		defer cancelRequest()
		transport.runSSE(response)
	})
}

func (transport *Transport) runSSE(response *stdhttp.Response) {
	defer func() { _ = closeHTTPResponse(response) }()
	limits, err := newSSELimits(transport.maxResponseBytes, transport.maxProtocolErrors)
	if err != nil {
		_ = closeHTTPResponse(response)
		transport.publishSSEFatal(ErrInvalidConfig)
		return
	}
	decoder, err := newSSEDecoder(response.Body, limits)
	if err != nil {
		_ = closeHTTPResponse(response)
		transport.publishSSEFatal(ErrInvalidConfig)
		return
	}
	for {
		frame, nextErr := decoder.Next()
		if nextErr != nil {
			_ = closeHTTPResponse(response)
			transport.publishSSEFatal(nextErr)
			return
		}
		if !transport.publishSSEFrame(frame) {
			return
		}
	}
}

func (transport *Transport) publishSSEFrame(frame json.RawMessage) bool {
	if transport == nil {
		return false
	}
	// SSE events currently arrive from the framing decoder as complete data
	// records. A bound tools/call writer must never be attached after that
	// boundary, so fail closed instead of buffering-and-replaying the payload.
	if transport.captures != nil && transport.captures.HasBindings() {
		transport.publishSSEFatal(ErrSSEStreamRead)
		return false
	}
	select {
	case transport.results <- transportReceiveResult{event: mcptransport.TransportEvent{Frame: frame}}:
		return true
	case <-transport.stop:
		return false
	}
}

func (transport *Transport) publishSSEFatal(err error) {
	if transport == nil || err == nil {
		return
	}
	select {
	case transport.results <- transportReceiveResult{err: err}:
	case <-transport.stop:
	}
}
