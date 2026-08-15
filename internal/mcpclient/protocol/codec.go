package protocol

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"

	"xagent/internal/budget"
)

var (
	ErrInvalidDecodeTarget = errors.New("invalid MCP protocol decode target")
	ErrMalformedFrame      = errors.New("malformed MCP protocol frame")
	ErrFrameEncode         = errors.New("failed to encode MCP protocol frame")
	ErrResultCapture       = errors.New("MCP result output capture failed")
)

// Decode consumes the complete frame from the cumulative byte budget before
// parsing it. Decode failures expose only fixed protocol errors, never the raw
// frame, and the target is published only after a complete successful parse.
func Decode(counter *budget.Counter, frame json.RawMessage, target any) error {
	targetValue := reflect.ValueOf(target)
	if !targetValue.IsValid() || targetValue.Kind() != reflect.Pointer || targetValue.IsNil() {
		return ErrInvalidDecodeTarget
	}
	if err := counter.Consume(budget.Bytes, int64(len(frame))); err != nil {
		return err
	}

	temporary := reflect.New(targetValue.Elem().Type())
	if err := json.Unmarshal(frame, temporary.Interface()); err != nil {
		return ErrMalformedFrame
	}
	targetValue.Elem().Set(temporary.Elem())
	return nil
}

// Encode produces one raw JSON frame. Marshal errors are intentionally
// collapsed so custom marshalers cannot copy sensitive input into errors.
func Encode(value any) (json.RawMessage, error) {
	frame, err := json.Marshal(value)
	if err != nil {
		return nil, ErrFrameEncode
	}
	return json.RawMessage(frame), nil
}

// DecodeCallToolResult decodes a tools/call result while forwarding user
// output to destination as each content block is decoded.  The complete
// CallToolResult is not published until all output has crossed the supplied
// writer; this keeps the production MCP path on the same first-byte Capture
// boundary as the other byte-stream producers.
//
// Only the protocol DTO is accepted here.  The writer is deliberately a
// narrow io.Writer so this package cannot obtain a Store, Reader, or Capture
// owner of its own.
func DecodeCallToolResult(frame json.RawMessage, destination io.Writer) (CallToolResult, error) {
	if destination == nil {
		return CallToolResult{}, ErrInvalidDecodeTarget
	}
	var envelope struct {
		Content           []json.RawMessage `json:"content,omitempty"`
		StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
		IsError           bool              `json:"isError,omitempty"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return CallToolResult{}, ErrMalformedFrame
	}
	result := CallToolResult{IsError: envelope.IsError}
	wrotePart := false
	writePart := func(value string) error {
		if wrotePart {
			if _, err := io.WriteString(destination, "\n"); err != nil {
				return ErrResultCapture
			}
		}
		wrotePart = true
		if _, err := io.WriteString(destination, value); err != nil {
			return ErrResultCapture
		}
		return nil
	}
	nonText := false
	for _, rawBlock := range envelope.Content {
		var block ContentBlock
		if err := json.Unmarshal(rawBlock, &block); err != nil {
			return CallToolResult{}, ErrMalformedFrame
		}
		if block.Type == "text" {
			if err := writePart(block.Text); err != nil {
				return CallToolResult{}, err
			}
		} else {
			nonText = true
		}
		result.Content = append(result.Content, block)
	}
	if nonText {
		if err := writePart("[non-text MCP content omitted]"); err != nil {
			return CallToolResult{}, err
		}
	}
	if len(envelope.StructuredContent) > 0 && string(envelope.StructuredContent) != "null" {
		if err := writePart("[structured MCP content omitted]"); err != nil {
			return CallToolResult{}, err
		}
		var structured any
		if err := json.Unmarshal(envelope.StructuredContent, &structured); err != nil {
			return CallToolResult{}, ErrMalformedFrame
		}
		result.StructuredContent = structured
	}
	return result, nil
}
