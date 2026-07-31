//go:build linux

package safefs

func platformCanonicalLeaf(leaf string) string {
	return leaf
}

func platformLeafRelation(first, second string) leafRelation {
	if first == second {
		return leafEquivalent
	}
	return leafDistinct
}
