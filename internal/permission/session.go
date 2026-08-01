package permission

import "sync"

type Session struct {
	mu    sync.RWMutex
	rules []Rule
}

func NewSession() *Session {
	return &Session{}
}

func (s *Session) Add(rule Rule) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.rules = appendUniqueRule(s.rules, rule)
	s.mu.Unlock()
}

func (s *Session) Rules() []Rule {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.rules) == 0 {
		return nil
	}
	out := make([]Rule, len(s.rules))
	copy(out, s.rules)
	return out
}
