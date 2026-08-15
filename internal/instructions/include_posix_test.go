//go:build darwin || linux

package instructions

import "testing"

func TestIncludeLoadRejectsSymlinkRace(t *testing.T) {
	runIncludeLinkRace(t)
}
