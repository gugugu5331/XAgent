package skill

import (
	"strings"
	"testing"
)

func parseDefault(data []byte) (Metadata, string, error) {
	return ParseWithLimits(data, DefaultLimits())
}

func TestParseFrontmatterBoundary(t *testing.T) {
	metadata, body, err := parseDefault([]byte("---\r\nname: Demo\r\ndescription: Run a demo\r\nmode: isolated\r\nhistory: 2\r\n---\r\nStep {{args}}\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Name != "demo" || metadata.Mode != ModeIsolated || metadata.History != 2 {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
	if body != "Step {{args}}\r\n" {
		t.Fatalf("body was not preserved: %q", body)
	}
	for name, input := range map[string]string{
		"leading whitespace": " \n---\nname: demo\ndescription: demo\nmode: shared\n---\nbody",
		"missing open":       "name: demo\n---\nbody",
		"missing close":      "---\nname: demo\ndescription: demo\nmode: shared\nbody",
		"indented close":     "---\nname: demo\ndescription: demo\nmode: shared\n ---\nbody",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseDefault([]byte(input)); err == nil {
				t.Fatal("expected boundary error")
			}
		})
	}
}

func TestValidateMetadata(t *testing.T) {
	metadata, err := ValidateMetadata(Metadata{
		Name: " Demo_1 ", Description: " demo ", Mode: "ISOLATED", History: 1,
		AllowedTools: []string{" Read ", "Bash", "Read"}, Model: " model-x ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Name != "demo_1" || metadata.Description != "demo" || metadata.Model != "model-x" {
		t.Fatalf("metadata not normalized: %#v", metadata)
	}
	if strings.Join(metadata.AllowedTools, ",") != "Bash,Read" {
		t.Fatalf("tools not normalized: %#v", metadata.AllowedTools)
	}
	tests := []Metadata{
		{Name: "", Description: "d", Mode: ModeShared},
		{Name: "bad/name", Description: "d", Mode: ModeShared},
		{Name: strings.Repeat("a", MaxSkillNameLength+1), Description: "d", Mode: ModeShared},
		{Name: "demo", Description: "", Mode: ModeShared},
		{Name: "demo", Description: "d", Mode: "other"},
		{Name: "demo", Description: "d", Mode: ModeIsolated, History: -1},
		{Name: "demo", Description: "d", Mode: ModeShared, History: 1},
		{Name: "demo", Description: "d", Mode: ModeShared, AllowedTools: []string{" "}},
	}
	for index, input := range tests {
		if _, err := ValidateMetadata(input); err == nil {
			t.Fatalf("case %d unexpectedly passed: %#v", index, input)
		}
	}
}

func TestParseRejectsUnknownFieldEmptyBodyAndLimitsWithoutLeakingBody(t *testing.T) {
	secret := "sk-test-super-secret"
	unknown := "---\nname: demo\ndescription: demo\nmode: shared\nunknown_field: true\n---\n" + secret
	if _, _, err := parseDefault([]byte(unknown)); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unexpected error: %v", err)
	}
	empty := "---\nname: demo\ndescription: demo\nmode: shared\n---\n  \n"
	if _, _, err := parseDefault([]byte(empty)); err == nil {
		t.Fatal("expected empty body error")
	}
	valid := "---\nname: demo\ndescription: demo\nmode: shared\n---\nbody"
	if _, _, err := ParseWithLimits([]byte(valid), Limits{MaxEntryBytes: int64(len(valid)), MaxBodyBytes: 4}); err != nil {
		t.Fatalf("boundary-sized entry failed: %v", err)
	}
	if _, _, err := ParseWithLimits([]byte(valid+"x"), Limits{MaxEntryBytes: int64(len(valid)), MaxBodyBytes: 5}); err == nil {
		t.Fatal("expected entry limit error")
	}
	if _, _, err := ParseWithLimits([]byte(valid), Limits{MaxEntryBytes: int64(len(valid)), MaxBodyBytes: 3}); err == nil {
		t.Fatal("expected body limit error")
	}
}

func TestParseRejectsMultipleYAMLDocuments(t *testing.T) {
	input := "---\nname: demo\ndescription: demo\nmode: shared\n...\nname: other\n---\nbody"
	if _, _, err := parseDefault([]byte(input)); err == nil {
		t.Fatal("expected multiple-document frontmatter to fail")
	}
}
