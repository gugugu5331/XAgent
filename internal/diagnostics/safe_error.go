package diagnostics

import "xagent/internal/redact"

// SafeError is an error whose message has crossed the runtime redaction
// boundary and is safe to publish outside the component that observed it.
type SafeError struct {
	Code        string
	Source      string
	Message     redact.SafeText
	Recoverable bool
}

func (err *SafeError) Error() string {
	if err == nil {
		return ""
	}
	return err.Message.Text()
}
