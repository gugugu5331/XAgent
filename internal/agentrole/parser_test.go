package agentrole

import (
	"strings"
	"testing"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

func TestParseMarkdownPreservesBodyAndPresenceSemantics(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	candidate, err := ParseMarkdown([]byte(`---
name: " ReviewER "
description: " Reviews changes "
allowed_tools: []
denied_tools: [Write, Read, Write]
model: sonnet
max_iterations: 0
permission_mode: strict
---
First instruction.

Second instruction.
`), ParseOptions{Origin: "reviewer.md", Limits: DefaultLimits(), Redactor: redactor})
	if err != nil {
		t.Fatalf("ParseMarkdown() error = %v", err)
	}
	if !candidate.Valid || len(candidate.Diagnostics) != 0 {
		t.Fatalf("candidate = %#v", candidate)
	}
	if candidate.Name != "reviewer" || candidate.Description.Text() != "Reviews changes" {
		t.Fatalf("metadata = %#v", candidate.Metadata)
	}
	if candidate.ToolAllow == nil || len(candidate.ToolAllow) != 0 {
		t.Fatalf("allowed_tools = %#v, want explicit empty", candidate.ToolAllow)
	}
	if got := strings.Join(candidate.ToolDeny, ","); got != "Read,Write" {
		t.Fatalf("denied_tools = %q", got)
	}
	if candidate.Model != ModelSonnet || candidate.PermissionMode != PermissionStrict {
		t.Fatalf("enum metadata = %#v", candidate.Metadata)
	}
	if candidate.MaxIterations == nil || *candidate.MaxIterations != 0 {
		t.Fatalf("max_iterations = %#v", candidate.MaxIterations)
	}
	if got := candidate.Instructions.Text(); got != "First instruction.\n\nSecond instruction." {
		t.Fatalf("instructions = %q", got)
	}

	withoutAllow, err := ParseMarkdown([]byte(`---
name: helper
description: Helps safely
---
Do the work.
`), ParseOptions{Origin: "helper.md", Limits: DefaultLimits(), Redactor: redactor})
	if err != nil || !withoutAllow.Valid {
		t.Fatalf("omitted fields candidate = %#v, error = %v", withoutAllow, err)
	}
	if withoutAllow.ToolAllow != nil {
		t.Fatalf("omitted allowed_tools = %#v, want nil", withoutAllow.ToolAllow)
	}
	if withoutAllow.Model != ModelInherit || withoutAllow.PermissionMode != PermissionInherit {
		t.Fatalf("omitted enums = %#v", withoutAllow.Metadata)
	}
}

func TestParseMarkdownRedactsBeforePublishing(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret("super-secret-token")
	candidate, err := ParseMarkdown([]byte(`---
name: helper
description: Uses super-secret-token safely
---
Never print super-secret-token.
`), ParseOptions{Origin: "super-secret-token.md", Limits: DefaultLimits(), Redactor: redactor})
	if err != nil || !candidate.Valid {
		t.Fatalf("candidate = %#v, error = %v", candidate, err)
	}
	for field, value := range map[string]string{
		"description":  candidate.Description.Text(),
		"instructions": candidate.Instructions.Text(),
		"origin":       candidate.Origin.Text(),
	} {
		if strings.Contains(value, "super-secret-token") {
			t.Fatalf("%s leaked registered secret: %q", field, value)
		}
	}
}

func TestYAMLUnsafeConstructsBecomeInvalidDiagnostics(t *testing.T) {
	tests := map[string]string{
		"unknown field": `name: helper
description: Helps
unexpected: true`,
		"duplicate key": `name: helper
name: duplicate
description: Helps`,
		"anchor": `name: &role helper
description: Helps`,
		"alias": `name: &role helper
description: *role`,
		"merge": `name: helper
description: Helps
<<: {model: haiku}`,
		"explicit standard tag": `name: !!str helper
description: Helps`,
		"custom tag": `name: !role helper
description: Helps`,
		"directive": `%YAML 1.2
name: helper
description: Helps`,
		"explicit null": `name: helper
description: null`,
		"wrong sequence type": `name: helper
description: Helps
allowed_tools: Read`,
		"string iterations": `name: helper
description: Helps
max_iterations: "2"`,
		"negative iterations": `name: helper
description: Helps
max_iterations: -1`,
		"float iterations": `name: helper
description: Helps
max_iterations: 1.5`,
	}
	for name, frontmatter := range tests {
		t.Run(name, func(t *testing.T) {
			raw := "---\n" + frontmatter + "\n---\nDo work.\n"
			candidate, err := ParseMarkdown([]byte(raw), ParseOptions{
				Origin:   "unsafe.md",
				Limits:   DefaultLimits(),
				Redactor: redact.NewRuntimeRedactor(),
			})
			if err != nil {
				t.Fatalf("content error escaped structured candidate: %v", err)
			}
			assertInvalidRoleCandidate(t, candidate)
		})
	}
}

func TestFrontmatterBoundariesAndLimitsBecomeInvalidDiagnostics(t *testing.T) {
	valid := `---
name: helper
description: Helps
---
Do work.
`
	tests := map[string][]byte{
		"missing opening delimiter": []byte(strings.TrimPrefix(valid, "---\n")),
		"delimiter not first line":  []byte("\n" + valid),
		"missing closing delimiter": []byte("---\nname: helper\ndescription: Helps\nDo work."),
		"empty body":                []byte("---\nname: helper\ndescription: Helps\n---\n  \n"),
		"invalid utf8":              append([]byte(valid), 0xff),
		"trailing yaml document":    []byte("---\nname: helper\ndescription: Helps\n---\n---\nname: other\n---\nBody\n"),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			candidate, err := ParseMarkdown(raw, ParseOptions{
				Origin:   "boundary.md",
				Limits:   DefaultLimits(),
				Redactor: redact.NewRuntimeRedactor(),
			})
			if err != nil {
				t.Fatalf("content error escaped structured candidate: %v", err)
			}
			assertInvalidRoleCandidate(t, candidate)
		})
	}

	limits := DefaultLimits()
	limits.MaxBodyBytes = 4
	limits.MaxInstructionBytes = 4
	candidate, err := ParseMarkdown([]byte(valid), ParseOptions{
		Origin: "limit.md", Limits: limits, Redactor: redact.NewRuntimeRedactor(),
	})
	if err != nil {
		t.Fatalf("limit error escaped structured candidate: %v", err)
	}
	assertInvalidRoleCandidate(t, candidate)
}

func TestParseRejectsInvalidNamesEnumsAndControls(t *testing.T) {
	tests := []string{
		"name: Bad Name\ndescription: Helps",
		"name: helper\ndescription: Helps\nmodel: Sonnet",
		"name: helper\ndescription: Helps\nmodel: ''",
		"name: helper\ndescription: Helps\npermission_mode: admin",
		"name: helper\ndescription: \"bad\\x1btext\"",
		"name: helper\ndescription: Helps\nallowed_tools: ['']",
	}
	for _, frontmatter := range tests {
		candidate, err := ParseMarkdown([]byte("---\n"+frontmatter+"\n---\nDo work.\n"), ParseOptions{
			Origin: "invalid.md", Limits: DefaultLimits(), Redactor: redact.NewRuntimeRedactor(),
		})
		if err != nil {
			t.Fatalf("content error escaped structured candidate: %v", err)
		}
		assertInvalidRoleCandidate(t, candidate)
	}
}

func assertInvalidRoleCandidate(t *testing.T, candidate Candidate) {
	t.Helper()
	if candidate.Valid || len(candidate.Diagnostics) == 0 {
		t.Fatalf("candidate = %#v, want invalid diagnostic", candidate)
	}
	for _, diagnostic := range candidate.Diagnostics {
		if diagnostic.Code == "" || diagnostic.Message.Text() == "" || diagnostic.Severity != diagnostics.SeverityError {
			t.Fatalf("unsafe diagnostic = %#v", diagnostic)
		}
	}
}
