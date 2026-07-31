package diagnostics

import (
	"sort"
	"strings"

	"xagent/internal/redact"
)

type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

type Diagnostic struct {
	Code       string            `json:"code"`
	Message    string            `json:"message"`
	Source     string            `json:"source,omitempty"`
	Path       string            `json:"path,omitempty"`
	Severity   Severity          `json:"severity"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type Redactor func(string) string

// SafeDiagnostic contains only stable metadata and text that has crossed the
// runtime redaction boundary.
type SafeDiagnostic struct {
	Code     string
	Source   string
	Hint     string
	Severity Severity
	Message  redact.SafeText
}

func (d SafeDiagnostic) stableIdentity() string {
	return strings.Join([]string{d.Code, d.Source, d.Hint, string(d.Severity)}, "\x00")
}

func (d SafeDiagnostic) retainedBytes(identity string) int64 {
	return int64(len(identity) + len(d.Code) + len(d.Source) + len(d.Hint) + len(d.Severity) + len(d.Message.Text()))
}

func New(code string, severity Severity, message string) Diagnostic {
	return Diagnostic{Code: strings.TrimSpace(code), Severity: severityOrDefault(severity), Message: strings.TrimSpace(message)}
}

func (d Diagnostic) WithSource(source string) Diagnostic {
	d.Source = strings.TrimSpace(source)
	return d
}

func (d Diagnostic) WithPath(path string) Diagnostic {
	d.Path = strings.TrimSpace(path)
	return d
}

// WithAttributes returns a diagnostic with a private copy of attributes.
// Replacing rather than merging keeps construction deterministic and avoids
// accidentally retaining data from a reused Diagnostic value.
func (d Diagnostic) WithAttributes(attributes map[string]string) Diagnostic {
	d.Attributes = cloneAttributes(attributes)
	return d
}

func (d Diagnostic) Safe(redact Redactor) Diagnostic {
	d.Attributes = cloneAttributes(d.Attributes)
	if redact == nil {
		return d
	}
	d.Message = redact(d.Message)
	d.Path = redact(d.Path)
	d.Source = redact(d.Source)
	redactedAttributes := make(map[string]string, len(d.Attributes))
	keys := make([]string, 0, len(d.Attributes))
	for key := range d.Attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := d.Attributes[key]
		redactedAttributes[redact(key)] = redact(value)
	}
	d.Attributes = redactedAttributes
	return d
}

func (d Diagnostic) Text() string {
	parts := []string{}
	if d.Code != "" {
		parts = append(parts, d.Code)
	}
	if d.Message != "" {
		parts = append(parts, d.Message)
	}
	if d.Path != "" {
		parts = append(parts, d.Path)
	}
	if len(d.Attributes) > 0 {
		keys := make([]string, 0, len(d.Attributes))
		for key := range d.Attributes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts = append(parts, key+"="+d.Attributes[key])
		}
	}
	return strings.Join(parts, ": ")
}

func cloneAttributes(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(attributes))
	for key, value := range attributes {
		cloned[key] = value
	}
	return cloned
}

func severityOrDefault(severity Severity) Severity {
	switch severity {
	case SeverityInfo, SeverityWarning, SeverityError:
		return severity
	default:
		return SeverityWarning
	}
}
