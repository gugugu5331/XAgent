package safefs

import (
	"bytes"
	"errors"
	"sort"
)

// CapabilityClass distinguishes ordinary product writes from the dedicated
// protected configuration writer. ProtectedWrite is not a superset.
type CapabilityClass uint8

const (
	OrdinaryWrite CapabilityClass = iota
	ProtectedWrite
)

type capabilitySeal struct {
	nonce [32]byte
}

// Capability is an opaque bearer value issued only by Bootstrap.
type Capability struct {
	seal  *capabilitySeal
	class CapabilityClass
	plan  *protectionSeal
}

// Capabilities is retained by the assembly root. Leaf modules receive only
// the one Capability appropriate to their role.
type Capabilities struct {
	ordinary  Capability
	protected Capability
}

const maxProtectionRoots = 256

// ProtectionPlanOptions contains already-opened roots. Authorization is based
// on their platform object identities and capability seals, never path
// prefixes. The plan borrows Roots; their owner remains responsible for Close.
type ProtectionPlanOptions struct {
	Writable []*Root
	Readonly []*Root
}

// ProtectionPlan is one immutable task protection snapshot. A writable Root
// is activated for this plan, after which its original Bootstrap capabilities
// are no longer accepted.
type ProtectionPlan struct {
	seal *protectionSeal
}

type protectionSeal struct {
	writable        map[Identity]*Root
	readonly        map[Identity]*Root
	orderedWritable []*Root
	orderedReadonly []*Root
}

type protectionRootSnapshot struct {
	root     *Root
	identity Identity
	writable bool
}

func NewProtectionPlan(options ProtectionPlanOptions) (*ProtectionPlan, error) {
	total := len(options.Writable) + len(options.Readonly)
	if total == 0 || total > maxProtectionRoots {
		return nil, errors.New("safefs protection plan roots are invalid")
	}
	snapshots := make([]protectionRootSnapshot, 0, total)
	seen := make(map[Identity]bool, total)
	appendRoot := func(root *Root, writable bool) error {
		if root == nil {
			return errors.New("safefs protection plan root is invalid")
		}
		root.mu.RLock()
		identity := root.identity
		valid := !root.closed && root.backend != nil && identity.valid() && root.capabilitySeal != nil
		root.mu.RUnlock()
		if !valid {
			return errors.New("safefs protection plan root is unavailable")
		}
		if previousWritable, duplicate := seen[identity]; duplicate {
			if previousWritable != writable {
				return errors.New("safefs protection plan root is both writable and readonly")
			}
			return errors.New("safefs protection plan contains an aliased root")
		}
		seen[identity] = writable
		snapshots = append(snapshots, protectionRootSnapshot{root: root, identity: identity, writable: writable})
		return nil
	}
	for _, root := range options.Writable {
		if err := appendRoot(root, true); err != nil {
			return nil, err
		}
	}
	for _, root := range options.Readonly {
		if err := appendRoot(root, false); err != nil {
			return nil, err
		}
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if compared := bytes.Compare(snapshots[i].identity.volume[:], snapshots[j].identity.volume[:]); compared != 0 {
			return compared < 0
		}
		return bytes.Compare(snapshots[i].identity.object[:], snapshots[j].identity.object[:]) < 0
	})
	for _, snapshot := range snapshots {
		snapshot.root.mu.Lock()
	}
	defer func() {
		for index := len(snapshots) - 1; index >= 0; index-- {
			snapshots[index].root.mu.Unlock()
		}
	}()
	for _, snapshot := range snapshots {
		root := snapshot.root
		if root.closed || root.backend == nil || root.identity != snapshot.identity || root.capabilitySeal == nil {
			return nil, errors.New("safefs protection plan root changed")
		}
		if snapshot.writable && root.protection != nil {
			return nil, errors.New("safefs writable root already has a protection plan")
		}
		if !snapshot.writable && root.protection != nil {
			return nil, errors.New("safefs readonly root is active in a writable protection plan")
		}
	}
	seal := &protectionSeal{
		writable:        make(map[Identity]*Root, len(options.Writable)),
		readonly:        make(map[Identity]*Root, len(options.Readonly)),
		orderedWritable: make([]*Root, 0, len(options.Writable)),
		orderedReadonly: make([]*Root, 0, len(options.Readonly)),
	}
	for _, snapshot := range snapshots {
		if snapshot.writable {
			seal.writable[snapshot.identity] = snapshot.root
			seal.orderedWritable = append(seal.orderedWritable, snapshot.root)
			snapshot.root.protection = seal
		} else {
			seal.readonly[snapshot.identity] = snapshot.root
			seal.orderedReadonly = append(seal.orderedReadonly, snapshot.root)
		}
	}
	return &ProtectionPlan{seal: seal}, nil
}

// Capabilities returns task-bound capabilities only for the exact writable
// Root instance registered with the plan.
func (plan *ProtectionPlan) Capabilities(root *Root) (Capabilities, error) {
	if plan == nil || plan.seal == nil || root == nil {
		return Capabilities{}, errors.New("safefs protection plan is unavailable")
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed || root.protection != plan.seal || plan.seal.writable[root.identity] != root || root.capabilitySeal == nil {
		return Capabilities{}, errors.New("safefs root is not writable in this protection plan")
	}
	return Capabilities{
		ordinary:  Capability{seal: root.capabilitySeal, class: OrdinaryWrite, plan: plan.seal},
		protected: Capability{seal: root.capabilitySeal, class: ProtectedWrite, plan: plan.seal},
	}, nil
}

// ProbeReadonlyRoot confirms that an exact, still-open Root belongs to the
// plan's readonly set and has not been activated as writable.
func (plan *ProtectionPlan) ProbeReadonlyRoot(root *Root) error {
	if plan == nil || plan.seal == nil || root == nil {
		return errors.New("safefs readonly probe failed")
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if root.closed || root.backend == nil || root.protection != nil || plan.seal.readonly[root.identity] != root {
		return errors.New("safefs readonly probe failed")
	}
	return nil
}

// ProbeReadonlyPath opens a directory without following a final symlink and
// compares its live platform identity with a still-open readonly Root.
func (plan *ProtectionPlan) ProbeReadonlyPath(rootPath string) error {
	if plan == nil || plan.seal == nil || !validRootPath(rootPath) {
		return errors.New("safefs readonly probe failed")
	}
	backend, err := openPlatformRoot(rootPath)
	if err != nil {
		return errors.New("safefs readonly probe failed")
	}
	identity := identityFromObject(backend.identity())
	closeErr := backend.close()
	root := plan.seal.readonly[identity]
	if closeErr != nil || root == nil {
		return errors.New("safefs readonly probe failed")
	}
	return plan.ProbeReadonlyRoot(root)
}

func (plan *ProtectionPlan) WritableRoots() []*Root {
	if plan == nil || plan.seal == nil {
		return nil
	}
	return append([]*Root(nil), plan.seal.orderedWritable...)
}

func (plan *ProtectionPlan) ReadonlyRoots() []*Root {
	if plan == nil || plan.seal == nil {
		return nil
	}
	return append([]*Root(nil), plan.seal.orderedReadonly...)
}

func (c Capabilities) Ordinary() Capability {
	return c.ordinary
}

func (c Capabilities) Protected() Capability {
	return c.protected
}

func (r *Root) authorizeWrite(capability Capability, binding Binding) error {
	if r == nil {
		return errors.New("safefs write authorization failed")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || capability.seal == nil || capability.seal != r.capabilitySeal {
		return errors.New("safefs write authorization failed")
	}
	if !binding.valid() || binding.seal != r.bindingSeal || binding.root != r.identity {
		return errors.New("safefs write authorization failed")
	}
	live, err := r.backend.bind(binding.relative)
	if err != nil || !live.valid() || live.kind != binding.kind || bindingDigest(r.identity, live) != binding.digest {
		return errors.New("safefs write authorization failed")
	}
	return r.authorizeResolution(capability, binding.relative, live)
}

func (r *Root) authorizeResolution(capability Capability, relative string, live bindingResolution) error {
	if capability.seal == nil || capability.seal != r.capabilitySeal {
		return errors.New("safefs write authorization failed")
	}
	if r.protection != nil {
		if capability.plan != r.protection || capability.plan.writable[r.identity] != r {
			return errors.New("safefs write authorization failed")
		}
	} else if capability.plan != nil {
		return errors.New("safefs write authorization failed")
	}
	class, err := r.policy.classify(r.backend, relative, live)
	if err != nil {
		return errors.New("safefs write authorization failed")
	}
	switch class {
	case ordinarySlot:
		if capability.class != OrdinaryWrite {
			return errors.New("safefs write authorization failed")
		}
	case protectedSlot:
		if capability.class != ProtectedWrite {
			return errors.New("safefs write authorization failed")
		}
	case readonlySlot:
		return errors.New("safefs write authorization failed")
	default:
		return errors.New("safefs write authorization failed")
	}
	return nil
}
