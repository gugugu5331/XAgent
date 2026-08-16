package tool

import (
	"fmt"
	"path/filepath"
	"strings"
)

// WorkspaceMode classifies whether a registered execution target can be used
// by a task whose project root differs from the assembly root.
type WorkspaceMode string

const (
	WorkspaceUnknown     WorkspaceMode = ""
	WorkspaceIndependent WorkspaceMode = "independent"
	WorkspaceContextual  WorkspaceMode = "contextual"
	WorkspaceFixed       WorkspaceMode = "fixed"
)

// WorkspacePolicy is public, immutable registration metadata. It is trusted
// only when supplied by the local registration boundary.
type WorkspacePolicy struct {
	Mode             WorkspaceMode
	WriteContainment bool
}

func (policy WorkspacePolicy) validate() error {
	switch policy.Mode {
	case WorkspaceUnknown, WorkspaceIndependent, WorkspaceContextual, WorkspaceFixed:
	default:
		return fmt.Errorf("workspace mode is invalid")
	}
	if policy.WriteContainment && policy.Mode != WorkspaceContextual {
		return fmt.Errorf("workspace write containment requires contextual binding")
	}
	return nil
}

// WorkspaceBinding is the immutable task-local input given to a trusted
// contextual binder. Paths must already be absolute; binders must not infer
// them from the process working directory.
type WorkspaceBinding struct {
	WorkspaceID string
	Root        string
	ScratchRoot string
}

func (binding WorkspaceBinding) validate() error {
	if strings.TrimSpace(binding.WorkspaceID) == "" || binding.WorkspaceID != strings.TrimSpace(binding.WorkspaceID) {
		return fmt.Errorf("workspace id is invalid")
	}
	if binding.Root == "" || !filepath.IsAbs(binding.Root) || filepath.Clean(binding.Root) != binding.Root {
		return fmt.Errorf("workspace root must be a clean absolute path")
	}
	if binding.ScratchRoot == "" || !filepath.IsAbs(binding.ScratchRoot) || filepath.Clean(binding.ScratchRoot) != binding.ScratchRoot {
		return fmt.Errorf("workspace scratch root must be a clean absolute path")
	}
	return nil
}

// WorkspaceBinder is a local assembly capability. It is retained privately
// by Registry and is never exposed through ToolDescriptor.
type WorkspaceBinder interface {
	BindWorkspace(WorkspaceBinding) (Tool, error)
}
