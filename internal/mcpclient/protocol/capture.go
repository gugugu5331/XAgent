package protocol

import (
	"encoding/json"
	"io"
)

// CapturedCallToolResponse is the payload a production transport gives the
// connection after it has written the result to its operation-local Capture.
// It deliberately contains no raw result frame.
type CapturedCallToolResponse struct {
	JSONRPC  string
	ID       RPCID
	HasError bool
	Result   CallToolResult
}

var ErrCaptureRoute = ErrInvalidDecodeTarget

// CaptureCallToolResponse writes a tools/call result before a transport makes
// its frame available to the connection receive loop.  The response id is
// decoded separately from Result so callers can select the operation-local
// writer without constructing or retaining a complete RPCResponse DTO.
func CaptureCallToolResponse(frame json.RawMessage, destination io.Writer) (CapturedCallToolResponse, error) {
	if destination == nil {
		return CapturedCallToolResponse{}, ErrInvalidDecodeTarget
	}
	return DecodeCapturedCallToolResponse(json.NewDecoder(bytesReader(frame)), destination)
}

// DecodeCapturedCallToolResponse incrementally decodes one RPC response. In
// particular, each tools/call content block crosses destination as soon as
// its JSON value is complete; a complete raw response frame is never required.
func DecodeCapturedCallToolResponse(decoder *json.Decoder, destination io.Writer) (CapturedCallToolResponse, error) {
	if decoder == nil || destination == nil {
		return CapturedCallToolResponse{}, ErrInvalidDecodeTarget
	}
	return decodeCapturedCallToolResponse(decoder, func(RPCID) (io.Writer, bool) { return destination, true })
}

// DecodeCapturedCallToolResponseRouted resolves the writer immediately after
// parsing id. A result that appears before id is rejected rather than being
// buffered: stdio must never fall back to a complete raw frame just to route
// user output.
func DecodeCapturedCallToolResponseRouted(decoder *json.Decoder, resolve func(RPCID) (io.Writer, bool)) (CapturedCallToolResponse, error) {
	if decoder == nil || resolve == nil {
		return CapturedCallToolResponse{}, ErrInvalidDecodeTarget
	}
	return decodeCapturedCallToolResponse(decoder, resolve)
}

func decodeCapturedCallToolResponse(decoder *json.Decoder, resolve func(RPCID) (io.Writer, bool)) (CapturedCallToolResponse, error) {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return CapturedCallToolResponse{}, ErrMalformedFrame
	}
	response := CapturedCallToolResponse{}
	resultSeen := false
	var destination io.Writer
	for decoder.More() {
		nameToken, tokenErr := decoder.Token()
		name, ok := nameToken.(string)
		if tokenErr != nil || !ok {
			return CapturedCallToolResponse{}, ErrMalformedFrame
		}
		switch name {
		case "jsonrpc":
			if err := decoder.Decode(&response.JSONRPC); err != nil {
				return CapturedCallToolResponse{}, ErrMalformedFrame
			}
		case "id":
			if err := decoder.Decode(&response.ID); err != nil {
				return CapturedCallToolResponse{}, ErrMalformedFrame
			}
			var ok bool
			destination, ok = resolve(response.ID)
			if !ok || destination == nil {
				return CapturedCallToolResponse{}, ErrCaptureRoute
			}
		case "error":
			nonNull, err := skipAndReportNonNull(decoder)
			if err != nil {
				return CapturedCallToolResponse{}, ErrMalformedFrame
			}
			response.HasError = nonNull
		case "result":
			if destination == nil {
				return CapturedCallToolResponse{}, ErrCaptureRoute
			}
			result, decodeErr := decodeCallToolResultStream(decoder, destination)
			if decodeErr != nil {
				return CapturedCallToolResponse{}, decodeErr
			}
			response.Result, resultSeen = result, true
		default:
			if err := skipJSONValue(decoder); err != nil {
				return CapturedCallToolResponse{}, ErrMalformedFrame
			}
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || !resultSeen {
		return CapturedCallToolResponse{}, ErrMalformedFrame
	}
	if err := ValidateRPCResponse(RPCResponse{JSONRPC: response.JSONRPC, ID: response.ID}); err != nil {
		return CapturedCallToolResponse{}, ErrMalformedFrame
	}
	return response, nil
}

// ResponseID returns the correlation id without decoding the result payload.
// It lets transports select a writer before forwarding the frame upward.
func ResponseID(frame json.RawMessage) (RPCID, error) {
	var envelope struct {
		JSONRPC string `json:"jsonrpc"`
		ID      RPCID  `json:"id"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return RPCID{}, ErrMalformedFrame
	}
	if err := ValidateRPCResponse(RPCResponse{JSONRPC: envelope.JSONRPC, ID: envelope.ID}); err != nil {
		return RPCID{}, ErrMalformedFrame
	}
	return envelope.ID, nil
}

func decodeCallToolResultStream(decoder *json.Decoder, destination io.Writer) (CallToolResult, error) {
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return CallToolResult{}, ErrMalformedFrame
	}
	result := CallToolResult{}
	wrotePart, nonText, structuredSeen := false, false, false
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
	for decoder.More() {
		nameToken, tokenErr := decoder.Token()
		name, ok := nameToken.(string)
		if tokenErr != nil || !ok {
			return CallToolResult{}, ErrMalformedFrame
		}
		switch name {
		case "content":
			token, err := decoder.Token()
			if err != nil || token != json.Delim('[') {
				return CallToolResult{}, ErrMalformedFrame
			}
			for decoder.More() {
				var block struct {
					Type string `json:"type"`
					Text string `json:"text,omitempty"`
				}
				if err := decoder.Decode(&block); err != nil {
					return CallToolResult{}, ErrMalformedFrame
				}
				if block.Type == "text" {
					if err := writePart(block.Text); err != nil {
						return CallToolResult{}, err
					}
				} else {
					nonText = true
				}
				// The captured path returns only non-sensitive block metadata.
				// User text already crossed destination and is never retained in
				// the DTO that escapes the transport.
				result.Content = append(result.Content, ContentBlock{Type: block.Type})
			}
			if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
				return CallToolResult{}, ErrMalformedFrame
			}
		case "isError":
			if err := decoder.Decode(&result.IsError); err != nil {
				return CallToolResult{}, ErrMalformedFrame
			}
		case "structuredContent":
			if err := skipJSONValue(decoder); err != nil {
				return CallToolResult{}, ErrMalformedFrame
			}
			structuredSeen = true
		default:
			if err := skipJSONValue(decoder); err != nil {
				return CallToolResult{}, ErrMalformedFrame
			}
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return CallToolResult{}, ErrMalformedFrame
	}
	if nonText {
		if err := writePart("[non-text MCP content omitted]"); err != nil {
			return CallToolResult{}, err
		}
	}
	if structuredSeen {
		if err := writePart("[structured MCP content omitted]"); err != nil {
			return CallToolResult{}, err
		}
	}
	return result, nil
}

func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || (delimiter != '{' && delimiter != '[') {
		return nil
	}
	for decoder.More() {
		if delimiter == '{' {
			if _, err := decoder.Token(); err != nil { // object key
				return err
			}
		}
		if err := skipJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func skipAndReportNonNull(decoder *json.Decoder) (bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return false, err
	}
	if token == nil {
		return false, nil
	}
	delimiter, ok := token.(json.Delim)
	if !ok || (delimiter != '{' && delimiter != '[') {
		return true, nil
	}
	for decoder.More() {
		if delimiter == '{' {
			if _, err := decoder.Token(); err != nil {
				return false, err
			}
		}
		if err := skipJSONValue(decoder); err != nil {
			return false, err
		}
	}
	_, err = decoder.Token()
	return true, err
}

// bytesReader is kept here to make CaptureCallToolResponse use the exact same
// streaming state machine as real transports.
func bytesReader(value []byte) io.Reader { return &sliceReader{value: value} }

type sliceReader struct{ value []byte }

func (reader *sliceReader) Read(destination []byte) (int, error) {
	if len(reader.value) == 0 {
		return 0, io.EOF
	}
	count := copy(destination, reader.value)
	reader.value = reader.value[count:]
	return count, nil
}
