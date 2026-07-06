package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
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
	Provider          UpdateProvider
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
}

type Status struct {
	UserDir         string
	ProjectDir      string
	UserDisabled    bool
	ProjectDisabled bool
	Diagnostics     []diagnostics.Diagnostic
}

func NewManager(options ManagerOptions) *Manager {
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
	return &Manager{options: options, writer: &Writer{}, disabled: map[Scope]bool{}, updates: make(chan UpdateInput, options.UpdateQueueSize), done: make(chan struct{}, options.UpdateQueueSize)}
}

func (m *Manager) LoadIndex(scope Scope) (Index, error) {
	root := m.root(scope)
	data, err := os.ReadFile(filepath.Join(root, IndexFileName))
	if os.IsNotExist(err) {
		return m.RebuildIndex(scope)
	}
	if err != nil {
		return Index{}, fmt.Errorf("读取记忆索引失败: %w", err)
	}
	index, err := ParseIndex(scope, data)
	if err != nil {
		m.addDiagnostic("memory_bad_index_rebuilt", "记忆索引损坏，已重建", filepath.Join(root, IndexFileName))
		return m.RebuildIndex(scope)
	}
	return index, nil
}

func (m *Manager) SaveNote(note Note) error {
	note = sanitizeNote(note)
	root := m.root(note.Scope)
	if root == "" {
		return fmt.Errorf("记忆目录为空")
	}
	if err := m.writer.WriteNote(root, note); err != nil {
		return err
	}
	_, err := m.RebuildIndex(note.Scope)
	return err
}

func (m *Manager) RebuildIndex(scope Scope) (Index, error) {
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
	return index, nil
}

func (m *Manager) DeleteNote(scope Scope, id string) error {
	if err := m.writer.DeleteNote(m.root(scope), id); err != nil {
		return err
	}
	_, err := m.RebuildIndex(scope)
	return err
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
	if scope == ScopeUser {
		return strings.TrimSpace(m.options.UserDir)
	}
	return strings.TrimSpace(m.options.ProjectDir)
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
