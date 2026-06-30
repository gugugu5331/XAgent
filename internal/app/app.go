package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/orchestrator"
	"xagent/internal/tui"
)

type screen string

const (
	screenList screen = "list"
	screenChat screen = "chat"
)

type Model struct {
	deps         Deps
	orchestrator *orchestrator.Orchestrator
	screen       screen
	list         list.Model
	input        tui.Input
	messages     tui.MessagesView
	status       tui.Status
	conversation *conversation.Conversation
	streaming    bool
	confirmation *Event
}

func New(deps Deps) Model {
	ctx := context.Background()
	conversations, _ := deps.Store.List(ctx)
	model := Model{
		deps:         deps,
		orchestrator: orchestrator.NewWithTools(deps.Provider, deps.Store, deps.Resources, deps.Config.LLM.Thinking, deps.Registry, deps.Executor),
		screen:       screenList,
		list:         tui.NewConversationList(conversations),
		input:        tui.NewInput(deps.Resources.UILabel("input_prompt")),
		messages:     tui.NewMessagesView(deps.Config.LLM.Thinking.Show),
		status: tui.Status{
			Provider: deps.Provider.Name(),
			Model:    deps.Config.LLM.Model,
		},
	}
	if deps.Config.UI.StartMode == config.StartModeNew {
		model.startNewConversation()
	}
	return model
}

func (m Model) Init() tea.Cmd {
	return nil
}

func (m Model) View() string {
	if m.screen == screenList {
		return m.list.View() + "\n" + m.status.View() + "\n按 Enter 选择，q 退出"
	}
	content := lipgloss.NewStyle().Padding(1, 2).Render(m.messages.View())
	footer := "Enter 提交，q/ctrl+c 退出"
	if m.confirmation != nil && m.confirmation.Confirmation != nil {
		footer = m.confirmation.Confirmation.Prompt
	}
	return fmt.Sprintf("%s\n\n%s\n%s\n%s", content, m.status.View(), m.input.Text.View(), footer)
}

func (m *Model) startNewConversation() {
	conv, err := m.deps.Store.Create(context.Background())
	if err != nil {
		m.status.Error = err
		return
	}
	m.conversation = conv
	m.messages.SetMessages(nil)
	m.screen = screenChat
}

func (m *Model) loadConversation(id string) {
	conv, err := m.deps.Store.Load(context.Background(), id)
	if err != nil {
		m.status.Error = err
		return
	}
	m.conversation = conv
	m.messages.SetMessages(conv.Messages)
	m.screen = screenChat
}

func listen(events <-chan Event) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return nil
		}
		return eventMsg{event: event, events: events}
	}
}

type eventMsg struct {
	event  Event
	events <-chan Event
}

func sanitizeInput(value string) string {
	return strings.TrimSpace(value)
}

func formatDuration(duration time.Duration) time.Duration {
	return duration.Round(time.Millisecond)
}
