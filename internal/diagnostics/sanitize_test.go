package diagnostics

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"xagent/internal/redact"
)

func TestSanitizeRedactsControlsAndTruncatesUTF8(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	canary := "runtime-value-5e91c2"
	redactor.RegisterSecret(canary)

	prefix := "provider failed: "
	maxBytes := len(prefix+"[redacted] ") + len("你") + 1
	input := SanitizeInput{
		Code:     "provider_stream_failed",
		Source:   "provider",
		Hint:     "retry_with_safe_configuration",
		Severity: SeverityError,
		Err: errors.New(
			prefix + canary + "\x1b[31m\x00\n你好世界",
		),
	}

	safe := Sanitize(input, SanitizeOptions{
		Redactor: redactor,
		MaxBytes: maxBytes,
	})
	if safe.Code != input.Code || safe.Source != input.Source || safe.Hint != input.Hint || safe.Severity != input.Severity {
		t.Fatal("stable diagnostic metadata changed during sanitization")
	}
	message := safe.Message.Text()
	if strings.Contains(message, canary) {
		t.Fatal("sanitized diagnostic leaked the runtime value")
	}
	if strings.ContainsAny(message, "\x00\x1b\n\r\t") {
		t.Fatal("sanitized diagnostic retained terminal control characters")
	}
	if len(message) > maxBytes || !utf8.ValidString(message) {
		t.Fatal("sanitized diagnostic did not truncate on a UTF-8 boundary")
	}
	if !strings.Contains(message, "[redacted]") || !strings.HasSuffix(message, "你") {
		t.Fatal("sanitization order did not redact before UTF-8 truncation")
	}

	typeOfSafe := reflect.TypeOf(SafeDiagnostic{})
	for index := 0; index < typeOfSafe.NumField(); index++ {
		field := typeOfSafe.Field(index)
		if field.Name == "Err" || field.Type.Implements(reflect.TypeOf((*error)(nil)).Elem()) {
			t.Fatalf("SafeDiagnostic retains raw error payload in field %q", field.Name)
		}
	}
}
