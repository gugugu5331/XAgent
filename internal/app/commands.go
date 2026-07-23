package app

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/command"
	"xagent/internal/orchestrator"
	"xagent/internal/skill"
	"xagent/internal/tui"
)

func (m *Model) dispatchInput() tea.Cmd {
	_ = m.refreshSkillCommands(context.Background())
	controller := &commandController{model: m}
	result := m.ensureCommandRegistry().Dispatch(m.input.Value(), controller)
	switch result.Kind {
	case command.DispatchEmpty:
		m.status.Error = nil
		m.exposeSkillNotice()
		return nil
	case command.DispatchPlainText:
		return m.submitUserMessage(result.Text)
	case command.DispatchExecuted, command.DispatchUnknown:
		m.input.Clear()
		m.commandMenu.Close()
		m.exposeSkillNotice()
		return controller.cmd
	default:
		return nil
	}
}

func (m *Model) submitUserMessage(text string) tea.Cmd {
	_ = m.refreshSkillCommands(context.Background())
	text = sanitizeInput(text)
	if text == "" {
		m.status.Error = nil
		return nil
	}
	if m.conversation == nil {
		m.startNewConversation()
	}
	if m.conversation == nil {
		m.exposeSkillNotice()
		return nil
	}
	if m.skillActivity == nil {
		m.skillActivity = skill.NewActivity()
	}
	if m.orchestrator == nil {
		err := m.redactError(fmt.Errorf("Orchestrator 未启用"))
		m.status.Error = err
		m.lastError = err
		m.exposeSkillNotice()
		return nil
	}
	profile, err := m.orchestrator.BuildExecutionProfile(m.mode, m.skillActivity, 0)
	if err != nil {
		safe := m.redactError(err)
		m.status.Error = safe
		m.lastError = safe
		m.exposeSkillNotice()
		return nil
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	events, err := m.orchestrator.SendRequest(requestCtx, m.conversation, orchestrator.RunRequest{
		UserText: text, Mode: m.mode, Profile: profile, Activity: m.skillActivity,
	})
	if err != nil {
		cancel()
		safe := m.redactError(err)
		m.status.Error = safe
		m.lastError = safe
		m.exposeSkillNotice()
		return nil
	}
	requestModel := ""
	if len(profile.Activity.Active) > 0 {
		requestModel = profile.Model
	}
	cmd := m.beginRequest(cancel, events, requestModel, false)
	m.syncSkillStatus()
	m.exposeSkillNotice()
	return cmd
}

func (m *Model) completeCommand() {
	_ = m.refreshSkillCommands(context.Background())
	suggestions := m.ensureCommandRegistry().Complete(m.input.Value())
	if len(suggestions) == 0 {
		m.commandMenu.Close()
		(&commandController{model: m}).DisplayNotice("没有匹配的命令；请使用 /help 查看可用命令")
		m.exposeSkillNotice()
		return
	}
	if len(suggestions) == 1 {
		m.input.SetValue("/" + suggestions[0].Name)
		m.commandMenu.Close()
		m.exposeSkillNotice()
		return
	}
	items := make([]tui.CommandMenuItem, 0, len(suggestions))
	for _, suggestion := range suggestions {
		items = append(items, tui.CommandMenuItem{Name: suggestion.Name, Description: suggestion.Description, ArgHint: suggestion.ArgHint, Badge: suggestion.Badge})
	}
	m.commandMenu.Open(items)
	m.exposeSkillNotice()
}

func (m *Model) ensureCommandRegistry() *command.Registry {
	if m.commandRegistry == nil {
		m.ensureBaseCommands()
		m.commandRegistry = command.MustNew(m.baseCommands...)
		if m.deps.SkillManager != nil {
			_ = m.installSkillSnapshot(m.deps.SkillManager.Snapshot())
		}
	}
	return m.commandRegistry
}

func (m *Model) acceptCommandCompletion() {
	item, ok := m.commandMenu.SelectedItem()
	if !ok {
		m.commandMenu.Close()
		return
	}
	m.input.SetValue("/" + item.Name)
	m.commandMenu.Close()
}
