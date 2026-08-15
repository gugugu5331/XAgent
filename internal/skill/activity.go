package skill

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"xagent/internal/redact"
)

type Activated struct {
	Name         string
	Description  string
	Mode         Mode
	Instructions string
	AllowedTools []string
	Model        string
	PackageRoot  string
	Source       Source
	Fingerprint  string
	Args         string
}

type Activity struct {
	mu          sync.RWMutex
	active      map[string]Activated
	cleanups    map[string]func() error
	limits      Limits
	materialize func(string) (string, func() error, error)
	redactor    *redact.RuntimeRedactor
}

type ActivitySnapshot struct {
	Active       []Activated
	Prompt       []SafeActivated
	Model        string
	AllowedTools []string
	ReadRoots    []string
}

// SafeActivated is the only active-skill view consumed by prompt rendering.
// Every text field has already crossed the RuntimeRedactor boundary.
type SafeActivated struct {
	Name         redact.SafeText
	Mode         redact.SafeText
	Source       redact.SafeText
	PackageRoot  redact.SafeText
	Instructions redact.SafeText
	AllowedTools []string
}

func NewActivity() *Activity {
	return NewActivityWithLimits(DefaultLimits())
}

func NewActivityWithLimits(limits Limits) *Activity {
	return NewActivityWithRedactor(limits, redact.NewRuntimeRedactor())
}

func NewActivityWithRedactor(limits Limits, redactor *redact.RuntimeRedactor) *Activity {
	if redactor == nil {
		redactor = redact.NewRuntimeRedactor()
	}
	return &Activity{
		active:      map[string]Activated{},
		cleanups:    map[string]func() error{},
		limits:      normalizeLimits(limits),
		materialize: MaterializeBuiltin,
		redactor:    redactor,
	}
}

func (a *Activity) Activate(def Definition, args string) (Activated, error) {
	if a == nil {
		return Activated{}, fmt.Errorf("skill activity is nil")
	}
	limits := normalizeLimits(a.limits)
	if len(args) > limits.MaxArgsBytes {
		return Activated{}, fmt.Errorf("skill arguments exceed %d bytes", limits.MaxArgsBytes)
	}
	metadata, err := ValidateMetadata(def.Metadata)
	if err != nil {
		return Activated{}, err
	}
	if strings.TrimSpace(def.Body) == "" {
		return Activated{}, fmt.Errorf("skill body is empty")
	}
	instructions := strings.ReplaceAll(def.Body, "{{args}}", args)
	if len(instructions) > limits.MaxBodyBytes {
		return Activated{}, fmt.Errorf("expanded skill instructions exceed %d bytes", limits.MaxBodyBytes)
	}
	activated := Activated{
		Name:         metadata.Name,
		Description:  redact.Text(metadata.Description),
		Mode:         metadata.Mode,
		Instructions: instructions,
		AllowedTools: append([]string(nil), metadata.AllowedTools...),
		Model:        metadata.Model,
		PackageRoot:  def.PackageRoot,
		Source:       def.Source,
		Fingerprint:  def.Fingerprint,
		Args:         args,
	}

	a.mu.Lock()
	candidate := cloneActivatedMap(a.active)
	candidate[activated.Name] = cloneActivated(activated)
	if _, err := aggregateActivity(candidate, a.redactor); err != nil {
		a.mu.Unlock()
		return Activated{}, err
	}
	var cleanup func() error
	if activated.Source == SourceBuiltin && strings.HasPrefix(activated.PackageRoot, "builtin://") {
		materialize := a.materialize
		if materialize == nil {
			materialize = MaterializeBuiltin
		}
		root, materializedCleanup, err := materialize(activated.Name)
		if err != nil {
			a.mu.Unlock()
			return Activated{}, fmt.Errorf("materialize builtin skill: %w", err)
		}
		activated.PackageRoot = root
		cleanup = materializedCleanup
		candidate[activated.Name] = cloneActivated(activated)
	}
	oldCleanup := a.cleanups[activated.Name]
	a.active = candidate
	if a.cleanups == nil {
		a.cleanups = map[string]func() error{}
	}
	if cleanup == nil {
		delete(a.cleanups, activated.Name)
	} else {
		a.cleanups[activated.Name] = cleanup
	}
	a.mu.Unlock()
	if oldCleanup != nil {
		_ = oldCleanup()
	}
	return cloneActivated(activated), nil
}

func (a *Activity) Snapshot() ActivitySnapshot {
	if a == nil {
		return ActivitySnapshot{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	snapshot, _ := aggregateActivity(a.active, a.redactor)
	return cloneActivitySnapshot(snapshot)
}

func (a *Activity) Clear() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.active = map[string]Activated{}
	cleanups := a.cleanups
	a.cleanups = map[string]func() error{}
	a.mu.Unlock()
	for _, cleanup := range cleanups {
		if cleanup != nil {
			_ = cleanup()
		}
	}
}

func aggregateActivity(active map[string]Activated, redactor *redact.RuntimeRedactor) (ActivitySnapshot, error) {
	if redactor == nil {
		redactor = redact.NewRuntimeRedactor()
	}
	names := make([]string, 0, len(active))
	for name := range active {
		names = append(names, name)
	}
	sort.Strings(names)
	snapshot := ActivitySnapshot{Active: make([]Activated, 0, len(names))}
	var restricted bool
	var allowed map[string]struct{}
	rootSet := map[string]struct{}{}
	for _, name := range names {
		item := cloneActivated(active[name])
		model := strings.TrimSpace(item.Model)
		if model != "" {
			if snapshot.Model != "" && snapshot.Model != model {
				return ActivitySnapshot{}, fmt.Errorf("active skills require different models")
			}
			snapshot.Model = model
		}
		if len(item.AllowedTools) > 0 {
			current := make(map[string]struct{}, len(item.AllowedTools))
			for _, toolName := range item.AllowedTools {
				current[toolName] = struct{}{}
			}
			if !restricted {
				allowed = current
				restricted = true
			} else {
				for toolName := range allowed {
					if _, exists := current[toolName]; !exists {
						delete(allowed, toolName)
					}
				}
			}
		}
		if root := strings.TrimSpace(item.PackageRoot); root != "" {
			rootSet[root] = struct{}{}
		}
		snapshot.Active = append(snapshot.Active, item)
		snapshot.Prompt = append(snapshot.Prompt, SafeActivated{
			Name:         redactor.Redact(item.Name),
			Mode:         redactor.Redact(string(item.Mode)),
			Source:       redactor.Redact(string(item.Source)),
			PackageRoot:  redactor.Redact(item.PackageRoot),
			Instructions: redactor.Redact(item.Instructions),
			AllowedTools: append([]string(nil), item.AllowedTools...),
		})
	}
	if restricted {
		snapshot.AllowedTools = make([]string, 0, len(allowed))
		for toolName := range allowed {
			snapshot.AllowedTools = append(snapshot.AllowedTools, toolName)
		}
		sort.Strings(snapshot.AllowedTools)
	}
	for root := range rootSet {
		snapshot.ReadRoots = append(snapshot.ReadRoots, root)
	}
	sort.Strings(snapshot.ReadRoots)
	return snapshot, nil
}

func cloneActivatedMap(source map[string]Activated) map[string]Activated {
	clone := make(map[string]Activated, len(source)+1)
	for name, item := range source {
		clone[name] = cloneActivated(item)
	}
	return clone
}

func cloneActivated(item Activated) Activated {
	item.AllowedTools = append([]string(nil), item.AllowedTools...)
	return item
}

func cloneActivitySnapshot(snapshot ActivitySnapshot) ActivitySnapshot {
	clone := ActivitySnapshot{
		Model:        snapshot.Model,
		AllowedTools: cloneStringSlicePreservingNil(snapshot.AllowedTools),
		ReadRoots:    append([]string(nil), snapshot.ReadRoots...),
		Active:       make([]Activated, len(snapshot.Active)),
		Prompt:       make([]SafeActivated, len(snapshot.Prompt)),
	}
	for index, item := range snapshot.Active {
		clone.Active[index] = cloneActivated(item)
	}
	for index, item := range snapshot.Prompt {
		item.AllowedTools = append([]string(nil), item.AllowedTools...)
		clone.Prompt[index] = item
	}
	return clone
}

func cloneStringSlicePreservingNil(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}
