package redact

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestRuntimeRedactorRegistersExpandedSecrets(t *testing.T) {
	redactor := NewRuntimeRedactor()
	expanded := os.Expand("opaque-${VALUE}", func(key string) string {
		if key == "VALUE" {
			return "runtime-7f3a9c"
		}
		return ""
	})
	short := "q7"

	var registrations sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		registrations.Add(1)
		go func() {
			defer registrations.Done()
			for attempt := 0; attempt < 20; attempt++ {
				redactor.RegisterSecret(expanded)
				redactor.RegisterSecret(short)
			}
		}()
	}
	registrations.Wait()

	if got := len(redactor.secrets); got != 2 {
		t.Fatalf("registered secret count = %d, want 2 unique values", got)
	}

	cause := errors.New("provider rejected " + expanded)
	derived := fmt.Errorf("hook expansion failed: %w", cause)
	safe := redactor.Redact("short value " + short + "; " + derived.Error())
	if strings.Contains(safe.Text(), expanded) {
		t.Fatal("SafeText leaked the expanded runtime value")
	}
	if strings.Contains(safe.Text(), short) {
		t.Fatal("SafeText leaked the short runtime value")
	}
	if strings.Count(safe.Text(), marker) < 2 {
		t.Fatal("SafeText did not redact every registered runtime value")
	}
}

func TestRuntimeCrossBoundarySecretCanary(t *testing.T) {
	const canary = "cny_" + "9f2c6d81e4a7"
	redactor := NewRuntimeRedactor()
	redactor.RegisterSecret(canary)

	header := make(http.Header)
	header.Set("Authorization", "Bearer "+canary)
	header.Set("Cookie", "session="+canary)

	requestURL := &url.URL{Scheme: "https", Host: "example.test", Path: "/runtime"}
	requestURL.User = url.UserPassword("agent", canary)
	query := requestURL.Query()
	query.Set("access_"+"token", canary)
	requestURL.RawQuery = query.Encode()

	environment := "SERVICE_" + "CREDENTIAL=" + canary
	cause := errors.New("provider rejected " + canary)
	wrapped := fmt.Errorf("runtime boundary failed: %w", cause)
	structuredJSON, err := json.Marshal(map[string]any{
		"headers": map[string]any{
			"authorization": header.Get("Authorization"),
			"cookie":        header.Get("Cookie"),
		},
		"nested": []any{
			map[string]any{"url": requestURL.String()},
			map[string]any{"environment": environment},
			map[string]any{"error": wrapped.Error()},
		},
	})
	if err != nil {
		t.Fatal("marshal cross-boundary structured value failed")
	}

	candidates := []SafeText{
		redactor.Redact(header.Get("Authorization")),
		redactor.Redact(header.Get("Cookie")),
		redactor.Redact(requestURL.String()),
		redactor.Redact(environment),
		redactor.Redact(wrapped.Error()),
		redactor.Redact(string(structuredJSON)),
	}
	for index, candidate := range candidates {
		if strings.Contains(candidate.Text(), canary) {
			t.Fatalf("SafeText channel %d retained a registered value", index)
		}
	}
}
