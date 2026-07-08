package diagnostics

import (
	"strings"
	"sync"
)

const (
	DefaultCollectorLimit          = 100
	DefaultDiagnosticFieldMaxBytes = 2048
)

type Collector struct {
	mu       sync.Mutex
	items    []Diagnostic
	limit    int
	maxBytes int
	redactor Redactor
}

type CollectorOptions struct {
	Limit    int
	MaxBytes int
	Redactor Redactor
}

func NewCollector(options CollectorOptions) *Collector {
	limit := options.Limit
	if limit <= 0 {
		limit = DefaultCollectorLimit
	}
	maxBytes := options.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultDiagnosticFieldMaxBytes
	}
	return &Collector{limit: limit, maxBytes: maxBytes, redactor: options.Redactor}
}

func (c *Collector) Add(items ...Diagnostic) {
	if c == nil || len(items) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, item := range items {
		c.items = append(c.items, c.safe(item))
		if len(c.items) > c.limit {
			c.items = c.items[len(c.items)-c.limit:]
		}
	}
}

func (c *Collector) List() []Diagnostic {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	items := make([]Diagnostic, len(c.items))
	copy(items, c.items)
	return items
}

func (c *Collector) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = nil
}

func (c *Collector) Count() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func (c *Collector) CountBySeverity() map[Severity]int {
	counts := map[Severity]int{}
	for _, item := range c.List() {
		counts[severityOrDefault(item.Severity)]++
	}
	return counts
}

func (c *Collector) safe(item Diagnostic) Diagnostic {
	item.Code = strings.TrimSpace(item.Code)
	item.Severity = severityOrDefault(item.Severity)
	redactor := c.redactor
	if redactor != nil {
		item = item.Safe(redactor)
	}
	item.Message = truncateString(item.Message, c.maxBytes)
	item.Path = truncateString(item.Path, c.maxBytes)
	item.Source = truncateString(item.Source, c.maxBytes)
	return item
}

func truncateString(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && (value[end]&0xC0) == 0x80 {
		end--
	}
	return value[:end]
}
