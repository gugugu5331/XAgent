package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

const defaultMaxNoteBytes = 64 * 1024

var (
	ErrWorkspaceManagerInvalid = errors.New("workspace memory manager binding is invalid")
	ErrWorkspaceManagerClosed  = errors.New("workspace memory manager is closed")
	ErrProjectIdentityChanged  = errors.New("workspace project memory identity changed")
	ErrProjectDirectoryChanged = errors.New("workspace project memory directory changed")
)

type ManagerOptions struct {
	UserDir           string
	ProjectDir        string
	MaxIndexLines     int
	MaxIndexBytes     int
	UpdateQueueSize   int
	UpdateConcurrency int
	UpdateTimeoutMS   int
	MaxCandidateBytes int
	MaxNoteBytes      int
	Provider          UpdateProvider
	Redactor          *redact.RuntimeRedactor
}

// WorkspaceManagerOptions separates shareable user memory from project memory
// that must be rebound for one canonical Worktree root.
type WorkspaceManagerOptions struct {
	ManagerOptions
	WorktreeRoot string
	ConfigDigest string
}

type Manager struct {
	options                        ManagerOptions
	writer                         *Writer
	disabled                       map[Scope]bool
	diagnostics                    []diagnostics.Diagnostic
	updates                        chan UpdateInput
	done                           chan struct{}
	mu                             sync.Mutex
	workersOnce                    sync.Once
	cacheMu                        sync.RWMutex
	indexCache                     map[Scope]cachedIndex
	scopeLocks                     map[Scope]*sync.Mutex
	project                        *workspaceProjectBinding
	projectIdentityDiagnosticOnce  sync.Once
	projectIdentityInvalid         atomic.Bool
	projectDirectoryDiagnosticOnce sync.Once
	projectDirectoryInvalid        atomic.Bool
	projectLifecycle               sync.RWMutex
	projectClosing                 atomic.Bool
	closed                         atomic.Bool
	closeOnce                      sync.Once
	closeErr                       error
}

type workspaceProjectBinding struct {
	identity     ProjectIdentity
	dir          string
	components   []string
	worktreeRoot *os.Root
	worktreeInfo os.FileInfo
	projectRoot  *os.Root
	projectInfo  os.FileInfo
}

type cachedIndex struct {
	index     Index
	path      string
	namespace string
	modTime   time.Time
	size      int64
	fileInfo  os.FileInfo
}

type Status struct {
	UserDir         string
	ProjectDir      string
	UserDisabled    bool
	ProjectDisabled bool
	ProjectIdentity string
	Diagnostics     []diagnostics.Diagnostic
}

func NewManager(options ManagerOptions) *Manager {
	return newManager(options, nil)
}

// NewWorkspaceManager binds project memory to one canonical Worktree object.
// It never falls back to the legacy ProjectDir if that object is replaced.
func NewWorkspaceManager(options WorkspaceManagerOptions) (*Manager, error) {
	root := strings.TrimSpace(options.WorktreeRoot)
	projectDir := strings.TrimSpace(options.ProjectDir)
	configDigest := strings.TrimSpace(options.ConfigDigest)
	if root == "" || root != options.WorktreeRoot || !filepath.IsAbs(root) || filepath.Clean(root) != root ||
		projectDir != options.ProjectDir ||
		projectDir == "" || !filepath.IsAbs(projectDir) || filepath.Clean(projectDir) != projectDir ||
		configDigest == "" || configDigest != options.ConfigDigest || !workspaceProjectDirAllowed(root, projectDir) {
		return nil, ErrWorkspaceManagerInvalid
	}
	identity, err := NewProjectIdentity(root, configDigest)
	if err != nil || identity.RootRealPath != root {
		return nil, ErrWorkspaceManagerInvalid
	}
	relativeDir, err := filepath.Rel(root, projectDir)
	if err != nil || relativeDir == "." || filepath.IsAbs(relativeDir) {
		return nil, ErrWorkspaceManagerInvalid
	}
	components := strings.Split(relativeDir, string(filepath.Separator))
	worktreeRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, ErrWorkspaceManagerInvalid
	}
	worktreeInfo, infoErr := worktreeRoot.Stat(".")
	pathInfo, pathErr := os.Stat(root)
	if infoErr != nil || pathErr != nil || !os.SameFile(worktreeInfo, pathInfo) || !identity.matchesLiveRoot() {
		_ = worktreeRoot.Close()
		return nil, ErrWorkspaceManagerInvalid
	}
	projectRoot, projectInfo, err := openWorkspaceProjectRoot(worktreeRoot, components, true)
	if err != nil || !identity.matchesLiveRoot() {
		if projectRoot != nil {
			_ = projectRoot.Close()
		}
		_ = worktreeRoot.Close()
		return nil, ErrWorkspaceManagerInvalid
	}
	manager := newManager(options.ManagerOptions, &workspaceProjectBinding{
		identity: identity, dir: projectDir, components: append([]string(nil), components...),
		worktreeRoot: worktreeRoot, worktreeInfo: worktreeInfo, projectRoot: projectRoot, projectInfo: projectInfo,
	})
	return manager, nil
}

func newManager(options ManagerOptions, project *workspaceProjectBinding) *Manager {
	if options.Redactor == nil {
		options.Redactor = redact.NewRuntimeRedactor()
	}
	if options.MaxIndexLines <= 0 {
		options.MaxIndexLines = 200
	}
	if options.MaxIndexBytes <= 0 {
		options.MaxIndexBytes = 25 * 1024
	}
	if options.UpdateQueueSize <= 0 {
		options.UpdateQueueSize = 8
	}
	if options.UpdateConcurrency <= 0 {
		options.UpdateConcurrency = 1
	}
	if options.UpdateTimeoutMS <= 0 {
		options.UpdateTimeoutMS = 30000
	}
	if options.MaxCandidateBytes <= 0 {
		options.MaxCandidateBytes = 64 * 1024
	}
	if options.MaxNoteBytes <= 0 {
		options.MaxNoteBytes = defaultMaxNoteBytes
	}
	return &Manager{
		options:    options,
		writer:     &Writer{redactor: options.Redactor, maxNoteBytes: options.MaxNoteBytes},
		disabled:   map[Scope]bool{},
		updates:    make(chan UpdateInput, options.UpdateQueueSize),
		done:       make(chan struct{}, options.UpdateQueueSize),
		indexCache: map[Scope]cachedIndex{},
		scopeLocks: map[Scope]*sync.Mutex{ScopeProject: &sync.Mutex{}, ScopeUser: &sync.Mutex{}},
		project:    project,
	}
}

func (m *Manager) LoadIndex(scope Scope) (Index, error) {
	scope = normalizeScope(scope)
	if scope == ScopeProject && m != nil && m.project != nil {
		release, err := m.beginWorkspaceProjectOperation()
		if err != nil {
			return Index{Scope: scope}, err
		}
		defer release()
		return m.loadWorkspaceProjectIndex()
	}
	if err := m.validateProjectIdentity(scope); err != nil {
		return Index{Scope: scope}, err
	}
	root := m.root(scope)
	path := filepath.Join(root, IndexFileName)
	info, err := os.Stat(path)
	if err == nil {
		if index, ok := m.cachedIndex(scope, path, info); ok {
			if err := m.validateProjectIdentity(scope); err != nil {
				return Index{Scope: scope}, err
			}
			return index, nil
		}
	}
	lock := m.scopeLock(scope)
	lock.Lock()
	defer lock.Unlock()
	if err := m.validateProjectIdentity(scope); err != nil {
		return Index{Scope: scope}, err
	}
	info, err = os.Stat(path)
	if err == nil {
		if index, ok := m.cachedIndex(scope, path, info); ok {
			if err := m.validateProjectIdentity(scope); err != nil {
				return Index{Scope: scope}, err
			}
			return index, nil
		}
	} else if os.IsNotExist(err) {
		return m.rebuildIndexLocked(scope)
	} else {
		m.invalidateIndex(scope)
		return Index{}, fmt.Errorf("读取记忆索引失败: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		m.invalidateIndex(scope)
		return Index{}, fmt.Errorf("读取记忆索引失败: %w", err)
	}
	index, err := ParseIndex(scope, data)
	if err != nil {
		m.invalidateIndex(scope)
		m.addDiagnostic("memory_bad_index_rebuilt", "记忆索引损坏，已重建", path)
		return m.rebuildIndexLocked(scope)
	}
	m.storeIndexCache(scope, path, info, index)
	if err := m.validateProjectIdentity(scope); err != nil {
		return Index{Scope: scope}, err
	}
	return cloneIndex(index), nil
}

func (m *Manager) SaveNote(note Note) error {
	var err error
	note, err = sanitizeBoundedNote(note, m.options.Redactor, m.options.MaxNoteBytes)
	if err != nil {
		return err
	}
	scope := normalizeScope(note.Scope)
	note.Scope = scope
	if scope == ScopeProject && m != nil && m.project != nil {
		release, beginErr := m.beginWorkspaceProjectOperation()
		if beginErr != nil {
			return beginErr
		}
		defer release()
		return m.saveWorkspaceProjectNote(note)
	}
	if err := m.validateProjectIdentity(scope); err != nil {
		return err
	}
	root := m.root(scope)
	if root == "" {
		return fmt.Errorf("记忆目录为空")
	}
	lock := m.scopeLock(scope)
	lock.Lock()
	defer lock.Unlock()
	if err := m.validateProjectIdentity(scope); err != nil {
		return err
	}
	if err := m.writer.WriteNote(root, note); err != nil {
		return err
	}
	if err := m.validateProjectIdentity(scope); err != nil {
		return err
	}
	if _, err := m.rebuildIndexLocked(scope); err != nil {
		m.invalidateIndex(scope)
		return err
	}
	return nil
}

// SaveNoteWithRefs persists only the typed opaque artifact identity. The
// artifact package deliberately exposes no filesystem path or payload here.
func (m *Manager) SaveNoteWithRefs(note Note, refs []artifact.Ref) error {
	if m == nil {
		return fmt.Errorf("memory manager is nil")
	}
	if len(refs) > 0 {
		var builder strings.Builder
		builder.WriteString(strings.TrimSpace(note.Body))
		for _, ref := range refs {
			id := strings.TrimSpace(ref.ID)
			if id == "" || safeID(id) != id || ref.Bytes < 0 {
				return fmt.Errorf("invalid opaque artifact ref")
			}
			if builder.Len() > 0 {
				builder.WriteByte('\n')
			}
			builder.WriteString("artifact-ref: id=")
			builder.WriteString(id)
			builder.WriteString(fmt.Sprintf(" bytes=%d created_at=%s available=%t complete=%t", ref.Bytes, ref.CreatedAt.UTC().Format(time.RFC3339), ref.Available, ref.Complete))
		}
		note.Body = builder.String()
	}
	return m.SaveNote(note)
}

func sanitizeBoundedNote(note Note, redactor *redact.RuntimeRedactor, maxBytes int) (Note, error) {
	if redactor == nil {
		redactor = redact.NewRuntimeRedactor()
	}
	note.ID = safeID(note.ID)
	note.Title = strings.TrimSpace(redactor.Text(note.Title))
	note.Source = strings.TrimSpace(redactor.Text(note.Source))
	note.Body = strings.TrimSpace(redactor.Text(note.Body))
	note = sanitizeNote(note)
	if maxBytes <= 0 {
		maxBytes = defaultMaxNoteBytes
	}
	if len(MarshalNote(note)) > maxBytes {
		return Note{}, fmt.Errorf("memory note exceeds %d bytes", maxBytes)
	}
	return note, nil
}

func (m *Manager) RebuildIndex(scope Scope) (Index, error) {
	scope = normalizeScope(scope)
	if scope == ScopeProject && m != nil && m.project != nil {
		release, err := m.beginWorkspaceProjectOperation()
		if err != nil {
			return Index{Scope: scope}, err
		}
		defer release()
		lock := m.scopeLock(scope)
		lock.Lock()
		defer lock.Unlock()
		return m.rebuildWorkspaceProjectIndexLocked()
	}
	if err := m.validateProjectIdentity(scope); err != nil {
		return Index{Scope: scope}, err
	}
	lock := m.scopeLock(scope)
	lock.Lock()
	defer lock.Unlock()
	return m.rebuildIndexLocked(scope)
}

func (m *Manager) rebuildIndexLocked(scope Scope) (Index, error) {
	if err := m.validateProjectIdentity(scope); err != nil {
		return Index{Scope: scope}, err
	}
	root := m.root(scope)
	if root == "" {
		return Index{Scope: scope}, fmt.Errorf("记忆目录为空")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Index{Scope: scope}, fmt.Errorf("创建记忆目录失败: %w", err)
	}
	if err := m.validateProjectIdentity(scope); err != nil {
		return Index{Scope: scope}, err
	}
	notes, err := readNotes(root)
	if err != nil {
		return Index{Scope: scope}, err
	}
	index, items := BuildIndex(scope, notes, m.options.MaxIndexLines, m.options.MaxIndexBytes)
	m.addDiagnostics(items)
	if err := m.writer.WriteIndex(root, index, m.options.MaxIndexLines, m.options.MaxIndexBytes); err != nil {
		return index, err
	}
	if err := m.validateProjectIdentity(scope); err != nil {
		return Index{Scope: scope}, err
	}
	m.cacheDiskIndex(scope, root, index)
	return index, nil
}

func (m *Manager) DeleteNote(scope Scope, id string) error {
	scope = normalizeScope(scope)
	if scope == ScopeProject && m != nil && m.project != nil {
		release, err := m.beginWorkspaceProjectOperation()
		if err != nil {
			return err
		}
		defer release()
		return m.deleteWorkspaceProjectNote(id)
	}
	if err := m.validateProjectIdentity(scope); err != nil {
		return err
	}
	lock := m.scopeLock(scope)
	lock.Lock()
	defer lock.Unlock()
	if err := m.validateProjectIdentity(scope); err != nil {
		return err
	}
	if err := m.writer.DeleteNote(m.root(scope), id); err != nil {
		return err
	}
	if err := m.validateProjectIdentity(scope); err != nil {
		return err
	}
	if _, err := m.rebuildIndexLocked(scope); err != nil {
		m.invalidateIndex(scope)
		return err
	}
	return nil
}

func (m *Manager) Disable(scope Scope) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disabled[scope] = true
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.project != nil {
		return Status{UserDisabled: m.disabled[ScopeUser], ProjectDisabled: m.disabled[ScopeProject], ProjectIdentity: m.project.identity.ID, Diagnostics: m.diagnosticsCopyLocked()}
	}
	return Status{UserDir: m.options.UserDir, ProjectDir: m.options.ProjectDir, UserDisabled: m.disabled[ScopeUser], ProjectDisabled: m.disabled[ScopeProject], Diagnostics: m.diagnosticsCopyLocked()}
}

// ProjectCacheNamespace is a stable, path-free key for project-scoped caches.
func (m *Manager) ProjectCacheNamespace() string {
	if m == nil || m.project == nil {
		return ""
	}
	return m.project.identity.ID
}

// Close releases the exact project handle before the Worktree identity handle.
// Legacy managers own no handles, so their Close is a no-op.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.closeOnce.Do(func() {
		if m.project != nil {
			m.projectClosing.Store(true)
			m.projectLifecycle.Lock()
			defer m.projectLifecycle.Unlock()
			m.closed.Store(true)
			var closeErrors []error
			if m.project.projectRoot != nil {
				closeErrors = append(closeErrors, m.project.projectRoot.Close())
			}
			if m.project.worktreeRoot != nil {
				closeErrors = append(closeErrors, m.project.worktreeRoot.Close())
			}
			m.closeErr = errors.Join(closeErrors...)
		} else {
			m.closed.Store(true)
		}
	})
	return m.closeErr
}

func (m *Manager) beginWorkspaceProjectOperation() (func(), error) {
	if m == nil || m.project == nil {
		return func() {}, nil
	}
	m.projectLifecycle.RLock()
	if m.projectClosing.Load() || m.closed.Load() {
		m.projectLifecycle.RUnlock()
		return func() {}, ErrWorkspaceManagerClosed
	}
	return m.projectLifecycle.RUnlock, nil
}

func (m *Manager) Diagnostics() []diagnostics.Diagnostic {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.diagnosticsCopyLocked()
}

func (m *Manager) root(scope Scope) string {
	if normalizeScope(scope) == ScopeUser {
		return strings.TrimSpace(m.options.UserDir)
	}
	if m != nil && m.project != nil {
		return m.project.dir
	}
	return strings.TrimSpace(m.options.ProjectDir)
}

func normalizeScope(scope Scope) Scope {
	if scope == ScopeUser {
		return ScopeUser
	}
	return ScopeProject
}

func (m *Manager) scopeLock(scope Scope) *sync.Mutex {
	scope = normalizeScope(scope)
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	lock := m.scopeLocks[scope]
	if lock == nil {
		lock = &sync.Mutex{}
		m.scopeLocks[scope] = lock
	}
	return lock
}

func (m *Manager) cachedIndex(scope Scope, path string, info os.FileInfo) (Index, bool) {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	cached, ok := m.indexCache[normalizeScope(scope)]
	if !ok || cached.path != path || cached.namespace != m.cacheNamespace(scope) || cached.size != info.Size() || !cached.modTime.Equal(info.ModTime()) ||
		m.workspaceCacheObjectChanged(scope, cached.fileInfo, info) {
		return Index{}, false
	}
	return cloneIndex(cached.index), true
}

func (m *Manager) storeIndexCache(scope Scope, path string, info os.FileInfo, index Index) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	m.indexCache[normalizeScope(scope)] = cachedIndex{index: cloneIndex(index), path: path, namespace: m.cacheNamespace(scope), modTime: info.ModTime(), size: info.Size(), fileInfo: info}
}

func (m *Manager) cacheDiskIndex(scope Scope, root string, index Index) {
	text := MarshalIndex(index, m.options.MaxIndexLines, m.options.MaxIndexBytes)
	diskIndex, err := ParseIndex(scope, []byte(text))
	if err != nil {
		m.invalidateIndex(scope)
		return
	}
	path := filepath.Join(root, IndexFileName)
	info, err := os.Stat(path)
	if err != nil {
		m.invalidateIndex(scope)
		return
	}
	m.storeIndexCache(scope, path, info, diskIndex)
}

func (m *Manager) invalidateIndex(scope Scope) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	delete(m.indexCache, normalizeScope(scope))
}

func cloneIndex(index Index) Index {
	entries := make([]IndexEntry, len(index.Entries))
	copy(entries, index.Entries)
	index.Entries = entries
	return index
}

func (m *Manager) addDiagnostic(code string, message string, path string) {
	if m != nil && m.project != nil {
		message = m.redactWorkspacePaths(message)
		path = ""
	}
	m.addDiagnostics([]diagnostics.Diagnostic{diagnostics.New(code, diagnostics.SeverityWarning, message).WithPath(path).Safe(redact.Text)})
}

func (m *Manager) cacheNamespace(scope Scope) string {
	if normalizeScope(scope) == ScopeProject && m != nil && m.project != nil {
		return m.project.identity.ID
	}
	return string(normalizeScope(scope))
}

func (m *Manager) loadWorkspaceProjectIndex() (Index, error) {
	if err := m.validateWorkspaceProjectState(); err != nil {
		return Index{Scope: ScopeProject}, err
	}
	path := IndexFileName
	info, err := m.workspaceRegularFileInfo(path)
	if err == nil {
		if index, ok := m.cachedIndex(ScopeProject, path, info); ok {
			if err := m.validateWorkspaceProjectState(); err != nil {
				return Index{Scope: ScopeProject}, err
			}
			return index, nil
		}
	}
	lock := m.scopeLock(ScopeProject)
	lock.Lock()
	defer lock.Unlock()
	if err := m.validateWorkspaceProjectState(); err != nil {
		return Index{Scope: ScopeProject}, err
	}
	info, err = m.workspaceRegularFileInfo(path)
	if err == nil {
		if index, ok := m.cachedIndex(ScopeProject, path, info); ok {
			if err := m.validateWorkspaceProjectState(); err != nil {
				return Index{Scope: ScopeProject}, err
			}
			return index, nil
		}
	} else if os.IsNotExist(err) {
		return m.rebuildWorkspaceProjectIndexLocked()
	} else {
		m.invalidateIndex(ScopeProject)
		return Index{Scope: ScopeProject}, fmt.Errorf("读取记忆索引失败")
	}
	data, err := m.project.projectRoot.ReadFile(path)
	if err != nil {
		m.invalidateIndex(ScopeProject)
		return Index{Scope: ScopeProject}, fmt.Errorf("读取记忆索引失败")
	}
	liveInfo, err := m.workspaceRegularFileInfo(path)
	if err != nil || !os.SameFile(info, liveInfo) {
		m.invalidateIndex(ScopeProject)
		return Index{Scope: ScopeProject}, fmt.Errorf("读取记忆索引失败")
	}
	index, err := ParseIndex(ScopeProject, data)
	if err != nil {
		m.invalidateIndex(ScopeProject)
		m.addDiagnostic("memory_bad_index_rebuilt", "记忆索引损坏，已重建", "")
		return m.rebuildWorkspaceProjectIndexLocked()
	}
	if err := m.validateWorkspaceProjectState(); err != nil {
		return Index{Scope: ScopeProject}, err
	}
	m.storeIndexCache(ScopeProject, path, liveInfo, index)
	return cloneIndex(index), nil
}

func (m *Manager) saveWorkspaceProjectNote(note Note) error {
	lock := m.scopeLock(ScopeProject)
	lock.Lock()
	defer lock.Unlock()
	if err := m.validateWorkspaceProjectState(); err != nil {
		return err
	}
	if err := m.writer.WriteNoteAt(m.project.projectRoot, note); err != nil {
		return m.workspaceDirectoryOperationError(err)
	}
	if err := m.validateWorkspaceProjectState(); err != nil {
		return err
	}
	_, err := m.rebuildWorkspaceProjectIndexLocked()
	if err != nil {
		m.invalidateIndex(ScopeProject)
	}
	return err
}

func (m *Manager) rebuildWorkspaceProjectIndexLocked() (Index, error) {
	if err := m.validateWorkspaceProjectState(); err != nil {
		return Index{Scope: ScopeProject}, err
	}
	notes, err := m.readWorkspaceProjectNotes()
	if err != nil {
		return Index{Scope: ScopeProject}, err
	}
	index, items := BuildIndex(ScopeProject, notes, m.options.MaxIndexLines, m.options.MaxIndexBytes)
	m.addDiagnostics(items)
	if err := m.writer.WriteIndexAt(m.project.projectRoot, index, m.options.MaxIndexLines, m.options.MaxIndexBytes); err != nil {
		return Index{Scope: ScopeProject}, m.workspaceDirectoryOperationError(err)
	}
	if err := m.validateWorkspaceProjectState(); err != nil {
		return Index{Scope: ScopeProject}, err
	}
	path := IndexFileName
	info, err := m.workspaceRegularFileInfo(path)
	if err != nil {
		m.invalidateIndex(ScopeProject)
		return Index{Scope: ScopeProject}, fmt.Errorf("读取记忆索引失败")
	}
	diskIndex, err := ParseIndex(ScopeProject, []byte(MarshalIndex(index, m.options.MaxIndexLines, m.options.MaxIndexBytes)))
	if err != nil {
		m.invalidateIndex(ScopeProject)
		return Index{Scope: ScopeProject}, fmt.Errorf("读取记忆索引失败")
	}
	m.storeIndexCache(ScopeProject, path, info, diskIndex)
	return index, nil
}

func (m *Manager) deleteWorkspaceProjectNote(id string) error {
	lock := m.scopeLock(ScopeProject)
	lock.Lock()
	defer lock.Unlock()
	if err := m.validateWorkspaceProjectState(); err != nil {
		return err
	}
	if err := m.writer.DeleteNoteAt(m.project.projectRoot, id); err != nil {
		return m.workspaceDirectoryOperationError(err)
	}
	if err := m.validateWorkspaceProjectState(); err != nil {
		return err
	}
	_, err := m.rebuildWorkspaceProjectIndexLocked()
	if err != nil {
		m.invalidateIndex(ScopeProject)
	}
	return err
}

func (m *Manager) readWorkspaceProjectNotes() ([]Note, error) {
	directory, err := m.project.projectRoot.Open(".")
	if err != nil {
		return nil, fmt.Errorf("读取记忆目录失败")
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("读取记忆目录失败")
	}
	notes := make([]Note, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" || entry.Name() == IndexFileName {
			continue
		}
		path := entry.Name()
		if _, err := m.workspaceRegularFileInfo(path); err != nil {
			continue
		}
		data, err := m.project.projectRoot.ReadFile(path)
		if err != nil {
			continue
		}
		note, err := ParseNote(data)
		if err == nil {
			notes = append(notes, note)
		}
	}
	return notes, nil
}

func (m *Manager) validateWorkspaceProjectState() error {
	if err := m.validateProjectIdentity(ScopeProject); err != nil {
		return err
	}
	if m == nil || m.project == nil || m.project.worktreeRoot == nil || m.project.projectRoot == nil {
		return ErrWorkspaceManagerInvalid
	}
	if m.projectDirectoryInvalid.Load() {
		return ErrProjectDirectoryChanged
	}
	currentRoot, currentInfo, err := openWorkspaceProjectRoot(m.project.worktreeRoot, m.project.components, false)
	if err != nil {
		return m.markProjectDirectoryChanged()
	}
	defer currentRoot.Close()
	handleInfo, handleErr := m.project.projectRoot.Stat(".")
	if handleErr != nil || !os.SameFile(handleInfo, m.project.projectInfo) || !os.SameFile(currentInfo, m.project.projectInfo) {
		return m.markProjectDirectoryChanged()
	}
	return nil
}

func (m *Manager) workspaceRegularFileInfo(path string) (os.FileInfo, error) {
	if m == nil || m.project == nil || m.project.projectRoot == nil || !safeRootBaseName(path) {
		return nil, ErrWorkspaceManagerInvalid
	}
	info, err := m.project.projectRoot.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("workspace memory file type is invalid")
	}
	return info, nil
}

func (m *Manager) workspaceDirectoryOperationError(operationErr error) error {
	if err := m.validateWorkspaceProjectState(); err != nil {
		return err
	}
	return operationErr
}

// openWorkspaceProjectRoot walks one component at a time from a frozen
// Worktree root. It never passes a multi-component path to os.Root, and every
// opened child is checked against the non-symlink directory observed through
// its parent before the walk advances.
func openWorkspaceProjectRoot(worktreeRoot *os.Root, components []string, create bool) (*os.Root, os.FileInfo, error) {
	if worktreeRoot == nil || len(components) == 0 {
		return nil, nil, ErrWorkspaceManagerInvalid
	}
	parent := worktreeRoot
	parentOwned := false
	closeParent := func() {
		if parentOwned {
			_ = parent.Close()
		}
	}
	for _, component := range components {
		if !safeRootBaseName(component) {
			closeParent()
			return nil, nil, ErrWorkspaceManagerInvalid
		}
		if create {
			if err := parent.Mkdir(component, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				closeParent()
				return nil, nil, err
			}
		}
		info, err := parent.Lstat(component)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			closeParent()
			if err != nil {
				return nil, nil, err
			}
			return nil, nil, ErrProjectDirectoryChanged
		}
		child, err := parent.OpenRoot(component)
		if err != nil {
			closeParent()
			return nil, nil, err
		}
		childInfo, err := child.Stat(".")
		if err != nil || !os.SameFile(info, childInfo) {
			_ = child.Close()
			closeParent()
			if err != nil {
				return nil, nil, err
			}
			return nil, nil, ErrProjectDirectoryChanged
		}
		closeParent()
		parent = child
		parentOwned = true
	}
	finalInfo, err := parent.Stat(".")
	if err != nil {
		closeParent()
		return nil, nil, err
	}
	return parent, finalInfo, nil
}

func (m *Manager) markProjectDirectoryChanged() error {
	m.projectDirectoryInvalid.Store(true)
	m.projectDirectoryDiagnosticOnce.Do(func() {
		m.invalidateIndex(ScopeProject)
		m.addDiagnostic("memory_project_directory_changed", "project memory was disabled because its workspace directory changed", "")
	})
	return ErrProjectDirectoryChanged
}

func (m *Manager) validateProjectIdentity(scope Scope) error {
	if normalizeScope(scope) != ScopeProject || m == nil || m.project == nil {
		return nil
	}
	if m.closed.Load() {
		return ErrWorkspaceManagerClosed
	}
	if m.projectIdentityInvalid.Load() {
		return ErrProjectIdentityChanged
	}
	handleInfo, handleErr := m.project.worktreeRoot.Stat(".")
	if handleErr == nil && os.SameFile(handleInfo, m.project.worktreeInfo) && m.project.identity.matchesLiveRoot() {
		return nil
	}
	m.projectIdentityInvalid.Store(true)
	m.projectIdentityDiagnosticOnce.Do(func() {
		m.invalidateIndex(ScopeProject)
		m.addDiagnostic("memory_project_identity_changed", "project memory was disabled because the bound workspace identity changed", "")
	})
	return ErrProjectIdentityChanged
}

func (m *Manager) workspaceCacheObjectChanged(scope Scope, cached os.FileInfo, live os.FileInfo) bool {
	return normalizeScope(scope) == ScopeProject && m != nil && m.project != nil &&
		(cached == nil || live == nil || !os.SameFile(cached, live))
}

func workspaceProjectDirAllowed(root string, projectDir string) bool {
	relative, err := filepath.Rel(root, projectDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return false
	}
	normalizedRelative := normalizeProjectPath(relative)
	if normalizedRelative == "." || normalizedRelative == ".git" || strings.HasPrefix(normalizedRelative, ".git/") ||
		normalizedRelative == ".control" || strings.HasPrefix(normalizedRelative, ".control/") ||
		normalizedRelative == ".xagent/worktrees" || strings.HasPrefix(normalizedRelative, ".xagent/worktrees/") {
		return false
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return true
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false
		}
	}
	return true
}

func (m *Manager) addDiagnostics(items []diagnostics.Diagnostic) {
	if len(items) == 0 {
		return
	}
	if m != nil && m.project != nil {
		cleaned := make([]diagnostics.Diagnostic, len(items))
		for index, item := range items {
			item.Message = m.redactWorkspacePaths(item.Message)
			item.Source = m.redactWorkspacePaths(item.Source)
			item.Path = ""
			cleaned[index] = item
		}
		items = cleaned
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.diagnostics = append(m.diagnostics, items...)
}

func (m *Manager) redactWorkspacePaths(value string) string {
	if m == nil || m.project == nil || value == "" {
		return value
	}
	for _, path := range []string{m.project.identity.RootRealPath, m.project.dir, strings.TrimSpace(m.options.UserDir)} {
		if filepath.IsAbs(path) {
			value = strings.ReplaceAll(value, path, "[workspace]")
		}
	}
	return value
}

func (m *Manager) diagnosticsCopyLocked() []diagnostics.Diagnostic {
	items := make([]diagnostics.Diagnostic, len(m.diagnostics))
	for index, item := range m.diagnostics {
		if len(item.Attributes) > 0 {
			attributes := make(map[string]string, len(item.Attributes))
			for key, value := range item.Attributes {
				attributes[key] = value
			}
			item.Attributes = attributes
		}
		items[index] = item
	}
	return items
}

func readNotes(root string) ([]Note, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取记忆目录失败: %w", err)
	}
	notes := []Note{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" || entry.Name() == IndexFileName {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		note, err := ParseNote(data)
		if err != nil {
			continue
		}
		notes = append(notes, note)
	}
	return notes, nil
}
