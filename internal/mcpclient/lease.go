package mcpclient

import (
	"errors"
	"sync"
)

var (
	errLeaseGateUnavailable = errors.New("MCP manager is closing")
	errLeaseGateInvalid     = errors.New("MCP manager lease gate is invalid")
)

type leaseGateState uint8

const (
	leaseGateStateRunning leaseGateState = iota
	leaseGateStateClosing
	leaseGateStateClosed
)

// leaseGate protects Manager call admission without the Add/Wait race of a
// WaitGroup. Running admission, active-count increments, and BeginClose all
// happen under the same lock.
type leaseGate struct {
	mu        sync.Mutex
	state     leaseGateState
	active    uint64
	closeDone chan struct{}
}

func newLeaseGate() *leaseGate {
	return &leaseGate{
		state:     leaseGateStateRunning,
		closeDone: make(chan struct{}),
	}
}

func (gate *leaseGate) Acquire() (*callLease, error) {
	if gate == nil {
		return nil, errLeaseGateInvalid
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.state != leaseGateStateRunning {
		return nil, errLeaseGateUnavailable
	}
	if gate.active == ^uint64(0) {
		return nil, errLeaseGateUnavailable
	}
	gate.active++
	return &callLease{gate: gate}, nil
}

// BeginClose atomically changes admission to Closing and returns the one
// completion signal shared by every closer. Existing leases remain valid and
// their final release performs the Closing-to-Closed transition.
func (gate *leaseGate) BeginClose() <-chan struct{} {
	if gate == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.state == leaseGateStateRunning {
		gate.state = leaseGateStateClosing
	}
	gate.finishCloseLocked()
	return gate.closeDone
}

func (gate *leaseGate) finishCloseLocked() {
	if gate.state != leaseGateStateClosing || gate.active != 0 {
		return
	}
	gate.state = leaseGateStateClosed
	close(gate.closeDone)
}

func (gate *leaseGate) release() {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.active == 0 {
		return
	}
	gate.active--
	gate.finishCloseLocked()
}

func (gate *leaseGate) snapshot() (leaseGateState, uint64) {
	if gate == nil {
		return leaseGateStateClosed, 0
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.state, gate.active
}

type callLease struct {
	gate *leaseGate
	once sync.Once
}

// Release is idempotent so deferred cleanup and explicit rollback cannot
// decrement the active count twice.
func (lease *callLease) Release() {
	if lease == nil || lease.gate == nil {
		return
	}
	lease.once.Do(lease.gate.release)
}
