package diagnostics

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultCollectorLimit            = 100
	DefaultDiagnosticFieldMaxBytes   = 2048
	MaxDiagnosticAttributes          = 8
	MaxDiagnosticAttributeKeyBytes   = 64
	MaxDiagnosticAttributeValueBytes = 256
	MaxDiagnosticAttributesBytes     = 2 * 1024
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
	for index, item := range c.items {
		item.Attributes = cloneAttributes(item.Attributes)
		items[index] = item
	}
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
	item.Code = strings.TrimSpace(sanitizeTerminalText(item.Code))
	item.Severity = severityOrDefault(item.Severity)
	item.Message = sanitizeTerminalText(item.Message)
	item.Path = sanitizeTerminalText(item.Path)
	item.Source = sanitizeTerminalText(item.Source)
	redactor := c.redactor
	if redactor != nil {
		item.Message = sanitizeTerminalText(redactor(item.Message))
		item.Path = sanitizeTerminalText(redactor(item.Path))
		item.Source = sanitizeTerminalText(redactor(item.Source))
	}
	item.Message = truncateString(item.Message, c.maxBytes)
	item.Path = truncateString(item.Path, c.maxBytes)
	item.Source = truncateString(item.Source, c.maxBytes)
	item.Attributes = c.safeAttributes(item.Attributes)
	return item
}

func (c *Collector) safeAttributes(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	if len(attributes) > MaxDiagnosticAttributes {
		return nil
	}
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	cleaned := make(map[string]string, len(attributes))
	for _, key := range keys {
		value := attributes[key]
		if len(key) > MaxDiagnosticAttributeKeyBytes || len(value) > MaxDiagnosticAttributeValueBytes {
			return nil
		}
		key = sanitizeTerminalText(key)
		value = sanitizeTerminalText(value)
		if c.redactor != nil {
			key = sanitizeTerminalText(c.redactor(key))
			value = sanitizeTerminalText(c.redactor(value))
		}
		if len(key) > MaxDiagnosticAttributeKeyBytes || len(value) > MaxDiagnosticAttributeValueBytes {
			return nil
		}
		if _, duplicate := cleaned[key]; duplicate {
			return nil
		}
		cleaned[key] = value
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil || len(encoded) > MaxDiagnosticAttributesBytes {
		return nil
	}
	return cleaned
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

func sanitizeTerminalText(value string) string {
	value = strings.ToValidUTF8(value, "")
	var safe strings.Builder
	safe.Grow(len(value))
	for index := 0; index < len(value); {
		if value[index] == 0x1b {
			index = skipEscapeSequence(value, index)
			continue
		}
		r, size := utf8.DecodeRuneInString(value[index:])
		index += size
		if unicode.IsControl(r) {
			if r == '\n' || r == '\r' || r == '\t' {
				safe.WriteByte(' ')
			}
			continue
		}
		safe.WriteRune(r)
	}
	return safe.String()
}

func skipEscapeSequence(value string, index int) int {
	index++
	if index >= len(value) {
		return index
	}
	switch value[index] {
	case '[': // CSI: parameters/intermediates followed by a final byte.
		index++
		for index < len(value) {
			current := value[index]
			index++
			if current >= 0x40 && current <= 0x7e {
				break
			}
		}
		return index
	case ']': // OSC: terminated by BEL or ST.
		index++
		for index < len(value) {
			if value[index] == 0x07 {
				return index + 1
			}
			if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
				return index + 2
			}
			index++
		}
		return index
	default:
		// Two-byte escape sequences consume their final byte as well.
		return index + 1
	}
}
