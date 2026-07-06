package diagnostics

import "strings"

type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

type Diagnostic struct {
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Source   string   `json:"source,omitempty"`
	Path     string   `json:"path,omitempty"`
	Severity Severity `json:"severity"`
}

type Redactor func(string) string

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

func (d Diagnostic) Safe(redact Redactor) Diagnostic {
	if redact == nil {
		return d
	}
	d.Message = redact(d.Message)
	d.Path = redact(d.Path)
	d.Source = redact(d.Source)
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
	return strings.Join(parts, ": ")
}

func severityOrDefault(severity Severity) Severity {
	switch severity {
	case SeverityInfo, SeverityWarning, SeverityError:
		return severity
	default:
		return SeverityWarning
	}
}
