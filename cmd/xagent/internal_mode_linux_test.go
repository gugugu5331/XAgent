//go:build linux

package main

import (
	"os"
	"testing"
)

func TestLauncherAcceptsOnlyInheritedPlan(t *testing.T) {
	const markerName = "XAGENT_INTERNAL_LINUX_LAUNCHER"
	previous, existed := os.LookupEnv(markerName)
	previousArgs := os.Args
	t.Cleanup(func() {
		os.Args = previousArgs
		if existed {
			_ = os.Setenv(markerName, previous)
		} else {
			_ = os.Unsetenv(markerName)
		}
	})
	_ = os.Unsetenv(markerName)
	if handled, err := runInternalMode(); handled || err != nil {
		t.Fatal("ordinary CLI invocation entered internal launcher mode")
	}
	if err := os.Setenv(markerName, "inherited-plan-v1"); err != nil {
		t.Fatal("set internal launcher fixture marker failed")
	}
	os.Args = []string{previousArgs[0], "--ordinary-cli-argument"}
	if handled, err := runInternalMode(); !handled || err == nil {
		t.Fatal("internal marker plus ordinary arguments bypassed inherited-plan validation")
	}
	if _, err := parseConfigPath([]string{"--xagent-internal-launcher"}); err == nil {
		t.Fatal("ordinary CLI exposed a hidden launcher flag")
	}
}
