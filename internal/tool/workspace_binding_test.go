package tool

import (
	"context"
	"path/filepath"
	"testing"

	"xagent/internal/agentrole"
)

func TestWorkspaceBindingRebuildsBeforeCapabilityFiltering(t *testing.T) {
	base := newEmptyRegistry()
	binder := &workspaceBindingProbe{name: "contextual", root: "/assembly"}
	independent := &workspaceBindingProbe{name: "independent", root: "/assembly"}
	fixed := &workspaceBindingProbe{name: "fixed", root: "/assembly"}
	unknown := &workspaceBindingProbe{name: "unknown", root: "/assembly"}
	registerWorkspaceProbe(t, base, binder, WorkspacePolicy{Mode: WorkspaceContextual}, binder)
	registerWorkspaceProbe(t, base, independent, WorkspacePolicy{Mode: WorkspaceIndependent}, nil)
	registerWorkspaceProbe(t, base, fixed, WorkspacePolicy{Mode: WorkspaceFixed}, nil)
	registerWorkspaceProbe(t, base, unknown, WorkspacePolicy{}, nil)
	if err := base.Seal(); err != nil {
		t.Fatalf("seal base registry: %v", err)
	}

	root := filepath.Join(t.TempDir(), "worktree")
	scratch := filepath.Join(t.TempDir(), "scratch")
	binding := WorkspaceBinding{WorkspaceID: "ws-1", Root: root, ScratchRoot: scratch}
	role := &agentrole.Definition{Metadata: agentrole.Metadata{ToolAllow: []string{"independent"}}}
	foreground, _, report, err := BuildWorkspaceCapabilityViews(base, binding, role, BackgroundPolicy{}, nil, false)
	if err != nil {
		t.Fatalf("build workspace capability views: %v", err)
	}
	if binder.bindCalls != 1 || binder.lastBinding != binding {
		t.Fatalf("contextual binder was not called before role filtering: calls=%d binding=%#v", binder.bindCalls, binder.lastBinding)
	}
	if len(foreground.Names) != 1 || foreground.Names[0] != "independent" {
		t.Fatalf("foreground tools = %v, want independent only", foreground.Names)
	}
	if report.Filtered["fixed"] != WorkspaceFilterFixed || report.Filtered["unknown"] != WorkspaceFilterUnknown {
		t.Fatalf("workspace filter report = %#v", report.Filtered)
	}
}

func TestWorkspaceBindingContextualNewInstanceIndependentReuseAndNewLineage(t *testing.T) {
	base := newEmptyRegistry()
	contextual := &workspaceBindingProbe{name: "contextual", root: "/assembly"}
	independent := &workspaceBindingProbe{name: "independent", root: "/assembly"}
	registerWorkspaceProbe(t, base, contextual, WorkspacePolicy{Mode: WorkspaceContextual, WriteContainment: true}, contextual)
	registerWorkspaceProbe(t, base, independent, WorkspacePolicy{Mode: WorkspaceIndependent}, nil)
	if err := base.Seal(); err != nil {
		t.Fatalf("seal base registry: %v", err)
	}
	binding := WorkspaceBinding{WorkspaceID: "ws-2", Root: filepath.Join(t.TempDir(), "worktree"), ScratchRoot: filepath.Join(t.TempDir(), "scratch")}

	bound, report, err := BindWorkspaceRegistry(base, binding)
	if err != nil {
		t.Fatalf("bind workspace registry: %v", err)
	}
	if len(report.Filtered) != 0 {
		t.Fatalf("unexpected workspace filters: %#v", report.Filtered)
	}
	if bound.lineage == base.lineage {
		t.Fatal("workspace registry reused base lineage")
	}
	boundContextual, _ := bound.executionTool("contextual")
	if boundContextual == contextual {
		t.Fatal("contextual tool instance was reused")
	}
	if got := boundContextual.(*workspaceBindingProbe).root; got != binding.Root {
		t.Fatalf("contextual root = %q, want %q", got, binding.Root)
	}
	boundIndependent, _ := bound.executionTool("independent")
	if boundIndependent != independent {
		t.Fatal("independent tool instance was not reused")
	}
}

func TestWorkspaceBindingRejectsInvalidBindingAndBinderFailure(t *testing.T) {
	base := newEmptyRegistry()
	binder := &workspaceBindingProbe{name: "contextual", bindErr: errWorkspaceBindingProbe}
	registerWorkspaceProbe(t, base, binder, WorkspacePolicy{Mode: WorkspaceContextual}, binder)
	if err := base.Seal(); err != nil {
		t.Fatalf("seal base registry: %v", err)
	}

	if _, _, err := BindWorkspaceRegistry(base, WorkspaceBinding{WorkspaceID: "ws", Root: "relative", ScratchRoot: "/tmp/scratch"}); err == nil {
		t.Fatal("relative workspace root was accepted")
	}
	valid := WorkspaceBinding{WorkspaceID: "ws", Root: filepath.Join(t.TempDir(), "root"), ScratchRoot: filepath.Join(t.TempDir(), "scratch")}
	if _, _, err := BindWorkspaceRegistry(base, valid); err == nil {
		t.Fatal("contextual binder failure was converted into a filtered success")
	}
}

func registerWorkspaceProbe(t *testing.T, registry *Registry, probe Tool, policy WorkspacePolicy, binder WorkspaceBinder) {
	t.Helper()
	if err := registry.RegisterWithOptions(probe, RegistrationOptions{Workspace: policy, WorkspaceBinder: binder}); err != nil {
		t.Fatalf("register %s: %v", probe.Name(), err)
	}
}

var errWorkspaceBindingProbe = &workspaceBindingError{}

type workspaceBindingError struct{}

func (*workspaceBindingError) Error() string { return "workspace binding probe failed" }

type workspaceBindingProbe struct {
	name        string
	root        string
	bindCalls   int
	lastBinding WorkspaceBinding
	bindErr     error
}

func (probe *workspaceBindingProbe) Name() string                    { return probe.name }
func (*workspaceBindingProbe) Description() string                   { return "workspace binding probe" }
func (*workspaceBindingProbe) Schema() Schema                        { return ObjectSchema(nil, nil) }
func (*workspaceBindingProbe) Risk() Risk                            { return RiskSafe }
func (*workspaceBindingProbe) Execute(context.Context, Input) Result { return Result{} }

func (probe *workspaceBindingProbe) BindWorkspace(binding WorkspaceBinding) (Tool, error) {
	probe.bindCalls++
	probe.lastBinding = binding
	if probe.bindErr != nil {
		return nil, probe.bindErr
	}
	return &workspaceBindingProbe{name: probe.name, root: binding.Root}, nil
}
