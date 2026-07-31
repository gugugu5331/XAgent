package redact

// SafeText is text that has crossed the runtime redaction boundary.
// Its value is intentionally inaccessible to callers outside this package.
type SafeText struct {
	value string
}

// Text returns the already-redacted text.
func (text SafeText) Text() string {
	return text.value
}
