package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

var sensitiveSamples = []string{
	"sk-test-secret",
	"Authorization: Bearer abc123",
	"authorization: bearer abc123",
	"api_key=secret-key",
	"apiKey: secret-key",
	"access_token=access-secret",
	"refresh_token=refresh-secret",
	"password=my-password",
	"secret=my-secret",
	"cookie=session-secret",
	"set-cookie=session-secret",
	"-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----",
	"https://example.test/?token=query-secret",
	"x-api-key: x-api-secret",
	"X-Api-Key: x-api-secret",
	"ANTHROPIC_API_KEY=anthropic-secret",
	"OPENAI_API_KEY=openai-secret",
	"GITHUB_TOKEN=ghp_secret",
	"AWS_SECRET_ACCESS_KEY=aws-secret",
	"https://user:pass@example.test/path",
	"Authorization: Basic dXNlcjpwYXNz",
	`{"headers":{"Authorization":"Bearer nested-secret"}}`,
	"stdout success contains success-secret",
}

func TestTextRedactsInlineSecrets(t *testing.T) {
	input := "api_key=secret-key token:secret-token authorization:Bearer secret password=secret sk-ant-test"
	redacted := Text(input)
	assertNoLeak(t, redacted, []string{"secret-key", "secret-token", "Bearer secret", "password=secret", "sk-ant-test"})
}

func TestRedactSensitiveMatrix(t *testing.T) {
	redactor := NewRuntimeRedactor()
	redactor.RegisterSecret("sk-test-secret")
	redactor.RegisterSecret("success-secret")
	redactor.RegisterSecret("nested-secret")

	input := strings.Join(sensitiveSamples, "\n")
	redacted := redactor.Text(input)
	assertNoLeak(t, redacted, []string{
		"sk-test-secret",
		"abc123",
		"secret-key",
		"access-secret",
		"refresh-secret",
		"my-password",
		"my-secret",
		"session-secret",
		"query-secret",
		"x-api-secret",
		"anthropic-secret",
		"openai-secret",
		"ghp_secret",
		"aws-secret",
		"user:pass",
		"dXNlcjpwYXNz",
		"nested-secret",
		"success-secret",
		"-----BEGIN PRIVATE KEY-----",
	})
}

func TestMapRedactsSensitiveKeysAndNestedStrings(t *testing.T) {
	redacted := Map(map[string]any{
		"api_key": "secret-key",
		"query":   "find token=secret-token",
		"nested":  map[string]any{"Authorization": "Bearer secret"},
		"items":   []any{"password=secret-password"},
	})
	data, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, string(data), []string{"secret-key", "secret-token", "Bearer secret", "secret-password"})
	if redacted["api_key"] != "[redacted]" {
		t.Fatalf("sensitive key not replaced: %#v", redacted)
	}
}

func TestRuntimeRedactorRegistersSecretsPerInstance(t *testing.T) {
	first := NewRuntimeRedactor()
	second := NewRuntimeRedactor()
	if got := first.MaxSecretBytes(); got != 0 {
		t.Fatalf("empty runtime redactor MaxSecretBytes() = %d, want 0", got)
	}
	first.RegisterSecret("short")
	first.RegisterSecret("runtime-secret")
	first.RegisterSecret("runtime-secret")
	if got, want := first.MaxSecretBytes(), len("runtime-secret"); got != want {
		t.Fatalf("MaxSecretBytes() = %d, want %d", got, want)
	}
	if got := second.MaxSecretBytes(); got != 0 {
		t.Fatalf("second runtime redactor unexpectedly inherited max secret size: %d", got)
	}

	firstText := first.Text("value short runtime-secret")
	assertNoLeak(t, firstText, []string{"short", "runtime-secret"})

	secondText := second.Text("value short runtime-secret")
	if !strings.Contains(secondText, "runtime-secret") || !strings.Contains(secondText, "short") {
		t.Fatalf("second redactor unexpectedly used first instance secrets: %q", secondText)
	}

	redacted := first.Map(map[string]any{
		"message": "runtime-secret",
		"nested":  []any{"short"},
	})
	data, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, string(data), []string{"runtime-secret", "short"})
}

func TestRuntimeRedactorReplacesOverlappingSecretsLongestFirst(t *testing.T) {
	redactor := NewRuntimeRedactor()
	redactor.RegisterSecret("abc")
	redactor.RegisterSecret("abcdef")

	got := redactor.Text("value=abcdef")
	assertNoLeak(t, got, []string{"abc", "def", "abcdef"})
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("overlapping secret was not replaced: %q", got)
	}
}

func assertNoLeak(t *testing.T, value string, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(value, secret) {
			t.Fatalf("redacted text leaked %q: %q", secret, value)
		}
	}
}
