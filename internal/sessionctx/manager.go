package sessionctx

import (
	"context"
	"fmt"
	"strings"

	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/memory"
	"xagent/internal/prompt"
)

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

type Manager struct {
	Instructions InstructionLoader
	Memory       MemoryIndexProvider
	Context      ContextPreparer
}

type PreparedContext struct {
	StableSections  []prompt.Section
	MessagesChanged bool
	Diagnostics     []diagnostics.Diagnostic
	ContextResult   contextmgr.Result
}

func (m *Manager) Prepare(ctx context.Context, conv *conversation.Conversation, mode PrepareMode) (PreparedContext, error) {
	prepared := PreparedContext{}
	if m == nil {
		return prepared, nil
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
	if m.Context != nil && conv != nil {
		result, err := m.Context.Prepare(ctx, conv, contextMode(mode))
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
