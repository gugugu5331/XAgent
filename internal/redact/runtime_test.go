package redact

import (
	"errors"
	"fmt"
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
