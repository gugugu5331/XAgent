//go:build windows

package tool

func selectShell(command string) (shellSelection, error) {
	return makeShellSelection("cmd.exe", []string{"/D", "/S", "/C"}, command), nil
}
