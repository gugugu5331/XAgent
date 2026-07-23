package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/command"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
	"xagent/internal/skill"
)

func (m *Model) installInitialSkillCommands() {
	m.ensureBaseCommands()
	if m.deps.SkillManager == nil {
		return
	}
	snapshot := m.deps.SkillManager.Snapshot()
	if err := m.installSkillSnapshot(snapshot); err != nil {
		m.skillNotice = "Skill 命令初始化失败，继续使用基础命令: " + m.redactSkillText(err.Error())
		return
	}
	m.skillNotice = formatSkillDiagnostics("Skill 诊断", snapshot.Diagnostics, nil, m.redactSkillText)
}

func (m *Model) refreshSkillCommands(ctx context.Context) error {
	manager := m.deps.SkillManager
	if manager == nil {
		return nil
	}
	result, err := manager.RefreshIfChanged(ctx)
	if err != nil {
		m.skillNotice = formatSkillDiagnostics("Skill 更新失败，继续使用上一版本", result.Diagnostics, err, m.redactSkillText)
		return err
	}
	snapshot := manager.Snapshot()
	if m.commandRegistry == nil || snapshot.Generation != m.skillGeneration {
		if err := m.installSkillSnapshot(snapshot); err != nil {
			m.skillNotice = "Skill 命令更新失败，继续使用上一版本: " + m.redactSkillText(err.Error())
			return err
		}
	}
	m.skillNotice = formatSkillDiagnostics("Skill 诊断", snapshot.Diagnostics, nil, m.redactSkillText)
	return nil
}

func (m *Model) installSkillSnapshot(snapshot skill.Snapshot) error {
	m.ensureBaseCommands()
	definitions := make([]command.Definition, 0, len(m.baseCommands)+len(snapshot.Catalog))
	definitions = append(definitions, m.baseCommands...)
	for _, item := range snapshot.Catalog {
		if !item.SlashEnabled {
			continue
		}
		definitions = append(definitions, command.Definition{
			Name:        item.Name,
			Description: item.Description,
			Usage:       "/" + item.Name + " [args]",
			Type:        command.TypePrompt,
			ArgHint:     "[args]",
			Badge:       "Skill/" + string(item.Mode),
			Handler:     skillCommandHandler,
		})
	}
	registry, err := command.New(definitions...)
	if err != nil {
		return err
	}
	m.commandRegistry = registry
	m.skillGeneration = snapshot.Generation
	return nil
}

func (m *Model) ensureBaseCommands() {
	if len(m.baseCommands) > 0 {
		return
	}
	if m.commandRegistry != nil {
		m.baseCommands = m.commandRegistry.Definitions()
		return
	}
	registry := command.MustNew(command.Builtins()...)
	m.commandRegistry = registry
	m.baseCommands = registry.Definitions()
}

func skillCommandHandler(context command.ExecutionContext, invocation command.Invocation) error {
	return context.Controller.ExecuteSkill(invocation.CanonicalName, invocation.Args, invocation.Raw)
}

func (m *Model) executeSkill(name string, args string, raw string) (tea.Cmd, error) {
	_ = m.refreshSkillCommands(context.Background())
	if m.orchestrator == nil {
		return nil, fmt.Errorf("Orchestrator 未启用")
	}
	if m.skillActivity == nil {
		m.skillActivity = skill.NewActivity()
	}
	if m.conversation == nil {
		m.startNewConversation()
	}
	if m.conversation == nil {
		return nil, fmt.Errorf("无法创建会话")
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	eventStream, prepared, err := m.orchestrator.SendSkill(requestCtx, m.conversation, skill.Invocation{
		Name: name, Args: args, Raw: raw, Origin: skill.OriginSlash,
	}, m.skillActivity, m.mode)
	if err != nil {
		cancel()
		return nil, err
	}
	independent := prepared.Mode == skill.ModeIsolated
	if independent {
		m.status.ActiveSkills = prepared.Definition.Name
	} else {
		m.syncSkillStatus()
	}
	requestModel := strings.TrimSpace(prepared.Activated.Model)
	if !independent {
		requestModel = strings.TrimSpace(m.skillActivity.Snapshot().Model)
	}
	if requestModel == "" && m.deps.Config != nil {
		requestModel = strings.TrimSpace(m.deps.Config.LLM.Model)
	}
	cmd := m.beginRequest(cancel, eventStream, requestModel, independent)
	m.exposeSkillNotice()
	return cmd, nil
}

func (m *Model) beginRequest(cancel context.CancelFunc, eventStream <-chan Event, requestModel string, independent bool) tea.Cmd {
	timeout := time.Duration(0)
	if m.deps.Config != nil {
		timeout = time.Duration(m.deps.Config.LLM.RequestTimeoutMS) * time.Millisecond
	}
	m.request = &RequestSession{
		Cancel: cancel, StartedAt: time.Now(), Timeout: timeout, Independent: independent,
	}
	if m.lifecycle == nil {
		m.lifecycle = newLifecycleState()
	}
	m.lifecycle.begin(m.request)
	m.input.Clear()
	m.input.SetEnabled(false)
	m.streaming = true
	m.status.Streaming = true
	m.status.WaitingConfirmation = false
	m.status.AgentIteration = 0
	m.status.AgentMaxIterations = 0
	m.status.StopReason = ""
	m.status.StopMessage = ""
	m.status.InputTokens = 0
	m.status.OutputTokens = 0
	m.status.CacheCreationInputTokens = 0
	m.status.CacheReadInputTokens = 0
	m.status.RequestModel = m.redactSkillText(strings.TrimSpace(requestModel))
	m.status.Error = nil
	m.status.Notice = ""
	return listen(eventStream)
}

func (m *Model) syncSkillStatus() {
	if m.skillActivity == nil {
		m.status.ActiveSkills = ""
		return
	}
	active := m.skillActivity.Snapshot().Active
	names := make([]string, 0, len(active))
	for _, item := range active {
		names = append(names, item.Name)
	}
	m.status.ActiveSkills = strings.Join(names, ", ")
}

func (m *Model) exposeSkillNotice() {
	if strings.TrimSpace(m.skillNotice) == "" {
		return
	}
	if strings.TrimSpace(m.status.Notice) == "" {
		m.status.Notice = m.skillNotice
		return
	}
	if !strings.Contains(m.status.Notice, m.skillNotice) {
		m.status.Notice += "；" + m.skillNotice
	}
}

func (m *Model) redactSkillText(value string) string {
	return m.redactText(value)
}

func formatSkillDiagnostics(prefix string, items []diagnostics.Diagnostic, err error, redactor func(string) string) string {
	if redactor == nil {
		redactor = redact.Text
	}
	parts := make([]string, 0, len(items)+1)
	for _, item := range items {
		if text := strings.TrimSpace(item.Safe(redactor).Text()); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 && err != nil {
		parts = append(parts, redactor(err.Error()))
	}
	if len(parts) == 0 {
		return ""
	}
	return prefix + ": " + strings.Join(parts, "; ")
}
