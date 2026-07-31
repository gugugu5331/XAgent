package redact_test

import (
	"reflect"
	"testing"

	"xagent/internal/redact"
)

func TestSafeTextConstructionBoundary(t *testing.T) {
	typeOfSafeText := reflect.TypeOf(redact.SafeText{})
	if typeOfSafeText.Kind() != reflect.Struct {
		t.Fatalf("SafeText kind = %s, want struct", typeOfSafeText.Kind())
	}
	if typeOfSafeText.NumField() != 1 {
		t.Fatalf("SafeText field count = %d, want 1", typeOfSafeText.NumField())
	}

	field := typeOfSafeText.Field(0)
	if field.IsExported() {
		t.Fatalf("SafeText field %q is exported; external callers could inject raw text", field.Name)
	}
	if field.Type.Kind() != reflect.String {
		t.Fatalf("SafeText field kind = %s, want string", field.Type.Kind())
	}

	var zero redact.SafeText
	if got := zero.Text(); got != "" {
		t.Fatalf("zero SafeText.Text() = %q, want empty", got)
	}
}
