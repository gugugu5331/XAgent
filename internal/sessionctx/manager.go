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
	"xagent/internal/safefs"
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

	userInstructions    InstructionLoader
	userMemory          MemoryIndexProvider
	projectInstructions InstructionLoader
	projectMemory       MemoryIndexProvider
	projectRoot         string
	projectIdentity     safefs.Identity
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
		if !m.projectContextAvailable() {
			m.invalidateProjectContext(&prepared)
			return prepared, ErrProjectRootChanged
		}
		var result contextmgr.Result
		var err error
		if optionsPreparer, ok := m.Context.(ContextOptionsPreparer); ok {
			result, err = optionsPreparer.PrepareWithOptions(ctx, conv, opts)
		} else if opts.PersistArtifacts {
			// Legacy preparers cannot promise transient/no-artifact behavior.
			// Retain their old path only for persistent main-session prepares.
			result, err = m.Context.Prepare(ctx, conv, opts.Mode)
		}
		if m.projectRoot != "" && !sameProjectRootIdentity(m.projectRoot, m.projectIdentity) {
			m.invalidateProjectContext(&prepared)
			return prepared, ErrProjectRootChanged
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
	if m.projectRoot != "" {
		m.prepareRootBoundStable(ctx, &prepared)
	} else if m.Instructions != nil {
		sections, items := m.Instructions.Load(ctx)
		prepared.StableSections = append(prepared.StableSections, sections...)
		prepared.Diagnostics = append(prepared.Diagnostics, items...)
	}
	if m.projectRoot == "" && m.Memory != nil {
		sections, items := m.memorySections()
		prepared.StableSections = append(prepared.StableSections, sections...)
		prepared.Diagnostics = append(prepared.Diagnostics, items...)
		prepared.Diagnostics = append(prepared.Diagnostics, m.Memory.Diagnostics()...)
	}
	prepared.StableSections = append(prepared.StableSections, restoreBoundarySection())
	prepared.StableSections = m.safeSections(prepared.StableSections)
	return prepared
}

// ProjectRoot 返回 Factory 校验过的规范化绝对项目根。该值仅供任务私有运行时
// 和路径相关缓存使用，不应写入跨任务事件或持久日志。
func (m *Manager) ProjectRoot() string {
	if m == nil {
		return ""
	}
	return m.projectRoot
}

// ProjectCacheKey 是项目级缓存的根命名空间。T46/T47 在其后追加各自的绝对
// 资源路径或身份，不能只使用相对路径。
func (m *Manager) ProjectCacheKey() string {
	return m.ProjectRoot()
}

func (m *Manager) prepareRootBoundStable(ctx context.Context, prepared *PreparedContext) {
	m.appendScopedInstructions(ctx, prepared, m.userInstructions, prompt.ScopeGlobal, prompt.ScopeUser)
	projectAvailable := sameProjectRootIdentity(m.projectRoot, m.projectIdentity)
	projectInstructions := PreparedContext{}
	projectMemory := PreparedContext{}
	if projectAvailable {
		m.appendScopedInstructions(ctx, &projectInstructions, m.projectInstructions, prompt.ScopeProject, prompt.ScopeRuntime)
		projectAvailable = sameProjectRootIdentity(m.projectRoot, m.projectIdentity)
	}
	if projectAvailable {
		m.appendMemoryScope(&projectMemory, m.projectMemory, memory.ScopeProject)
		projectAvailable = sameProjectRootIdentity(m.projectRoot, m.projectIdentity)
	}
	userMemory := PreparedContext{}
	m.appendMemoryScope(&userMemory, m.userMemory, memory.ScopeUser)
	if projectAvailable {
		// 用户级依赖执行期间根仍可能被外部替换。项目内容只在最终身份
		// 复验后一次性提交，避免返回半旧半新的上下文。
		projectAvailable = sameProjectRootIdentity(m.projectRoot, m.projectIdentity)
	}
	if projectAvailable {
		prepared.StableSections = append(prepared.StableSections, projectInstructions.StableSections...)
		prepared.Diagnostics = append(prepared.Diagnostics, projectInstructions.Diagnostics...)
	} else {
		prepared.Diagnostics = append(prepared.Diagnostics, diagnostics.New(
			"sessionctx_project_root_changed", diagnostics.SeverityWarning, "project context was skipped because the bound root identity changed",
		))
	}
	if m.userMemory != nil || projectAvailable && m.projectMemory != nil {
		prepared.StableSections = append(prepared.StableSections, memoryBoundarySection())
	}
	if projectAvailable {
		prepared.StableSections = append(prepared.StableSections, projectMemory.StableSections...)
		prepared.Diagnostics = append(prepared.Diagnostics, projectMemory.Diagnostics...)
	}
	prepared.StableSections = append(prepared.StableSections, userMemory.StableSections...)
	prepared.Diagnostics = append(prepared.Diagnostics, userMemory.Diagnostics...)
}

func (m *Manager) appendScopedInstructions(ctx context.Context, prepared *PreparedContext, loader InstructionLoader, allowed ...prompt.Scope) {
	if loader == nil {
		return
	}
	sections, items := loader.Load(ctx)
	prepared.Diagnostics = append(prepared.Diagnostics, items...)
	accepted := make(map[prompt.Scope]struct{}, len(allowed))
	for _, scope := range allowed {
		accepted[scope] = struct{}{}
	}
	mismatch := false
	for _, section := range sections {
		if _, ok := accepted[section.Scope]; !ok {
			mismatch = true
			continue
		}
		prepared.StableSections = append(prepared.StableSections, section)
	}
	if mismatch {
		prepared.Diagnostics = append(prepared.Diagnostics, diagnostics.New(
			"sessionctx_scope_mismatch", diagnostics.SeverityWarning, "a root-bound context section had an unexpected scope and was skipped",
		))
	}
}

func (m *Manager) appendMemoryScope(prepared *PreparedContext, provider MemoryIndexProvider, scope memory.Scope) {
	if provider == nil {
		return
	}
	index, err := provider.LoadIndex(scope)
	if err != nil {
		prepared.Diagnostics = append(prepared.Diagnostics, diagnostics.New("sessionctx_memory_index_failed", diagnostics.SeverityWarning, err.Error()))
	} else if index.Scope != scope {
		prepared.Diagnostics = append(prepared.Diagnostics, diagnostics.New(
			"sessionctx_memory_scope_mismatch", diagnostics.SeverityWarning, "a memory provider returned an index for a different scope and it was skipped",
		))
	} else if content := strings.TrimSpace(memory.MarshalIndex(index, 200, 25*1024)); content != "" && len(index.Entries) > 0 {
		prepared.StableSections = append(prepared.StableSections, prompt.Section{
			Name: fmt.Sprintf("长期记忆索引-%s", scope), Priority: memoryPriority(scope), Content: content, Stable: true,
			Scope: memoryPromptScope(scope),
		})
	}
	prepared.Diagnostics = append(prepared.Diagnostics, provider.Diagnostics()...)
}

func (m *Manager) projectContextAvailable() bool {
	return m == nil || m.projectRoot == "" || sameProjectRootIdentity(m.projectRoot, m.projectIdentity)
}

func (m *Manager) invalidateProjectContext(prepared *PreparedContext) {
	if prepared == nil {
		return
	}
	retained := prepared.StableSections[:0]
	for _, section := range prepared.StableSections {
		if section.Scope == prompt.ScopeProject || section.Scope == prompt.ScopeRuntime {
			continue
		}
		retained = append(retained, section)
	}
	prepared.StableSections = retained
	prepared.ContextResult = contextmgr.Result{}
	prepared.MessagesChanged = false
	prepared.Diagnostics = append(prepared.Diagnostics, diagnostics.New(
		"sessionctx_project_root_changed", diagnostics.SeverityWarning, "project context was discarded because the bound root identity changed",
	))
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
		sections = append(sections, prompt.Section{
			Name: fmt.Sprintf("长期记忆索引-%s", scope), Priority: memoryPriority(scope), Content: content, Stable: true,
			Scope: memoryPromptScope(scope),
		})
	}
	return sections, items
}

func memoryPromptScope(scope memory.Scope) prompt.Scope {
	switch scope {
	case memory.ScopeUser:
		return prompt.ScopeUser
	case memory.ScopeProject:
		return prompt.ScopeProject
	default:
		return ""
	}
}

func contextMode(mode PrepareMode) contextmgr.Mode {
	if mode == PrepareManual {
		return contextmgr.ModeManual
	}
	return contextmgr.ModeAuto
}
