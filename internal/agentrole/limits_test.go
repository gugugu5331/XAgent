package agentrole

import "testing"

func TestDefaultLimitsValidateAndRejectUnsafeRelations(t *testing.T) {
	limits := DefaultLimits()
	if err := limits.Validate(); err != nil {
		t.Fatalf("default limits: %v", err)
	}
	if limits.MaxFiles != 256 || limits.MaxEntryBytes != 256<<10 || limits.MaxTotalBytes != 64<<20 {
		t.Fatalf("unexpected role defaults: %#v", limits)
	}

	invalid := limits
	invalid.MaxDiagnostics = 0
	if err := invalid.Validate(); err == nil {
		t.Fatal("zero diagnostics budget was accepted")
	}
	invalid = limits
	invalid.MaxFrontmatterBytes = invalid.MaxEntryBytes + 1
	if err := invalid.Validate(); err == nil {
		t.Fatal("frontmatter above entry size was accepted")
	}
	invalid = limits
	invalid.MaxInstructionBytes = invalid.MaxBodyBytes + 1
	if err := invalid.Validate(); err == nil {
		t.Fatal("instruction limit above body limit was accepted")
	}
}
