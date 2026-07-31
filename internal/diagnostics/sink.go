package diagnostics

import (
	"errors"
	"math"
	"sync"

	"xagent/internal/budget"
	"xagent/internal/redact"
)

// BoundedSink is the process-wide diagnostics boundary injected into owners.
type BoundedSink interface {
	Add(SanitizeInput)
	Snapshot() Snapshot
}

type BoundedSinkOptions struct {
	Redactor      *redact.RuntimeRedactor
	MaxItems      int64
	MaxItemBytes  int64
	MaxTotalBytes int64
}

type boundedSink struct {
	mu           sync.Mutex
	redactor     *redact.RuntimeRedactor
	maxItemBytes int64
	counter      *budget.Counter
	items        []SnapshotItem
	index        map[string]int
	dropped      uint64
}

func NewBoundedSink(options BoundedSinkOptions) (BoundedSink, error) {
	if options.Redactor == nil {
		return nil, errors.New("diagnostics bounded sink requires a runtime redactor")
	}
	maxItems, itemsHard, err := resolveDiagnosticLimit(budget.DiagnosticsMaxItems, options.MaxItems)
	if err != nil {
		return nil, err
	}
	maxItemBytes, _, err := resolveDiagnosticLimit(budget.DiagnosticsMaxItemBytes, options.MaxItemBytes)
	if err != nil {
		return nil, err
	}
	maxTotalBytes, bytesHard, err := resolveDiagnosticLimit(budget.DiagnosticsMaxTotalBytes, options.MaxTotalBytes)
	if err != nil {
		return nil, err
	}
	if maxItemBytes > maxTotalBytes {
		return nil, errors.New("diagnostics item limit exceeds total limit")
	}

	effective, err := budget.NewLimits(
		budget.Limit{Dimension: budget.Items, Value: maxItems},
		budget.Limit{Dimension: budget.Bytes, Value: maxTotalBytes},
	)
	if err != nil {
		return nil, err
	}
	hard, err := budget.NewLimits(
		budget.Limit{Dimension: budget.Items, Value: itemsHard},
		budget.Limit{Dimension: budget.Bytes, Value: bytesHard},
	)
	if err != nil {
		return nil, err
	}
	counter, err := budget.NewCounter(effective, hard)
	if err != nil {
		return nil, err
	}
	return &boundedSink{
		redactor:     options.Redactor,
		maxItemBytes: maxItemBytes,
		counter:      counter,
		index:        make(map[string]int),
	}, nil
}

func resolveDiagnosticLimit(scope budget.Scope, candidate int64) (int64, int64, error) {
	for _, spec := range budget.AllSpecs() {
		if spec.Scope != scope {
			continue
		}
		resolved, err := spec.Resolve(&candidate)
		return resolved, spec.HardCap, err
	}
	return 0, 0, errors.New("diagnostics budget specification is missing")
}

func (s *boundedSink) Add(input SanitizeInput) {
	if s == nil {
		return
	}
	safe := Sanitize(input, SanitizeOptions{Redactor: s.redactor, MaxBytes: int(s.maxItemBytes)})
	identity := safe.stableIdentity()
	retainedBytes := safe.retainedBytes(identity)

	s.mu.Lock()
	defer s.mu.Unlock()
	if index, exists := s.index[identity]; exists {
		if s.items[index].Count == math.MaxUint64 {
			s.incrementDropped()
			return
		}
		s.items[index].Count++
		return
	}
	if retainedBytes <= 0 || retainedBytes > s.maxItemBytes || s.counter.Remaining(budget.Items) < 1 || s.counter.Remaining(budget.Bytes) < retainedBytes {
		s.incrementDropped()
		return
	}
	if err := s.counter.Consume(budget.Items, 1); err != nil {
		s.incrementDropped()
		return
	}
	if err := s.counter.Consume(budget.Bytes, retainedBytes); err != nil {
		// The sink lock and Remaining check make this unreachable unless a
		// counter invariant is broken. Fail closed by dropping the record.
		s.incrementDropped()
		return
	}
	s.index[identity] = len(s.items)
	s.items = append(s.items, SnapshotItem{Diagnostic: safe, Count: 1})
}

func (s *boundedSink) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]SnapshotItem, len(s.items))
	copy(items, s.items)
	return Snapshot{
		items:   items,
		dropped: s.dropped,
		bytes:   s.counter.Snapshot().Used(budget.Bytes),
	}
}

func (s *boundedSink) incrementDropped() {
	if s.dropped < math.MaxUint64 {
		s.dropped++
	}
}
