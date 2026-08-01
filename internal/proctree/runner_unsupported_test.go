package proctree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"xagent/internal/safefs"
)

const unsupportedTargetMarkerEnvironment = "XAGENT_UNSUPPORTED_TARGET_MARKER"

func TestUnsupportedRunnerFailsBeforeTargetStart(t *testing.T) {
	testCases := []struct {
		name     string
		mode     ProtectionMode
		withPlan bool
	}{
		{name: "unsupported_platform", mode: ProtectionRequired, withPlan: true},
		{name: "mode_zero", mode: 0, withPlan: true},
		{name: "zero_plan", mode: ProtectionRequired, withPlan: false},
		{name: "capability_unavailable", mode: ProtectionRequired, withPlan: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			root := bootstrapTestRoot(t)
			defer root.Close()
			plan := ProtectionPlan{}
			if testCase.withPlan {
				var err error
				plan, err = newProtectionPlanWithScratch([]*safefs.Root{root}, t.TempDir())
				if err != nil {
					t.Fatal("create unsupported protection plan failed")
				}
				defer plan.cleanupScratch()
			}
			marker := filepath.Join(t.TempDir(), "target-started")
			runner, err := newUnsupportedRunner(Options{})
			if err != nil {
				t.Fatal("create unsupported runner failed")
			}
			process, err := runner.Start(context.Background(), Request{
				Executable: os.Args[0],
				Args:       []string{"-test.run=^TestUnsupportedRunnerTargetHelper$", "-test.count=1"},
				WorkingDir: root,
				Env:        append(os.Environ(), unsupportedTargetMarkerEnvironment+"="+marker),
				Mode:       testCase.mode,
				Protection: plan,
			})
			var startErr *StartError
			if process != nil || !errors.As(err, &startErr) ||
				startErr.Code != startCodeProtectedExecUnavailable || startErr.TargetStarted {
				t.Fatal("unsupported runner did not fail closed before target start")
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatal("unsupported runner started its target")
			}
			if testCase.withPlan && plan.valid() {
				t.Fatal("unsupported runner retained its private scratch")
			}
		})
	}
}

func TestUnsupportedRunnerTargetHelper(t *testing.T) {
	marker := os.Getenv(unsupportedTargetMarkerEnvironment)
	if marker == "" {
		return
	}
	if err := os.WriteFile(marker, []byte("started"), 0o600); err != nil {
		t.Fatal("write unsupported target marker failed")
	}
}
