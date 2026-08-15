//go:build darwin || linux

package tool

func selectShell(command string) (shellSelection, error) {
	return makeShellSelection("/bin/sh", []string{"-c"}, command), nil
}
