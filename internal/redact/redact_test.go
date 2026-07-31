package redact

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
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
		t.Fatal("sensitive key was not replaced")
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
		t.Fatal("second redactor unexpectedly used another instance's secrets")
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
		t.Fatal("overlapping secret was not replaced")
	}
}

func TestRuntimeRedactorShortSecretsMatchWholeTokensAndFieldValues(t *testing.T) {
	redactor := NewRuntimeRedactor()
	redactor.RegisterSecret("")
	redactor.RegisterSecret("x")
	redactor.RegisterSecret("ab")
	redactor.RegisterSecret("abc")

	got := redactor.Text("x ab abc xagent alphabet abcdef suffix-x punctuation(ab)")
	for _, leaked := range []string{" x ", " ab ", " abc "} {
		if strings.Contains(" "+got+" ", leaked) {
			t.Fatal("whole short token was not redacted")
		}
	}
	for _, ordinary := range []string{"xagent", "alphabet", "abcdef"} {
		if !strings.Contains(got, ordinary) {
			t.Fatal("short-secret redaction damaged ordinary text")
		}
	}
	if strings.Contains(got, "suffix-x") || strings.Contains(got, "(ab)") {
		t.Fatal("punctuation-delimited token was not redacted")
	}
	if got := redactor.Text("x"); got != marker {
		t.Fatal("complete short field value was not redacted")
	}
	redactedMap := redactor.Map(map[string]any{"ordinary": "x", "api_key": "not-registered"})
	if redactedMap["ordinary"] != marker || redactedMap["api_key"] != marker {
		t.Fatal("field values were not safely redacted")
	}
	if got := redactor.MaxSecretBytes(); got != len("abc") {
		t.Fatalf("MaxSecretBytes() = %d, want %d", got, len("abc"))
	}
}

func TestRuntimeRedactorSecretsConcurrentRegistrationAndUse(t *testing.T) {
	redactor := NewRuntimeRedactor()
	secrets := []string{"a", "bb", "long-secret", "long-secret", ""}
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				for _, secret := range secrets {
					redactor.RegisterSecret(secret)
				}
				_ = redactor.Text("a bb long-secret ordinary")
				_ = redactor.MaxSecretBytes()
			}
		}()
	}
	wait.Wait()
	got := redactor.Text("a bb long-secret ordinary")
	assertNoLeak(t, got, []string{" a ", " bb ", "long-secret"})
	if !strings.Contains(got, "ordinary") {
		t.Fatal("runtime redaction damaged ordinary text")
	}
}

func TestRuntimeSecretCanaryMatrix(t *testing.T) {
	redactor := NewRuntimeRedactor()
	canary := "runtime-canary-4d2f8a"
	redactor.RegisterSecret(canary)

	structured := redactor.Any(map[string]any{
		"headers": map[string]any{"Authorization": "Bearer " + canary},
		"env":     map[string]any{"SERVICE_CREDENTIAL": canary},
		"nested":  []any{map[string]any{"message": "provider returned " + canary}},
	})
	structuredJSON, err := json.Marshal(structured)
	if err != nil {
		t.Fatal("marshal redacted structured value failed")
	}

	invalidUTF8 := string([]byte{'p', 'r', 'e', 'f', 'i', 'x', '-', 0xff, '-'}) + canary
	samples := []string{
		"Authorization: Bearer " + canary,
		"https://user:" + canary + "@example.test/path?token=" + canary + "&plain=ok",
		"SERVICE_CREDENTIAL=" + canary,
		string(structuredJSON),
		"-----BEGIN PRIVATE KEY-----\n" + canary + "\n-----END PRIVATE KEY-----",
		invalidUTF8,
	}

	for index, sample := range samples {
		safe := redactor.Redact(sample)
		if strings.Contains(safe.Text(), canary) {
			t.Fatalf("sample %d leaked the registered runtime value", index)
		}
		if !utf8.ValidString(safe.Text()) {
			t.Fatalf("sample %d produced invalid UTF-8", index)
		}
	}
}

func assertNoLeak(t *testing.T, value string, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(value, secret) {
			t.Fatal("redacted text retained a protected value")
		}
	}
}
