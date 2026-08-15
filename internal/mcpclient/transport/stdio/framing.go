package stdio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sync"
	"unicode/utf8"

	"xagent/internal/budget"
	"xagent/internal/mcpclient/protocol"
)

var (
	// ErrFatalFraming identifies a stdio stream that can no longer be
	// resynchronized. Its concrete error never retains the offending frame.
	ErrFatalFraming = errors.New("MCP stdio fatal framing error")

	errFrameMalformed = errors.New("MCP stdio frame is malformed")
	errFrameRead      = errors.New("MCP stdio frame could not be read")
	errFrameStreamEnd = errors.New("MCP stdio frame stream ended")
)

const stdioFrameReadBufferBytes int64 = 32 * 1024

type fatalFramingError struct {
	cause error
}

func (*fatalFramingError) Error() string {
	return ErrFatalFraming.Error()
}

func (*fatalFramingError) Is(target error) bool {
	return target == ErrFatalFraming
}

func (failure *fatalFramingError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

type framingLimits struct {
	raw        *framingCounter
	events     *framingCounter
	frameBytes int64
}

type framingCounter struct {
	dimension budget.Dimension
	counter   *budget.Counter
}

func newFramingLimits(maxResponseBytes int64) (*framingLimits, error) {
	spec, ok := stdioResponseBudgetSpec()
	if !ok {
		return nil, ErrInvalidConfig
	}
	resolved, err := spec.Resolve(&maxResponseBytes)
	if err != nil {
		return nil, err
	}
	raw, err := newFramingCounter(budget.Bytes, resolved, spec.HardCap)
	if err != nil {
		return nil, err
	}
	events, err := newFramingCounter(budget.Items, resolved, spec.HardCap)
	if err != nil {
		return nil, err
	}
	return &framingLimits{raw: raw, events: events, frameBytes: resolved}, nil
}

func stdioResponseBudgetSpec() (budget.Spec, bool) {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope == budget.MCPMaxResponseBytes {
			return spec, true
		}
	}
	return budget.Spec{}, false
}

func newFramingCounter(dimension budget.Dimension, effective, hard int64) (*framingCounter, error) {
	effectiveLimits, err := budget.NewLimits(budget.Limit{Dimension: dimension, Value: effective})
	if err != nil {
		return nil, err
	}
	hardLimits, err := budget.NewLimits(budget.Limit{Dimension: dimension, Value: hard})
	if err != nil {
		return nil, err
	}
	counter, err := budget.NewCounter(effectiveLimits, hardLimits)
	if err != nil {
		return nil, err
	}
	return &framingCounter{dimension: dimension, counter: counter}, nil
}

func (limits *framingLimits) consumeRaw(amount int64) error {
	return limits.consume(limits.raw, amount)
}

func (limits *framingLimits) consumeEvent() error {
	return limits.consume(limits.events, 1)
}

func (limits *framingLimits) checkFrameBytes(observed int64) error {
	if limits == nil || limits.frameBytes <= 0 || observed < 0 {
		return ErrInvalidConfig
	}
	if observed <= limits.frameBytes {
		return nil
	}
	return &budget.LimitError{
		Scope:     string(budget.MCPMaxResponseBytes),
		Dimension: budget.Bytes,
		Limit:     limits.frameBytes,
		Observed:  observed,
	}
}

func (limits *framingLimits) consume(counter *framingCounter, amount int64) error {
	if limits == nil || counter == nil || counter.counter == nil {
		return ErrInvalidConfig
	}
	err := counter.counter.Consume(counter.dimension, amount)
	if err == nil {
		return nil
	}
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) {
		return errFrameRead
	}
	return &budget.LimitError{
		Scope:     string(budget.MCPMaxResponseBytes),
		Dimension: limitErr.Dimension,
		Limit:     limitErr.Limit,
		Observed:  limitErr.Observed,
	}
}

// newlineDecoder owns the cumulative budget for one shared stdout stream.
// Its mutex serializes accidental concurrent Receive calls and makes the first
// fatal error sticky, so no caller can resume reading an unrecoverable stream.
type newlineDecoder struct {
	mu            sync.Mutex
	reader        *bufio.Reader
	limits        *framingLimits
	fatal         error
	capturedBytes int64
}

func newNewlineDecoder(reader io.Reader, maxResponseBytes int64) (*newlineDecoder, error) {
	if reader == nil {
		return nil, ErrInvalidConfig
	}
	limits, err := newFramingLimits(maxResponseBytes)
	if err != nil {
		return nil, err
	}
	bufferBytes := stdioFrameReadBufferBytes
	if maxResponseBytes < bufferBytes {
		bufferBytes = maxResponseBytes + 1
	}
	return &newlineDecoder{
		reader: bufio.NewReaderSize(reader, int(bufferBytes)),
		limits: limits,
	}, nil
}

func (decoder *newlineDecoder) Next(ctx context.Context) (json.RawMessage, error) {
	if decoder == nil || decoder.reader == nil || decoder.limits == nil {
		return nil, ErrInvalidConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}

	decoder.mu.Lock()
	defer decoder.mu.Unlock()
	if decoder.fatal != nil {
		return nil, decoder.fatal
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	frame, err := decoder.readFrame(ctx)
	if err != nil {
		return nil, decoder.fail(err)
	}
	// Count the complete frame before JSON validation. A malformed frame still
	// consumes an event and can never be used to bypass the cumulative count.
	if err := decoder.limits.consumeEvent(); err != nil {
		return nil, decoder.fail(err)
	}
	if len(frame) == 0 || !utf8.Valid(frame) || !json.Valid(frame) {
		return nil, decoder.fail(errFrameMalformed)
	}
	return append(json.RawMessage(nil), frame...), nil
}

// NextCaptured parses one response directly from the newline-framed stdout
// stream. It never constructs a raw frame: id must arrive before result so a
// bound writer can be selected before the first user-content byte is decoded.
func (decoder *newlineDecoder) NextCaptured(ctx context.Context, resolve func(protocol.RPCID) (io.Writer, bool)) (protocol.CapturedCallToolResponse, error) {
	if decoder == nil || decoder.reader == nil || decoder.limits == nil || resolve == nil {
		return protocol.CapturedCallToolResponse{}, ErrInvalidConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	decoder.mu.Lock()
	defer decoder.mu.Unlock()
	if decoder.fatal != nil {
		return protocol.CapturedCallToolResponse{}, decoder.fatal
	}
	stream := &newlineCaptureReader{decoder: decoder, ctx: ctx}
	jsonDecoder := json.NewDecoder(stream)
	response, err := protocol.DecodeCapturedCallToolResponseRouted(jsonDecoder, resolve)
	if err != nil {
		return protocol.CapturedCallToolResponse{}, decoder.fail(errFrameMalformed)
	}
	var trailing any
	if err := jsonDecoder.Decode(&trailing); err != io.EOF {
		return protocol.CapturedCallToolResponse{}, decoder.fail(errFrameMalformed)
	}
	if err := decoder.limits.consumeEvent(); err != nil {
		return protocol.CapturedCallToolResponse{}, decoder.fail(err)
	}
	decoder.capturedBytes = stream.bytes
	return response, nil
}

func (decoder *newlineDecoder) lastCapturedBytes() int64 {
	if decoder == nil {
		return 0
	}
	return decoder.capturedBytes
}

type newlineCaptureReader struct {
	decoder *newlineDecoder
	ctx     context.Context
	bytes   int64
	ended   bool
}

func (reader *newlineCaptureReader) Read(destination []byte) (int, error) {
	if reader == nil || reader.decoder == nil || reader.decoder.reader == nil || reader.ctx == nil {
		return 0, ErrInvalidConfig
	}
	if reader.ended {
		return 0, io.EOF
	}
	for count := 0; count < len(destination); {
		if err := reader.ctx.Err(); err != nil {
			return count, err
		}
		value, err := reader.decoder.reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return count, errFrameStreamEnd
			}
			return count, errFrameRead
		}
		reader.bytes++
		if err := reader.decoder.limits.consumeRaw(1); err != nil {
			return count, err
		}
		if err := reader.decoder.limits.checkFrameBytes(reader.bytes); err != nil {
			return count, err
		}
		if value == '\n' {
			reader.ended = true
			if count == 0 {
				return 0, io.EOF
			}
			return count, nil
		}
		if value == '\r' {
			continue
		}
		destination[count] = value
		count++
		// Return promptly after each byte. This prevents a decoder read-ahead
		// from waiting for the frame tail before it can publish a completed
		// content block to Capture.
		return count, nil
	}
	return len(destination), nil
}

// readFrame uses ReadSlice so peer-controlled frames are exposed only in a
// fixed-size fragment. Raw wire bytes include the LF or CRLF delimiter; this
// makes every delimiter consume both byte and event budget. Raw and
// single-frame byte reservations happen before any fragment is appended.
func (decoder *newlineDecoder) readFrame(ctx context.Context) (json.RawMessage, error) {
	var frame json.RawMessage
	var frameBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fragment, readErr := decoder.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			amount := int64(len(fragment))
			if err := decoder.limits.consumeRaw(amount); err != nil {
				return nil, err
			}
			observed := int64(math.MaxInt64)
			if amount <= math.MaxInt64-frameBytes {
				observed = frameBytes + amount
			}
			if err := decoder.limits.checkFrameBytes(observed); err != nil {
				return nil, err
			}
			frameBytes = observed

			end := len(fragment)
			if readErr == nil {
				if fragment[end-1] != '\n' {
					return nil, errFrameRead
				}
				end--
			}
			frame = append(frame, fragment[:end]...)
		}

		switch {
		case readErr == nil:
			if len(frame) > 0 && frame[len(frame)-1] == '\r' {
				frame = frame[:len(frame)-1]
			}
			return frame, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case errors.Is(readErr, io.EOF) && len(fragment) == 0:
			return nil, errFrameStreamEnd
		case errors.Is(readErr, io.EOF):
			return nil, errFrameMalformed
		default:
			return nil, errFrameRead
		}
	}
}

func (decoder *newlineDecoder) fail(cause error) error {
	if decoder.fatal == nil {
		decoder.fatal = &fatalFramingError{cause: cause}
	}
	return decoder.fatal
}
