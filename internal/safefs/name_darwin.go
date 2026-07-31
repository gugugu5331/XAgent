//go:build darwin

package safefs

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

func platformCanonicalLeaf(leaf string) string {
	return leaf
}

func platformLeafRelation(first, second string) leafRelation {
	if first == second {
		return leafEquivalent
	}
	if strings.EqualFold(norm.NFD.String(first), norm.NFD.String(second)) {
		// A Darwin root may reside on either a case-sensitive or a
		// case-insensitive volume. Existing aliases are identified by their
		// opened object. A still-missing ambiguous spelling is denied to both
		// capability classes rather than guessed.
		return leafAmbiguous
	}
	return leafDistinct
}
