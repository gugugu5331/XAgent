//go:build linux

package proctree

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xagent/internal/safefs"
)

const linuxTestLauncherMode = "XAGENT_TEST_LINUX_LAUNCHER_MODE"

func TestLauncherAcceptsOnlyInheritedPlan(t *testing.T) {
	project := bootstrapTestRoot(t)
	defer project.Close()
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project}, t.TempDir())
	if err != nil {
		t.Fatal("create Linux protocol protection plan failed")
	}
	defer plan.cleanupScratch()
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("resolve Linux test executable failed")
	}
	request := Request{
		Executable: executable,
		Args:       []string{"-test.run=^TestLinuxTargetMarkerHelper$", "-test.count=1"},
		WorkingDir: project,
		Env:        []string{"XAGENT_TEST_LINUX_TARGET_MARKER=/tmp/unused"},
		Mode:       ProtectionRequired,
		Protection: plan,
	}
	derived, err := newLinuxLaunchPlan(request)
	if err != nil {
		t.Fatal("derive Linux inherited plan failed")
	}
	prepared, err := prepareLinuxCommand(request)
	if err != nil {
		t.Fatal("prepare Linux self-reexec command failed")
	}
	if prepared.command.Path != linuxSelfExecutable || len(prepared.command.Args) != 1 || len(prepared.command.ExtraFiles) != 2 {
		prepared.closeAll()
		t.Fatal("Linux self-reexec did not use the current kernel executable with inherited handles only")
	}
	prepared.closeAll()
	encoded, err := encodeLinuxLaunchPlan(derived)
	if err != nil {
		t.Fatal("encode Linux inherited plan failed")
	}
	decoded, err := decodeLinuxLaunchPlan(encoded)
	if err != nil || decoded.executable != derived.executable || decoded.scratchPath != derived.scratchPath ||
		len(decoded.args) != len(derived.args) || len(decoded.environment) != len(derived.environment) {
		t.Fatal("versioned Linux inherited plan did not round trip")
	}
	tampered := append([]byte(nil), encoded...)
	tampered[len(linuxPlanMagic)] = 0
	tampered[len(linuxPlanMagic)+1] = 0
	if _, err := decodeLinuxLaunchPlan(tampered); err == nil {
		t.Fatal("tampered Linux plan version was accepted")
	}
	request.Env = append(request.Env, linuxLauncherEnvironment+"=forged")
	if _, err := newLinuxLaunchPlan(request); err == nil {
		t.Fatal("ordinary target environment selected internal launcher mode")
	}
	if encoded, err := json.Marshal(plan); err == nil || len(encoded) != 0 {
		t.Fatal("ProtectionPlan became serializable through Linux launcher support")
	}

	unsealedPath := filepath.Join(t.TempDir(), "ordinary-plan")
	if err := os.WriteFile(unsealedPath, encoded, 0o600); err != nil {
		t.Fatal("create unsealed plan fixture failed")
	}
	unsealed, err := os.Open(unsealedPath)
	if err != nil {
		t.Fatal("open unsealed plan fixture failed")
	}
	defer unsealed.Close()
	if _, err := readSealedLinuxPlan(unsealed); err == nil {
		t.Fatal("ordinary file was accepted as an inherited sealed plan")
	}
}

func TestExecHandshakeReportsTargetNotStarted(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		mode        string
		wantStarted bool
	}{
		{name: "protection failure", mode: "reject", wantStarted: false},
		{name: "successful exec", mode: "allow", wantStarted: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "target-marker")
			command, handshake, closeChildEnds := linuxHandshakeFixtureCommand(t, testCase.mode, marker)
			defer handshake.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := command.Start(); err != nil {
				closeChildEnds()
				t.Fatal("start Linux launcher fixture failed")
			}
			closeChildEnds()
			handshakeErr := awaitLinuxExecHandshake(ctx, handshake)
			waitErr := command.Wait()
			if testCase.wantStarted {
				if handshakeErr != nil || waitErr != nil {
					t.Fatal("successful target exec did not close the handshake")
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatal("successful exec target did not run")
				}
				return
			}
			var startErr *StartError
			if !errors.As(handshakeErr, &startErr) || startErr.TargetStarted || startErr.Code != startCodeProtectedExecUnavailable {
				t.Fatal("pre-exec protection failure was not reported as target-not-started")
			}
			if waitErr == nil {
				t.Fatal("rejected launcher fixture unexpectedly exited successfully")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("target marker appeared after protection failure")
			}
		})
	}
}

func linuxHandshakeFixtureCommand(t *testing.T, mode, marker string) (*exec.Cmd, *os.File, func()) {
	t.Helper()
	project := bootstrapTestRoot(t)
	t.Cleanup(func() { _ = project.Close() })
	plan, err := newProtectionPlanWithScratch([]*safefs.Root{project}, t.TempDir())
	if err != nil {
		t.Fatal("create Linux handshake plan failed")
	}
	t.Cleanup(func() { _ = plan.cleanupScratch() })
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal("resolve Linux handshake target failed")
	}
	targetEnvironment := replaceLinuxEnvironment(os.Environ(), "XAGENT_TEST_LINUX_TARGET_MARKER", marker)
	request := Request{
		Executable: executable,
		Args:       []string{"-test.run=^TestLinuxTargetMarkerHelper$", "-test.count=1"},
		WorkingDir: project,
		Env:        targetEnvironment,
		Mode:       ProtectionRequired,
		Protection: plan,
	}
	derived, err := newLinuxLaunchPlan(request)
	if err != nil {
		t.Fatal("derive Linux handshake plan failed")
	}
	encoded, err := encodeLinuxLaunchPlan(derived)
	if err != nil {
		t.Fatal("encode Linux handshake plan failed")
	}
	planFile, err := createSealedLinuxPlan(encoded)
	if err != nil {
		t.Fatal("create Linux handshake plan handle failed")
	}
	handshakeRead, handshakeWrite, err := os.Pipe()
	if err != nil {
		_ = planFile.Close()
		t.Fatal("create Linux handshake fixture pipe failed")
	}
	launcherEnvironment := replaceLinuxEnvironment(os.Environ(), linuxTestLauncherMode, mode)
	command := exec.Command(os.Args[0], "-test.run=^TestLinuxLauncherHandshakeHelper$", "-test.count=1")
	command.Env = launcherEnvironment
	command.ExtraFiles = []*os.File{planFile, handshakeWrite}
	var once sync.Once
	closeChildEnds := func() {
		once.Do(func() {
			_ = planFile.Close()
			_ = handshakeWrite.Close()
		})
	}
	return command, handshakeRead, closeChildEnds
}

func TestLinuxLauncherHandshakeHelper(t *testing.T) {
	mode := os.Getenv(linuxTestLauncherMode)
	if mode == "" {
		return
	}
	planFile := os.NewFile(linuxPlanFD, "test-inherited-plan")
	handshake := os.NewFile(linuxHandshakeFD, "test-exec-handshake")
	installer := func(linuxLaunchPlan) error { return nil }
	if mode == "reject" {
		installer = func(linuxLaunchPlan) error { return errLinuxProtectionUnavailable }
	}
	if err := runLinuxInheritedLauncher(planFile, handshake, installer); err != nil {
		os.Exit(41)
	}
	os.Exit(42)
}

func TestLinuxTargetMarkerHelper(t *testing.T) {
	marker := os.Getenv("XAGENT_TEST_LINUX_TARGET_MARKER")
	if marker == "" {
		return
	}
	if err := os.WriteFile(marker, []byte("started"), 0o600); err != nil {
		t.Fatal("write Linux target marker failed")
	}
}
