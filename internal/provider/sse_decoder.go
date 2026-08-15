package provider

import (
	"bufio"
	"errors"
	"io"
	"math"
	"strings"

	"xagent/internal/budget"
)

type sseTermination uint8

type sseEvent struct {
	Event string
	Data  string
}

const (
	sseTerminationReading sseTermination = iota
	sseTerminationProtocol
	sseTerminationUnexpectedEOF
	sseTerminationScanError
	sseTerminationLimitExceeded
)

// sseDecoder synchronously reads one bounded event at a time. It owns no
// goroutine, so cancellation and closure remain under the stream producer's
// lifecycle.
type sseDecoder struct {
	reader *bufio.Reader
	limits *streamLimits

	eventName string
	data      strings.Builder
	hasData   bool
	eventSize int64
	current   sseEvent

	err         error
	termination sseTermination
	done        bool
}

func newSSEDecoder(reader io.Reader, limits *streamLimits) (*sseDecoder, error) {
	if reader == nil {
		return nil, errors.New("provider SSE reader is nil")
	}
	if limits == nil {
		return nil, errors.New("provider SSE limits are nil")
	}
	bufferSize := min(
		limits.eventPreallocation(maxStreamInitialAllocation),
		limits.responsePreallocation(maxStreamInitialAllocation),
	)
	if bufferSize <= 0 {
		return nil, errors.New("provider SSE buffer budget is exhausted")
	}
	return &sseDecoder{
		reader:      bufio.NewReaderSize(reader, bufferSize),
		limits:      limits,
		termination: sseTerminationReading,
	}, nil
}

func (d *sseDecoder) Next() bool {
	if d == nil || d.done || d.err != nil {
		return false
	}
	d.current = sseEvent{}

	for {
		line, readErr := d.readLine()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			d.fail(readErr)
			return false
		}

		if len(line) > 0 {
			d.processLine(line)
		} else if readErr == nil {
			if d.dispatch() {
				return true
			}
			if d.err != nil {
				return false
			}
		}

		if errors.Is(readErr, io.EOF) {
			if d.dispatch() {
				if d.done {
					return true
				}
				d.fail(io.ErrUnexpectedEOF)
				return true
			}
			d.fail(io.ErrUnexpectedEOF)
			return false
		}
	}
}

func (d *sseDecoder) Event() sseEvent {
	if d == nil {
		return sseEvent{}
	}
	return d.current
}

func (d *sseDecoder) Err() error {
	if d == nil {
		return errors.New("provider SSE decoder is nil")
	}
	return d.err
}

func (d *sseDecoder) Termination() sseTermination {
	if d == nil {
		return sseTerminationScanError
	}
	return d.termination
}

// readLine uses ReadSlice so one malicious line cannot trigger an unbounded
// allocation. Every fragment is budgeted before it is appended to line.
func (d *sseDecoder) readLine() ([]byte, error) {
	initialCapacity := d.limits.eventPreallocation(maxStreamInitialAllocation)
	line := make([]byte, 0, initialCapacity)
	for {
		fragment, err := d.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if consumeErr := d.consumeFragment(len(fragment)); consumeErr != nil {
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
			return nil, err
		}
	}
}

func (d *sseDecoder) consumeFragment(size int) error {
	amount := int64(size)
	if err := d.limits.consumeResponse(amount); err != nil {
		return err
	}
	observed := int64(math.MaxInt64)
	if amount <= math.MaxInt64-d.eventSize {
		observed = d.eventSize + amount
	}
	if err := d.limits.consumeEvent(observed); err != nil {
		return err
	}
	d.eventSize = observed
	return nil
}

func (d *sseDecoder) processLine(line []byte) {
	if len(line) == 0 || line[0] == ':' {
		return
	}
	field, value, ok := strings.Cut(string(line), ":")
	if !ok {
		return
	}
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		d.eventName = value
	case "data":
		if d.hasData {
			d.data.WriteByte('\n')
		}
		d.data.WriteString(value)
		d.hasData = true
	}
}

func (d *sseDecoder) dispatch() bool {
	if d.eventName == "" && !d.hasData {
		d.resetEvent()
		return false
	}
	if err := d.limits.consumeEventCount(); err != nil {
		d.fail(err)
		return false
	}

	d.current = sseEvent{Event: d.eventName, Data: d.data.String()}
	protocolDone := d.current.Data == "[DONE]"
	d.resetEvent()
	if protocolDone {
		d.done = true
		d.termination = sseTerminationProtocol
	}
	return true
}

func (d *sseDecoder) resetEvent() {
	d.eventName = ""
	d.data = strings.Builder{}
	d.hasData = false
	d.eventSize = 0
}

func (d *sseDecoder) fail(err error) {
	if d.err != nil || d.done {
		return
	}
	d.err = err
	d.done = true
	var limitErr *budget.LimitError
	switch {
	case errors.As(err, &limitErr):
		d.termination = sseTerminationLimitExceeded
	case errors.Is(err, io.ErrUnexpectedEOF):
		d.termination = sseTerminationUnexpectedEOF
	default:
		d.termination = sseTerminationScanError
	}
}
