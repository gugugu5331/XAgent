//go:build darwin || linux

package proctree

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"xagent/internal/safefs"
)

const posixHelperModeEnvironment = "XAGENT_POSIX_HELPER_MODE"

func TestPOSIXRunnerKillsDescendants(t *testing.T) {
	project := bootstrapTestRoot(t)
	defer project.Close()
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project}, t.TempDir())
	if err != nil {
		t.Fatal("create POSIX test protection plan failed")
	}
	defer plan.cleanupScratch()
	marker := filepath.Join(t.TempDir(), "grandchild-marker")
	runner, err := newPOSIXRunner(
		Options{CleanupTimeout: time.Second, Diagnostics: &recordingSink{}},
		func(request Request) (*exec.Cmd, error) {
			command := exec.Command(request.Executable, request.Args...)
			command.Env = append([]string(nil), request.Env...)
			return command, nil
		},
	)
	if err != nil {
		t.Fatal("create POSIX runner failed")
	}
	process, err := runner.Start(context.Background(), Request{
		Executable: os.Args[0],
		Args:       []string{"-test.run=^TestPOSIXRunnerHelper$", "-test.count=1"},
		WorkingDir: project,
		Env: append(os.Environ(),
			posixHelperModeEnvironment+"=parent",
			"XAGENT_POSIX_HELPER_MARKER="+marker,
		),
		Mode:       ProtectionRequired,
		Protection: plan,
	})
	if err != nil {
		t.Fatal("start POSIX process failed")
	}
	closed := false
	defer func() {
		if !closed {
			_ = process.Close(context.Background())
		}
	}()

	scanner := bufio.NewScanner(process.Pipes().Stdout)
	pids := make([]int, 0, 2)
	for len(pids) < 2 && scanner.Scan() {
		pid, parseErr := strconv.Atoi(scanner.Text())
		if parseErr != nil || pid <= 0 {
			t.Fatal("helper reported an invalid descendant PID")
		}
		pids = append(pids, pid)
	}
	if len(pids) != 2 {
		t.Fatal("helper did not report child and grandchild PIDs")
	}
	waitForMarker(t, marker)
	if err := process.Close(context.Background()); err != nil {
		t.Fatal("close POSIX process tree failed")
	}
	closed = true
	for _, pid := range pids {
		waitForProcessExit(t, pid)
	}
	before, err := os.Stat(marker)
	if err != nil {
		t.Fatal("stat descendant marker failed")
	}
	time.Sleep(100 * time.Millisecond)
	after, err := os.Stat(marker)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("descendant continued producing side effects after Close")
	}
	if plan.valid() {
		t.Fatal("terminal process cleanup retained its scratch Root")
	}
}

func TestPOSIXRunnerHelper(t *testing.T) {
	mode := os.Getenv(posixHelperModeEnvironment)
	if mode == "" {
		return
	}
	switch mode {
	case "parent":
		runPOSIXHelperChild(t, "child")
	case "child":
		signal.Ignore(syscall.SIGTERM)
		runPOSIXHelperChild(t, "grandchild")
	case "grandchild":
		signal.Ignore(syscall.SIGTERM)
		marker := os.Getenv("XAGENT_POSIX_HELPER_MARKER")
		for {
			file, err := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal("open helper marker failed")
			}
			if _, err := file.WriteString("x"); err != nil {
				_ = file.Close()
				t.Fatal("write helper marker failed")
			}
			if err := file.Close(); err != nil {
				t.Fatal("close helper marker failed")
			}
			time.Sleep(10 * time.Millisecond)
		}
	default:
		t.Fatal("unknown POSIX helper mode")
	}
}

func runPOSIXHelperChild(t *testing.T, mode string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestPOSIXRunnerHelper$", "-test.count=1")
	command.Env = replaceEnvironment(os.Environ(), posixHelperModeEnvironment, mode)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal("start POSIX helper descendant failed")
	}
	if _, err := fmt.Fprintln(os.Stdout, command.Process.Pid); err != nil {
		t.Fatal("report POSIX helper PID failed")
	}
	if err := command.Wait(); err != nil {
		t.Fatal("wait POSIX helper descendant failed")
	}
}

func replaceEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	replaced := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if len(item) >= len(prefix) && item[:len(prefix)] == prefix {
			continue
		}
		replaced = append(replaced, item)
	}
	return append(replaced, prefix+value)
}

func waitForMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(marker); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("grandchild did not produce its marker")
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("descendant remained after process tree close")
}
