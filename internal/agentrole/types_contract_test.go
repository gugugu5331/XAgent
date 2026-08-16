package agentrole

import (
	"testing"

	"xagent/internal/redact"
)

func TestDefinitionPreservesRoleCapabilityMetadata(t *testing.T) {
	definition := Definition{
		Metadata: Metadata{
			Name:           "review",
			ToolAllow:      []string{},
			ToolDeny:       []string{"Agent"},
			Model:          ModelSonnet,
			PermissionMode: PermissionStrict,
		},
		Instructions: redact.NewRuntimeRedactor().Redact("review safely"),
		Provenance:   Provenance{Source: SourceProject, SourceID: "project"},
	}
	if definition.ToolAllow == nil || len(definition.ToolAllow) != 0 || len(definition.ToolDeny) != 1 {
		t.Fatalf("role capability metadata lost presence: %#v", definition)
	}
	if definition.Source != SourceProject || definition.Model != ModelSonnet {
		t.Fatalf("role identity metadata changed: %#v", definition)
	}
}

func TestIsolationModeContract(t *testing.T) {
	if IsolationNone != IsolationMode("") {
		t.Fatalf("IsolationNone = %q, want empty value", IsolationNone)
	}
	if IsolationWorktree != IsolationMode("worktree") {
		t.Fatalf("IsolationWorktree = %q", IsolationWorktree)
	}
	definition := Definition{Metadata: Metadata{Isolation: IsolationWorktree}}
	if definition.Isolation != IsolationWorktree {
		t.Fatalf("definition isolation = %q", definition.Isolation)
	}
}

func TestDefinitionFingerprintIncludesIsolation(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	base := Definition{
		Metadata: Metadata{
			Name: "reviewer", Description: redactor.Redact("Reviews safely"),
			Model: ModelInherit, PermissionMode: PermissionInherit,
		},
		Instructions: redactor.Redact("Review the change."),
		Provenance:   Provenance{Source: SourceProject, SourceID: "project", Origin: redactor.Redact("reviewer.md")},
	}
	isolated := base.Clone()
	isolated.Isolation = IsolationWorktree
	if definitionFingerprint(base) == definitionFingerprint(isolated) {
		t.Fatal("changing isolation did not change definition fingerprint")
	}
}
