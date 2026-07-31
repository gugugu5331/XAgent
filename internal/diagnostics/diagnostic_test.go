package diagnostics

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"xagent/internal/redact"
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

func TestDiagnosticAttributesCopyStableTextAndLegacyOutput(t *testing.T) {
	legacy := New("legacy", SeverityInfo, "message").WithSource("source").WithPath("path")
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"code":"legacy","message":"message","source":"source","path":"path","severity":"info"}`; got != want {
		t.Fatalf("legacy JSON changed: %s", got)
	}
	if got, want := legacy.Text(), "legacy: message: path"; got != want {
		t.Fatalf("legacy Text changed: %q", got)
	}

	attributes := map[string]string{"stage": "execute", "action": "command"}
	withAttributes := legacy.WithAttributes(attributes)
	attributes["stage"] = "mutated"
	if got := withAttributes.Attributes["stage"]; got != "execute" {
		t.Fatalf("WithAttributes retained caller map: %q", got)
	}
	if got, want := withAttributes.Text(), "legacy: message: path: action=command: stage=execute"; got != want {
		t.Fatalf("attribute Text is not stable: %q", got)
	}
}

func TestCollectorStoresBoundedCopies(t *testing.T) {
	collector := NewCollector(CollectorOptions{Limit: 2, MaxBytes: 5})
	collector.Add(New("one", SeverityInfo, "123456789").WithPath("abcdef").WithSource("source-long"))
	collector.Add(New("two", SeverityWarning, "two"))
	collector.Add(New("three", SeverityError, "three"))

	if collector.Count() != 2 {
		t.Fatalf("Count() = %d, want 2", collector.Count())
	}
	items := collector.List()
	if len(items) != 2 || items[0].Code != "two" || items[1].Code != "three" {
		t.Fatalf("items = %#v", items)
	}
	items[0].Code = "mutated"
	if collector.List()[0].Code == "mutated" {
		t.Fatalf("List returned mutable backing slice")
	}

	collector.Clear()
	if collector.Count() != 0 {
		t.Fatalf("Count() after Clear = %d, want 0", collector.Count())
	}
	if len(collector.List()) != 0 {
		t.Fatalf("Clear did not remove items")
	}

	collector.Add(New("long", SeverityInfo, "123456789").WithPath("abcdef").WithSource("source-long"))
	items = collector.List()
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if items[0].Message != "12345" || items[0].Path != "abcde" || items[0].Source != "sourc" {
		t.Fatalf("item fields were not truncated to MaxBytes: %#v", items[0])
	}
}

func TestCollectorTruncatesUTF8Safely(t *testing.T) {
	collector := NewCollector(CollectorOptions{MaxBytes: 5})
	collector.Add(New("utf8", SeverityInfo, "你好世界"))

	items := collector.List()
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if items[0].Message != "你" {
		t.Fatalf("Message = %q, want first complete rune", items[0].Message)
	}
	if !utf8.ValidString(items[0].Message) {
		t.Fatalf("Message is not valid UTF-8: %q", items[0].Message)
	}
}

func TestCollectorRedactsSensitiveDiagnostics(t *testing.T) {
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret("runtime-secret")
	collector := NewCollector(CollectorOptions{Redactor: runtimeRedactor.Text})
	collector.Add(New("secret_code", "", "token=secret-token runtime-secret").WithPath("/tmp/runtime-secret").WithSource("Authorization: Bearer abc123"))

	items := collector.List()
	if len(items) != 1 {
		t.Fatalf("len(items) = %d", len(items))
	}
	combined := items[0].Message + items[0].Path + items[0].Source + items[0].Text()
	for _, secret := range []string{"secret-token", "runtime-secret", "abc123"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("collector leaked %q: %#v text=%q", secret, items[0], items[0].Text())
		}
	}
	if !strings.Contains(items[0].Message, "token=") || !strings.Contains(items[0].Message, "[redacted]") {
		t.Fatalf("message did not preserve field name with redacted value: %q", items[0].Message)
	}
	if !strings.Contains(strings.ToLower(items[0].Source), "authorization:") || !strings.Contains(items[0].Source, "[redacted]") {
		t.Fatalf("source did not preserve field name with redacted value: %q", items[0].Source)
	}
	if items[0].Severity != SeverityWarning {
		t.Fatalf("default severity = %q", items[0].Severity)
	}
}

func TestCollectorAttributesAreBoundedSanitizedAndCopied(t *testing.T) {
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret("attribute-secret")
	collector := NewCollector(CollectorOptions{Redactor: runtimeRedactor.Text})
	original := map[string]string{
		"stage":  "\x1b[31mexecute\x1b[0m",
		"detail": "attribute-secret\x00" + string([]byte{0xff}),
	}
	collector.Add(New("hook", SeverityWarning, "safe").WithAttributes(original))
	original["stage"] = "mutated"

	items := collector.List()
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if got := items[0].Attributes["stage"]; got != "execute" {
		t.Fatalf("stage was not sanitized/copied: %q", got)
	}
	if got := items[0].Attributes["detail"]; strings.Contains(got, "attribute-secret") || !utf8.ValidString(got) {
		t.Fatalf("detail was not redacted/sanitized: %q", got)
	}
	items[0].Attributes["stage"] = "list-mutated"
	if got := collector.List()[0].Attributes["stage"]; got != "execute" {
		t.Fatalf("List returned mutable attributes: %q", got)
	}
	if !strings.Contains(collector.List()[0].Text(), "detail=[redacted]") {
		t.Fatalf("Text did not include safe attributes: %q", collector.List()[0].Text())
	}
}

func TestCollectorAttributesDropsWholeMapAtAnyBoundary(t *testing.T) {
	tests := []struct {
		name       string
		attributes map[string]string
	}{
		{name: "count", attributes: func() map[string]string {
			values := map[string]string{}
			for index := 0; index < MaxDiagnosticAttributes+1; index++ {
				values[string(rune('a'+index))] = "value"
			}
			return values
		}()},
		{name: "key", attributes: map[string]string{strings.Repeat("k", MaxDiagnosticAttributeKeyBytes+1): "value"}},
		{name: "value", attributes: map[string]string{"key": strings.Repeat("v", MaxDiagnosticAttributeValueBytes+1)}},
		{name: "total", attributes: func() map[string]string {
			values := map[string]string{}
			for index := 0; index < MaxDiagnosticAttributes; index++ {
				values[strings.Repeat(string(rune('a'+index)), MaxDiagnosticAttributeKeyBytes)] = strings.Repeat("v", MaxDiagnosticAttributeValueBytes)
			}
			return values
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collector := NewCollector(CollectorOptions{})
			collector.Add(New("base", SeverityWarning, "retained").WithAttributes(test.attributes))
			items := collector.List()
			if len(items) != 1 || items[0].Code != "base" || items[0].Message != "retained" {
				t.Fatalf("base diagnostic was not retained: %#v", items)
			}
			if items[0].Attributes != nil {
				t.Fatalf("over-limit attributes were partially retained: %#v", items[0].Attributes)
			}
		})
	}
}

func TestCollectorAttributesConcurrentAddAndList(t *testing.T) {
	collector := NewCollector(CollectorOptions{Limit: 500})
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				collector.Add(New("concurrent", SeverityInfo, "message").WithAttributes(map[string]string{"stage": "execute"}))
				items := collector.List()
				if len(items) > 0 {
					items[0].Attributes["stage"] = "caller mutation"
				}
			}
		}()
	}
	wait.Wait()
	for _, item := range collector.List() {
		if item.Attributes["stage"] != "execute" {
			t.Fatalf("collector state was mutated: %#v", item.Attributes)
		}
	}
}

func TestCollectorCountBySeverity(t *testing.T) {
	collector := NewCollector(CollectorOptions{})
	collector.Add(New("info", SeverityInfo, "info"), New("warn", SeverityWarning, "warn"), New("err", SeverityError, "err"))
	counts := collector.CountBySeverity()
	if counts[SeverityInfo] != 1 || counts[SeverityWarning] != 1 || counts[SeverityError] != 1 {
		t.Fatalf("counts = %#v", counts)
	}
}

func TestSafeHTTPErrorSummaryRedactsAndLimitsBody(t *testing.T) {
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret("runtime-secret")
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"Authorization: Bearer abc123 runtime-secret"}` + strings.Repeat("x", 100))),
		Request: &http.Request{Header: http.Header{
			"Authorization": []string{"Bearer request-secret"},
		}},
	}

	summary := SafeHTTPErrorSummary(resp, HTTPErrorSummaryOptions{MaxBodyBytes: 48, Redactor: runtimeRedactor})
	if summary.StatusCode != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d", summary.StatusCode)
	}
	if !summary.Truncated {
		t.Fatalf("Truncated = false, want true")
	}
	for _, secret := range []string{"abc123", "runtime-secret", "request-secret"} {
		if strings.Contains(summary.BodyPreview.Text(), secret) {
			t.Fatal("HTTP summary leaked a protected value")
		}
	}
}

func TestSafeHTTPErrorSummarySuppressesBinaryBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"application/octet-stream"}},
		Body:       io.NopCloser(strings.NewReader("\x00secret\x01")),
	}

	summary := SafeHTTPErrorSummary(resp, HTTPErrorSummaryOptions{MaxBodyBytes: DefaultHTTPBodyPreviewBytes, Redactor: redact.NewRuntimeRedactor()})
	if strings.Contains(summary.BodyPreview.Text(), "secret") {
		t.Fatal("binary HTTP summary leaked response content")
	}
	if !strings.Contains(summary.BodyPreview.Text(), "binary") {
		t.Fatal("binary HTTP summary did not retain its safe hint")
	}
}
