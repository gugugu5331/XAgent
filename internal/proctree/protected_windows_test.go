//go:build windows

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

	"golang.org/x/sys/windows"

	"xagent/internal/safefs"
)

const windowsProtectedHelperMode = "XAGENT_WINDOWS_PROTECTED_HELPER"

func TestWindowsProtectedExec(t *testing.T) {
	testWindowsProtectionFailureDoesNotStartTarget(t)

	projectPath, project := bootstrapWindowsRootAtPath(t)
	defer project.Close()
	permissionPath, permissionRoot := bootstrapWindowsRootAtPath(t)
	defer permissionRoot.Close()
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project, permissionRoot}, t.TempDir())
	if err != nil {
		t.Fatal("create Windows protected plan failed")
	}
	defer plan.cleanupScratch()
	token, err := newWindowsRestrictedLowToken()
	if err != nil {
		t.Fatalf("Windows restricted low token primitive is unavailable: %v", err)
	}
	if err := setWindowsScratchLowLabel(plan.scratchOwner.path); err != nil {
		_ = token.Close()
		t.Fatalf("Windows scratch low label primitive is unavailable: %v", err)
	}
	if err := verifyWindowsProtectionAccess(plan, token); err != nil {
		_ = token.Close()
		t.Fatalf("Windows Root AccessCheck primitive rejected the protection plan: %v", err)
	}
	if err := token.Close(); err != nil {
		t.Fatal("close Windows protection probe token failed")
	}
	startMarker := filepath.Join(t.TempDir(), "target-started-unprotected")
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("resolve Windows protected helper failed")
	}
	runner, err := newWindowsRunner(Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}})
	if err != nil {
		t.Fatal("create Windows protected runner failed")
	}
	environment := replaceWindowsEnvironment(os.Environ(), windowsProtectedHelperMode, "main")
	environment = replaceWindowsEnvironment(environment, "XAGENT_WINDOWS_PROJECT", projectPath)
	environment = replaceWindowsEnvironment(environment, "XAGENT_WINDOWS_PERMISSION", permissionPath)
	environment = replaceWindowsEnvironment(environment, "XAGENT_WINDOWS_SCRATCH", plan.scratchOwner.path)
	environment = replaceWindowsEnvironment(environment, "XAGENT_WINDOWS_PROTECTED_START", startMarker)
	process, err := runner.Start(context.Background(), Request{
		Executable: executable,
		Args:       []string{"-test.run=^TestWindowsProtectedExecHelper$", "-test.count=1"},
		WorkingDir: project,
		Env:        environment,
		Mode:       ProtectionRequired,
		Protection: plan,
	})
	if err != nil {
		var startErr *StartError
		if !errors.As(err, &startErr) || startErr.Code != startCodeProtectedExecUnavailable || startErr.TargetStarted {
			t.Fatal("Windows protection capability failure did not fail closed")
		}
		if _, statErr := os.Stat(startMarker); !os.IsNotExist(statErr) || plan.valid() {
			t.Fatal("Windows protection failure started target or retained scratch")
		}
		t.Fatal("Windows native runner did not provide required protected execution capability")
	}
	closed := false
	defer func() {
		if !closed {
			_ = process.Close(context.Background())
		}
	}()
	output, readErr := io.ReadAll(process.Pipes().Stdout)
	if readErr != nil {
		t.Fatal("read Windows protected helper output failed")
	}
	waitContext, cancelWait := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelWait()
	result, waitErr := process.Wait(waitContext)
	if waitErr != nil || result.ExitCode != 0 || !strings.Contains(string(output), "protected-exec-ok") {
		t.Fatalf("Windows protected helper failed: exit=%d output=%q", result.ExitCode, output)
	}
	if err := process.Close(context.Background()); err != nil {
		t.Fatal("close Windows protected process failed")
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
			t.Fatal("Windows protected target wrote outside scratch")
		}
	}
	if plan.valid() {
		t.Fatal("Windows protected process retained scratch")
	}
}

func testWindowsProtectionFailureDoesNotStartTarget(t *testing.T) {
	t.Helper()
	projectPath, project := bootstrapWindowsRootAtPath(t)
	defer project.Close()
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project}, t.TempDir())
	if err != nil {
		t.Fatal("create Windows failed protection plan failed")
	}
	defer plan.cleanupScratch()
	marker := filepath.Join(projectPath, "protection-failure-marker")
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("resolve Windows failed protection helper failed")
	}
	runner, err := newWindowsRunnerWithProtection(
		Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}},
		createWindowsSuspendedProcess,
		func(Request, windows.Handle) error { return errors.New("injected Windows protection failure") },
	)
	if err != nil {
		t.Fatal("create Windows failed protection runner failed")
	}
	environment := replaceWindowsEnvironment(os.Environ(), windowsProtectedHelperMode, "marker")
	environment = replaceWindowsEnvironment(environment, "XAGENT_WINDOWS_PROTECTED_START", marker)
	_, err = runner.Start(context.Background(), Request{
		Executable: executable,
		Args:       []string{"-test.run=^TestWindowsProtectedExecHelper$", "-test.count=1"},
		WorkingDir: project,
		Env:        environment,
		Mode:       ProtectionRequired,
		Protection: plan,
	})
	var startErr *StartError
	if !errors.As(err, &startErr) || startErr.Code != startCodeProtectedExecUnavailable || startErr.TargetStarted {
		t.Fatal("Windows injected protection failure did not report target-not-started")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) || plan.valid() {
		t.Fatal("Windows injected protection failure started target or retained scratch")
	}
}

func TestWindowsProtectedExecHelper(t *testing.T) {
	mode := os.Getenv(windowsProtectedHelperMode)
	if mode == "" {
		return
	}
	marker := os.Getenv("XAGENT_WINDOWS_PROTECTED_START")
	_ = os.WriteFile(marker, []byte("unprotected"), 0o600)
	if mode == "marker" {
		return
	}
	if mode == "descendant" {
		if err := os.WriteFile(os.Getenv("XAGENT_WINDOWS_DESCENDANT_TARGET"), []byte("escape"), 0o600); err == nil {
			os.Exit(31)
		}
		return
	}
	project := os.Getenv("XAGENT_WINDOWS_PROJECT")
	permissionRoot := os.Getenv("XAGENT_WINDOWS_PERMISSION")
	scratch := os.Getenv("XAGENT_WINDOWS_SCRATCH")
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
	interpreterTarget := filepath.Join(project, "interpreter")
	interpreter := exec.Command("cmd.exe", "/D", "/S", "/C", `echo escape>"`+interpreterTarget+`"`)
	interpreter.Env = os.Environ()
	if err := interpreter.Run(); err == nil {
		os.Exit(36)
	}
	descendant := exec.Command(os.Args[0], "-test.run=^TestWindowsProtectedExecHelper$", "-test.count=1")
	descendantEnvironment := replaceWindowsEnvironment(os.Environ(), windowsProtectedHelperMode, "descendant")
	descendantTarget := filepath.Join(permissionRoot, "descendant")
	descendantEnvironment = replaceWindowsEnvironment(descendantEnvironment, "XAGENT_WINDOWS_DESCENDANT_TARGET", descendantTarget)
	descendant.Env = descendantEnvironment
	_ = descendant.Run()
	if _, err := os.Stat(descendantTarget); !os.IsNotExist(err) {
		os.Exit(37)
	}
	if _, err := fmt.Fprintln(os.Stdout, "protected-exec-ok"); err != nil {
		os.Exit(38)
	}
}

func bootstrapWindowsRootAtPath(t *testing.T) (string, *safefs.Root) {
	t.Helper()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "anchor"), nil, 0o600); err != nil {
		t.Fatal("create Windows Root anchor failed")
	}
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap Windows test Root failed")
	}
	return rootPath, opened.Root
}
