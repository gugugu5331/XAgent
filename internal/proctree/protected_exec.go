package proctree

import (
	"errors"

	"xagent/internal/safefs"
)

func NewProtectionPlan(roots []*safefs.Root, scratch *safefs.Root) (ProtectionPlan, error) {
	if len(roots) == 0 || scratch == nil || scratch.Identity() == (safefs.Identity{}) {
		return ProtectionPlan{}, errors.New("proctree protection plan is invalid")
	}
	copyRoots := make([]*safefs.Root, len(roots))
	scratchIdentity := scratch.Identity()
	seen := make(map[safefs.Identity]struct{}, len(roots))
	for index, root := range roots {
		if root == nil {
			return ProtectionPlan{}, errors.New("proctree protection plan is invalid")
		}
		identity := root.Identity()
		if identity == (safefs.Identity{}) || identity == scratchIdentity {
			return ProtectionPlan{}, errors.New("proctree protection plan is invalid")
		}
		if _, duplicate := seen[identity]; duplicate {
			return ProtectionPlan{}, errors.New("proctree protection plan contains duplicate roots")
		}
		seen[identity] = struct{}{}
		copyRoots[index] = root
	}
	return ProtectionPlan{seal: &protectionSeal{}, roots: copyRoots, scratch: scratch}, nil
}

func (p ProtectionPlan) valid() bool {
	if p.seal == nil || len(p.roots) == 0 || p.scratch == nil || p.scratch.Identity() == (safefs.Identity{}) {
		return false
	}
	scratchIdentity := p.scratch.Identity()
	seen := make(map[safefs.Identity]struct{}, len(p.roots))
	for _, root := range p.roots {
		if root == nil {
			return false
		}
		identity := root.Identity()
		if identity == (safefs.Identity{}) || identity == scratchIdentity {
			return false
		}
		if _, duplicate := seen[identity]; duplicate {
			return false
		}
		seen[identity] = struct{}{}
	}
	return true
}
