//go:build windows

package tool

import (
	"reflect"
	"testing"
)

func TestShellSelectionUsesExactPlatformArgv(t *testing.T) {
	const command = `echo shell-selection-canary`
	selection, err := selectShell(command)
	if err != nil {
		t.Fatal("Windows shell selection was unavailable")
	}
	wantArgs := []string{"/D", "/S", "/C", command}
	if selection.executable != "cmd.exe" || !reflect.DeepEqual(selection.args, wantArgs) {
		t.Fatalf("Windows shell selection = executable %q args %#v", selection.executable, selection.args)
	}
	if selection.identity != "cmd.exe\x00/D\x00/S\x00/C" {
		t.Fatalf("Windows shell identity did not bind the fixed argv prefix: %q", selection.identity)
	}
}
