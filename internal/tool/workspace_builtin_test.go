package tool

import (
	"path/filepath"
	"testing"
)

func TestWorkspaceBuiltinFileToolsRebindToTaskRoot(t *testing.T) {
	base, err := NewRegistry(filepath.Join(t.TempDir(), "main"))
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	if err := base.Seal(); err != nil {
		t.Fatalf("seal registry: %v", err)
	}
	binding := WorkspaceBinding{WorkspaceID: "builtin", Root: filepath.Join(t.TempDir(), "worktree"), ScratchRoot: filepath.Join(t.TempDir(), "scratch")}
	bound, report, err := BindWorkspaceRegistry(base, binding)
	if err != nil {
		t.Fatalf("bind builtins: %v", err)
	}

	for _, name := range []string{"Read", "Write", "Edit", "Glob", "Grep"} {
		original, _ := base.executionTool(name)
		rebuilt, ok := bound.executionTool(name)
		if !ok {
			t.Fatalf("%s was filtered instead of rebound: %#v", name, report.Filtered)
		}
		if rebuilt == original {
			t.Fatalf("%s reused the main-root instance", name)
		}
		if got := builtinProjectRoot(rebuilt); got != binding.Root {
			t.Fatalf("%s root = %q, want %q", name, got, binding.Root)
		}
	}
	if _, ok := bound.Get("Bash"); ok {
		t.Fatal("Bash without trusted write containment was exposed")
	}
	if report.Filtered["Bash"] != WorkspaceFilterNoWriteContainment {
		t.Fatalf("Bash filter reason = %q", report.Filtered["Bash"])
	}
}

func TestBashContainmentRequiresTrustedContextualPolicy(t *testing.T) {
	for _, test := range []struct {
		name        string
		containment bool
		wantAllowed bool
	}{
		{name: "missing containment", containment: false, wantAllowed: false},
		{name: "trusted containment", containment: true, wantAllowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newEmptyRegistry()
			bash := NewBashTool(filepath.Join(t.TempDir(), "main"))
			binder := workspaceBashTestBinder{}
			if err := base.RegisterWithOptions(bash, RegistrationOptions{
				Workspace:       WorkspacePolicy{Mode: WorkspaceContextual, WriteContainment: test.containment},
				WorkspaceBinder: binder,
			}); err != nil {
				t.Fatalf("register Bash: %v", err)
			}
			if err := base.Seal(); err != nil {
				t.Fatalf("seal Bash registry: %v", err)
			}
			binding := WorkspaceBinding{WorkspaceID: "bash", Root: filepath.Join(t.TempDir(), "worktree"), ScratchRoot: filepath.Join(t.TempDir(), "scratch")}
			bound, report, err := BindWorkspaceRegistry(base, binding)
			if err != nil {
				t.Fatalf("bind Bash registry: %v", err)
			}
			_, allowed := bound.Get("Bash")
			if allowed != test.wantAllowed {
				t.Fatalf("Bash allowed = %v, want %v; filters=%#v", allowed, test.wantAllowed, report.Filtered)
			}
		})
	}
}

type workspaceBashTestBinder struct{}

func (workspaceBashTestBinder) BindWorkspace(binding WorkspaceBinding) (Tool, error) {
	return NewBashTool(binding.Root), nil
}

func builtinProjectRoot(target Tool) string {
	switch typed := target.(type) {
	case *ReadTool:
		return typed.projectRoot
	case *WriteTool:
		return typed.projectRoot
	case *EditTool:
		return typed.projectRoot
	case *GlobTool:
		return typed.projectRoot
	case *GrepTool:
		return typed.projectRoot
	default:
		return ""
	}
}
