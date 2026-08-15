package sessionctx

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/memory"
	"xagent/internal/prompt"
	"xagent/internal/redact"
)

const defaultMaxSectionBytes = 25 * 1024

type PrepareMode string

const (
	PrepareAuto   PrepareMode = "auto"
	PrepareManual PrepareMode = "manual"
)

type InstructionLoader interface {
	Load(ctx context.Context) ([]prompt.Section, []diagnostics.Diagnostic)
}

type MemoryIndexProvider interface {
	LoadIndex(scope memory.Scope) (memory.Index, error)
	Diagnostics() []diagnostics.Diagnostic
}

type ContextPreparer interface {
	Prepare(ctx context.Context, conv *conversation.Conversation, mode contextmgr.Mode) (contextmgr.Result, error)
}

type ContextOptionsPreparer interface {
	PrepareWithOptions(ctx context.Context, conv *conversation.Conversation, opts contextmgr.PrepareOptions) (contextmgr.Result, error)
}

type Manager struct {
	Instructions    InstructionLoader
	Memory          MemoryIndexProvider
	Context         ContextPreparer
	Redactor        *redact.RuntimeRedactor
	MaxSectionBytes int
}

type PreparedContext struct {
	StableSections  []prompt.Section
	MessagesChanged bool
	Diagnostics     []diagnostics.Diagnostic
	ContextResult   contextmgr.Result
}

func (m *Manager) Prepare(ctx context.Context, conv *conversation.Conversation, mode PrepareMode) (PreparedContext, error) {
	return m.PrepareWithOptions(ctx, conv, contextmgr.PrepareOptions{
		Mode:             contextMode(mode),
		PersistArtifacts: true,
	})
}

func (m *Manager) PrepareWithOptions(ctx context.Context, conv *conversation.Conversation, opts contextmgr.PrepareOptions) (PreparedContext, error) {
	prepared := m.PrepareStable(ctx)
	if m == nil {
		return prepared, nil
	}
	if m.Context != nil && conv != nil {
		var result contextmgr.Result
		var err error
		if optionsPreparer, ok := m.Context.(ContextOptionsPreparer); ok {
			result, err = optionsPreparer.PrepareWithOptions(ctx, conv, opts)
		} else if opts.PersistArtifacts {
			// Legacy preparers cannot promise transient/no-artifact behavior.
			// Retain their old path only for persistent main-session prepares.
			result, err = m.Context.Prepare(ctx, conv, opts.Mode)
		}
		prepared.ContextResult = result
		if result.Changed {
			prepared.MessagesChanged = true
		}
		if err != nil {
			prepared.Diagnostics = append(prepared.Diagnostics, diagnostics.New("sessionctx_context_prepare_failed", diagnostics.SeverityWarning, err.Error()))
			return prepared, err
		}
	}
	return prepared, nil
}

// PrepareStable loads system instructions and memory without compacting,
// externalizing, or otherwise mutating a Conversation. Callers that need an
// in-memory transient summary use PrepareWithOptions with PersistArtifacts
// disabled instead.
func (m *Manager) PrepareStable(ctx context.Context) PreparedContext {
	prepared := PreparedContext{}
	if m == nil {
		return prepared
	}
	if m.Instructions != nil {
		sections, items := m.Instructions.Load(ctx)
		prepared.StableSections = append(prepared.StableSections, sections...)
		prepared.Diagnostics = append(prepared.Diagnostics, items...)
	}
	if m.Memory != nil {
		sections, items := m.memorySections()
		prepared.StableSections = append(prepared.StableSections, sections...)
		prepared.Diagnostics = append(prepared.Diagnostics, items...)
		prepared.Diagnostics = append(prepared.Diagnostics, m.Memory.Diagnostics()...)
	}
	prepared.StableSections = append(prepared.StableSections, restoreBoundarySection())
	prepared.StableSections = m.safeSections(prepared.StableSections)
	return prepared
}

func (m *Manager) safeSections(sections []prompt.Section) []prompt.Section {
	if m == nil {
		return nil
	}
	redactor := m.Redactor
	if redactor == nil {
		redactor = redact.NewRuntimeRedactor()
	}
	limit := m.MaxSectionBytes
	if limit <= 0 {
		limit = defaultMaxSectionBytes
	}
	result := make([]prompt.Section, 0, len(sections))
	for _, section := range sections {
		section.Name = boundedUTF8(redactor.Text(strings.TrimSpace(section.Name)), limit)
		section.Content = boundedUTF8(redactor.Text(strings.TrimSpace(section.Content)), limit)
		if section.Name == "" || section.Content == "" {
			continue
		}
		result = append(result, section)
	}
	return result
}

func boundedUTF8(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func (m *Manager) memorySections() ([]prompt.Section, []diagnostics.Diagnostic) {
	sections := []prompt.Section{memoryBoundarySection()}
	var items []diagnostics.Diagnostic
	for _, scope := range []memory.Scope{memory.ScopeProject, memory.ScopeUser} {
		index, err := m.Memory.LoadIndex(scope)
		if err != nil {
			items = append(items, diagnostics.New("sessionctx_memory_index_failed", diagnostics.SeverityWarning, err.Error()))
			continue
		}
		content := strings.TrimSpace(memory.MarshalIndex(index, 200, 25*1024))
		if content == "" || len(index.Entries) == 0 {
			continue
		}
		sections = append(sections, prompt.Section{Name: fmt.Sprintf("长期记忆索引-%s", scope), Priority: memoryPriority(scope), Content: content, Stable: true})
	}
	return sections, items
}

func contextMode(mode PrepareMode) contextmgr.Mode {
	if mode == PrepareManual {
		return contextmgr.ModeManual
	}
	return contextmgr.ModeAuto
}
