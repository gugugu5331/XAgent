package safefs

import "errors"

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
}

// Capabilities is retained by the assembly root. Leaf modules receive only
// the one Capability appropriate to their role.
type Capabilities struct {
	ordinary  Capability
	protected Capability
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
	r.mu.Lock()
	defer r.mu.Unlock()
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
	class, err := r.policy.classify(r.backend, binding.relative, live)
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
	default:
		return errors.New("safefs write authorization failed")
	}
	return nil
}
