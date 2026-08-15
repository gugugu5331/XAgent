//go:build windows

package instructions

import "testing"

func TestIncludeLoadRejectsReparseRace(t *testing.T) {
	runIncludeLinkRace(t)
}
