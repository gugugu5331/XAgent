package instructions

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"

	"xagent/internal/safefs"
)

const (
	fileIdentityEncodingVersion byte = 1
	cacheKeyEncodingVersion     byte = 1
	maxGraphSourceNameBytes          = 256
	maxGraphPathBytes                = 4_096
	maxGraphFiles                    = 1_024
	maxGraphEdges                    = 8_192
)

type Scope string

const (
	ScopeProjectRoot Scope = "project_root"
	ScopeProjectDir  Scope = "project_dir"
	ScopeUserDir     Scope = "user_dir"
)

const (
	PriorityProjectRoot = 1000
	PriorityProjectDir  = 1010
	PriorityUserDir     = 1020
)

// Source is one loaded instruction source after include expansion.
type Source struct {
	Name     string
	Path     string
	Priority int
	Scope    Scope
	Content  string
}

// FileIdentity is the stable filesystem-object identity of one instruction
// file. Paths are deliberately kept out so hardlink/case aliases of the same
// object retain include duplicate/cycle semantics.
type FileIdentity struct {
	digest [sha256.Size]byte
}

// FileIdentityFromBinding derives an instruction object identity from safefs.
func FileIdentityFromBinding(binding safefs.Binding) (FileIdentity, error) {
	encoded, err := binding.MarshalBinary()
	if err != nil {
		return FileIdentity{}, errors.New("instruction file identity is invalid")
	}
	frame := make([]byte, 0, len(encoded)+32)
	frame = append(frame, "xagent-instruction-file-v1"...)
	frame = append(frame, 0, fileIdentityEncodingVersion)
	frame = appendLengthPrefixed(frame, encoded)
	return FileIdentity{digest: sha256.Sum256(frame)}, nil
}

func (i FileIdentity) valid() bool {
	return i.digest != ([sha256.Size]byte{})
}

// MarshalBinary returns a versioned canonical copy of this opaque identity.
func (i FileIdentity) MarshalBinary() ([]byte, error) {
	if !i.valid() {
		return nil, errors.New("instruction file identity is invalid")
	}
	encoded := make([]byte, 1+len(i.digest))
	encoded[0] = fileIdentityEncodingVersion
	copy(encoded[1:], i.digest[:])
	return encoded, nil
}

// FileVersion binds stable file identity to the exact bytes observed while
// building an instruction graph.
type FileVersion struct {
	Identity      FileIdentity
	PathDigest    [sha256.Size]byte
	ContentDigest [sha256.Size]byte
}

// NewFileVersion records a content version without retaining instruction
// bytes in cache-key material.
func NewFileVersion(absolutePath string, identity FileIdentity, content []byte) (FileVersion, error) {
	absolutePath = strings.TrimSpace(absolutePath)
	if absolutePath == "" || len(absolutePath) > maxGraphPathBytes || !filepath.IsAbs(absolutePath) || filepath.Clean(absolutePath) != absolutePath {
		return FileVersion{}, errors.New("instruction file version path is invalid")
	}
	if !identity.valid() {
		return FileVersion{}, errors.New("instruction file version has an invalid identity")
	}
	return FileVersion{Identity: identity, PathDigest: sha256.Sum256([]byte(absolutePath)), ContentDigest: sha256.Sum256(content)}, nil
}

// IncludeEdge is one ordered parent-to-child edge in the expanded graph.
// Position is the zero-based include occurrence within the parent file.
type IncludeEdge struct {
	From     FileIdentity
	To       FileIdentity
	Position uint64
}

// GraphSource identifies the fixed source whose include graph was expanded.
type GraphSource struct {
	Name     string
	Scope    Scope
	Priority int
	RootPath string
	Root     FileIdentity
}

// CacheKey is an opaque, comparable digest of one complete instruction graph.
type CacheKey struct {
	digest [sha256.Size]byte
}

// NewCacheKey produces a canonical key. File and edge order is significant:
// callers pass deterministic traversal order so include output order remains
// part of the cached result's identity.
func NewCacheKey(source GraphSource, files []FileVersion, edges []IncludeEdge) (CacheKey, error) {
	name := strings.TrimSpace(source.Name)
	if name == "" || len(name) > maxGraphSourceNameBytes || !validScope(source.Scope) || !validScopePriority(source.Scope, source.Priority) || !source.Root.valid() {
		return CacheKey{}, errors.New("instruction cache source is invalid")
	}
	if source.RootPath == "" || len(source.RootPath) > maxGraphPathBytes || !filepath.IsAbs(source.RootPath) || filepath.Clean(source.RootPath) != source.RootPath {
		return CacheKey{}, errors.New("instruction cache source path is invalid")
	}
	if len(files) == 0 || len(files) > maxGraphFiles {
		return CacheKey{}, errors.New("instruction cache graph has no files")
	}
	if len(edges) > maxGraphEdges {
		return CacheKey{}, errors.New("instruction cache graph has too many include edges")
	}

	known := make(map[FileIdentity]struct{}, len(files))
	rootFound := false
	frame := make([]byte, 0, 256+len(files)*64+len(edges)*72)
	frame = append(frame, "xagent-instruction-cache-key-v1"...)
	frame = append(frame, 0, cacheKeyEncodingVersion)
	frame = appendLengthPrefixed(frame, []byte(name))
	frame = appendLengthPrefixed(frame, []byte(source.Scope))
	frame = appendUint64(frame, uint64(int64(source.Priority)))
	frame = appendLengthPrefixed(frame, []byte(source.RootPath))
	frame = append(frame, source.Root.digest[:]...)
	frame = appendUint64(frame, uint64(len(files)))
	for _, file := range files {
		if !file.Identity.valid() || file.PathDigest == ([sha256.Size]byte{}) {
			return CacheKey{}, errors.New("instruction cache graph has an invalid file identity")
		}
		if _, duplicate := known[file.Identity]; duplicate {
			return CacheKey{}, errors.New("instruction cache graph has a duplicate file identity")
		}
		known[file.Identity] = struct{}{}
		rootFound = rootFound || file.Identity == source.Root
		frame = append(frame, file.Identity.digest[:]...)
		frame = append(frame, file.PathDigest[:]...)
		frame = append(frame, file.ContentDigest[:]...)
	}
	if !rootFound {
		return CacheKey{}, errors.New("instruction cache graph is missing its root file")
	}

	frame = appendUint64(frame, uint64(len(edges)))
	for _, edge := range edges {
		if !edge.From.valid() || !edge.To.valid() {
			return CacheKey{}, errors.New("instruction cache graph has an invalid include edge")
		}
		if _, ok := known[edge.From]; !ok {
			return CacheKey{}, errors.New("instruction cache edge parent is not in the graph")
		}
		if _, ok := known[edge.To]; !ok {
			return CacheKey{}, errors.New("instruction cache edge child is not in the graph")
		}
		frame = append(frame, edge.From.digest[:]...)
		frame = append(frame, edge.To.digest[:]...)
		frame = appendUint64(frame, edge.Position)
	}
	return CacheKey{digest: sha256.Sum256(frame)}, nil
}

func (k CacheKey) valid() bool {
	return k.digest != ([sha256.Size]byte{})
}

// MarshalBinary returns a versioned canonical copy of this opaque key.
func (k CacheKey) MarshalBinary() ([]byte, error) {
	if !k.valid() {
		return nil, errors.New("instruction cache key is invalid")
	}
	encoded := make([]byte, 1+len(k.digest))
	encoded[0] = cacheKeyEncodingVersion
	copy(encoded[1:], k.digest[:])
	return encoded, nil
}

func validScope(scope Scope) bool {
	switch scope {
	case ScopeProjectRoot, ScopeProjectDir, ScopeUserDir:
		return true
	default:
		return false
	}
}

func validScopePriority(scope Scope, priority int) bool {
	switch scope {
	case ScopeProjectRoot:
		return priority >= PriorityProjectRoot && priority < PriorityProjectDir
	case ScopeProjectDir:
		return priority == PriorityProjectDir
	case ScopeUserDir:
		return priority == PriorityUserDir
	default:
		return false
	}
}

func appendLengthPrefixed(dst []byte, value []byte) []byte {
	dst = appendUint64(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendUint64(dst []byte, value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return append(dst, encoded[:]...)
}
