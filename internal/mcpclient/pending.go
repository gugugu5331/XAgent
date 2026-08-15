package mcpclient

import (
	"errors"
	"sync"

	"xagent/internal/mcpclient/protocol"
)

var (
	errPendingIDInvalid = errors.New("MCP pending request id is invalid")
	errPendingDuplicate = errors.New("MCP pending request id is already registered")
)

type pendingOutcome struct {
	response protocol.RPCResponse
	captured *protocol.CapturedCallToolResponse
	err      error
}

type pendingCall struct {
	method  string
	outcome chan pendingOutcome
}

func newPendingCall(method string) *pendingCall {
	return &pendingCall{
		method:  method,
		outcome: make(chan pendingOutcome, 1),
	}
}

// deliver cannot block after a successful takePending because the channel has
// exactly one slot and only the atomic winner may call it. The channel is
// deliberately never closed: cancellation can abandon the receiver while a
// response winner still completes safely into the buffer.
func (call *pendingCall) deliver(outcome pendingOutcome) {
	call.outcome <- outcome
}

type pendingTable struct {
	mu      sync.Mutex
	entries map[string]*pendingCall
}

func newPendingTable() *pendingTable {
	return &pendingTable{entries: make(map[string]*pendingCall)}
}

func (table *pendingTable) registerPending(id protocol.RPCID, method string) (*pendingCall, error) {
	if table == nil {
		return nil, errPendingIDInvalid
	}
	key, err := id.Key()
	if err != nil {
		return nil, errPendingIDInvalid
	}
	call := newPendingCall(method)
	table.mu.Lock()
	defer table.mu.Unlock()
	if _, exists := table.entries[key]; exists {
		return nil, errPendingDuplicate
	}
	table.entries[key] = call
	return call, nil
}

// takePending atomically transfers completion ownership to exactly one path.
// The returned call is already absent from the table.
func (table *pendingTable) takePending(id protocol.RPCID) (*pendingCall, bool) {
	if table == nil {
		return nil, false
	}
	key, err := id.Key()
	if err != nil {
		return nil, false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	call, exists := table.entries[key]
	if exists {
		delete(table.entries, key)
	}
	return call, exists
}

// takeAllPending gives connection termination exclusive ownership of every
// call that was not already won by response, cancellation, or send failure.
func (table *pendingTable) takeAllPending() []*pendingCall {
	if table == nil {
		return nil
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if len(table.entries) == 0 {
		return nil
	}
	calls := make([]*pendingCall, 0, len(table.entries))
	for key, call := range table.entries {
		calls = append(calls, call)
		delete(table.entries, key)
	}
	return calls
}

func (table *pendingTable) count() int {
	if table == nil {
		return 0
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	return len(table.entries)
}
