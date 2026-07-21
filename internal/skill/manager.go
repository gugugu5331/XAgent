package skill

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

type SourceFS struct {
	Source Source
	FS     fs.FS
	Root   string
}

type ManagerOptions struct {
	Sources         []SourceFS
	ToolNames       []string
	ReservedCommand []string
	Limits          Limits
	Redact          func(string) string
}

type RefreshResult struct {
	Changed     bool
	Generation  uint64
	Diagnostics []diagnostics.Diagnostic
}

type Manager struct {
	mu                  sync.RWMutex
	options             ManagerOptions
	snapshot            Snapshot
	manifestFingerprint string
}

func NewManager(options ManagerOptions) (*Manager, error) {
	if options.Redact == nil {
		options.Redact = redact.Text
	}
	options.Limits = normalizeLimits(options.Limits)
	manager := &Manager{options: cloneManagerOptions(options)}
	manifest, err := fingerprintSources(context.Background(), manager.options.Sources, manager.options.Limits)
	if err != nil {
		return nil, safeManagerError("scan skill sources", err, manager.options.Redact)
	}
	snapshot, err := buildManagerSnapshot(context.Background(), manager.options, 1)
	if err != nil {
		return nil, safeManagerError("load skill snapshot", err, manager.options.Redact)
	}
	manager.snapshot = snapshot.Clone()
	manager.manifestFingerprint = manifest
	return manager, nil
}

func (m *Manager) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshot.Clone()
}

func (m *Manager) Resolve(name string) (Definition, bool) {
	if m == nil {
		return Definition{}, false
	}
	name = strings.ToLower(strings.TrimSpace(name))
	m.mu.RLock()
	defer m.mu.RUnlock()
	definition, exists := m.snapshot.Definitions[name]
	return cloneDefinition(definition), exists
}

func (m *Manager) RefreshIfChanged(ctx context.Context) (RefreshResult, error) {
	if m == nil {
		return RefreshResult{}, fmt.Errorf("skill manager is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return RefreshResult{Generation: m.snapshot.Generation}, err
	}
	manifest, err := fingerprintSources(ctx, m.options.Sources, m.options.Limits)
	if err != nil {
		safeErr := safeManagerError("scan skill sources", err, m.options.Redact)
		return RefreshResult{Generation: m.snapshot.Generation, Diagnostics: []diagnostics.Diagnostic{managerFailureDiagnostic(safeErr, m.options.Redact)}}, safeErr
	}
	if manifest == m.manifestFingerprint {
		return RefreshResult{Generation: m.snapshot.Generation}, nil
	}
	candidate, err := buildManagerSnapshot(ctx, m.options, m.snapshot.Generation+1)
	if err != nil {
		safeErr := safeManagerError("refresh skill snapshot", err, m.options.Redact)
		return RefreshResult{Generation: m.snapshot.Generation, Diagnostics: []diagnostics.Diagnostic{managerFailureDiagnostic(safeErr, m.options.Redact)}}, safeErr
	}
	m.snapshot = candidate.Clone()
	m.manifestFingerprint = manifest
	return RefreshResult{Changed: true, Generation: candidate.Generation, Diagnostics: append([]diagnostics.Diagnostic(nil), candidate.Diagnostics...)}, nil
}

func cloneManagerOptions(options ManagerOptions) ManagerOptions {
	options.Sources = append([]SourceFS(nil), options.Sources...)
	options.ToolNames = append([]string(nil), options.ToolNames...)
	options.ReservedCommand = append([]string(nil), options.ReservedCommand...)
	return options
}

func safeManagerError(prefix string, err error, redactor func(string) string) error {
	message := prefix
	if err != nil {
		message += ": " + err.Error()
	}
	if redactor != nil {
		message = redactor(message)
	}
	return fmt.Errorf("%s", message)
}

func managerFailureDiagnostic(err error, redactor func(string) string) diagnostics.Diagnostic {
	message := "skill snapshot refresh failed"
	if err != nil {
		message = err.Error()
	}
	return diagnostics.New("skill_snapshot_rejected", diagnostics.SeverityError, message).Safe(redactor)
}
