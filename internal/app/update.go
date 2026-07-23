package app

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/memory"
	"xagent/internal/redact"
	"xagent/internal/tui"
)

func (m *Model) sendConfirmation(action events.PermissionAction, allowed bool) {
	m.confirmation.Confirmation.Decision <- events.ToolConfirmationDecision{CallID: m.confirmation.Confirmation.CallID, Allowed: allowed, Action: action}
	m.confirmation = nil
	m.status.WaitingConfirmation = false
}

func (m *Model) cancelRequest(notice string) {
	if m.lifecycle == nil {
		m.lifecycle = newLifecycleState()
		if m.request != nil {
			m.lifecycle.begin(m.request)
		}
	}
	if !m.lifecycle.cancel() {
		return
	}
	m.status.Notice = m.redactText(notice)
	m.status.Error = nil
	m.status.Streaming = true
	m.input.SetEnabled(false)
}

func parseMemoryScope(value string) (memory.Scope, error) {
	switch strings.TrimSpace(value) {
	case string(memory.ScopeUser):
		return memory.ScopeUser, nil
	case string(memory.ScopeProject):
		return memory.ScopeProject, nil
	default:
		return "", fmt.Errorf("memory scope 必须是 user 或 project")
	}
}

func formatMemoryStatus(status memory.Status) string {
	parts := []string{fmt.Sprintf("memory 状态: user=%s project=%s", enabledText(!status.UserDisabled), enabledText(!status.ProjectDisabled))}
	if strings.TrimSpace(status.UserDir) != "" {
		parts = append(parts, "user_dir="+status.UserDir)
	}
	if strings.TrimSpace(status.ProjectDir) != "" {
		parts = append(parts, "project_dir="+status.ProjectDir)
	}
	if diagnostics := formatMemoryDiagnostics(status.Diagnostics); diagnostics != "" {
		parts = append(parts, diagnostics)
	}
	return redact.Text(strings.Join(parts, " | "))
}

func formatMemoryIndex(index memory.Index) string {
	if len(index.Entries) == 0 {
		return fmt.Sprintf("memory %s 索引为空", index.Scope)
	}
	items := make([]string, 0, len(index.Entries))
	for _, entry := range index.Entries {
		items = append(items, fmt.Sprintf("%s(%s)", entry.Title, entry.ID))
	}
	return redact.Text(fmt.Sprintf("memory %s 索引: %d 条 | %s", index.Scope, len(index.Entries), strings.Join(items, ", ")))
}

func formatMemoryDiagnostics(items []diagnostics.Diagnostic) string {
	if len(items) == 0 {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if text := item.Safe(redact.Text).Text(); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "diagnostics=" + strings.Join(parts, "; ")
}

func enabledText(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.lifecycle == nil {
		m.lifecycle = newLifecycleState()
		if m.request != nil {
			m.lifecycle.begin(m.request)
		}
	}
	request, _, _ := m.lifecycle.active()
	m.request = request
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.streaming && (msg.String() == "esc" || msg.String() == "ctrl+c" || msg.String() == "q") {
			m.cancelRequest("正在取消请求，等待当前轮次收尾")
			return m, nil
		}
		if m.confirmation == nil && m.commandMenu.Visible {
			switch msg.String() {
			case "up":
				m.commandMenu.Move(-1)
				return m, nil
			case "down":
				m.commandMenu.Move(1)
				return m, nil
			case "enter":
				m.acceptCommandCompletion()
				return m, nil
			case "esc":
				m.commandMenu.Close()
				return m, nil
			default:
				m.commandMenu.Close()
			}
		}
		if m.confirmation == nil && m.screen == screenChat && !m.streaming && msg.String() == "tab" {
			m.completeCommand()
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "y":
			if m.confirmation != nil && m.confirmation.Confirmation != nil {
				m.sendConfirmation(events.PermissionAllowOnce, true)
				return m, nil
			}
		case "s":
			if m.confirmation != nil && m.confirmation.Confirmation != nil {
				m.sendConfirmation(events.PermissionAllowSession, true)
				return m, nil
			}
		case "p":
			if m.confirmation != nil && m.confirmation.Confirmation != nil && m.confirmation.Confirmation.AllowPermanent {
				m.sendConfirmation(events.PermissionAllowPermanent, true)
				return m, nil
			}
			if m.confirmation != nil && m.confirmation.Confirmation != nil {
				return m, nil
			}
		case "n":
			if m.confirmation != nil && m.confirmation.Confirmation != nil {
				m.sendConfirmation(events.PermissionDeny, false)
				return m, nil
			}
		case "esc":
			if m.confirmation != nil && m.confirmation.Confirmation != nil {
				m.sendConfirmation(events.PermissionCancel, false)
				return m, nil
			}
		case "enter":
			if m.confirmation != nil && m.confirmation.Confirmation != nil {
				m.sendConfirmation(events.PermissionDeny, false)
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
			return m, m.dispatchInput()
		}
	}

	if m.screen == screenList {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	if _, ok := msg.(eventStreamClosedMsg); ok {
		m.finishRequestAfterStream()
		return m, nil
	}

	if eventMsg, ok := msg.(eventMsg); ok {
		if eventMsg.event.Transient {
			m.trackTransientID(eventMsg.event.IndependentID)
		}
		switch eventMsg.event.Type {
		case EventUserSubmitted:
			if !eventMsg.event.Transient {
				m.messages.AppendUser(eventMsg.event.Text)
			}
		case EventTextDelta:
			if eventMsg.event.Transient {
				m.messages.AppendTransientAssistantDelta(eventMsg.event.IndependentID, eventMsg.event.Text)
			} else {
				if m.request != nil && m.request.Independent {
					m.clearRequestTransient()
				}
				m.messages.AppendAssistantDelta(eventMsg.event.Text)
			}
		case EventThinkingDelta:
			if eventMsg.event.Transient {
				m.messages.AppendTransientThinkingDelta(eventMsg.event.IndependentID, eventMsg.event.Text)
			} else {
				m.messages.AppendThinkingDelta(eventMsg.event.Text)
			}
		case EventToolPending, EventToolRunning, EventToolSuccess, EventToolError, EventToolDenied:
			if eventMsg.event.Tool != nil {
				if eventMsg.event.Transient {
					m.messages.UpsertTransientTool(eventMsg.event.IndependentID, *eventMsg.event.Tool)
				} else {
					m.messages.UpsertTool(*eventMsg.event.Tool)
				}
			}
		case EventToolWaitingConfirmation:
			if eventMsg.event.Tool != nil {
				if eventMsg.event.Transient {
					m.messages.UpsertTransientTool(eventMsg.event.IndependentID, *eventMsg.event.Tool)
				} else {
					m.messages.UpsertTool(*eventMsg.event.Tool)
				}
			}
			m.confirmation = &eventMsg.event
			m.status.WaitingConfirmation = true
		case EventAgentProgress:
			if eventMsg.event.Progress != nil {
				m.status.AgentIteration = eventMsg.event.Progress.Iteration
				m.status.AgentMaxIterations = eventMsg.event.Progress.Max
				m.status.StopReason = eventMsg.event.Progress.StopReason
				m.status.StopMessage = m.redactText(eventMsg.event.Progress.Message)
			}
		case EventUsageUpdated:
			if eventMsg.event.Usage != nil {
				m.status.InputTokens += eventMsg.event.Usage.InputTokens
				m.status.OutputTokens += eventMsg.event.Usage.OutputTokens
				m.status.CacheCreationInputTokens += eventMsg.event.Usage.CacheCreationInputTokens
				m.status.CacheReadInputTokens += eventMsg.event.Usage.CacheReadInputTokens
			}
		case EventMainTraceReset:
			if m.conversation != nil {
				m.messages.SetMessages(m.conversation.Messages)
			}
		case EventDone:
			m.messages.CommitAssistant()
			m.status.Duration = formatDuration(eventMsg.event.Duration)
			m.lifecycle.markTerminal()
			return m, listen(eventMsg.events)
		case EventError:
			m.messages.CommitAssistant()
			safe := m.redactError(eventMsg.event.Err)
			m.status.Error = safe
			m.lastError = safe
			m.lifecycle.markTerminal()
			return m, listen(eventMsg.events)
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

func (m *Model) trackTransientID(independentID string) {
	if m.request == nil {
		return
	}
	for _, existing := range m.request.TransientIDs {
		if existing == independentID {
			return
		}
	}
	request := *m.request
	request.TransientIDs = append(append([]string(nil), m.request.TransientIDs...), independentID)
	m.request = &request
	if m.lifecycle != nil {
		m.lifecycle.replace(m.request)
	}
}

func (m *Model) clearRequestTransient() {
	if m.request == nil {
		return
	}
	for _, independentID := range m.request.TransientIDs {
		m.messages.ClearTransient(independentID)
	}
	if len(m.request.TransientIDs) > 0 {
		request := *m.request
		request.TransientIDs = nil
		m.request = &request
		if m.lifecycle != nil {
			m.lifecycle.replace(m.request)
		}
	}
}

func (m *Model) finishRequestAfterStream() {
	if m.orchestrator != nil {
		_ = m.orchestrator.WaitIdle(context.Background())
	}
	m.clearRequestTransient()
	if m.lifecycle != nil {
		m.lifecycle.finish()
	}
	m.request = nil
	m.streaming = false
	m.status.Streaming = false
	m.status.WaitingConfirmation = false
	m.status.RequestModel = ""
	m.syncSkillStatus()
	m.input.SetEnabled(true)
	m.confirmation = nil
}

func compactNotice(changed bool, externalized int, summarized bool) string {
	if summarized && externalized > 0 {
		return "上下文已压缩，并外置大型工具结果"
	}
	if summarized {
		return "上下文已压缩"
	}
	if externalized > 0 {
		return "大型工具结果已外置"
	}
	if changed {
		return "上下文已更新"
	}
	return "当前上下文无需压缩"
}
