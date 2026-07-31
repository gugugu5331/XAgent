//go:build !darwin && !linux && !windows

package safefs

import "errors"

func openPlatformRoot(string) (rootBackend, error) {
	return nil, errors.New("safefs is unsupported on this platform")
}
