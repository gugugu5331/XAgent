package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTextRedactsInlineSecrets(t *testing.T) {
	input := "api_key=secret-key token:secret-token authorization:Bearer secret password=secret sk-ant-test"
	redacted := Text(input)
	for _, secret := range []string{"secret-key", "secret-token", "Bearer secret", "password=secret", "sk-ant-test"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("redacted text leaked %q: %q", secret, redacted)
		}
	}
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
	for _, secret := range []string{"secret-key", "secret-token", "Bearer secret", "secret-password"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("redacted map leaked %q: %s", secret, data)
		}
	}
	if redacted["api_key"] != "[redacted]" {
		t.Fatalf("sensitive key not replaced: %#v", redacted)
	}
}
