package safefs

import (
	"errors"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

var errInvalidRelativePath = errors.New("safefs relative path is invalid")

// Policy declares root-relative slots that only the dedicated protected
// capability may write. Bootstrap compiles and copies this input.
type Policy struct {
	ProtectedSlots []string
}

type slotClass uint8

const (
	ordinarySlot slotClass = iota + 1
	protectedSlot
	reservedAncestorSlot
)

type compiledPolicy struct {
	protected []string
}

func compilePolicy(policy Policy) (compiledPolicy, error) {
	protected := make([]string, len(policy.ProtectedSlots))
	seen := make(map[string]struct{}, len(policy.ProtectedSlots))
	for index, candidate := range policy.ProtectedSlots {
		canonical, err := canonicalRelative(candidate)
		if err != nil {
			return compiledPolicy{}, errors.New("safefs policy contains an invalid protected slot")
		}
		if _, duplicate := seen[canonical]; duplicate {
			return compiledPolicy{}, errors.New("safefs policy contains a duplicate protected slot")
		}
		seen[canonical] = struct{}{}
		protected[index] = canonical
	}
	sort.Strings(protected)
	return compiledPolicy{protected: protected}, nil
}

func (p compiledPolicy) validate(backend rootBackend) error {
	resolved := make([]bindingResolution, 0, len(p.protected))
	for _, protected := range p.protected {
		resolution, err := backend.bind(protected)
		if err != nil || !resolution.valid() {
			return errors.New("safefs protected slot identity is unavailable")
		}
		for _, ancestor := range pathAncestors(protected) {
			ancestorResolution, ancestorErr := backend.bind(ancestor)
			if ancestorErr != nil || ancestorResolution.kind != existingBinding {
				return errors.New("safefs protected slot ancestor is unavailable")
			}
		}
		for _, previous := range resolved {
			if protectedSlotsOverlap(backend, previous, resolution) {
				return errors.New("safefs policy contains an aliased protected slot")
			}
		}
		resolved = append(resolved, resolution)
	}
	return nil
}

func (p compiledPolicy) classify(backend rootBackend, relative string, candidate bindingResolution) (slotClass, error) {
	for _, protected := range p.protected {
		resolution, err := backend.bind(protected)
		if err != nil || !resolution.valid() {
			return reservedAncestorSlot, errors.New("safefs protected slot identity is unavailable")
		}
		if relative == protected || sameExistingObject(candidate, resolution) {
			return protectedSlot, nil
		}
		if candidate.parent == resolution.parent {
			switch backend.leafRelation(candidate.leaf, resolution.leaf) {
			case leafEquivalent:
				return protectedSlot, nil
			case leafAmbiguous:
				return reservedAncestorSlot, nil
			}
		}
		for _, ancestor := range pathAncestors(protected) {
			ancestorResolution, ancestorErr := backend.bind(ancestor)
			if ancestorErr != nil || ancestorResolution.kind != existingBinding {
				return reservedAncestorSlot, errors.New("safefs protected slot ancestor is unavailable")
			}
			if relative == ancestor || sameExistingObject(candidate, ancestorResolution) {
				return reservedAncestorSlot, nil
			}
		}
	}
	return ordinarySlot, nil
}

func sameExistingObject(first, second bindingResolution) bool {
	return first.kind == existingBinding && second.kind == existingBinding && first.object == second.object
}

func protectedSlotsOverlap(backend rootBackend, first, second bindingResolution) bool {
	if sameExistingObject(first, second) {
		return true
	}
	if first.parent != second.parent {
		return false
	}
	return backend.leafRelation(first.leaf, second.leaf) != leafDistinct
}

func pathAncestors(relative string) []string {
	components := strings.Split(relative, "/")
	if len(components) < 2 {
		return nil
	}
	ancestors := make([]string, 0, len(components)-1)
	for index := 1; index < len(components); index++ {
		ancestors = append(ancestors, strings.Join(components[:index], "/"))
	}
	return ancestors
}

func canonicalRelative(value string) (string, error) {
	if value == "" || value == "." || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return "", errInvalidRelativePath
	}
	if strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") {
		return "", errInvalidRelativePath
	}
	if len(value) >= 2 && value[1] == ':' && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) {
		return "", errInvalidRelativePath
	}
	canonical := path.Clean(value)
	if canonical != value || path.IsAbs(canonical) {
		return "", errInvalidRelativePath
	}
	for _, component := range strings.Split(canonical, "/") {
		if component == "" || component == "." || component == ".." {
			return "", errInvalidRelativePath
		}
	}
	return canonical, nil
}
