package permission

type Session struct {
	rules []Rule
}

func NewSession() *Session {
	return &Session{}
}

func (s *Session) Add(rule Rule) {
	if s == nil {
		return
	}
	s.rules = append(s.rules, rule)
}

func (s *Session) Rules() []Rule {
	if s == nil || len(s.rules) == 0 {
		return nil
	}
	out := make([]Rule, len(s.rules))
	copy(out, s.rules)
	return out
}
