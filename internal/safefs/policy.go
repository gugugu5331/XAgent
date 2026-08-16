package safefs

import (
	"errors"
	"sort"
	"strings"
)

// Policy declares root-relative protected and readonly slots. Readonly slots
// reject both capability classes. Bootstrap compiles and copies this input.
type Policy struct {
	ProtectedSlots []string
	ReadonlySlots  []string
}

type slotClass uint8

const (
	ordinarySlot slotClass = iota + 1
	protectedSlot
	readonlySlot
	reservedAncestorSlot
)

type compiledPolicy struct {
	protected []string
	readonly  []string
}

func compilePolicy(policy Policy) (compiledPolicy, error) {
	seen := make(map[string]struct{}, len(policy.ProtectedSlots)+len(policy.ReadonlySlots))
	protected, err := compileSlots(policy.ProtectedSlots, seen)
	if err != nil {
		return compiledPolicy{}, err
	}
	readonly, err := compileSlots(policy.ReadonlySlots, seen)
	if err != nil {
		return compiledPolicy{}, err
	}
	all := make([]string, 0, len(protected)+len(readonly))
	all = append(all, protected...)
	all = append(all, readonly...)
	for first := 0; first < len(all); first++ {
		for second := first + 1; second < len(all); second++ {
			if strings.HasPrefix(all[first], all[second]+"/") || strings.HasPrefix(all[second], all[first]+"/") {
				return compiledPolicy{}, errors.New("safefs policy contains ancestor-overlapping slots")
			}
		}
	}
	return compiledPolicy{protected: protected, readonly: readonly}, nil
}

func compileSlots(candidates []string, seen map[string]struct{}) ([]string, error) {
	compiled := make([]string, len(candidates))
	for index, candidate := range candidates {
		canonical, err := canonicalRelative(candidate)
		if err != nil {
			return nil, errors.New("safefs policy contains an invalid slot")
		}
		if _, duplicate := seen[canonical]; duplicate {
			return nil, errors.New("safefs policy contains a duplicate or overlapping slot")
		}
		seen[canonical] = struct{}{}
		compiled[index] = canonical
	}
	sort.Strings(compiled)
	return compiled, nil
}

func (p compiledPolicy) validate(backend rootBackend) error {
	resolved := make([]bindingResolution, 0, len(p.protected)+len(p.readonly))
	all := make([]string, 0, len(p.protected)+len(p.readonly))
	all = append(all, p.protected...)
	all = append(all, p.readonly...)
	for _, slot := range all {
		resolution, err := backend.bind(slot)
		if err != nil || !resolution.valid() {
			return errors.New("safefs policy slot identity is unavailable")
		}
		for _, ancestor := range pathAncestors(slot) {
			ancestorResolution, ancestorErr := backend.bind(ancestor)
			if ancestorErr != nil || ancestorResolution.kind != existingBinding {
				return errors.New("safefs policy slot ancestor is unavailable")
			}
		}
		for _, previous := range resolved {
			if protectedSlotsOverlap(backend, previous, resolution) {
				return errors.New("safefs policy contains aliased slots")
			}
		}
		resolved = append(resolved, resolution)
	}
	return nil
}

func (p compiledPolicy) classify(backend rootBackend, relative string, candidate bindingResolution) (slotClass, error) {
	for _, readonly := range p.readonly {
		resolution, err := backend.bind(readonly)
		if err != nil || !resolution.valid() {
			return reservedAncestorSlot, errors.New("safefs readonly slot identity is unavailable")
		}
		if relative == readonly {
			return readonlySlot, nil
		}
		if candidate.parent == resolution.parent {
			switch backend.leafRelation(candidate.leaf, resolution.leaf) {
			case leafEquivalent:
				return readonlySlot, nil
			case leafAmbiguous:
				return readonlySlot, nil
			}
		}
		if sameExistingObject(candidate, resolution) {
			return readonlySlot, nil
		}
		for _, ancestor := range pathAncestors(readonly) {
			ancestorResolution, ancestorErr := backend.bind(ancestor)
			if ancestorErr != nil || ancestorResolution.kind != existingBinding {
				return reservedAncestorSlot, errors.New("safefs readonly slot ancestor is unavailable")
			}
			if relative == ancestor || sameExistingObject(candidate, ancestorResolution) {
				return reservedAncestorSlot, nil
			}
		}
		for _, ancestor := range pathAncestors(relative) {
			ancestorResolution, ancestorErr := backend.bind(ancestor)
			if ancestorErr != nil || ancestorResolution.kind != existingBinding {
				return reservedAncestorSlot, errors.New("safefs candidate ancestor is unavailable")
			}
			if ancestor == readonly || sameExistingObject(ancestorResolution, resolution) {
				return readonlySlot, nil
			}
		}
	}
	for _, protected := range p.protected {
		resolution, err := backend.bind(protected)
		if err != nil || !resolution.valid() {
			return reservedAncestorSlot, errors.New("safefs protected slot identity is unavailable")
		}
		if relative == protected {
			return protectedSlot, nil
		}
		if candidate.parent == resolution.parent {
			switch backend.leafRelation(candidate.leaf, resolution.leaf) {
			case leafEquivalent:
				return protectedSlot, nil
			case leafAmbiguous:
				if sameExistingObject(candidate, resolution) {
					return protectedSlot, nil
				}
				return reservedAncestorSlot, nil
			}
		}
		if sameExistingObject(candidate, resolution) {
			return reservedAncestorSlot, nil
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
