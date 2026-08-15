package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

const defaultMaxNoteBytes = 64 * 1024

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

type Manager struct {
	options     ManagerOptions
	writer      *Writer
	disabled    map[Scope]bool
	diagnostics []diagnostics.Diagnostic
	updates     chan UpdateInput
	done        chan struct{}
	mu          sync.Mutex
	workersOnce sync.Once
	cacheMu     sync.RWMutex
	indexCache  map[Scope]cachedIndex
	scopeLocks  map[Scope]*sync.Mutex
}

type cachedIndex struct {
	index   Index
	path    string
	modTime time.Time
	size    int64
}

type Status struct {
	UserDir         string
	ProjectDir      string
	UserDisabled    bool
	ProjectDisabled bool
	Diagnostics     []diagnostics.Diagnostic
}

func NewManager(options ManagerOptions) *Manager {
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
	}
}

func (m *Manager) LoadIndex(scope Scope) (Index, error) {
	scope = normalizeScope(scope)
	root := m.root(scope)
	path := filepath.Join(root, IndexFileName)
	info, err := os.Stat(path)
	if err == nil {
		if index, ok := m.cachedIndex(scope, path, info); ok {
			return index, nil
		}
	}
	lock := m.scopeLock(scope)
	lock.Lock()
	defer lock.Unlock()
	info, err = os.Stat(path)
	if err == nil {
		if index, ok := m.cachedIndex(scope, path, info); ok {
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
	root := m.root(scope)
	if root == "" {
		return fmt.Errorf("记忆目录为空")
	}
	lock := m.scopeLock(scope)
	lock.Lock()
	defer lock.Unlock()
	if err := m.writer.WriteNote(root, note); err != nil {
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
	lock := m.scopeLock(scope)
	lock.Lock()
	defer lock.Unlock()
	return m.rebuildIndexLocked(scope)
}

func (m *Manager) rebuildIndexLocked(scope Scope) (Index, error) {
	root := m.root(scope)
	if root == "" {
		return Index{Scope: scope}, fmt.Errorf("记忆目录为空")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Index{Scope: scope}, fmt.Errorf("创建记忆目录失败: %w", err)
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
	m.cacheDiskIndex(scope, root, index)
	return index, nil
}

func (m *Manager) DeleteNote(scope Scope, id string) error {
	scope = normalizeScope(scope)
	lock := m.scopeLock(scope)
	lock.Lock()
	defer lock.Unlock()
	if err := m.writer.DeleteNote(m.root(scope), id); err != nil {
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
	return Status{UserDir: m.options.UserDir, ProjectDir: m.options.ProjectDir, UserDisabled: m.disabled[ScopeUser], ProjectDisabled: m.disabled[ScopeProject], Diagnostics: m.diagnosticsCopyLocked()}
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
	if !ok || cached.path != path || cached.size != info.Size() || !cached.modTime.Equal(info.ModTime()) {
		return Index{}, false
	}
	return cloneIndex(cached.index), true
}

func (m *Manager) storeIndexCache(scope Scope, path string, info os.FileInfo, index Index) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	m.indexCache[normalizeScope(scope)] = cachedIndex{index: cloneIndex(index), path: path, modTime: info.ModTime(), size: info.Size()}
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
	m.addDiagnostics([]diagnostics.Diagnostic{diagnostics.New(code, diagnostics.SeverityWarning, message).WithPath(path).Safe(redact.Text)})
}

func (m *Manager) addDiagnostics(items []diagnostics.Diagnostic) {
	if len(items) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.diagnostics = append(m.diagnostics, items...)
}

func (m *Manager) diagnosticsCopyLocked() []diagnostics.Diagnostic {
	items := make([]diagnostics.Diagnostic, len(m.diagnostics))
	copy(items, m.diagnostics)
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
