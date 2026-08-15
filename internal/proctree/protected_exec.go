package proctree

import (
	"context"
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

type protectionPlanFactory struct {
	roots         []*safefs.Root
	scratchParent string
}

func NewProtectionPlanFactory(roots []*safefs.Root, scratchParent string) (ProtectionPlanFactory, error) {
	if scratchParent == "" || !filepath.IsAbs(scratchParent) || filepath.Clean(scratchParent) != scratchParent {
		return nil, errors.New("proctree scratch parent is invalid")
	}
	copyRoots, err := copyValidProtectionRoots(roots)
	if err != nil {
		return nil, err
	}
	return &protectionPlanFactory{roots: copyRoots, scratchParent: scratchParent}, nil
}

func (f *protectionPlanFactory) Create(ctx context.Context) (ProtectionPlan, error) {
	if f == nil || ctx == nil {
		return ProtectionPlan{}, errors.New("proctree protection plan factory is unavailable")
	}
	select {
	case <-ctx.Done():
		return ProtectionPlan{}, ctx.Err()
	default:
	}
	plan, err := newProtectionPlanWithScratch(f.roots, f.scratchParent)
	if err != nil {
		return ProtectionPlan{}, err
	}
	select {
	case <-ctx.Done():
		_ = plan.cleanupScratch()
		return ProtectionPlan{}, ctx.Err()
	default:
		return plan, nil
	}
}

func copyValidProtectionRoots(roots []*safefs.Root) ([]*safefs.Root, error) {
	if len(roots) == 0 {
		return nil, errors.New("proctree protection roots are invalid")
	}
	copyRoots := make([]*safefs.Root, len(roots))
	seen := make(map[safefs.Identity]struct{}, len(roots))
	for index, root := range roots {
		if root == nil {
			return nil, errors.New("proctree protection roots are invalid")
		}
		identity := root.Identity()
		if identity == (safefs.Identity{}) {
			return nil, errors.New("proctree protection roots are invalid")
		}
		if _, duplicate := seen[identity]; duplicate {
			return nil, errors.New("proctree protection roots contain duplicates")
		}
		seen[identity] = struct{}{}
		copyRoots[index] = root
	}
	return copyRoots, nil
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

func (p ProtectionPlan) containsReadRoot(root *safefs.Root) bool {
	if !p.valid() || root == nil {
		return false
	}
	identity := root.Identity()
	for _, allowed := range p.roots {
		if allowed.Identity() == identity {
			return true
		}
	}
	return false
}

func (p ProtectionPlan) validForStart() bool {
	return p.valid() && p.scratchOwner != nil && p.scratchOwner.root == p.scratch
}

func (p ProtectionPlan) cleanupScratch() error {
	if p.scratchOwner == nil || p.scratchOwner.root != p.scratch {
		return errors.New("proctree scratch ownership is unavailable")
	}
	return p.scratchOwner.cleanup()
}

func cleanupUnstartedPlan(request Request) {
	if request.Protection.validForStart() {
		_ = request.Protection.cleanupScratch()
	}
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
