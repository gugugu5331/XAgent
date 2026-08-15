//go:build !darwin && !linux && !windows

package tool

import "errors"

func selectShell(string) (shellSelection, error) {
	return shellSelection{}, errors.New("shell execution is unsupported")
}
