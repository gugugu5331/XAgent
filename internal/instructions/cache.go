package instructions

import (
	"errors"
	"fmt"
	"sync"

	"xagent/internal/budget"
)

const (
	maxCachedExpansionBytes        = 32 << 20
	maxExpansionCacheBytes   int64 = 64 << 20
	maxExpansionCacheEntries       = 256
)

// CachedExpansion is an immutable-by-convention expansion snapshot. Cache
// operations copy slice fields so callers cannot mutate stored graph data.
type CachedExpansion struct {
	Source  GraphSource
	Content string
	Files   []FileVersion
	Edges   []IncludeEdge
}

// ExpansionCache stores completed instruction expansions by graph key.
type ExpansionCache struct {
	mu      sync.RWMutex
	entries map[CacheKey]CachedExpansion
	bytes   int64
}

// Store inserts or replaces a completed expansion using defensive copies.
func (c *ExpansionCache) Store(key CacheKey, expansion CachedExpansion) error {
	if c == nil {
		return errors.New("instruction expansion cache is nil")
	}
	if !key.valid() {
		return errors.New("instruction expansion cache key is invalid")
	}
	if len(expansion.Content) > maxCachedExpansionBytes {
		return errors.New("instruction expansion cache content exceeds the hard limit")
	}
	graphKey, err := NewCacheKey(expansion.Source, expansion.Files, expansion.Edges)
	if err != nil {
		return fmt.Errorf("instruction expansion cache graph is invalid: %w", err)
	}
	if graphKey != key {
		return errors.New("instruction expansion cache graph does not match its key")
	}
	copyExpansion := cloneCachedExpansion(expansion)
	entryBytes, err := cachedExpansionSize(copyExpansion)
	if err != nil || entryBytes > maxExpansionCacheBytes {
		return errors.New("instruction expansion cache entry exceeds the hard limit")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[CacheKey]CachedExpansion)
	}
	if _, replacing := c.entries[key]; !replacing && len(c.entries) >= maxExpansionCacheEntries {
		return errors.New("instruction expansion cache entry limit reached")
	}
	previousBytes := int64(0)
	if previous, replacing := c.entries[key]; replacing {
		previousBytes, _ = cachedExpansionSize(previous)
	}
	if c.bytes-previousBytes > maxExpansionCacheBytes-entryBytes {
		return errors.New("instruction expansion cache byte limit reached")
	}
	c.entries[key] = copyExpansion
	c.bytes = c.bytes - previousBytes + entryBytes
	return nil
}

// lookupCharged is used only after the current load has already charged the
// graph expansion budget. It avoids charging the same bytes twice while still
// returning the cache's defensive snapshot.
func (c *ExpansionCache) lookupCharged(key CacheKey) (CachedExpansion, bool) {
	if c == nil || !key.valid() {
		return CachedExpansion{}, false
	}
	c.mu.RLock()
	expansion, found := c.entries[key]
	if found {
		expansion = cloneCachedExpansion(expansion)
	}
	c.mu.RUnlock()
	return expansion, found
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
	if counter == nil {
		return CachedExpansion{}, false, errors.New("instruction expansion cache budget is unavailable")
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

func cachedExpansionSize(expansion CachedExpansion) (int64, error) {
	size := int64(len(expansion.Content) + len(expansion.Source.Name) + len(expansion.Source.RootPath) + 128)
	fileBytes := int64(len(expansion.Files)) * 128
	edgeBytes := int64(len(expansion.Edges)) * 192
	if fileBytes < 0 || edgeBytes < 0 || size > maxExpansionCacheBytes-fileBytes || size+fileBytes > maxExpansionCacheBytes-edgeBytes {
		return 0, errors.New("instruction expansion cache size overflow")
	}
	return size + fileBytes + edgeBytes, nil
}
