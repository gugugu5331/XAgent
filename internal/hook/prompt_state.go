package hook

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

type promptAvailability uint8

const (
	promptAvailable promptAvailability = iota
	promptLeased
)

type promptEntry struct {
	id           uint64
	scope        PromptScope
	sessionID    string
	executionID  string
	turnID       string
	sequence     uint64
	ordinal      int
	source       string
	content      string
	availability promptAvailability
	leaseID      uint64
	leaseOwner   string
}

type promptState struct {
	mu               sync.Mutex
	entries          map[uint64]*promptEntry
	nextID           uint64
	nextLease        uint64
	activeSessions   map[string]bool
	activeExecutions map[string]ExecutionRef
	changed          chan struct{}
	limits           Limits
}

func newPromptState(limits Limits) *promptState {
	return &promptState{entries: map[uint64]*promptEntry{}, activeSessions: map[string]bool{}, activeExecutions: map[string]ExecutionRef{}, changed: make(chan struct{}), limits: normalizeLimits(limits)}
}

func (s *promptState) notifyLocked() { close(s.changed); s.changed = make(chan struct{}) }
func (s *promptState) startSession(id string) {
	s.mu.Lock()
	s.activeSessions[id] = true
	s.notifyLocked()
	s.mu.Unlock()
}
func (s *promptState) startTurn(ref ExecutionRef) {
	s.mu.Lock()
	s.activeExecutions[ref.ExecutionID] = ref
	s.notifyLocked()
	s.mu.Unlock()
}

func (s *promptState) endTurn(ref ExecutionRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.activeExecutions, ref.ExecutionID)
	for id, entry := range s.entries {
		if (entry.scope == ScopeNext && entry.executionID == ref.ExecutionID) || (entry.scope == ScopeTurn && entry.turnID == ref.TurnID) {
			delete(s.entries, id)
			continue
		}
		if entry.scope == ScopeNext && entry.executionID == "" && entry.availability == promptLeased && entry.leaseOwner == ref.ExecutionID {
			entry.availability, entry.leaseID, entry.leaseOwner = promptAvailable, 0, ""
		}
	}
	s.notifyLocked()
}

func (s *promptState) endSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.activeSessions, id)
	for executionID, ref := range s.activeExecutions {
		if ref.SessionID == id {
			delete(s.activeExecutions, executionID)
		}
	}
	for entryID, entry := range s.entries {
		if entry.sessionID == id {
			delete(s.entries, entryID)
		}
	}
	s.notifyLocked()
}

func (s *promptState) append(event *frozenEvent, rule Rule, scope PromptScope, content string) error {
	if event == nil {
		return fmt.Errorf("event unavailable")
	}
	ctx := event.value
	entry := &promptEntry{scope: scope, sequence: ctx.Sequence, ordinal: rule.Source.EffectiveOrdinal, source: rule.Source.Path, content: content}
	if ctx.Session != nil {
		entry.sessionID = ctx.Session.ID
	}
	if ctx.Execution != nil {
		entry.executionID = ctx.Execution.ID
	}
	if ctx.Turn != nil {
		entry.turnID = ctx.Turn.ID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch scope {
	case ScopeNext:
		if entry.executionID != "" {
			if _, ok := s.activeExecutions[entry.executionID]; !ok {
				return fmt.Errorf("execution prompt owner unavailable")
			}
		} else if entry.sessionID == "" || !s.activeSessions[entry.sessionID] {
			return fmt.Errorf("session prompt owner unavailable")
		}
	case ScopeTurn:
		if entry.executionID == "" || entry.turnID == "" {
			return fmt.Errorf("turn prompt owner unavailable")
		}
		if active, ok := s.activeExecutions[entry.executionID]; !ok || active.TurnID != entry.turnID {
			return fmt.Errorf("turn prompt owner unavailable")
		}
	case ScopeSession:
		if entry.sessionID == "" || !s.activeSessions[entry.sessionID] {
			return fmt.Errorf("session prompt owner unavailable")
		}
	default:
		return fmt.Errorf("unknown prompt scope")
	}
	if len(content) > s.limits.PromptFragmentBytes {
		return fmt.Errorf("prompt fragment exceeds limit")
	}
	ownerBytes := len(content)
	for _, existing := range s.entries {
		if samePromptOwner(existing, entry) {
			ownerBytes += len(existing.content)
		}
	}
	if ownerBytes > s.limits.PromptOwnerBytes {
		return fmt.Errorf("prompt owner budget exceeded")
	}
	if !s.visibleBudgetAllowsLocked(entry) {
		return fmt.Errorf("prompt visible budget exceeded")
	}
	s.nextID++
	entry.id = s.nextID
	s.entries[entry.id] = entry
	s.notifyLocked()
	return nil
}

func samePromptOwner(left, right *promptEntry) bool {
	leftExecution := left.executionID != "" && (left.scope == ScopeTurn || left.scope == ScopeNext)
	rightExecution := right.executionID != "" && (right.scope == ScopeTurn || right.scope == ScopeNext)
	if leftExecution || rightExecution {
		return leftExecution && rightExecution && left.executionID == right.executionID
	}
	return left.sessionID != "" && left.sessionID == right.sessionID
}

func (s *promptState) visibleBudgetAllowsLocked(candidate *promptEntry) bool {
	refs := []ExecutionRef{}
	for _, ref := range s.activeExecutions {
		if ref.SessionID == candidate.sessionID {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		refs = append(refs, ExecutionRef{SessionID: candidate.sessionID, ExecutionID: candidate.executionID, TurnID: candidate.turnID})
	}
	for _, ref := range refs {
		total := 0
		for _, entry := range s.entries {
			if promptVisible(entry, ref) {
				total += len(entry.content)
			}
		}
		if promptVisible(candidate, ref) {
			total += len(candidate.content)
		}
		if total > s.limits.PromptOwnerBytes {
			return false
		}
	}
	return true
}

func promptVisible(entry *promptEntry, ref ExecutionRef) bool {
	switch entry.scope {
	case ScopeSession:
		return entry.sessionID == ref.SessionID
	case ScopeTurn:
		return entry.executionID == ref.ExecutionID && entry.turnID == ref.TurnID
	case ScopeNext:
		if entry.executionID != "" {
			return entry.executionID == ref.ExecutionID
		}
		return entry.sessionID == ref.SessionID
	default:
		return false
	}
}

func (s *promptState) acquire(ctx context.Context, ref ExecutionRef) (PromptLease, error) {
	for {
		s.mu.Lock()
		active, ok := s.activeExecutions[ref.ExecutionID]
		if !ok || active.SessionID != ref.SessionID || active.TurnID != ref.TurnID {
			s.mu.Unlock()
			return noopPromptLease{}, nil
		}
		blocked := false
		for _, entry := range s.entries {
			if entry.scope == ScopeNext && promptVisible(entry, ref) && entry.availability == promptLeased {
				blocked = true
				break
			}
		}
		if blocked {
			changed := s.changed
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-changed:
				continue
			}
		}
		s.nextLease++
		leaseID := s.nextLease
		entries := []*promptEntry{}
		nextIDs := []uint64{}
		for _, entry := range s.entries {
			if !promptVisible(entry, ref) {
				continue
			}
			if entry.scope == ScopeNext {
				if entry.availability != promptAvailable {
					continue
				}
				entry.availability, entry.leaseID, entry.leaseOwner = promptLeased, leaseID, ref.ExecutionID
				nextIDs = append(nextIDs, entry.id)
			}
			entries = append(entries, entry)
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].sequence == entries[j].sequence {
				return entries[i].ordinal < entries[j].ordinal
			}
			return entries[i].sequence < entries[j].sequence
		})
		blocks := make([]PromptBlock, 0, len(entries))
		for _, entry := range entries {
			blocks = append(blocks, PromptBlock{Name: fmt.Sprintf("hook:%d:%d", entry.sequence, entry.ordinal), Content: entry.content, Source: entry.source})
		}
		s.mu.Unlock()
		return &promptLease{state: s, id: leaseID, nextIDs: nextIDs, blocks: blocks}, nil
	}
}

type promptLease struct {
	state    *promptState
	id       uint64
	nextIDs  []uint64
	blocks   []PromptBlock
	terminal atomic.Bool
}

func (l *promptLease) Blocks() []PromptBlock {
	if l == nil {
		return nil
	}
	result := make([]PromptBlock, len(l.blocks))
	copy(result, l.blocks)
	return result
}
func (l *promptLease) Commit()  { l.finish(true) }
func (l *promptLease) Release() { l.finish(false) }
func (l *promptLease) finish(commit bool) {
	if l == nil || l.state == nil || !l.terminal.CompareAndSwap(false, true) {
		return
	}
	s := l.state
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range l.nextIDs {
		entry := s.entries[id]
		if entry == nil || entry.leaseID != l.id {
			continue
		}
		if commit {
			delete(s.entries, id)
		} else {
			entry.availability, entry.leaseID, entry.leaseOwner = promptAvailable, 0, ""
		}
	}
	s.notifyLocked()
}
