package diagnostics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"xagent/internal/redact"
)

func TestDiagnosticsCrossBoundarySecretCanary(t *testing.T) {
	const canary = "cny_" + "9f2c6d81e4a7"
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(canary)

	requestURL := &url.URL{Scheme: "https", Host: "example.test", Path: "/diagnostics"}
	requestURL.User = url.UserPassword("agent", canary)
	query := requestURL.Query()
	query.Set("access_"+"token", canary)
	requestURL.RawQuery = query.Encode()
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+canary)
	header.Set("Cookie", "session="+canary)
	environment := "SERVICE_" + "CREDENTIAL=" + canary
	cause := errors.New("provider rejected " + canary)
	wrapped := fmt.Errorf("diagnostic boundary failed: %w", cause)
	nestedJSON, err := json.Marshal(map[string]any{
		"headers": map[string]any{
			"authorization": header.Get("Authorization"),
			"cookie":        header.Get("Cookie"),
		},
		"nested": []any{
			requestURL.String(),
			environment,
			wrapped.Error(),
		},
	})
	if err != nil {
		t.Fatal("marshal diagnostic structured value failed")
	}

	for index, safe := range []redact.SafeText{
		redactor.Redact(header.Get("Authorization")),
		redactor.Redact(header.Get("Cookie")),
		redactor.Redact(requestURL.String()),
		redactor.Redact(environment),
		redactor.Redact(wrapped.Error()),
		redactor.Redact(string(nestedJSON)),
	} {
		if strings.Contains(safe.Text(), canary) {
			t.Fatalf("SafeText channel %d retained a registered value", index)
		}
	}

	responseHeader := make(http.Header)
	responseHeader.Set("Content-Type", "application/json; boundary="+canary)
	responseHeader.Set("Authorization", "Bearer "+canary)
	summary := SafeHTTPErrorSummary(&http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     responseHeader,
		Body: io.NopCloser(strings.NewReader(
			`{"error":"` + canary + `","nested":` + string(nestedJSON) + `}`,
		)),
		Request: &http.Request{URL: requestURL, Header: header},
	}, HTTPErrorSummaryOptions{MaxBodyBytes: 96, Redactor: redactor})
	for index, value := range []string{summary.MediaType, summary.BodyPreview.Text()} {
		if strings.Contains(value, canary) {
			t.Fatalf("HTTP summary channel %d retained a registered value", index)
		}
	}

	sink, err := NewBoundedSink(BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      4,
		MaxItemBytes:  2048,
		MaxTotalBytes: 4096,
	})
	if err != nil {
		t.Fatal("create diagnostic canary sink failed")
	}
	sink.Add(SanitizeInput{
		Code:     "provider_" + canary,
		Source:   requestURL.String(),
		Hint:     environment + " " + header.Get("Cookie"),
		Severity: SeverityError,
		Err:      fmt.Errorf("snapshot boundary %s: %w", string(nestedJSON), wrapped),
	})
	items := sink.Snapshot().Items()
	if len(items) != 1 {
		t.Fatalf("diagnostic snapshot item count = %d, want 1", len(items))
	}
	diagnostic := items[0].Diagnostic
	for index, value := range []string{
		diagnostic.Code,
		diagnostic.Source,
		diagnostic.Hint,
		diagnostic.Message.Text(),
	} {
		if strings.Contains(value, canary) {
			t.Fatalf("diagnostic snapshot channel %d retained a registered value", index)
		}
	}
}
