package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"xagent/internal/budget"
)

const artifactIDBytes = 32

const (
	defaultArtifactRetention = 7 * 24 * time.Hour
	maxArtifactRetention     = 365 * 24 * time.Hour
)

type artifactRecord struct {
	ref      Ref
	metadata Metadata
}

type fileStore struct {
	mu sync.RWMutex

	root          string
	workspaceRoot string
	options       FileStoreOptions
	now           func() time.Time
	totalBytes    int64
	active        map[string]*fileWriter
	records       map[string]artifactRecord
	closed        bool
	closeErr      error
}

func NewFileStore(options FileStoreOptions) (Store, error) {
	root, workspace, err := validateStoreRoots(options.Root, options.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	if options.Retention == 0 {
		options.Retention = defaultArtifactRetention
	}
	if err := validateStoreLimits(options); err != nil {
		return nil, err
	}
	options.Root = ""
	options.WorkspaceRoot = ""
	return &fileStore{
		root:          root,
		workspaceRoot: workspace,
		options:       options,
		now:           time.Now,
		active:        make(map[string]*fileWriter),
		records:       make(map[string]artifactRecord),
	}, nil
}

func validateStoreLimits(options FileStoreOptions) error {
	fileSpec, totalSpec, ok := artifactBudgetSpecs()
	if !ok {
		return errors.New("artifact budget specification is unavailable")
	}
	if _, err := fileSpec.Resolve(&options.MaxFileBytes); err != nil {
		return errors.New("artifact file budget is invalid")
	}
	if _, err := totalSpec.Resolve(&options.MaxTotalBytes); err != nil {
		return errors.New("artifact total budget is invalid")
	}
	if options.MaxFileBytes > options.MaxTotalBytes {
		return errors.New("artifact file budget exceeds total budget")
	}
	if options.Retention < time.Nanosecond || options.Retention > maxArtifactRetention {
		return errors.New("artifact retention is invalid")
	}
	return nil
}

func artifactBudgetSpecs() (budget.Spec, budget.Spec, bool) {
	var fileSpec, totalSpec budget.Spec
	var fileFound, totalFound bool
	for _, spec := range budget.AllSpecs() {
		switch spec.Scope {
		case budget.ArtifactMaxFileBytes:
			fileSpec, fileFound = spec, true
		case budget.ArtifactMaxTotalBytes:
			totalSpec, totalFound = spec, true
		}
	}
	return fileSpec, totalSpec, fileFound && totalFound
}

func validateStoreRoots(root, workspace string) (string, string, error) {
	if !validAbsolutePath(root) || !validAbsolutePath(workspace) {
		return "", "", errors.New("artifact store roots are invalid")
	}
	canonicalWorkspace, err := canonicalFuturePath(workspace)
	if err != nil {
		return "", "", errors.New("artifact workspace root is unavailable")
	}
	canonicalRoot, err := canonicalFuturePath(root)
	if err != nil {
		return "", "", errors.New("artifact store root is unavailable")
	}
	inside, err := pathWithin(canonicalWorkspace, canonicalRoot)
	if err != nil {
		return "", "", errors.New("artifact store root relation is unavailable")
	}
	containsWorkspace, err := pathWithin(canonicalRoot, canonicalWorkspace)
	if err != nil {
		return "", "", errors.New("artifact store root relation is unavailable")
	}
	if inside || containsWorkspace {
		return "", "", errors.New("artifact store root must be outside the workspace")
	}
	return canonicalRoot, canonicalWorkspace, nil
}

func validAbsolutePath(path string) bool {
	return path != "" && utf8.ValidString(path) && filepath.IsAbs(path) && filepath.Clean(path) == path
}

// canonicalFuturePath resolves the deepest existing ancestor before appending
// missing components. Validation therefore does not create a forbidden path
// through an existing symlink into the workspace.
func canonicalFuturePath(path string) (string, error) {
	current := path
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	resolved, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for index := len(missing) - 1; index >= 0; index-- {
		resolved = filepath.Join(resolved, missing[index])
	}
	if !filepath.IsAbs(resolved) {
		return "", errors.New("canonical path is not absolute")
	}
	return filepath.Clean(resolved), nil
}

func pathWithin(parent, candidate string) (bool, error) {
	relative, err := filepath.Rel(parent, candidate)
	if err != nil {
		return false, err
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}

func (s *fileStore) Begin(ctx context.Context, metadata Metadata) (Writer, error) {
	if err := s.available(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("artifact store is closed")
	}
	if err := ensurePrivateArtifactRoot(s.root); err != nil {
		return nil, errors.New("artifact store initialization failed")
	}
	id, err := newArtifactID()
	if err != nil {
		return nil, errors.New("artifact identity creation failed")
	}
	staging := filepath.Join(s.root, "."+id+".staging")
	file, err := createPrivateArtifactFile(staging)
	if err != nil {
		return nil, errors.New("artifact staging creation failed")
	}
	writer := &fileWriter{
		store:       s,
		file:        file,
		id:          id,
		stagingPath: staging,
		finalPath:   filepath.Join(s.root, id+".artifact"),
		metadata:    metadata,
		createdAt:   s.now().UTC(),
		state:       writerActive,
	}
	s.active[id] = writer
	return writer, nil
}

func (s *fileStore) OpenForUser(ctx context.Context, id string) (io.ReadCloser, Ref, error) {
	if err := s.available(ctx); err != nil {
		return nil, Ref{}, err
	}
	if !validArtifactID(id) {
		return nil, Ref{}, errors.New("artifact identity is invalid")
	}
	s.mu.RLock()
	record, ok := s.records[id]
	root := s.root
	s.mu.RUnlock()
	if !ok || !record.ref.Available {
		return nil, Ref{}, errors.New("artifact is unavailable")
	}
	file, err := openPrivateArtifactFile(filepath.Join(root, id+".artifact"))
	if err != nil {
		return nil, Ref{}, errors.New("artifact is unavailable")
	}
	return file, record.ref, nil
}

func (s *fileStore) available(ctx context.Context) error {
	if s == nil || ctx == nil {
		return errors.New("artifact store request is invalid")
	}
	select {
	case <-ctx.Done():
		return errors.New("artifact store request canceled")
	default:
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return errors.New("artifact store is closed")
	}
	return nil
}

func (s *fileStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.workspaceRoot = ""
	return s.closeErr
}

func newArtifactID() (string, error) {
	var raw [artifactIDBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func validArtifactID(id string) bool {
	if len(id) != artifactIDBytes*2 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != artifactIDBytes {
		return false
	}
	return id == strings.ToLower(id)
}

func (s *fileStore) reserve(writer *fileWriter, requested int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("artifact store is closed")
	}
	fileRemaining := s.options.MaxFileBytes - writer.bytes
	totalRemaining := s.options.MaxTotalBytes - s.totalBytes
	allowed := requested
	scope := budget.ArtifactMaxFileBytes
	limit := s.options.MaxFileBytes
	observed := saturatingAdd(writer.bytes, requested)
	if allowed > fileRemaining {
		allowed = fileRemaining
	}
	if allowed > totalRemaining {
		allowed = totalRemaining
		scope = budget.ArtifactMaxTotalBytes
		limit = s.options.MaxTotalBytes
		observed = saturatingAdd(s.totalBytes, requested)
	}
	if allowed < 0 {
		allowed = 0
	}
	s.totalBytes += allowed
	if allowed < requested {
		return allowed, &budget.LimitError{
			Scope:     string(scope),
			Dimension: budget.Bytes,
			Limit:     limit,
			Observed:  observed,
		}
	}
	return allowed, nil
}

func (s *fileStore) releaseReserved(amount int64) {
	if amount <= 0 {
		return
	}
	s.mu.Lock()
	s.totalBytes -= amount
	if s.totalBytes < 0 {
		s.totalBytes = 0
	}
	s.mu.Unlock()
}

func (s *fileStore) abortWriter(id string, bytes int64) {
	s.mu.Lock()
	delete(s.active, id)
	s.totalBytes -= bytes
	if s.totalBytes < 0 {
		s.totalBytes = 0
	}
	s.mu.Unlock()
}

func (s *fileStore) commitWriter(writer *fileWriter, ref Ref) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("artifact store is closed")
	}
	if s.active[writer.id] != writer {
		return errors.New("artifact writer is not active")
	}
	delete(s.active, writer.id)
	s.records[writer.id] = artifactRecord{ref: ref, metadata: writer.metadata}
	return nil
}

func saturatingAdd(first, second int64) int64 {
	if second > math.MaxInt64-first {
		return math.MaxInt64
	}
	return first + second
}
