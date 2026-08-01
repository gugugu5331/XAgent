//go:build linux

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

	"golang.org/x/sys/unix"

	"xagent/internal/safefs"
)

const (
	linuxProtectedHelperMode = "XAGENT_TEST_LINUX_PROTECTED_HELPER"
	linuxRejectProtection    = "XAGENT_TEST_LINUX_REJECT_PROTECTION"
)

func TestMain(m *testing.M) {
	if os.Getenv(linuxLauncherEnvironment) == linuxLauncherMarker && os.Getenv(linuxRejectProtection) == "1" {
		_ = os.Unsetenv(linuxLauncherEnvironment)
		planFile := os.NewFile(linuxPlanFD, "test-inherited-plan")
		handshake := os.NewFile(linuxHandshakeFD, "test-exec-handshake")
		err := runLinuxInheritedLauncher(planFile, handshake, func(linuxLaunchPlan) error {
			return errLinuxProtectionUnavailable
		})
		if err != nil {
			os.Exit(41)
		}
		os.Exit(42)
	}
	handled, err := RunLinuxInheritedLauncher()
	if handled {
		if err != nil {
			os.Exit(41)
		}
		os.Exit(42)
	}
	os.Exit(m.Run())
}

func TestLinuxProtectedExec(t *testing.T) {
	t.Run("scratch identity metadata validation", testLinuxScratchIdentityMetadata)
	t.Run("capability failure does not start target", testLinuxCapabilityFailure)
	t.Run("scratch-only write policy", testLinuxScratchOnlyWrites)
}

func testLinuxScratchIdentityMetadata(t *testing.T) {
	t.Helper()
	project := bootstrapTestRoot(t)
	defer project.Close()
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project}, t.TempDir())
	if err != nil {
		t.Fatal("create Linux scratch validation plan failed")
	}
	defer plan.cleanupScratch()
	identity, err := plan.scratch.Identity().MarshalBinary()
	if err != nil {
		t.Fatal("marshal Linux scratch identity failed")
	}
	fd, err := unix.Open(plan.scratchOwner.path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal("open Linux scratch validation handle failed")
	}
	defer unix.Close(fd)
	if err := verifyLinuxScratchHandle(fd, identity); err != nil {
		t.Fatal("safe Linux scratch handle was rejected")
	}
	changedIdentity := append([]byte(nil), identity...)
	changedIdentity[len(changedIdentity)-1] ^= 1
	if err := verifyLinuxScratchHandle(fd, changedIdentity); err == nil {
		t.Fatal("changed Linux scratch identity was accepted")
	}
	if err := os.Chmod(plan.scratchOwner.path, 0o750); err != nil {
		t.Fatal("change Linux scratch permissions failed")
	}
	if err := verifyLinuxScratchHandle(fd, identity); err == nil {
		t.Fatal("non-private Linux scratch permissions were accepted")
	}
}

func testLinuxCapabilityFailure(t *testing.T) {
	t.Helper()
	projectPath, project := bootstrapLinuxRootAtPath(t)
	defer project.Close()
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project}, t.TempDir())
	if err != nil {
		t.Fatal("create Linux rejected protection plan failed")
	}
	defer plan.cleanupScratch()
	marker := filepath.Join(projectPath, "capability-failure-marker")
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("resolve Linux rejected helper executable failed")
	}
	t.Setenv(linuxRejectProtection, "1")
	runner, err := newLinuxRunner(Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}})
	if err != nil {
		t.Fatal("create Linux rejected runner failed")
	}
	environment := replaceEnvironment(os.Environ(), linuxProtectedHelperMode, "marker")
	environment = replaceEnvironment(environment, "XAGENT_TEST_LINUX_START_MARKER", marker)
	_, err = runner.Start(context.Background(), Request{
		Executable: executable,
		Args:       []string{"-test.run=^TestLinuxProtectedExecHelper$", "-test.count=1"},
		WorkingDir: project,
		Env:        environment,
		Mode:       ProtectionRequired,
		Protection: plan,
	})
	var startErr *StartError
	if !errors.As(err, &startErr) || startErr.Code != startCodeProtectedExecUnavailable || startErr.TargetStarted {
		t.Fatal("Linux capability failure did not report target-not-started")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("Linux capability failure started the target")
	}
	if plan.valid() {
		t.Fatal("Linux capability failure retained scratch")
	}
}

func testLinuxScratchOnlyWrites(t *testing.T) {
	t.Helper()
	projectPath, project := bootstrapLinuxRootAtPath(t)
	defer project.Close()
	permissionPath, permissionRoot := bootstrapLinuxRootAtPath(t)
	defer permissionRoot.Close()
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project, permissionRoot}, t.TempDir())
	if err != nil {
		t.Fatal("create Linux protection plan failed")
	}
	defer plan.cleanupScratch()
	startMarker := filepath.Join(t.TempDir(), "target-started-unprotected")
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("resolve Linux protected helper executable failed")
	}
	runner, err := newLinuxRunner(Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}})
	if err != nil {
		t.Fatal("create Linux runner failed")
	}
	environment := replaceEnvironment(os.Environ(), linuxProtectedHelperMode, "main")
	environment = replaceEnvironment(environment, "XAGENT_TEST_LINUX_PROJECT", projectPath)
	environment = replaceEnvironment(environment, "XAGENT_TEST_LINUX_PERMISSION", permissionPath)
	environment = replaceEnvironment(environment, "XAGENT_TEST_LINUX_SCRATCH", plan.scratchOwner.path)
	environment = replaceEnvironment(environment, "XAGENT_TEST_LINUX_START_MARKER", startMarker)
	process, err := runner.Start(context.Background(), Request{
		Executable: executable,
		Args:       []string{"-test.run=^TestLinuxProtectedExecHelper$", "-test.count=1"},
		WorkingDir: project,
		Env:        environment,
		Mode:       ProtectionRequired,
		Protection: plan,
	})
	if err != nil {
		var startErr *StartError
		if !errors.As(err, &startErr) || startErr.Code != startCodeProtectedExecUnavailable || startErr.TargetStarted {
			t.Fatal("Linux capability failure did not fail closed")
		}
		if _, statErr := os.Stat(startMarker); !os.IsNotExist(statErr) || plan.valid() {
			t.Fatal("Linux capability failure started target or retained scratch")
		}
		t.Log("Linux Landlock ABI 5 capability unavailable; fail-closed path verified")
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
		t.Fatal("read Linux protected helper output failed")
	}
	waitContext, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWait()
	result, waitErr := process.Wait(waitContext)
	if waitErr != nil || result.ExitCode != 0 || !strings.Contains(string(output), "protected-exec-ok") {
		t.Fatalf("Linux protected helper failed: exit=%d output=%q", result.ExitCode, output)
	}
	if err := process.Close(context.Background()); err != nil {
		t.Fatal("close Linux protected process failed")
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
			t.Fatal("Linux protected target wrote outside scratch")
		}
	}
	if plan.valid() {
		t.Fatal("Linux terminal cleanup retained scratch")
	}
	t.Log("Linux Landlock scratch-only write policy and bypass matrix verified")
}

func TestLinuxProtectedExecHelper(t *testing.T) {
	mode := os.Getenv(linuxProtectedHelperMode)
	if mode == "" {
		return
	}
	marker := os.Getenv("XAGENT_TEST_LINUX_START_MARKER")
	_ = os.WriteFile(marker, []byte("unprotected"), 0o600)
	if mode == "marker" {
		return
	}
	if mode == "descendant" {
		if err := os.WriteFile(os.Getenv("XAGENT_TEST_LINUX_DESCENDANT_TARGET"), []byte("escape"), 0o600); err == nil {
			os.Exit(31)
		}
		return
	}
	project := os.Getenv("XAGENT_TEST_LINUX_PROJECT")
	permissionRoot := os.Getenv("XAGENT_TEST_LINUX_PERMISSION")
	scratch := os.Getenv("XAGENT_TEST_LINUX_SCRATCH")
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
	interpreter := exec.Command("/bin/sh", "-c", `printf escape > "$XAGENT_TEST_LINUX_PROJECT/interpreter"`)
	interpreter.Env = os.Environ()
	if err := interpreter.Run(); err == nil {
		os.Exit(36)
	}
	descendant := exec.Command(os.Args[0], "-test.run=^TestLinuxProtectedExecHelper$", "-test.count=1")
	descendantEnvironment := replaceEnvironment(os.Environ(), linuxProtectedHelperMode, "descendant")
	descendantTarget := filepath.Join(permissionRoot, "descendant")
	descendantEnvironment = replaceEnvironment(descendantEnvironment, "XAGENT_TEST_LINUX_DESCENDANT_TARGET", descendantTarget)
	descendant.Env = descendantEnvironment
	_ = descendant.Run()
	if _, err := os.Stat(descendantTarget); !os.IsNotExist(err) {
		os.Exit(37)
	}
	if _, err := fmt.Fprintln(os.Stdout, "protected-exec-ok"); err != nil {
		os.Exit(38)
	}
}

func bootstrapLinuxRootAtPath(t *testing.T) (string, *safefs.Root) {
	t.Helper()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "anchor"), nil, 0o600); err != nil {
		t.Fatal("create Linux root anchor failed")
	}
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap Linux test Root failed")
	}
	return rootPath, opened.Root
}
