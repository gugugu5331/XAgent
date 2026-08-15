//go:build !darwin && !linux && !windows

package hook

import "errors"

func selectCommandShell(string) (string, []string, error) {
	return "", nil, errors.New("command shell is unavailable")
}
