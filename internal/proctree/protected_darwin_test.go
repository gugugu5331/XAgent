//go:build darwin

package proctree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xagent/internal/safefs"
)

const darwinHelperModeEnvironment = "XAGENT_DARWIN_HELPER_MODE"

func TestDarwinProtectedExec(t *testing.T) {
	testDarwinProbeFailureDoesNotStartTarget(t)

	projectPath, project := bootstrapRootAtPath(t)
	defer project.Close()
	permissionPath, permissionRoot := bootstrapRootAtPath(t)
	defer permissionRoot.Close()
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project, permissionRoot}, t.TempDir())
	if err != nil {
		t.Fatal("create Darwin protection plan failed")
	}
	defer plan.cleanupScratch()
	startMarker := filepath.Join(t.TempDir(), "target-started-unprotected")
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("resolve Darwin helper executable failed")
	}
	runner, err := newDarwinRunner(Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}})
	if err != nil {
		t.Fatal("create Darwin runner failed")
	}
	environment := replaceEnvironment(os.Environ(), darwinHelperModeEnvironment, "main")
	environment = replaceEnvironment(environment, "XAGENT_DARWIN_PROJECT", projectPath)
	environment = replaceEnvironment(environment, "XAGENT_DARWIN_PERMISSION", permissionPath)
	environment = replaceEnvironment(environment, "XAGENT_DARWIN_SCRATCH", plan.scratchOwner.path)
	environment = replaceEnvironment(environment, "XAGENT_DARWIN_START_MARKER", startMarker)
	request := Request{
		Executable: executable,
		Args:       []string{"-test.run=^TestDarwinProtectedExecHelper$", "-test.count=1"},
		WorkingDir: project,
		Env:        environment,
		Mode:       ProtectionRequired,
		Protection: plan,
	}
	process, err := runner.Start(context.Background(), request)
	if err != nil {
		var startErr *StartError
		if !errors.As(err, &startErr) || startErr.Code != startCodeProtectedExecUnavailable || startErr.TargetStarted {
			t.Fatal("Darwin capability failure did not fail closed")
		}
		if _, statErr := os.Stat(startMarker); !os.IsNotExist(statErr) || plan.valid() {
			t.Fatal("Darwin capability failure started the target or retained scratch")
		}
		t.Log("Darwin Seatbelt capability unavailable; fail-closed path verified")
		return
	}
	closed := false
	defer func() {
		if !closed {
			_ = process.Close(context.Background())
		}
	}()
	output, readErr := io.ReadAll(process.Pipes().Stdout)
	if readErr != nil {
		t.Fatal("read Darwin helper output failed")
	}
	waitContext, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWait()
	result, waitErr := process.Wait(waitContext)
	if waitErr != nil || result.ExitCode != 0 || !strings.Contains(string(output), "protected-exec-ok") {
		t.Fatalf("Darwin protected helper failed: exit=%d output=%q", result.ExitCode, output)
	}
	if err := process.Close(context.Background()); err != nil {
		t.Fatal("close Darwin protected process failed")
	}
	closed = true
	for _, escaped := range []string{
		filepath.Join(projectPath, "direct"),
		filepath.Join(projectPath, "variable"),
		filepath.Join(projectPath, "concatenated"),
		filepath.Join(projectPath, "interpreter"),
		filepath.Join(permissionPath, "descendant"),
		startMarker,
	} {
		if _, statErr := os.Stat(escaped); !os.IsNotExist(statErr) {
			t.Fatal("Darwin protected target wrote outside scratch")
		}
	}
	if plan.valid() {
		t.Fatal("Darwin terminal cleanup retained scratch")
	}
	t.Log("Darwin Seatbelt scratch-only write policy and bypass matrix verified")
}

func testDarwinProbeFailureDoesNotStartTarget(t *testing.T) {
	t.Helper()
	projectPath, project := bootstrapRootAtPath(t)
	defer project.Close()
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project}, t.TempDir())
	if err != nil {
		t.Fatal("create failed-probe protection plan failed")
	}
	defer plan.cleanupScratch()
	marker := filepath.Join(projectPath, "probe-failure-marker")
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("resolve failed-probe helper executable failed")
	}
	runner, err := newDarwinRunnerWithProtection(
		Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}},
		darwinProtection{
			launcher: darwinSeatbeltLauncher,
			probe: func(context.Context, string, string, string) error {
				return errors.New("injected Seatbelt probe failure")
			},
		},
	)
	if err != nil {
		t.Fatal("create failed-probe Darwin runner failed")
	}
	environment := replaceEnvironment(os.Environ(), darwinHelperModeEnvironment, "marker")
	environment = replaceEnvironment(environment, "XAGENT_DARWIN_START_MARKER", marker)
	_, err = runner.Start(context.Background(), Request{
		Executable: executable,
		Args:       []string{"-test.run=^TestDarwinProtectedExecHelper$", "-test.count=1"},
		WorkingDir: project,
		Env:        environment,
		Mode:       ProtectionRequired,
		Protection: plan,
	})
	var startErr *StartError
	if !errors.As(err, &startErr) || startErr.Code != startCodeProtectedExecUnavailable || startErr.TargetStarted {
		t.Fatal("injected Darwin probe failure did not fail closed")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) || plan.valid() {
		t.Fatal("probe failure started target or retained scratch")
	}
}
func TestDarwinProtectedExecHelper(t *testing.T) {
	mode := os.Getenv(darwinHelperModeEnvironment)
	if mode == "" {
		return
	}
	marker := os.Getenv("XAGENT_DARWIN_START_MARKER")
	_ = os.WriteFile(marker, []byte("unprotected"), 0o600)
	if mode == "marker" {
		return
	}
	if mode == "descendant" {
		if err := os.WriteFile(os.Getenv("XAGENT_DARWIN_DESCENDANT_TARGET"), []byte("escape"), 0o600); err == nil {
			os.Exit(31)
		}
		return
	}
	project := os.Getenv("XAGENT_DARWIN_PROJECT")
	permissionRoot := os.Getenv("XAGENT_DARWIN_PERMISSION")
	scratch := os.Getenv("XAGENT_DARWIN_SCRATCH")
	if err := os.WriteFile(filepath.Join(scratch, "allowed"), []byte("ok"), 0o600); err != nil {
		os.Exit(32)
	}
	if err := os.WriteFile(filepath.Join(project, "direct"), []byte("escape"), 0o600); err == nil {
		os.Exit(33)
	}
	variableTarget := filepath.Join(project, "variable")
	if err := os.WriteFile(variableTarget, []byte("escape"), 0o600); err == nil {
		os.Exit(34)
	}
	concatenatedTarget := project + string(os.PathSeparator) + "concatenated"
	if err := os.WriteFile(concatenatedTarget, []byte("escape"), 0o600); err == nil {
		os.Exit(35)
	}
	interpreter := exec.Command("/bin/sh", "-c", `printf escape > "$XAGENT_DARWIN_PROJECT/interpreter"`)
	interpreter.Env = os.Environ()
	if err := interpreter.Run(); err == nil {
		os.Exit(36)
	}
	descendant := exec.Command(os.Args[0], "-test.run=^TestDarwinProtectedExecHelper$", "-test.count=1")
	descendantEnvironment := replaceEnvironment(os.Environ(), darwinHelperModeEnvironment, "descendant")
	descendantTarget := filepath.Join(permissionRoot, "descendant")
	descendantEnvironment = replaceEnvironment(descendantEnvironment, "XAGENT_DARWIN_DESCENDANT_TARGET", descendantTarget)
	descendant.Env = descendantEnvironment
	_ = descendant.Run()
	if _, err := os.Stat(descendantTarget); !os.IsNotExist(err) {
		os.Exit(37)
	}
	if _, err := fmt.Fprintln(os.Stdout, "protected-exec-ok"); err != nil {
		os.Exit(38)
	}
}

func bootstrapRootAtPath(t *testing.T) (string, *safefs.Root) {
	t.Helper()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "anchor"), nil, 0o600); err != nil {
		t.Fatal("create Darwin root anchor failed")
	}
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap Darwin test Root failed")
	}
	return rootPath, opened.Root
}
