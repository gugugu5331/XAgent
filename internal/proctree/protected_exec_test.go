package proctree

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"xagent/internal/safefs"
)

func TestProtectionPlanFactoryCreatesPrivatePerRunPlan(t *testing.T) {
	project := bootstrapTestRoot(t)
	defer project.Close()
	parent := t.TempDir()
	roots := []*safefs.Root{project}
	factory, err := NewProtectionPlanFactory(roots, parent)
	if err != nil {
		t.Fatal("create protection plan factory failed")
	}
	roots[0] = nil

	first, err := factory.Create(context.Background())
	if err != nil {
		t.Fatal("create first factory plan failed")
	}
	second, err := factory.Create(context.Background())
	if err != nil {
		_ = first.cleanupScratch()
		t.Fatal("create second factory plan failed")
	}
	defer second.cleanupScratch()
	if !first.validForStart() || !second.validForStart() ||
		first.scratchOwner.path == second.scratchOwner.path ||
		first.scratch.Identity() == second.scratch.Identity() {
		t.Fatal("factory did not create independent runnable plans")
	}
	for _, plan := range []ProtectionPlan{first, second} {
		info, statErr := os.Stat(plan.scratchOwner.path)
		if statErr != nil || !info.IsDir() {
			t.Fatal("factory scratch was unavailable")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Fatal("factory scratch was not private")
		}
	}
	firstPath := first.scratchOwner.path
	if err := first.cleanupScratch(); err != nil {
		t.Fatal("cleanup first factory plan failed")
	}
	if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
		t.Fatal("factory plan cleanup retained scratch")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := factory.Create(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled factory create did not fail before scratch creation")
	}

	third, err := factory.Create(context.Background())
	if err != nil {
		t.Fatal("create third factory plan failed")
	}
	thirdPath := third.scratchOwner.path
	runner, err := NewRunner(Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}})
	if err != nil {
		_ = third.cleanupScratch()
		t.Fatal("create platform runner failed")
	}
	startCtx, stop := context.WithCancel(context.Background())
	stop()
	if _, err := runner.Start(startCtx, Request{WorkingDir: project, Mode: ProtectionRequired, Protection: third}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled start did not report cancellation")
	}
	if third.valid() {
		t.Fatal("runner retained plan after canceled start")
	}
	if _, err := os.Stat(thirdPath); !os.IsNotExist(err) {
		t.Fatal("runner retained scratch after canceled start")
	}
}

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
