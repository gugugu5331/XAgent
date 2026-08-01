//go:build !darwin && !linux && !windows

package safefs

import (
	"errors"
	"os"
)

func openPlatformRoot(string) (rootBackend, error) {
	return nil, errors.New("safefs is unsupported on this platform")
}

func platformDirectoryEntryInfo(*os.File, string) (directoryEntryInfo, error) {
	return directoryEntryInfo{}, errors.New("safefs is unsupported on this platform")
}
