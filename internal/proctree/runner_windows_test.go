//go:build windows

package proctree

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"xagent/internal/safefs"
)

const windowsHelperModeEnvironment = "XAGENT_WINDOWS_HELPER_MODE"

func TestWindowsRunnerKillsDescendants(t *testing.T) {
	testWindowsRunnerRejectsUnprotectedTarget(t)

	project := bootstrapTestRoot(t)
	defer project.Close()
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project}, t.TempDir())
	if err != nil {
		t.Fatal("create Windows lifecycle protection plan failed")
	}
	defer plan.cleanupScratch()
	startMarker := filepath.Join(t.TempDir(), "target-started")
	activityMarker := filepath.Join(t.TempDir(), "descendant-activity")
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("resolve Windows helper executable failed")
	}
	runner, err := newWindowsRunnerWithProtection(
		Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}},
		createWindowsSuspendedProcess,
		func(Request, windows.Handle) error {
			if _, statErr := os.Stat(startMarker); !os.IsNotExist(statErr) {
				return errors.New("Windows target ran before protection approval")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal("create Windows lifecycle runner failed")
	}
	environment := replaceWindowsEnvironment(os.Environ(), windowsHelperModeEnvironment, "parent")
	environment = replaceWindowsEnvironment(environment, "XAGENT_WINDOWS_START_MARKER", startMarker)
	environment = replaceWindowsEnvironment(environment, "XAGENT_WINDOWS_ACTIVITY_MARKER", activityMarker)
	process, err := runner.Start(context.Background(), Request{
		Executable: executable,
		Args:       []string{"-test.run=^TestWindowsRunnerHelper$", "-test.count=1"},
		WorkingDir: project,
		Env:        environment,
		Mode:       ProtectionRequired,
		Protection: plan,
	})
	if err != nil {
		t.Fatal("start Windows lifecycle process failed")
	}
	closed := false
	defer func() {
		if !closed {
			_ = process.Close(context.Background())
		}
	}()
	waitHandles := readWindowsDescendantHandles(t, process)
	defer func() {
		for _, handle := range waitHandles {
			_ = windows.CloseHandle(handle)
		}
	}()
	waitForWindowsMarker(t, startMarker)
	waitForWindowsMarker(t, activityMarker)
	if err := process.Close(context.Background()); err != nil {
		t.Fatal("close Windows process tree failed")
	}
	closed = true
	for _, handle := range waitHandles {
		status, waitErr := windows.WaitForSingleObject(handle, 2_000)
		if waitErr != nil || status != windows.WAIT_OBJECT_0 {
			t.Fatal("Windows descendant remained after Job Object close")
		}
	}
	before, err := os.Stat(activityMarker)
	if err != nil {
		t.Fatal("stat Windows descendant marker failed")
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.Stat(activityMarker)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("Windows descendant continued producing side effects after Close")
	}
	if plan.valid() {
		t.Fatal("Windows terminal cleanup retained scratch")
	}
}

func testWindowsRunnerRejectsUnprotectedTarget(t *testing.T) {
	t.Helper()
	project := bootstrapTestRoot(t)
	defer project.Close()
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project}, t.TempDir())
	if err != nil {
		t.Fatal("create Windows rejected protection plan failed")
	}
	defer plan.cleanupScratch()
	marker := filepath.Join(t.TempDir(), "unprotected-target")
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("resolve Windows rejected helper failed")
	}
	runner, err := newWindowsRunner(Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}})
	if err != nil {
		t.Fatal("create default Windows runner failed")
	}
	environment := replaceWindowsEnvironment(os.Environ(), windowsHelperModeEnvironment, "marker")
	environment = replaceWindowsEnvironment(environment, "XAGENT_WINDOWS_START_MARKER", marker)
	_, err = runner.Start(context.Background(), Request{
		Executable: executable,
		Args:       []string{"-test.run=^TestWindowsRunnerHelper$", "-test.count=1"},
		WorkingDir: project,
		Env:        environment,
		Mode:       ProtectionRequired,
		Protection: plan,
	})
	var startErr *StartError
	if !errors.As(err, &startErr) || startErr.Code != startCodeProtectedExecUnavailable || startErr.TargetStarted {
		t.Fatal("default Windows runner did not reject before resume")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) || plan.valid() {
		t.Fatal("default Windows runner started target or retained scratch")
	}
}

func TestWindowsRunnerHelper(t *testing.T) {
	mode := os.Getenv(windowsHelperModeEnvironment)
	if mode == "" {
		return
	}
	if mode == "marker" {
		if err := os.WriteFile(os.Getenv("XAGENT_WINDOWS_START_MARKER"), []byte("started"), 0o600); err != nil {
			t.Fatal("write Windows start marker failed")
		}
		return
	}
	if mode == "grandchild" {
		marker := os.Getenv("XAGENT_WINDOWS_ACTIVITY_MARKER")
		for {
			file, err := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				os.Exit(31)
			}
			_, _ = file.WriteString("x")
			_ = file.Close()
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := os.WriteFile(os.Getenv("XAGENT_WINDOWS_START_MARKER"), []byte("started"), 0o600); err != nil {
		t.Fatal("write Windows helper start marker failed")
	}
	next := "child"
	if mode == "child" {
		next = "grandchild"
	}
	command := exec.Command(os.Args[0], "-test.run=^TestWindowsRunnerHelper$", "-test.count=1")
	command.Env = replaceWindowsEnvironment(os.Environ(), windowsHelperModeEnvironment, next)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal("start Windows helper descendant failed")
	}
	if _, err := fmt.Fprintln(os.Stdout, command.Process.Pid); err != nil {
		t.Fatal("report Windows descendant PID failed")
	}
	if err := command.Wait(); err != nil {
		t.Fatal("wait Windows helper descendant failed")
	}
}

func readWindowsDescendantHandles(t *testing.T, process Process) []windows.Handle {
	t.Helper()
	scanner := bufio.NewScanner(process.Pipes().Stdout)
	handles := make([]windows.Handle, 0, 2)
	for len(handles) < 2 && scanner.Scan() {
		pid, err := strconv.ParseUint(strings.TrimSpace(scanner.Text()), 10, 32)
		if err != nil || pid == 0 {
			continue
		}
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			t.Fatal("open Windows descendant wait handle failed")
		}
		handles = append(handles, handle)
	}
	if len(handles) != 2 {
		t.Fatal("Windows helper did not report child and grandchild PIDs")
	}
	return handles
}

func waitForWindowsMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(marker); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Windows helper did not produce its marker")
}

func replaceWindowsEnvironment(environment []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	replaced := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if strings.HasPrefix(strings.ToUpper(item), prefix) {
			continue
		}
		replaced = append(replaced, item)
	}
	return append(replaced, key+"="+value)
}
