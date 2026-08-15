//go:build darwin || linux

package tool

import (
	"os"
	"reflect"
	"testing"

	"xagent/internal/permission"
	"xagent/internal/safefs"
)

func TestShellSelectionUsesExactPlatformArgv(t *testing.T) {
	const command = `printf '%s' 'shell-selection-canary'`
	selection, err := selectShell(command)
	if err != nil {
		t.Fatal("POSIX shell selection was unavailable")
	}
	if selection.executable != "/bin/sh" || !reflect.DeepEqual(selection.args, []string{"-c", command}) {
		t.Fatalf("POSIX shell selection = executable %q args %#v", selection.executable, selection.args)
	}
	if selection.identity != "/bin/sh\x00-c" {
		t.Fatalf("POSIX shell identity did not bind the fixed argv prefix: %q", selection.identity)
	}

	rootPath := t.TempDir()
	opened, err := safefs.Bootstrap(rootPath, safefs.Policy{})
	if err != nil {
		t.Fatal("bootstrap Bash working directory failed")
	}
	t.Cleanup(func() {
		if err := opened.Root.Close(); err != nil {
			t.Error("close Bash working directory failed")
		}
	})
	registry, err := NewRegistry(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutorWithWriteAccess(registry, rootPath, 0, 0, opened.Root, opened.Capabilities.Ordinary())
	call := Call{ID: "shell-identity", Name: "Bash", ArgumentsJSON: `{"command":"printf '%s' 'shell-selection-canary'"}`}
	validated, err := executor.PrepareCall(nil, call)
	if err != nil {
		t.Fatal("prepare Bash call failed")
	}
	got, err := executor.CallIdentity(validated)
	if err != nil {
		t.Fatal("build Bash identity failed")
	}
	want, err := permission.NewBashIdentity(permission.BashIdentityInput{
		Shell:             selection.identity,
		WorkingDirectory:  opened.Root.Identity(),
		RawCommand:        []byte(command),
		EnvironmentDigest: executionEnvironmentDigest(os.Environ()),
	})
	if err != nil {
		t.Fatal("build expected Bash identity failed")
	}
	if got != want {
		t.Fatal("Bash identity did not use the selected POSIX shell semantics")
	}
	legacy, err := permission.NewBashIdentity(permission.BashIdentityInput{
		Shell:             selection.executable,
		WorkingDirectory:  opened.Root.Identity(),
		RawCommand:        []byte(command),
		EnvironmentDigest: executionEnvironmentDigest(os.Environ()),
	})
	if err != nil {
		t.Fatal("build legacy Bash identity failed")
	}
	if got == legacy {
		t.Fatal("Bash identity omitted the fixed POSIX argv prefix")
	}
}
