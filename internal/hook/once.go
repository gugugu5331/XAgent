package hook

import "sync"

type onceStatus uint8

const (
	onceIdle onceStatus = iota
	oncePending
	onceDone
)

type onceState struct {
	mu     sync.Mutex
	states map[string]onceStatus
}

func newOnceState() *onceState { return &onceState{states: map[string]onceStatus{}} }
func (s *onceState) reserve(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states[key] != onceIdle {
		return false
	}
	s.states[key] = oncePending
	return true
}
func (s *onceState) commit(key string) {
	s.mu.Lock()
	if s.states[key] == oncePending {
		s.states[key] = onceDone
	}
	s.mu.Unlock()
}
func (s *onceState) release(key string) {
	s.mu.Lock()
	if s.states[key] == oncePending {
		delete(s.states, key)
	}
	s.mu.Unlock()
}
