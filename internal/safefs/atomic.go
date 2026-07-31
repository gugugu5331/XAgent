package safefs

import (
	"context"
	"errors"
	"io"
	"io/fs"
)

type atomicWriteBackend interface {
	atomicWrite(
		ctx context.Context,
		relative string,
		perm fs.FileMode,
		write func(io.Writer) error,
		validate func(bindingResolution) error,
	) error
}

// AtomicWrite publishes one complete version beneath the Root. The target
// identity and capability class are checked both before staging and directly
// before the platform replace operation.
func (r *Root) AtomicWrite(
	ctx context.Context,
	capability Capability,
	relative string,
	perm fs.FileMode,
	write func(io.Writer) error,
) error {
	if ctx == nil || write == nil || perm != perm.Perm() {
		return errors.New("safefs atomic write request is invalid")
	}
	select {
	case <-ctx.Done():
		return errors.New("safefs atomic write canceled")
	default:
	}
	canonical, err := canonicalRelative(relative)
	if err != nil {
		return err
	}
	if r == nil {
		return errors.New("safefs root is unavailable")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || r.backend == nil {
		return errors.New("safefs root is closed")
	}
	backend, ok := r.backend.(atomicWriteBackend)
	if !ok {
		return errors.New("safefs atomic write is unsupported")
	}
	initial, err := r.backend.bind(canonical)
	if err != nil || !initial.valid() {
		return errors.New("safefs atomic target identity is unavailable")
	}
	if err := r.authorizeResolution(capability, canonical, initial); err != nil {
		return errors.New("safefs atomic write authorization failed")
	}
	validate := func(live bindingResolution) error {
		if !live.valid() || !sameAtomicTarget(initial, live) {
			return errors.New("safefs atomic target changed")
		}
		return r.authorizeResolution(capability, canonical, live)
	}
	if err := backend.atomicWrite(ctx, canonical, perm, write, validate); err != nil {
		return errors.New("safefs atomic write failed")
	}
	return nil
}

func sameAtomicTarget(initial, live bindingResolution) bool {
	if initial.kind != live.kind || initial.parent != live.parent || initial.leaf != live.leaf {
		return false
	}
	return initial.kind == missingBinding || initial.object == live.object
}
