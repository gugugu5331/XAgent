package app

import "sync"

// lifecycleState is shared by every Bubble Tea Model value copy. Mutable
// process/request ownership must not live solely on Model itself because
// Bubble Tea passes Models by value through Update.
type lifecycleState struct {
	mu        sync.Mutex
	request   *RequestSession
	canceling bool
	terminal  bool
	closeOnce sync.Once
	closeErr  error
}

func newLifecycleState() *lifecycleState {
	return &lifecycleState{}
}

func (s *lifecycleState) begin(request *RequestSession) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.request = request
	s.canceling = false
	s.terminal = false
	s.mu.Unlock()
}

func (s *lifecycleState) replace(request *RequestSession) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.request = request
	s.mu.Unlock()
}

func (s *lifecycleState) active() (*RequestSession, bool, bool) {
	if s == nil {
		return nil, false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.request, s.canceling, s.terminal
}

func (s *lifecycleState) cancel() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	request := s.request
	if request == nil || s.canceling {
		s.mu.Unlock()
		return request != nil
	}
	s.canceling = true
	cancel := request.Cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

func (s *lifecycleState) markTerminal() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.request != nil {
		s.terminal = true
	}
	s.mu.Unlock()
}

func (s *lifecycleState) finish() *RequestSession {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	request := s.request
	s.request = nil
	s.canceling = false
	s.terminal = false
	s.mu.Unlock()
	return request
}

func (s *lifecycleState) close(fn func() error) error {
	if s == nil {
		if fn == nil {
			return nil
		}
		return fn()
	}
	s.closeOnce.Do(func() {
		if fn != nil {
			s.closeErr = fn()
		}
	})
	return s.closeErr
}
