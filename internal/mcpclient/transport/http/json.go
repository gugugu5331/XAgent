package http

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strings"

	"xagent/internal/budget"
	"xagent/internal/mcpclient/protocol"
)

var (
	ErrJSONResponseRead      = errors.New("MCP HTTP JSON response could not be read")
	ErrJSONResponseMalformed = errors.New("MCP HTTP JSON response is malformed")
)

const (
	jsonResponseReadChunkBytes = 32 * 1024
	maxConsecutiveEmptyReads   = 100
)

func resolveJSONResponseLimit(configured int64) (int64, error) {
	spec, ok := mcpResponseBudgetSpec()
	if !ok {
		return 0, ErrInvalidConfig
	}
	if configured == 0 {
		return spec.Resolve(nil)
	}
	return spec.Resolve(&configured)
}

// readCapturedJSONResponse decodes directly from the HTTP body. The bounded
// reader reserves every byte before json.Decoder observes it, while the codec
// forwards each completed content block to Capture without building a raw
// response frame.
func readCapturedJSONResponse(reader io.Reader, limit int64, destination io.Writer) (protocol.CapturedCallToolResponse, int64, error) {
	if reader == nil || destination == nil {
		return protocol.CapturedCallToolResponse{}, 0, ErrResponseUnavailable
	}
	counter, err := newJSONResponseCounter(limit)
	if err != nil {
		return protocol.CapturedCallToolResponse{}, 0, ErrInvalidConfig
	}
	bounded := &jsonBudgetReader{reader: reader, counter: counter}
	decoder := json.NewDecoder(bounded)
	decoded, err := protocol.DecodeCapturedCallToolResponse(decoder, destination)
	if err != nil {
		return protocol.CapturedCallToolResponse{}, 0, ErrJSONResponseMalformed
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return protocol.CapturedCallToolResponse{}, 0, ErrJSONResponseMalformed
	}
	return decoded, bounded.bytes, nil
}

type jsonBudgetReader struct {
	reader  io.Reader
	counter *budget.Counter
	bytes   int64
}

func (reader *jsonBudgetReader) Read(destination []byte) (int, error) {
	if reader == nil || reader.reader == nil || reader.counter == nil {
		return 0, ErrJSONResponseRead
	}
	count, err := reader.reader.Read(destination)
	if count < 0 || count > len(destination) {
		return 0, ErrJSONResponseRead
	}
	if count > 0 {
		if consumeErr := consumeJSONResponseBytes(reader.counter, int64(count)); consumeErr != nil {
			return 0, consumeErr
		}
		reader.bytes += int64(count)
	}
	return count, err
}

func mcpResponseBudgetSpec() (budget.Spec, bool) {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope == budget.MCPMaxResponseBytes {
			return spec, true
		}
	}
	return budget.Spec{}, false
}

func newJSONResponseCounter(limit int64) (*budget.Counter, error) {
	spec, ok := mcpResponseBudgetSpec()
	if !ok {
		return nil, ErrInvalidConfig
	}
	resolved, err := spec.Resolve(&limit)
	if err != nil {
		return nil, err
	}
	effective, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: resolved})
	if err != nil {
		return nil, err
	}
	hard, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: spec.HardCap})
	if err != nil {
		return nil, err
	}
	return budget.NewCounter(effective, hard)
}

// readJSONResponse uses a fresh counter for each body. The fixed-size scratch
// buffer is independent of the peer-controlled response size; bytes are
// reserved before they are appended to the frame or passed to JSON parsing.
func readJSONResponse(reader io.Reader, limit int64) (json.RawMessage, error) {
	if reader == nil {
		return nil, ErrResponseUnavailable
	}
	counter, err := newJSONResponseCounter(limit)
	if err != nil {
		return nil, ErrInvalidConfig
	}

	var scratch [jsonResponseReadChunkBytes]byte
	var frame json.RawMessage
	emptyReads := 0
	for {
		read, readErr := reader.Read(scratch[:])
		if read < 0 || read > len(scratch) {
			return nil, ErrJSONResponseRead
		}
		if read > 0 {
			emptyReads = 0
			if err := consumeJSONResponseBytes(counter, int64(read)); err != nil {
				return nil, err
			}
			frame = append(frame, scratch[:read]...)
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= maxConsecutiveEmptyReads {
				return nil, ErrJSONResponseRead
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, ErrJSONResponseRead
		}
	}
	if len(frame) == 0 || !json.Valid(frame) {
		return nil, ErrJSONResponseMalformed
	}
	return frame, nil
}

func consumeJSONResponseBytes(counter *budget.Counter, amount int64) error {
	err := counter.Consume(budget.Bytes, amount)
	if err == nil {
		return nil
	}
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) {
		return ErrJSONResponseRead
	}
	return &budget.LimitError{
		Scope:     string(budget.MCPMaxResponseBytes),
		Dimension: limitErr.Dimension,
		Limit:     limitErr.Limit,
		Observed:  limitErr.Observed,
	}
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, contentTypeJSON)
}
