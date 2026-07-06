package mcpclient

import (
	"strings"
	"testing"
)

func TestRegisteredToolNameSanitizesUnsafeNames(t *testing.T) {
	identity := RegisteredToolName("server/name\x1b[31m", "tool name\n../../x", nil)
	if !strings.HasPrefix(identity.RegisteredName, "mcp__") {
		t.Fatalf("expected mcp prefix, got %q", identity.RegisteredName)
	}
	if strings.ContainsAny(identity.RegisteredName, "/\n\x1b .") {
		t.Fatalf("registered name contains unsafe chars: %q", identity.RegisteredName)
	}
	if identity.ServerName != "server/name\x1b[31m" || identity.RemoteToolName != "tool name\n../../x" {
		t.Fatalf("original names not preserved: %#v", identity)
	}
}

func TestRegisteredToolNameHandlesEmptyAndCollisions(t *testing.T) {
	used := map[string]bool{}
	first := RegisteredToolName("", "", used)
	second := RegisteredToolName("", "", used)
	if first.RegisteredName == second.RegisteredName {
		t.Fatalf("expected stable collision suffix, got %q", first.RegisteredName)
	}
	if !strings.Contains(first.RegisteredName, "server_") || !strings.Contains(first.RegisteredName, "tool_") {
		t.Fatalf("expected hashed fallback parts, got %q", first.RegisteredName)
	}
}

func TestRegisteredToolNameTruncatesLongNames(t *testing.T) {
	identity := RegisteredToolName(strings.Repeat("s", 200), strings.Repeat("t", 200), nil)
	if len(identity.RegisteredName) > maxToolNameLen {
		t.Fatalf("registered name too long: %d", len(identity.RegisteredName))
	}
}

func TestSanitizeMetadataRemovesControlCharsAndLimitsLength(t *testing.T) {
	got := SanitizeMetadata("hello\x1b[31m\nworld", 12)
	if strings.ContainsAny(got, "\x1b\n") || len(got) > 12 {
		t.Fatalf("metadata not sanitized: %q", got)
	}
}

func TestRedactArgumentsRedactsSecretsInNonSensitiveStringValues(t *testing.T) {
	redacted := RedactArguments(map[string]any{
		"query": "find token=secret-token",
		"items": []any{"authorization:Bearer-secret"},
	})
	data := redacted["query"].(string) + " " + redacted["items"].([]any)[0].(string)
	if strings.Contains(data, "secret-token") || strings.Contains(data, "Bearer-secret") {
		t.Fatalf("non-sensitive string value leaked secret: %#v", redacted)
	}
}

func TestRedactTextRemovesInlineSecrets(t *testing.T) {
	redacted := RedactText("authorization=Bearer-secret api_key:secret-token ok")
	if strings.Contains(redacted, "Bearer-secret") || strings.Contains(redacted, "secret-token") {
		t.Fatalf("text secret not redacted: %q", redacted)
	}
}

func TestRedactArgumentsRecursivelyRedactsSensitiveKeys(t *testing.T) {
	redacted := RedactArguments(map[string]any{
		"api_key": "secret",
		"nested": map[string]any{
			"Authorization": "Bearer secret",
		},
	})
	if redacted["api_key"] != "[redacted]" {
		t.Fatalf("api_key not redacted: %#v", redacted)
	}
	nested := redacted["nested"].(map[string]any)
	if nested["Authorization"] != "[redacted]" {
		t.Fatalf("authorization not redacted: %#v", redacted)
	}
}
