//go:build windows

package hook

func selectCommandShell(command string) (string, []string, error) {
	return "cmd.exe", []string{"/d", "/s", "/c", command}, nil
}
