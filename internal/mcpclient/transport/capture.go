package transport

import (
	"encoding/json"
	"errors"
	"io"
	"sync"

	"xagent/internal/mcpclient/protocol"
)

var ErrOutputCaptureBinding = errors.New("MCP output capture binding is invalid")

// OutputCaptureBindings owns only borrowed writers.  A binding is consumed
// exactly once when its matching response arrives, preventing both output
// duplication and a stale writer from receiving a later call's output.
type OutputCaptureBindings struct {
	mu      sync.Mutex
	writers map[string]io.Writer
}

func NewOutputCaptureBindings() *OutputCaptureBindings {
	return &OutputCaptureBindings{writers: make(map[string]io.Writer)}
}

func (bindings *OutputCaptureBindings) Bind(frame json.RawMessage, destination io.Writer) error {
	if bindings == nil || destination == nil {
		return ErrOutputCaptureBinding
	}
	var request protocol.RPCRequest
	if err := json.Unmarshal(frame, &request); err != nil || request.Method != "tools/call" {
		return ErrOutputCaptureBinding
	}
	key, err := request.ID.Key()
	if err != nil {
		return ErrOutputCaptureBinding
	}
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	if _, exists := bindings.writers[key]; exists {
		return ErrOutputCaptureBinding
	}
	bindings.writers[key] = destination
	return nil
}

func (bindings *OutputCaptureBindings) Remove(frame json.RawMessage) {
	if bindings == nil {
		return
	}
	var request protocol.RPCRequest
	if err := json.Unmarshal(frame, &request); err != nil {
		return
	}
	key, err := request.ID.Key()
	if err != nil {
		return
	}
	bindings.mu.Lock()
	delete(bindings.writers, key)
	bindings.mu.Unlock()
}

// TakeRequest transfers a writer to a synchronous transport response reader
// before that reader performs its first body read.
func (bindings *OutputCaptureBindings) TakeRequest(frame json.RawMessage) (io.Writer, error) {
	if bindings == nil {
		return nil, ErrOutputCaptureBinding
	}
	var request protocol.RPCRequest
	if err := json.Unmarshal(frame, &request); err != nil {
		return nil, ErrOutputCaptureBinding
	}
	key, err := request.ID.Key()
	if err != nil {
		return nil, ErrOutputCaptureBinding
	}
	bindings.mu.Lock()
	destination := bindings.writers[key]
	delete(bindings.writers, key)
	bindings.mu.Unlock()
	return destination, nil
}

// TakeID is used by framed transports that learn the response id before its
// result field. The writer is removed before the first content byte is read.
func (bindings *OutputCaptureBindings) TakeID(id protocol.RPCID) (io.Writer, bool) {
	if bindings == nil {
		return nil, false
	}
	key, err := id.Key()
	if err != nil {
		return nil, false
	}
	bindings.mu.Lock()
	destination, ok := bindings.writers[key]
	if ok {
		delete(bindings.writers, key)
	}
	bindings.mu.Unlock()
	return destination, ok
}

func (bindings *OutputCaptureBindings) HasBindings() bool {
	if bindings == nil {
		return false
	}
	bindings.mu.Lock()
	defer bindings.mu.Unlock()
	return len(bindings.writers) != 0
}

// CaptureResponse writes the matching tools/call payload before the caller
// receives the frame.  An unmatched response remains an ordinary protocol
// frame.  Failures deliberately remove the binding: it belongs to this one
// wire response and can never be reused.
func (bindings *OutputCaptureBindings) CaptureResponse(frame json.RawMessage) (*protocol.CapturedCallToolResponse, error) {
	if bindings == nil {
		return nil, nil
	}
	id, err := protocol.ResponseID(frame)
	if err != nil {
		return nil, nil // connection owns malformed-frame failure semantics
	}
	key, err := id.Key()
	if err != nil {
		return nil, nil
	}
	bindings.mu.Lock()
	destination, exists := bindings.writers[key]
	if exists {
		delete(bindings.writers, key)
	}
	bindings.mu.Unlock()
	if !exists {
		return nil, nil
	}
	captured, err := protocol.CaptureCallToolResponse(frame, destination)
	if err != nil {
		return nil, err
	}
	return &captured, nil
}
