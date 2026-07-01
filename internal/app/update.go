package app

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/events"
	"xagent/internal/tui"
)

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.conversation != nil {
				_ = m.deps.Store.Save(context.Background(), m.conversation)
			}
			return m, tea.Quit
		case "y":
			if m.confirmation != nil && m.confirmation.Confirmation != nil {
				m.confirmation.Confirmation.Decision <- events.ToolConfirmationDecision{CallID: m.confirmation.Confirmation.CallID, Allowed: true}
				m.confirmation = nil
				m.status.WaitingConfirmation = false
				return m, nil
			}
		case "n":
			if m.confirmation != nil && m.confirmation.Confirmation != nil {
				m.confirmation.Confirmation.Decision <- events.ToolConfirmationDecision{CallID: m.confirmation.Confirmation.CallID, Allowed: false}
				m.confirmation = nil
				m.status.WaitingConfirmation = false
				return m, nil
			}
		case "enter":
			if m.confirmation != nil {
				return m, nil
			}
			if m.screen == screenList {
				item, ok := m.list.SelectedItem().(tui.ConversationItem)
				if !ok {
					return m, nil
				}
				if item.ID == tui.NewConversationID {
					m.startNewConversation()
				} else {
					m.loadConversation(item.ID)
				}
				return m, nil
			}
			if m.streaming {
				return m, nil
			}
			text := sanitizeInput(m.input.Value())
			if text == "" {
				m.status.Error = nil
				return m, nil
			}
			if m.conversation == nil {
				m.startNewConversation()
			}
			events, err := m.orchestrator.Send(context.Background(), m.conversation, text)
			if err != nil {
				m.status.Error = err
				return m, nil
			}
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
			return m, listen(events)
		}
	}

	if m.screen == screenList {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	if eventMsg, ok := msg.(eventMsg); ok {
		switch eventMsg.event.Type {
		case EventUserSubmitted:
			m.messages.AppendUser(eventMsg.event.Text)
		case EventTextDelta:
			m.messages.AppendAssistantDelta(eventMsg.event.Text)
		case EventThinkingDelta:
			m.messages.AppendThinkingDelta(eventMsg.event.Text)
		case EventToolPending, EventToolRunning, EventToolSuccess, EventToolError, EventToolDenied:
			if eventMsg.event.Tool != nil {
				m.messages.UpsertTool(*eventMsg.event.Tool)
			}
		case EventToolWaitingConfirmation:
			if eventMsg.event.Tool != nil {
				m.messages.UpsertTool(*eventMsg.event.Tool)
			}
			m.confirmation = &eventMsg.event
			m.status.WaitingConfirmation = true
		case EventAgentProgress:
			if eventMsg.event.Progress != nil {
				m.status.AgentIteration = eventMsg.event.Progress.Iteration
				m.status.AgentMaxIterations = eventMsg.event.Progress.Max
				m.status.StopReason = eventMsg.event.Progress.StopReason
				m.status.StopMessage = eventMsg.event.Progress.Message
			}
		case EventUsageUpdated:
			if eventMsg.event.Usage != nil {
				m.status.InputTokens += eventMsg.event.Usage.InputTokens
				m.status.OutputTokens += eventMsg.event.Usage.OutputTokens
				m.status.CacheCreationInputTokens += eventMsg.event.Usage.CacheCreationInputTokens
				m.status.CacheReadInputTokens += eventMsg.event.Usage.CacheReadInputTokens
			}
		case EventDone:
			m.messages.CommitAssistant()
			m.streaming = false
			m.status.Streaming = false
			m.status.WaitingConfirmation = false
			m.status.Duration = formatDuration(eventMsg.event.Duration)
			m.input.SetEnabled(true)
			m.confirmation = nil
			return m, nil
		case EventError:
			m.messages.CommitAssistant()
			m.streaming = false
			m.status.Streaming = false
			m.status.WaitingConfirmation = false
			m.status.Error = eventMsg.event.Err
			m.input.SetEnabled(true)
			m.confirmation = nil
			return m, nil
		}
		return m, listen(eventMsg.events)
	}

	if m.confirmation != nil {
		return m, nil
	}

	var cmd tea.Cmd
	m.input.Text, cmd = m.input.Text.Update(msg)
	return m, cmd
}
