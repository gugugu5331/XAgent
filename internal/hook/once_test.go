package hook

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestOnceTransitions(t *testing.T) {
	state := newOnceState()
	if !state.reserve("r") {
		t.Fatal("initial reserve")
	}
	if state.reserve("r") {
		t.Fatal("pending reserve")
	}
	state.release("r")
	if !state.reserve("r") {
		t.Fatal("release did not reset")
	}
	state.commit("r")
	if state.reserve("r") {
		t.Fatal("done reserve")
	}
}

func TestOnceConcurrent(t *testing.T) {
	state := newOnceState()
	start := make(chan struct{})
	var winners atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 100; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			if state.reserve("r") {
				winners.Add(1)
			}
		}()
	}
	close(start)
	group.Wait()
	if winners.Load() != 1 {
		t.Fatalf("winners = %d", winners.Load())
	}
}
