package budget

import (
	"errors"
	"math"
	"sync"
)

const counterScope = "counter"

// Counter accumulates all configured dimensions for one operation. Limits and
// used values are private and every read-modify-write is protected by one lock.
type Counter struct {
	mu        sync.Mutex
	effective Limits
	used      [dimensionCount]int64
}

// Snapshot is an immutable copy of one Counter state.
type Snapshot struct {
	limits Limits
	used   [dimensionCount]int64
}

func NewCounter(effective, hard Limits) (*Counter, error) {
	if err := ValidateLimits(counterScope, effective, hard); err != nil {
		return nil, err
	}
	return &Counter{effective: effective}, nil
}

// Consume reserves amount before the caller reads, allocates, or writes the
// corresponding resource. Failed reservations never alter accumulated state.
func (c *Counter) Consume(dimension Dimension, amount int64) error {
	if c == nil {
		return errors.New("budget counter is nil")
	}
	if !dimension.Valid() {
		return errors.New("budget counter received an unknown dimension")
	}
	if amount < 0 {
		return errors.New("budget consumption must not be negative")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	limit, configured := c.effective.Value(dimension)
	if !configured {
		return errors.New("budget dimension is not configured")
	}
	used := c.used[dimension]
	if amount > limit-used {
		observed := int64(math.MaxInt64)
		if amount <= math.MaxInt64-used {
			observed = used + amount
		}
		return &LimitError{
			Scope:     counterScope,
			Dimension: dimension,
			Limit:     limit,
			Observed:  observed,
		}
	}
	c.used[dimension] = used + amount
	return nil
}

func (c *Counter) Remaining(dimension Dimension) int64 {
	if c == nil || !dimension.Valid() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	limit, configured := c.effective.Value(dimension)
	if !configured {
		return 0
	}
	return limit - c.used[dimension]
}

func (c *Counter) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Snapshot{
		limits: c.effective,
		used:   c.used,
	}
}

func (s Snapshot) Used(dimension Dimension) int64 {
	if !dimension.Valid() {
		return 0
	}
	if _, configured := s.limits.Value(dimension); !configured {
		return 0
	}
	return s.used[dimension]
}

func (s Snapshot) Remaining(dimension Dimension) int64 {
	limit, configured := s.limits.Value(dimension)
	if !configured {
		return 0
	}
	return limit - s.used[dimension]
}
