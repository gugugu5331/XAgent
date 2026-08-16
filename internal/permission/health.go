package permission

import (
	"sort"
	"sync"
)

// SafetyEpochAdvancer invalidates execution capabilities issued under an
// earlier permission safety state.
type SafetyEpochAdvancer interface {
	AdvanceSafetyEpoch()
}

// Health tracks whether any permission configuration layer is corrupt. It
// retains only layer kinds: raw loader errors must not cross the diagnostic or
// authorization boundary.
type Health struct {
	mu sync.RWMutex

	advancer      SafetyEpochAdvancer
	degraded      bool
	corruptLayers map[SourceKind]struct{}
}

func NewHealth(advancer SafetyEpochAdvancer) *Health {
	return &Health{
		advancer:      advancer,
		corruptLayers: make(map[SourceKind]struct{}),
	}
}

// Update replaces the observed corrupt-layer set. Every newly degraded state
// or change to that corrupt set advances the safety epoch before returning.
// Recovery cannot revive tickets because epoch advancement removes them from
// the issuing authority.
func (h *Health) Update(loadErrors []LoadError) {
	if h == nil {
		return
	}
	next := make(map[SourceKind]struct{}, len(loadErrors))
	for _, loadError := range loadErrors {
		kind := loadError.Source.Kind
		if kind == "" {
			kind = SourceNone
		}
		next[kind] = struct{}{}
	}

	h.mu.Lock()
	changedCorruption := len(next) > 0 && !sameCorruptLayers(h.corruptLayers, next)
	h.degraded = len(next) > 0
	h.corruptLayers = next
	if changedCorruption && h.advancer != nil {
		h.advancer.AdvanceSafetyEpoch()
	}
	h.mu.Unlock()
}

func (h *Health) Degraded() bool {
	if h == nil {
		return false
	}
	h.mu.RLock()
	degraded := h.degraded
	h.mu.RUnlock()
	return degraded
}

func (h *Health) corruptLayerKinds() []SourceKind {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	kinds := make([]SourceKind, 0, len(h.corruptLayers))
	for kind := range h.corruptLayers {
		kinds = append(kinds, kind)
	}
	h.mu.RUnlock()
	sort.Slice(kinds, func(first, second int) bool { return kinds[first] < kinds[second] })
	return kinds
}

func sameCorruptLayers(left, right map[SourceKind]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for layer := range left {
		if _, ok := right[layer]; !ok {
			return false
		}
	}
	return true
}

// isConservativeReadOnlyTool is intentionally a closed built-in allowlist.
// Remote MCP and unknown tools never inherit read-only status from names or
// untrusted annotations.
func isConservativeReadOnlyTool(toolName string) bool {
	switch toolName {
	case "Read", "Glob", "Grep":
		return true
	default:
		return false
	}
}
