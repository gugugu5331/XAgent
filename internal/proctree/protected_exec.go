package proctree

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"xagent/internal/safefs"
)

const scratchDirectoryPrefix = "xagent-run-"

var errProtectionPlanSerialization = errors.New("proctree protection plan cannot be serialized")

type scratchOwner struct {
	root *safefs.Root
	path string

	once sync.Once
	err  error
}

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

// newProtectionPlanWithScratch creates the scratch directory and its opened
// Root as one ownership unit. Platform runners consume the plan while the
// terminal lifecycle path calls cleanupScratch.
func newProtectionPlanWithScratch(roots []*safefs.Root, scratchParent string) (ProtectionPlan, error) {
	if scratchParent == "" || !filepath.IsAbs(scratchParent) || filepath.Clean(scratchParent) != scratchParent {
		return ProtectionPlan{}, errors.New("proctree scratch parent is invalid")
	}
	scratchPath, err := os.MkdirTemp(scratchParent, scratchDirectoryPrefix)
	if err != nil {
		return ProtectionPlan{}, errors.New("proctree scratch creation failed")
	}
	removeScratch := true
	defer func() {
		if removeScratch {
			_ = os.RemoveAll(scratchPath)
		}
	}()
	if err := os.Chmod(scratchPath, 0o700); err != nil {
		return ProtectionPlan{}, errors.New("proctree scratch privacy setup failed")
	}
	opened, err := safefs.Bootstrap(scratchPath, safefs.Policy{})
	if err != nil {
		return ProtectionPlan{}, errors.New("proctree scratch bootstrap failed")
	}
	plan, err := NewProtectionPlan(roots, opened.Root)
	if err != nil {
		_ = opened.Root.Close()
		return ProtectionPlan{}, err
	}
	plan.scratchOwner = &scratchOwner{root: opened.Root, path: scratchPath}
	removeScratch = false
	return plan, nil
}

func (p ProtectionPlan) allowsWrite(root *safefs.Root) bool {
	if !p.valid() || root == nil {
		return false
	}
	return root.Identity() == p.scratch.Identity()
}

func (p ProtectionPlan) cleanupScratch() error {
	if p.scratchOwner == nil || p.scratchOwner.root != p.scratch {
		return errors.New("proctree scratch ownership is unavailable")
	}
	return p.scratchOwner.cleanup()
}

func (p ProtectionPlan) MarshalJSON() ([]byte, error) {
	return nil, errProtectionPlanSerialization
}

func (o *scratchOwner) cleanup() error {
	if o == nil {
		return errors.New("proctree scratch ownership is unavailable")
	}
	o.once.Do(func() {
		closeErr := o.root.Close()
		removeErr := os.RemoveAll(o.path)
		if closeErr != nil || removeErr != nil {
			o.err = errors.New("proctree scratch cleanup failed")
		}
	})
	return o.err
}
