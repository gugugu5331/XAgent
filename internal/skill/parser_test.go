package skill

import (
	"fmt"
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

func TestSkillHistoryDefaultExplicitZeroAndIsolatedBoundaries(t *testing.T) {
	defaulted, _, err := parseDefault(skillHistoryDocument(ModeIsolated, "", "default body"))
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.History != 0 || defaulted.historySet {
		t.Fatalf("unset history did not retain default presence: %#v", defaulted)
	}

	explicitZero, _, err := parseDefault(skillHistoryDocument(ModeIsolated, "history: 0\n", "zero body"))
	if err != nil {
		t.Fatal(err)
	}
	if explicitZero.History != 0 || !explicitZero.historySet {
		t.Fatalf("explicit zero history lost presence: %#v", explicitZero)
	}

	boundary, _, err := parseDefault(skillHistoryDocument(ModeIsolated, "history: 1000\n", "boundary body"))
	if err != nil {
		t.Fatal(err)
	}
	if boundary.History != maxSkillHistoryTurns || !boundary.historySet {
		t.Fatalf("isolated history boundary changed: %#v", boundary)
	}
	validated, err := ValidateMetadata(Metadata{Name: "direct", Description: "direct", Mode: ModeIsolated, History: maxSkillHistoryTurns})
	if err != nil || validated.History != maxSkillHistoryTurns {
		t.Fatalf("ValidateMetadata rejected isolated boundary: %#v %v", validated, err)
	}
}

func TestSharedSkillRejectsPositiveHistory(t *testing.T) {
	const bodyCanary = "shared-history-body-canary"
	_, _, err := parseDefault(skillHistoryDocument(ModeShared, "history: 1\n", bodyCanary))
	assertSkillHistoryError(t, err, 0, "1", bodyCanary)

	_, err = ValidateMetadata(Metadata{Name: "shared", Description: "shared", Mode: ModeShared, History: 1})
	assertSkillHistoryError(t, err, 0, "1")

	metadata, _, err := parseDefault(skillHistoryDocument(ModeShared, "history: 0\n", "shared zero"))
	if err != nil || metadata.History != 0 || !metadata.historySet {
		t.Fatalf("shared explicit zero was rejected: %#v %v", metadata, err)
	}
}

func TestSkillHistoryRejectsNegativeCapPlusOneOverflowAndWrongTypeWithoutLeak(t *testing.T) {
	const bodyCanary = "isolated-history-body-canary"
	invalid := []struct {
		name   string
		scalar string
	}{
		{name: "negative", scalar: "-7"},
		{name: "cap plus one", scalar: "1001"},
		{name: "integer overflow", scalar: "9223372036854775808"},
		{name: "string", scalar: `"history-wrong-type-canary"`},
		{name: "boolean", scalar: "true"},
		{name: "float", scalar: "1.5"},
		{name: "sequence", scalar: "[1]"},
		{name: "mapping", scalar: "{value: 1}"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			input := skillHistoryDocument(ModeIsolated, "history: "+test.scalar+"\n", bodyCanary)
			_, _, err := parseDefault(input)
			assertSkillHistoryError(t, err, maxSkillHistoryTurns, test.scalar, bodyCanary)
		})
	}

	for _, value := range []int{-1, maxSkillHistoryTurns + 1} {
		_, err := ValidateMetadata(Metadata{Name: "direct", Description: "direct", Mode: ModeIsolated, History: value})
		assertSkillHistoryError(t, err, maxSkillHistoryTurns, fmt.Sprint(value))
	}
}

func skillHistoryDocument(mode Mode, historyLine, body string) []byte {
	return []byte(fmt.Sprintf("---\nname: history-test\ndescription: history test\nmode: %s\n%s---\n%s", mode, historyLine, body))
}

func assertSkillHistoryError(t *testing.T, err error, maximum int, forbidden ...string) {
	t.Helper()
	want := fmt.Sprintf("skill metadata field history must be an integer in range 0..%d", maximum)
	if err == nil || err.Error() != want {
		t.Fatalf("history error = %v, want %q", err, want)
	}
	for _, value := range forbidden {
		if value != "" && strings.Contains(err.Error(), value) {
			t.Fatalf("history error leaked rejected input %q: %v", value, err)
		}
	}
}

func TestParseRejectsUnknownFieldEmptyBodyAndLimitsWithoutLeakingBody(t *testing.T) {
	bodyCanary := "parser-body-leak-canary"
	unknown := "---\nname: demo\ndescription: demo\nmode: shared\nunknown_field: true\n---\n" + bodyCanary
	if _, _, err := parseDefault([]byte(unknown)); err == nil || strings.Contains(err.Error(), bodyCanary) {
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
