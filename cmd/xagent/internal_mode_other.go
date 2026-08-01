//go:build !linux

package main

func runPlatformInternalMode() (bool, error) {
	return false, nil
}
