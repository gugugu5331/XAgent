package tool

import (
	"path/filepath"
	"testing"
)

func TestWorkspaceFixedSystemToolsAreFiltered(t *testing.T) {
	registry := newEmptyRegistry()
	if err := registry.Register(NewLoadSkillTool()); err != nil {
		t.Fatalf("register load_skill: %v", err)
	}
	if err := registry.RegisterWithOptions(NewAgentTool(), RegistrationOptions{Route: RouteSystem}); err != nil {
		t.Fatalf("register Agent: %v", err)
	}
	for _, name := range []string{LoadSkillToolName, AgentToolName} {
		descriptor, ok := registry.Descriptor(name)
		if !ok || descriptor.Workspace.Mode != WorkspaceFixed {
			t.Fatalf("%s workspace policy = %#v, want Fixed", name, descriptor.Workspace)
		}
	}
	if err := registry.Seal(); err != nil {
		t.Fatalf("seal registry: %v", err)
	}
	binding := WorkspaceBinding{WorkspaceID: "fixed", Root: filepath.Join(t.TempDir(), "root"), ScratchRoot: filepath.Join(t.TempDir(), "scratch")}
	bound, report, err := BindWorkspaceRegistry(registry, binding)
	if err != nil {
		t.Fatalf("bind workspace registry: %v", err)
	}
	if len(bound.Names()) != 0 || report.Filtered[LoadSkillToolName] != WorkspaceFilterFixed || report.Filtered[AgentToolName] != WorkspaceFilterFixed {
		t.Fatalf("fixed tools leaked: names=%v report=%#v", bound.Names(), report.Filtered)
	}
}
