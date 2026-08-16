package tool

import (
	"fmt"
	"reflect"

	"xagent/internal/agentrole"
)

// WorkspaceFilterReason is a stable, bounded preparation diagnostic. Binder
// errors abort preparation and are therefore not represented as successful
// filters.
type WorkspaceFilterReason string

const (
	WorkspaceFilterFixed              WorkspaceFilterReason = "fixed_workspace"
	WorkspaceFilterUnknown            WorkspaceFilterReason = "unknown_workspace"
	WorkspaceFilterNoWriteContainment WorkspaceFilterReason = "write_containment_unavailable"
)

type WorkspaceBindingReport struct {
	Filtered map[string]WorkspaceFilterReason
}

// BindWorkspaceRegistry creates a new registry lineage for one task. It
// rebuilds every contextual execution target before any role/placement view
// can be derived, reuses only explicitly independent targets, and filters all
// fixed or unknown targets.
func BindWorkspaceRegistry(source *Registry, binding WorkspaceBinding) (*Registry, WorkspaceBindingReport, error) {
	report := WorkspaceBindingReport{Filtered: make(map[string]WorkspaceFilterReason)}
	if source == nil || !source.IsSealed() {
		return nil, report, fmt.Errorf("workspace source registry must be sealed")
	}
	if err := binding.validate(); err != nil {
		return nil, report, err
	}

	bound := newEmptyRegistry()
	bound.safeCandidate = source.safeCandidate
	for _, name := range source.Names() {
		descriptor, ok := source.Descriptor(name)
		if !ok {
			return nil, report, fmt.Errorf("workspace tool %q metadata is unavailable", name)
		}
		executor, ok := source.executionTool(name)
		if !ok || executor == nil {
			return nil, report, fmt.Errorf("workspace tool %q execution target is unavailable", name)
		}

		var target Tool
		var binder WorkspaceBinder
		switch descriptor.Workspace.Mode {
		case WorkspaceIndependent:
			target = executor
		case WorkspaceContextual:
			if name == "Bash" && !descriptor.Workspace.WriteContainment {
				report.Filtered[name] = WorkspaceFilterNoWriteContainment
				continue
			}
			binder = source.workspaceBinders[name]
			if binder == nil {
				return nil, report, fmt.Errorf("workspace tool %q binder is unavailable", name)
			}
			var err error
			target, err = binder.BindWorkspace(binding)
			if err != nil || target == nil {
				return nil, report, fmt.Errorf("workspace tool %q binding failed", name)
			}
			if !sameWorkspaceToolDefinition(descriptor, target) {
				return nil, report, fmt.Errorf("workspace tool %q binding changed its public definition", name)
			}
		case WorkspaceFixed:
			if name == "Bash" {
				report.Filtered[name] = WorkspaceFilterNoWriteContainment
			} else {
				report.Filtered[name] = WorkspaceFilterFixed
			}
			continue
		case WorkspaceUnknown:
			report.Filtered[name] = WorkspaceFilterUnknown
			continue
		default:
			return nil, report, fmt.Errorf("workspace tool %q policy is invalid", name)
		}

		options := RegistrationOptions{
			Route:           descriptor.Route,
			Policy:          descriptor.Policy,
			Workspace:       descriptor.Workspace,
			WorkspaceBinder: binder,
			TargetDigest:    descriptor.TargetDigest,
		}
		if err := bound.RegisterWithOptions(target, options); err != nil {
			return nil, report, fmt.Errorf("workspace tool %q registration failed: %w", name, err)
		}
		registered, ok := bound.Descriptor(name)
		if !ok || registered.Route != descriptor.Route {
			return nil, report, fmt.Errorf("workspace tool %q route changed during binding", name)
		}
	}
	if err := bound.Seal(); err != nil {
		return nil, report, fmt.Errorf("workspace registry seal failed: %w", err)
	}
	return bound, report, nil
}

// BuildWorkspaceCapabilityViews makes the security order explicit for task
// assembly: workspace rebinding first, then monotonic role/placement filters.
func BuildWorkspaceCapabilityViews(
	source *Registry,
	binding WorkspaceBinding,
	role *agentrole.Definition,
	background BackgroundPolicy,
	globalDenied map[string]struct{},
	planMode bool,
) (CapabilitySet, CapabilitySet, WorkspaceBindingReport, error) {
	registry, report, err := BindWorkspaceRegistry(source, binding)
	if err != nil {
		return CapabilitySet{}, CapabilitySet{}, report, err
	}
	foreground, detached, err := BuildCapabilityViews(registry, role, background, globalDenied, planMode)
	return foreground, detached, report, err
}

func sameWorkspaceToolDefinition(descriptor ToolDescriptor, target Tool) bool {
	return target != nil &&
		target.Name() == descriptor.Name &&
		target.Description() == descriptor.Description &&
		target.Risk() == descriptor.Risk &&
		reflect.DeepEqual(cloneSchema(target.Schema()), descriptor.Schema)
}
