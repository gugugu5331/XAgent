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
