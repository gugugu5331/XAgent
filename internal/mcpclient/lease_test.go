package mcpclient

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLeaseAcquireCloseRace(t *testing.T) {
	const (
		iterations = 128
		contenders = 16
	)
	for iteration := range iterations {
		gate := newLeaseGate()
		held, err := gate.Acquire()
		if err != nil {
			t.Fatalf("iteration %d acquire held lease: %v", iteration, err)
		}

		start := make(chan struct{})
		closeResult := make(chan (<-chan struct{}), 1)
		go func() {
			<-start
			closeResult <- gate.BeginClose()
		}()

		acquired := make(chan *callLease, contenders)
		var accepted atomic.Int64
		var rejected atomic.Int64
		var waiters sync.WaitGroup
		for range contenders {
			waiters.Add(1)
			go func() {
				defer waiters.Done()
				<-start
				lease, err := gate.Acquire()
				switch {
				case err == nil:
					accepted.Add(1)
					acquired <- lease
				case errors.Is(err, errLeaseGateUnavailable):
					rejected.Add(1)
				default:
					t.Errorf("iteration %d Acquire error = %v", iteration, err)
				}
			}()
		}

		close(start)
		closeDone := <-closeResult
		waiters.Wait()
		close(acquired)

		if total := accepted.Load() + rejected.Load(); total != contenders {
			t.Fatalf("iteration %d completed contenders = %d, want %d", iteration, total, contenders)
		}
		if _, err := gate.Acquire(); !errors.Is(err, errLeaseGateUnavailable) {
			t.Fatalf("iteration %d Acquire after BeginClose = %v", iteration, err)
		}
		select {
		case <-closeDone:
			t.Fatalf("iteration %d close completed before held leases released", iteration)
		default:
		}

		for lease := range acquired {
			lease.Release()
			lease.Release()
		}
		held.Release()
		held.Release()
		select {
		case <-closeDone:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d timed out waiting for closeDone", iteration)
		}

		if repeated := gate.BeginClose(); repeated != closeDone {
			t.Fatalf("iteration %d repeated BeginClose returned a different signal", iteration)
		}
		state, active := gate.snapshot()
		if state != leaseGateStateClosed || active != 0 {
			t.Fatalf("iteration %d final state/active = %d/%d, want Closed/0", iteration, state, active)
		}
	}
}
