package budget

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestEveryBudgetDimensionDefaultMinCapAndOverflow(t *testing.T) {
	expectedSpecs := []Spec{
		{ToolInlineOutputBytes, Bytes, 32 * kibibyte, 1, 1 * mebibyte},
		{ToolCaptureBytes, Bytes, 64 * mebibyte, 1, 512 * mebibyte},
		{ArtifactMaxFileBytes, Bytes, 64 * mebibyte, 1, 512 * mebibyte},
		{ArtifactMaxTotalBytes, Bytes, 1 * gibibyte, 1, 8 * gibibyte},
		{FilesReadMaxBytes, Bytes, 16 * mebibyte, 1, 256 * mebibyte},
		{FilesScanMaxBytes, Bytes, 256 * mebibyte, 1, 2 * gibibyte},
		{FilesScanMaxFiles, Files, 100_000, 1, 1_000_000},
		{FilesScanMaxDirectories, Directories, 25_000, 1, 250_000},
		{FilesScanMaxLines, Lines, 1_000_000, 1, 10_000_000},
		{InstructionsMaxFileBytes, Bytes, 64 * kibibyte, 1, 1 * mebibyte},
		{InstructionsMaxTotalBytes, Bytes, 1 * mebibyte, 1, 16 * mebibyte},
		{InstructionsMaxFiles, Files, 64, 1, 1_024},
		{InstructionsMaxExpandedBytes, ExpandedBytes, 2 * mebibyte, 1, 32 * mebibyte},
		{MCPMaxResponseBytes, Bytes, 1 * mebibyte, 1, 16 * mebibyte},
		{MCPMaxTools, Items, 128, 1, 1_024},
		{MCPMaxPages, Items, 32, 1, 128},
		{MCPMaxProtocolErrors, ProtocolErrors, 32, 1, 256},
		{ProviderMaxResponseBytes, Bytes, 16 * mebibyte, 1, 64 * mebibyte},
		{ProviderMaxEventBytes, Bytes, 1 * mebibyte, 1, 4 * mebibyte},
		{ProviderMaxEvents, Items, 100_000, 1, 1_000_000},
		{ProviderMaxTextBytes, Bytes, 8 * mebibyte, 1, 32 * mebibyte},
		{ProviderMaxThinkingBytes, Bytes, 8 * mebibyte, 1, 32 * mebibyte},
		{ProviderMaxToolArgumentsBytes, Bytes, 1 * mebibyte, 1, 8 * mebibyte},
		{SessionMaxRecordBytes, Bytes, 16 * mebibyte, 1, 64 * mebibyte},
		{SessionMaxSessionBytes, Bytes, 256 * mebibyte, 1, 1 * gibibyte},
		{SessionMaxScanFiles, Files, 1_000, 1, 100_000},
		{SessionMaxScanBytes, Bytes, 10 * mebibyte, 1, 1 * gibibyte},
		{DiagnosticsMaxItems, Items, 100, 1, 1_000},
		{DiagnosticsMaxItemBytes, Bytes, 2 * kibibyte, 1, 64 * kibibyte},
		{DiagnosticsMaxTotalBytes, Bytes, 2 * mebibyte, 1, 16 * mebibyte},
	}

	specs := AllSpecs()
	if len(specs) != len(expectedSpecs) {
		t.Fatalf("budget spec count = %d, want %d", len(specs), len(expectedSpecs))
	}

	seenScopes := make(map[Scope]bool, len(specs))
	seenDimensions := make(map[Dimension]bool, dimensionCount)
	for index, spec := range specs {
		if spec != expectedSpecs[index] {
			t.Fatalf("budget spec %d does not match the approved scope/default/min/cap", index)
		}
		if seenScopes[spec.Scope] {
			t.Fatalf("duplicate budget scope %q", spec.Scope)
		}
		seenScopes[spec.Scope] = true
		seenDimensions[spec.Dimension] = true

		if !spec.Dimension.Valid() {
			t.Fatalf("scope %q has invalid dimension", spec.Scope)
		}
		if spec.Minimum != 1 || spec.Default < spec.Minimum || spec.Default > spec.HardCap {
			t.Fatalf("scope %q has invalid default/min/cap ordering", spec.Scope)
		}

		if got, err := spec.Resolve(nil); err != nil || got != spec.Default {
			t.Fatalf("scope %q default resolve failed", spec.Scope)
		}
		minimum := spec.Minimum
		if got, err := spec.Resolve(&minimum); err != nil || got != minimum {
			t.Fatalf("scope %q minimum resolve failed", spec.Scope)
		}
		capValue := spec.HardCap
		if got, err := spec.Resolve(&capValue); err != nil || got != capValue {
			t.Fatalf("scope %q hard cap resolve failed", spec.Scope)
		}
		aboveCap := spec.HardCap + 1
		if _, err := spec.Resolve(&aboveCap); err == nil {
			t.Fatalf("scope %q accepted cap+1", spec.Scope)
		}
		zero := int64(0)
		if _, err := spec.Resolve(&zero); err == nil {
			t.Fatalf("scope %q accepted explicit zero", spec.Scope)
		}
		negative := int64(-1)
		if _, err := spec.Resolve(&negative); err == nil {
			t.Fatalf("scope %q accepted a negative value", spec.Scope)
		}
		if _, err := spec.ResolveUnsigned(math.MaxUint64); !errors.Is(err, ErrIntegerOverflow) {
			t.Fatalf("scope %q did not reject integer overflow", spec.Scope)
		}
	}

	for _, expected := range expectedSpecs {
		if !seenScopes[expected.Scope] {
			t.Fatalf("budget scope %q is missing", expected.Scope)
		}
	}
	for dimension := Bytes; dimension < dimensionCount; dimension++ {
		if !seenDimensions[dimension] {
			t.Fatalf("budget dimension %s has no specification", dimension)
		}
	}
}

func TestLimitsRejectNegativeAndAboveHardCap(t *testing.T) {
	limitErrorType := reflect.TypeOf(LimitError{})
	wantFields := []string{"Scope", "Dimension", "Limit", "Observed"}
	if limitErrorType.NumField() != len(wantFields) {
		t.Fatalf("LimitError field count = %d, want %d safe metadata fields", limitErrorType.NumField(), len(wantFields))
	}
	for index, name := range wantFields {
		if got := limitErrorType.Field(index).Name; got != name {
			t.Fatalf("LimitError field %d = %q, want %q", index, got, name)
		}
	}

	for dimension := Bytes; dimension < dimensionCount; dimension++ {
		if _, err := NewLimits(Limit{Dimension: dimension, Value: 0}); err == nil {
			t.Fatalf("dimension %s accepted zero", dimension)
		}
		if _, err := NewLimits(Limit{Dimension: dimension, Value: -1}); err == nil {
			t.Fatalf("dimension %s accepted a negative limit", dimension)
		}

		effective, err := NewLimits(Limit{Dimension: dimension, Value: 11})
		if err != nil {
			t.Fatalf("dimension %s effective limits: %v", dimension, err)
		}
		hard, err := NewLimits(Limit{Dimension: dimension, Value: 10})
		if err != nil {
			t.Fatalf("dimension %s hard limits: %v", dimension, err)
		}
		err = ValidateLimits("test-scope", effective, hard)
		var limitErr *LimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("dimension %s above-cap error type = %T, want *LimitError", dimension, err)
		}
		if limitErr.Scope != "test-scope" || limitErr.Dimension != dimension || limitErr.Limit != 10 || limitErr.Observed != 11 {
			t.Fatalf("dimension %s returned incorrect safe limit metadata", dimension)
		}
	}

	if _, err := NewLimits(Limit{Dimension: dimensionCount, Value: 1}); err == nil {
		t.Fatal("unknown dimension was accepted")
	}
}
