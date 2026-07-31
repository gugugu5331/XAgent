package safefs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"path/filepath"
	"sync"
	"unicode/utf8"
)

const (
	identityEncodingVersion byte = 1
	bindingEncodingVersion  byte = 1
)

// objectIdentity is the stable platform identity of one opened filesystem
// object. It intentionally contains no authorization or policy state.
type objectIdentity struct {
	volume [16]byte
	object [16]byte
}

func (i objectIdentity) valid() bool {
	return i.volume != ([16]byte{}) || i.object != ([16]byte{})
}

func objectIdentityFromNumbers(volume, object uint64) objectIdentity {
	var identity objectIdentity
	binary.BigEndian.PutUint64(identity.volume[8:], volume)
	binary.BigEndian.PutUint64(identity.object[8:], object)
	return identity
}

// Identity is an opaque, comparable identity for one opened Root.
type Identity struct {
	volume [16]byte
	object [16]byte
}

func identityFromObject(object objectIdentity) Identity {
	return Identity{volume: object.volume, object: object.object}
}

func (i Identity) valid() bool {
	return i.volume != ([16]byte{}) || i.object != ([16]byte{})
}

// MarshalBinary returns a versioned copy suitable for a canonical caller
// identity. There is intentionally no inverse constructor.
func (i Identity) MarshalBinary() ([]byte, error) {
	if !i.valid() {
		return nil, errors.New("safefs identity is invalid")
	}
	encoded := make([]byte, 1+len(i.volume)+len(i.object))
	encoded[0] = identityEncodingVersion
	copy(encoded[1:], i.volume[:])
	copy(encoded[1+len(i.volume):], i.object[:])
	return encoded, nil
}

type bindingKind uint8

const (
	existingBinding bindingKind = iota + 1
	missingBinding
)

type bindingSeal struct {
	nonce [32]byte
}

// Binding is a versioned, opaque resource identity created by Root.Bind.
// Its canonical bytes contain filesystem identity only; the private relative
// path is retained solely so authorization can revalidate live state.
type Binding struct {
	root     Identity
	digest   [32]byte
	seal     *bindingSeal
	relative string
	version  byte
	kind     bindingKind
}

func (b Binding) valid() bool {
	return b.root.valid() && b.seal != nil && b.relative != "" &&
		b.version == bindingEncodingVersion &&
		(b.kind == existingBinding || b.kind == missingBinding)
}

// MarshalBinary returns a canonical copy without exposing the private seal,
// relative path, or any authorization classification.
func (b Binding) MarshalBinary() ([]byte, error) {
	if !b.valid() {
		return nil, errors.New("safefs binding is invalid")
	}
	encoded := make([]byte, 2+len(b.root.volume)+len(b.root.object)+len(b.digest))
	encoded[0] = b.version
	encoded[1] = byte(b.kind)
	offset := 2
	copy(encoded[offset:], b.root.volume[:])
	offset += len(b.root.volume)
	copy(encoded[offset:], b.root.object[:])
	offset += len(b.root.object)
	copy(encoded[offset:], b.digest[:])
	return encoded, nil
}

type bindingResolution struct {
	kind   bindingKind
	object objectIdentity
	parent objectIdentity
	leaf   string
}

func (r bindingResolution) valid() bool {
	switch r.kind {
	case existingBinding:
		return r.object.valid() && r.parent.valid() && r.leaf != ""
	case missingBinding:
		return r.parent.valid() && r.leaf != ""
	default:
		return false
	}
}

type leafRelation uint8

const (
	leafDistinct leafRelation = iota
	leafEquivalent
	leafAmbiguous
)

type rootBackend interface {
	identity() objectIdentity
	bind(relative string) (bindingResolution, error)
	openRead(relative string) (platformOpenedFile, error)
	leafRelation(first, second string) leafRelation
	close() error
}

// OpenResult is the only Bootstrap result. The Capabilities container is
// retained by the assembly root rather than by Root itself.
type OpenResult struct {
	Root         *Root
	Capabilities Capabilities
}

// Root owns an already-open platform directory handle and the validation
// seals for Bindings and Capabilities. It has no capability issuance method.
type Root struct {
	mu sync.RWMutex

	backend        rootBackend
	identity       Identity
	policy         compiledPolicy
	capabilitySeal *capabilitySeal
	bindingSeal    *bindingSeal
	closed         bool
	closeErr       error
}

// Bootstrap opens one allowed root, snapshots its policy, and issues the two
// non-overlapping write capabilities to the assembly owner.
func Bootstrap(rootPath string, policy Policy) (OpenResult, error) {
	compiled, err := compilePolicy(policy)
	if err != nil {
		return OpenResult{}, err
	}
	if rootPath == "" || !utf8.ValidString(rootPath) || !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return OpenResult{}, errors.New("safefs bootstrap root is invalid")
	}
	backend, err := openPlatformRoot(rootPath)
	if err != nil {
		return OpenResult{}, errors.New("safefs bootstrap failed")
	}
	if err := compiled.validate(backend); err != nil {
		_ = backend.close()
		return OpenResult{}, err
	}
	identity := identityFromObject(backend.identity())
	if !identity.valid() {
		_ = backend.close()
		return OpenResult{}, errors.New("safefs bootstrap failed")
	}

	var entropy [64]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		_ = backend.close()
		return OpenResult{}, errors.New("safefs bootstrap failed")
	}
	capabilitySeal := &capabilitySeal{}
	copy(capabilitySeal.nonce[:], entropy[:32])
	bindingSeal := &bindingSeal{}
	copy(bindingSeal.nonce[:], entropy[32:])
	root := &Root{
		backend:        backend,
		identity:       identity,
		policy:         compiled,
		capabilitySeal: capabilitySeal,
		bindingSeal:    bindingSeal,
	}
	return OpenResult{
		Root: root,
		Capabilities: Capabilities{
			ordinary:  Capability{seal: capabilitySeal, class: OrdinaryWrite},
			protected: Capability{seal: capabilitySeal, class: ProtectedWrite},
		},
	}, nil
}

func (r *Root) Identity() Identity {
	if r == nil {
		return Identity{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.identity
}

// Bind resolves an existing target identity or binds a missing leaf to its
// current parent identity and platform-canonical name.
func (r *Root) Bind(relative string) (Binding, error) {
	canonical, err := canonicalRelative(relative)
	if err != nil {
		return Binding{}, err
	}
	if r == nil {
		return Binding{}, errors.New("safefs root is unavailable")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || r.backend == nil {
		return Binding{}, errors.New("safefs root is closed")
	}
	resolution, err := r.backend.bind(canonical)
	if err != nil || !resolution.valid() {
		return Binding{}, errors.New("safefs target identity is unavailable")
	}
	return r.binding(canonical, resolution), nil
}

// OpenRead opens a regular file through the same no-follow platform handle
// chain used for identity validation.
func (r *Root) OpenRead(ctx context.Context, relative string) (*File, error) {
	if ctx == nil {
		return nil, errors.New("safefs read context is invalid")
	}
	select {
	case <-ctx.Done():
		return nil, errors.New("safefs read canceled")
	default:
	}
	canonical, err := canonicalRelative(relative)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("safefs root is unavailable")
	}
	r.mu.RLock()
	if r.closed || r.backend == nil {
		r.mu.RUnlock()
		return nil, errors.New("safefs root is closed")
	}
	opened, err := r.backend.openRead(canonical)
	r.mu.RUnlock()
	if err != nil || opened.file == nil || !opened.identity.valid() {
		if opened.file != nil {
			_ = opened.file.Close()
		}
		return nil, errors.New("safefs read open failed")
	}
	select {
	case <-ctx.Done():
		_ = opened.file.Close()
		return nil, errors.New("safefs read canceled")
	default:
	}
	return newFile(opened), nil
}

func (r *Root) binding(relative string, resolution bindingResolution) Binding {
	return Binding{
		root:     r.identity,
		digest:   bindingDigest(r.identity, resolution),
		seal:     r.bindingSeal,
		relative: relative,
		version:  bindingEncodingVersion,
		kind:     resolution.kind,
	}
}

func bindingDigest(root Identity, resolution bindingResolution) [32]byte {
	frame := make([]byte, 0, 128+len(resolution.leaf))
	frame = append(frame, "xagent-safefs-binding-v1"...)
	frame = append(frame, 0, byte(resolution.kind))
	frame = append(frame, root.volume[:]...)
	frame = append(frame, root.object[:]...)
	if resolution.kind == existingBinding {
		frame = append(frame, resolution.object.volume[:]...)
		frame = append(frame, resolution.object.object[:]...)
	} else {
		frame = append(frame, resolution.parent.volume[:]...)
		frame = append(frame, resolution.parent.object[:]...)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(resolution.leaf)))
		frame = append(frame, length[:]...)
		frame = append(frame, resolution.leaf...)
	}
	return sha256.Sum256(frame)
}

// Close is idempotent and returns the same final result to every caller.
func (r *Root) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	if r.backend != nil {
		if err := r.backend.close(); err != nil {
			r.closeErr = errors.New("safefs root close failed")
		}
		r.backend = nil
	}
	return r.closeErr
}
