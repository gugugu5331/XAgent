package diagnostics

import (
	"strings"
	"testing"
)

func TestDiagnosticSafeRedactsMessagePathAndSource(t *testing.T) {
	diagnostic := New("secret", "", "token=secret-token").WithPath("/tmp/secret-token").WithSource("authorization: secret-token")
	redacted := diagnostic.Safe(func(value string) string {
		return strings.ReplaceAll(value, "secret-token", "[redacted]")
	})
	combined := redacted.Message + redacted.Path + redacted.Source
	if strings.Contains(combined, "secret-token") {
		t.Fatalf("diagnostic leaked secret: %#v", redacted)
	}
	if redacted.Severity != SeverityWarning {
		t.Fatalf("expected default warning severity, got %q", redacted.Severity)
	}
}
