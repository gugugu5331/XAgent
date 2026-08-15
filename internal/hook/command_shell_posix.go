//go:build darwin || linux

package hook

func selectCommandShell(command string) (string, []string, error) {
	return "/bin/sh", []string{"-c", command}, nil
}
