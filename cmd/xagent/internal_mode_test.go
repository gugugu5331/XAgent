package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

const (
	launcherEnvironment      = "XAGENT_INTERNAL_LINUX_LAUNCHER"
	launcherMarker           = "inherited-plan-v1"
	launcherPlanEnvironment  = "XAGENT_INTERNAL_PLAN"
	launcherSourceHelper     = "XAGENT_TEST_INTERNAL_LAUNCHER_SOURCE"
	launcherSourcePlanCanary = "versioned-plan-from-public-source-canary"
)

func TestInternalLauncherCannotBeEnteredFromPublicCLI(t *testing.T) {
	t.Run("arguments are rejected without runtime dispatch or suggestions", func(t *testing.T) {
		for _, args := range [][]string{
			{"--xagent-internal-launcher"},
			{"--internal-launcher", launcherSourcePlanCanary},
			{"internal-launcher", launcherSourcePlanCanary},
			{"--completion", "internal-launcher"},
		} {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			runtimeCalls := 0
			code := runCLI(args, &stdout, &stderr, func([]string) error {
				runtimeCalls++
				return nil
			})
			if code != exitUsage || runtimeCalls != 0 || stdout.Len() != 0 {
				t.Fatalf("args=%v code=%d runtime calls=%d stdout=%q stderr=%q", args, code, runtimeCalls, stdout.String(), stderr.String())
			}
			assertNoInternalLauncherDisclosure(t, stderr.String())
		}
	})

	t.Run("environment marker plus ordinary arguments fails closed safely", func(t *testing.T) {
		previousArgs := os.Args
		previousMarker, markerExisted := os.LookupEnv(launcherEnvironment)
		t.Cleanup(func() {
			os.Args = previousArgs
			if markerExisted {
				_ = os.Setenv(launcherEnvironment, previousMarker)
			} else {
				_ = os.Unsetenv(launcherEnvironment)
			}
		})
		if err := os.Setenv(launcherEnvironment, launcherMarker); err != nil {
			t.Fatal("set forged launcher marker failed")
		}
		os.Args = []string{previousArgs[0], "--config", launcherSourcePlanCanary}
		handled, err := runInternalMode()
		if runtime.GOOS == "linux" {
			if !handled || err == nil || err.Error() != protectedLaunchFailure {
				t.Fatalf("Linux forged marker result handled=%t err=%v", handled, err)
			}
			assertNoInternalLauncherDisclosure(t, err.Error())
			return
		}
		if handled || err != nil {
			t.Fatalf("unsupported platform selected internal mode handled=%t err=%v", handled, err)
		}
	})

	t.Run("environment and stdin cannot replace inherited handles", func(t *testing.T) {
		command := exec.Command(os.Args[0], "-test.run=^TestInternalLauncherSourceHelper$", "-test.count=1")
		command.Env = replaceTestEnvironment(os.Environ(), launcherSourceHelper, "1")
		command.Env = replaceTestEnvironment(command.Env, launcherEnvironment, launcherMarker)
		command.Env = replaceTestEnvironment(command.Env, launcherPlanEnvironment, launcherSourcePlanCanary)
		command.Stdin = strings.NewReader(launcherSourcePlanCanary)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated launcher source check failed: %v output=%q", err, output)
		}
		if strings.Contains(string(output), launcherSourcePlanCanary) {
			t.Fatalf("isolated launcher source check disclosed plan canary: %q", output)
		}
	})

	assertNoInternalLauncherDisclosure(t, publicUsage)
}

func TestInternalLauncherSourceHelper(t *testing.T) {
	if os.Getenv(launcherSourceHelper) != "1" {
		return
	}
	if os.Getenv(launcherPlanEnvironment) != launcherSourcePlanCanary {
		t.Fatal("isolated launcher plan environment is missing")
	}
	os.Args = []string{os.Args[0]}
	if runtime.GOOS == "linux" {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := runCLI(nil, &stdout, &stderr, runArgs)
		if code != exitFailure || stdout.Len() != 0 || stderr.String() != "运行错误: "+protectedLaunchFailure+"\n" {
			t.Fatalf("Linux inherited-handle rejection code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		assertNoInternalLauncherDisclosure(t, stderr.String())
	} else if handled, err := runInternalMode(); handled || err != nil {
		t.Fatalf("unsupported platform selected internal mode handled=%t err=%v", handled, err)
	}
	remaining, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal("read isolated stdin fixture failed")
	}
	if string(remaining) != launcherSourcePlanCanary {
		t.Fatalf("internal mode consumed stdin: %q", remaining)
	}
}

func replaceTestEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	replaced := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			replaced = append(replaced, item)
		}
	}
	return append(replaced, prefix+value)
}

func assertNoInternalLauncherDisclosure(t *testing.T, output string) {
	t.Helper()
	lower := strings.ToLower(output)
	for _, forbidden := range []string{"internal", "launcher", "inherited", "handshake", "plan handle", "fd3", "fd4", "xagent_internal"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("public CLI disclosed hidden launcher token %q: %q", forbidden, output)
		}
	}
}
