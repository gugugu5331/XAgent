package app

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/command"
	"xagent/internal/tui"
)

func (m *Model) dispatchInput() tea.Cmd {
	controller := &commandController{model: m}
	result := m.ensureCommandRegistry().Dispatch(m.input.Value(), controller)
	switch result.Kind {
	case command.DispatchEmpty:
		m.status.Error = nil
		return nil
	case command.DispatchPlainText:
		return m.submitUserMessage(result.Text)
	case command.DispatchExecuted, command.DispatchUnknown:
		m.input.Clear()
		m.commandMenu.Close()
		return controller.cmd
	default:
		return nil
	}
}

func (m *Model) submitUserMessage(text string) tea.Cmd {
	text = sanitizeInput(text)
	if text == "" {
		m.status.Error = nil
		return nil
	}
	if m.conversation == nil {
		m.startNewConversation()
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	events, err := m.orchestrator.SendWithMode(requestCtx, m.conversation, text, m.mode)
	if err != nil {
		cancel()
		m.status.Error = err
		m.lastError = err
		return nil
	}
	m.request = &RequestSession{Cancel: cancel, StartedAt: time.Now(), Timeout: time.Duration(m.deps.Config.LLM.RequestTimeoutMS) * time.Millisecond}
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
	m.status.Error = nil
	m.status.Notice = ""
	return listen(events)
}

func (m *Model) completeCommand() {
	suggestions := m.ensureCommandRegistry().Complete(m.input.Value())
	if len(suggestions) == 0 {
		m.commandMenu.Close()
		(&commandController{model: m}).DisplayNotice("没有匹配的命令；请使用 /help 查看可用命令")
		return
	}
	if len(suggestions) == 1 {
		m.input.SetValue("/" + suggestions[0].Name)
		m.commandMenu.Close()
		return
	}
	items := make([]tui.CommandMenuItem, 0, len(suggestions))
	for _, suggestion := range suggestions {
		items = append(items, tui.CommandMenuItem{Name: suggestion.Name, Description: suggestion.Description, ArgHint: suggestion.ArgHint})
	}
	m.commandMenu.Open(items)
}

func (m *Model) ensureCommandRegistry() *command.Registry {
	if m.commandRegistry == nil {
		m.commandRegistry = command.MustNew(command.Builtins()...)
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
