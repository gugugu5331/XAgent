package artifact

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

var errStoreUnavailable = errors.New("artifact store operation is unavailable")

type fileStore struct {
	mu sync.RWMutex

	root          string
	workspaceRoot string
	options       FileStoreOptions
	closed        bool
	closeErr      error
}

func NewFileStore(options FileStoreOptions) (Store, error) {
	root, workspace, err := validateStoreRoots(options.Root, options.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	options.Root = ""
	options.WorkspaceRoot = ""
	return &fileStore{
		root:          root,
		workspaceRoot: workspace,
		options:       options,
	}, nil
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
	return nil, errStoreUnavailable
}

func (s *fileStore) OpenForUser(ctx context.Context, id string) (io.ReadCloser, Ref, error) {
	if err := s.available(ctx); err != nil {
		return nil, Ref{}, err
	}
	return nil, Ref{}, errStoreUnavailable
}

func (s *fileStore) Cleanup(ctx context.Context) (CleanupResult, error) {
	if err := s.available(ctx); err != nil {
		return CleanupResult{}, err
	}
	return CleanupResult{}, errStoreUnavailable
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
	s.root = ""
	s.workspaceRoot = ""
	return s.closeErr
}
