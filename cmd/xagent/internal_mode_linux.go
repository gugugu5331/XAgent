//go:build linux

package main

import "xagent/internal/proctree"

func runPlatformInternalMode() (bool, error) {
	return proctree.RunLinuxInheritedLauncher()
}
