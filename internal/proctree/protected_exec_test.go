package proctree

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"xagent/internal/safefs"
)

func TestProtectionPlanOnlyAllowsScratchWrites(t *testing.T) {
	project := bootstrapTestRoot(t)
	defer project.Close()
	permissionRoot := bootstrapTestRoot(t)
	defer permissionRoot.Close()

	roots := []*safefs.Root{project, permissionRoot}
	plan, err := newProtectionPlanWithScratch(roots, t.TempDir())
	if err != nil {
		t.Fatal("create protection plan with scratch failed")
	}
	defer plan.cleanupScratch()

	if !plan.valid() || plan.allowsWrite(project) || plan.allowsWrite(permissionRoot) || !plan.allowsWrite(plan.scratch) {
		t.Fatal("protection plan write classification was incorrect")
	}
	roots[0] = plan.scratch
	if !plan.valid() || plan.allowsWrite(project) || plan.allowsWrite(permissionRoot) {
		t.Fatal("caller root slice mutation widened the protection plan")
	}
	if encoded, err := json.Marshal(plan); err == nil || len(encoded) != 0 {
		t.Fatal("protection plan serialization was not rejected")
	}
}

func TestScratchIsPrivateAndPerRun(t *testing.T) {
	project := bootstrapTestRoot(t)
	defer project.Close()
	parent := t.TempDir()

	first, err := newProtectionPlanWithScratch([]*safefs.Root{project}, parent)
	if err != nil {
		t.Fatal("create first scratch failed")
	}
	second, err := newProtectionPlanWithScratch([]*safefs.Root{project}, parent)
	if err != nil {
		_ = first.cleanupScratch()
		t.Fatal("create second scratch failed")
	}
	defer second.cleanupScratch()

	if first.scratchOwner.path == second.scratchOwner.path || first.scratch.Identity() == second.scratch.Identity() {
		t.Fatal("two runs shared one scratch")
	}
	for _, scratchPath := range []string{first.scratchOwner.path, second.scratchOwner.path} {
		info, statErr := os.Stat(scratchPath)
		if statErr != nil || !info.IsDir() {
			t.Fatal("scratch directory was unavailable")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Fatal("scratch directory was not private")
		}
		if filepath.Dir(scratchPath) != parent {
			t.Fatal("scratch escaped its designated parent")
		}
	}
	if err := os.WriteFile(filepath.Join(second.scratchOwner.path, "live"), nil, 0o600); err != nil {
		t.Fatal("create second scratch marker failed")
	}

	firstCopy := first
	if err := first.cleanupScratch(); err != nil {
		t.Fatal("first scratch cleanup failed")
	}
	if err := firstCopy.cleanupScratch(); err != nil {
		t.Fatal("scratch cleanup was not idempotent across plan copies")
	}
	if first.valid() {
		t.Fatal("plan remained valid after scratch cleanup")
	}
	if _, err := os.Stat(first.scratchOwner.path); !os.IsNotExist(err) {
		t.Fatal("first scratch directory remained after cleanup")
	}
	if _, err := os.Stat(filepath.Join(second.scratchOwner.path, "live")); err != nil {
		t.Fatal("first cleanup affected another run scratch")
	}
}
