package tool

import (
	"context"
	"encoding/json"
	"testing"
)

func TestWorkspacePolicyRegistrationCloneAndFingerprint(t *testing.T) {
	contextual := sealedWorkspacePolicyRegistry(t, WorkspacePolicy{Mode: WorkspaceContextual, WriteContainment: true})
	descriptor, ok := contextual.Descriptor("workspace_probe")
	if !ok {
		t.Fatal("workspace policy descriptor is missing")
	}
	if descriptor.Workspace.Mode != WorkspaceContextual || !descriptor.Workspace.WriteContainment {
		t.Fatalf("workspace policy = %#v, want contextual containment", descriptor.Workspace)
	}

	descriptor.Workspace.Mode = WorkspaceFixed
	again, _ := contextual.Descriptor("workspace_probe")
	if again.Workspace.Mode != WorkspaceContextual {
		t.Fatalf("descriptor mutation escaped clone boundary: %#v", again.Workspace)
	}

	independent := sealedWorkspacePolicyRegistry(t, WorkspacePolicy{Mode: WorkspaceIndependent})
	contextualCapabilities := CapabilitySet{Registry: contextual, Names: contextual.Names(), Rejections: map[string]FilterReason{}}
	independentCapabilities := CapabilitySet{Registry: independent, Names: independent.Names(), Rejections: map[string]FilterReason{}}
	if capabilityFingerprint(contextualCapabilities) == capabilityFingerprint(independentCapabilities) {
		t.Fatal("workspace policy did not participate in capability fingerprint")
	}
}

func TestWorkspacePolicyRemoteAnnotationsCannotElevate(t *testing.T) {
	registry := newEmptyRegistry()
	annotations := json.RawMessage(`{"x-xagent-workspace":{"mode":"independent","writeContainment":true}}`)
	if err := registry.RegisterWithOptions(workspacePolicyProbe{}, RegistrationOptions{RemoteAnnotations: annotations}); err != nil {
		t.Fatalf("register remote annotated tool: %v", err)
	}
	descriptor, ok := registry.Descriptor("workspace_probe")
	if !ok {
		t.Fatal("descriptor is missing")
	}
	if descriptor.Workspace != (WorkspacePolicy{}) {
		t.Fatalf("remote annotations elevated workspace policy: %#v", descriptor.Workspace)
	}
}

func TestWorkspacePolicyRejectsInvalidLocalMode(t *testing.T) {
	registry := newEmptyRegistry()
	err := registry.RegisterWithOptions(workspacePolicyProbe{}, RegistrationOptions{
		Workspace: WorkspacePolicy{Mode: WorkspaceMode("remote-claimed")},
	})
	if err == nil {
		t.Fatal("invalid local workspace mode was accepted")
	}
}

func sealedWorkspacePolicyRegistry(t *testing.T, policy WorkspacePolicy) *Registry {
	t.Helper()
	registry := newEmptyRegistry()
	options := RegistrationOptions{Workspace: policy}
	if policy.Mode == WorkspaceContextual {
		options.WorkspaceBinder = workspacePolicyProbe{}
	}
	if err := registry.RegisterWithOptions(workspacePolicyProbe{}, options); err != nil {
		t.Fatalf("register workspace policy probe: %v", err)
	}
	if err := registry.Seal(); err != nil {
		t.Fatalf("seal workspace policy registry: %v", err)
	}
	return registry
}

type workspacePolicyProbe struct{}

func (workspacePolicyProbe) Name() string                              { return "workspace_probe" }
func (workspacePolicyProbe) Description() string                       { return "workspace policy probe" }
func (workspacePolicyProbe) Schema() Schema                            { return ObjectSchema(nil, nil) }
func (workspacePolicyProbe) Risk() Risk                                { return RiskSafe }
func (workspacePolicyProbe) Execute(_ context.Context, _ Input) Result { return Result{} }
func (workspacePolicyProbe) BindWorkspace(WorkspaceBinding) (Tool, error) {
	return workspacePolicyProbe{}, nil
}
