package diagnostics

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"xagent/internal/redact"
)

// SanitizeInput is an internal-boundary candidate. Err may contain raw
// provider, transport, filesystem, or process text and must not be retained.
type SanitizeInput struct {
	Code     string
	Source   string
	Hint     string
	Severity Severity
	Err      error
}

type SanitizeOptions struct {
	Redactor *redact.RuntimeRedactor
	MaxBytes int
}

// SafeDiagnostic is safe to retain or publish. It deliberately has no raw
// error field.
type SafeDiagnostic struct {
	Code     string
	Source   string
	Hint     string
	Severity Severity
	Message  redact.SafeText
}

// Sanitize applies runtime redaction before terminal cleaning and UTF-8-safe
// truncation. The final Redact call only reconstructs the unforgeable SafeText
// boundary after cleaning; it cannot reintroduce raw data.
func Sanitize(input SanitizeInput, options SanitizeOptions) SafeDiagnostic {
	maximum := options.MaxBytes
	if maximum <= 0 {
		maximum = DefaultDiagnosticFieldMaxBytes
	}

	raw := ""
	if input.Err != nil {
		raw = input.Err.Error()
	}
	message := options.Redactor.Redact(raw).Text()
	message = truncateString(sanitizeTerminalText(message), maximum)

	return SafeDiagnostic{
		Code:     sanitizeStableMetadata(input.Code, options.Redactor, maximum),
		Source:   sanitizeStableMetadata(input.Source, options.Redactor, maximum),
		Hint:     sanitizeStableMetadata(input.Hint, options.Redactor, maximum),
		Severity: severityOrDefault(input.Severity),
		Message:  options.Redactor.Redact(message),
	}
}

func sanitizeStableMetadata(value string, redactor *redact.RuntimeRedactor, maximum int) string {
	value = redactor.Redact(value).Text()
	return strings.TrimSpace(truncateString(sanitizeTerminalText(value), maximum))
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
