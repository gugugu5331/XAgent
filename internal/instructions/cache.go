package instructions

import (
	"errors"
	"sync"

	"xagent/internal/budget"
)

// CachedExpansion is an immutable-by-convention expansion snapshot. Cache
// operations copy slice fields so callers cannot mutate stored graph data.
type CachedExpansion struct {
	Content string
	Files   []FileVersion
	Edges   []IncludeEdge
}

// ExpansionCache stores completed instruction expansions by graph key.
type ExpansionCache struct {
	mu      sync.RWMutex
	entries map[CacheKey]CachedExpansion
}

// Store inserts or replaces a completed expansion using defensive copies.
func (c *ExpansionCache) Store(key CacheKey, expansion CachedExpansion) error {
	if c == nil {
		return errors.New("instruction expansion cache is nil")
	}
	if !key.valid() {
		return errors.New("instruction expansion cache key is invalid")
	}
	copyExpansion := cloneCachedExpansion(expansion)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[CacheKey]CachedExpansion)
	}
	c.entries[key] = copyExpansion
	return nil
}

// Lookup returns a defensive copy of a cached expansion. A hit reserves its
// full expanded size against the caller's operation budget before exposing
// any cached content. A failed reservation returns no expansion data.
func (c *ExpansionCache) Lookup(key CacheKey, counter *budget.Counter) (CachedExpansion, bool, error) {
	if c == nil {
		return CachedExpansion{}, false, errors.New("instruction expansion cache is nil")
	}
	if !key.valid() {
		return CachedExpansion{}, false, errors.New("instruction expansion cache key is invalid")
	}
	c.mu.RLock()
	expansion, found := c.entries[key]
	if found {
		expansion = cloneCachedExpansion(expansion)
	}
	c.mu.RUnlock()
	if !found {
		return CachedExpansion{}, false, nil
	}
	if err := counter.Consume(budget.ExpandedBytes, int64(len(expansion.Content))); err != nil {
		return CachedExpansion{}, true, err
	}
	return expansion, true, nil
}

func cloneCachedExpansion(expansion CachedExpansion) CachedExpansion {
	copyExpansion := expansion
	copyExpansion.Files = append([]FileVersion(nil), expansion.Files...)
	copyExpansion.Edges = append([]IncludeEdge(nil), expansion.Edges...)
	return copyExpansion
}
