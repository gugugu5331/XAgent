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

func (m *Model) handleMemoryCommand(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 || fields[0] != "/memory" {
		return false
	}
	if m.deps.Memory == nil {
		m.setMemoryError(fmt.Errorf("memory 管理器未启用"))
		return true
	}
	if len(fields) == 1 {
		m.setMemoryNotice("用法: /memory status|index|off|delete|rebuild")
		return true
	}
	switch fields[1] {
	case "status":
		m.setMemoryNotice(formatMemoryStatus(m.deps.Memory.Status()))
	case "index":
		scope := memory.ScopeProject
		if len(fields) > 2 {
			parsed, err := parseMemoryScope(fields[2])
			if err != nil {
				m.setMemoryError(err)
				return true
			}
			scope = parsed
		}
		index, err := m.deps.Memory.LoadIndex(scope)
		if err != nil {
			m.setMemoryError(err)
			return true
		}
		m.setMemoryNotice(formatMemoryIndex(index))
	case "off":
		scope := memory.ScopeProject
		if len(fields) > 2 {
			parsed, err := parseMemoryScope(fields[2])
			if err != nil {
				m.setMemoryError(err)
				return true
			}
			scope = parsed
		}
		m.deps.Memory.Disable(scope)
		m.setMemoryNotice(fmt.Sprintf("memory %s 自动记忆已关闭", scope))
	case "delete":
		if len(fields) != 4 {
			m.setMemoryError(fmt.Errorf("用法: /memory delete <user|project> <id>"))
			return true
		}
		scope, err := parseMemoryScope(fields[2])
		if err != nil {
			m.setMemoryError(err)
			return true
		}
		if err := m.deps.Memory.DeleteNote(scope, fields[3]); err != nil {
			m.setMemoryError(err)
			return true
		}
		m.setMemoryNotice(fmt.Sprintf("memory %s 记忆 %s 已删除", scope, redact.Text(fields[3])))
	case "rebuild":
		if len(fields) != 3 {
			m.setMemoryError(fmt.Errorf("用法: /memory rebuild <user|project>"))
			return true
		}
		scope, err := parseMemoryScope(fields[2])
		if err != nil {
			m.setMemoryError(err)
			return true
		}
		index, err := m.deps.Memory.RebuildIndex(scope)
		if err != nil {
			m.setMemoryError(err)
			return true
		}
		m.setMemoryNotice(fmt.Sprintf("memory %s 索引已重建，%d 条", scope, len(index.Entries)))
	default:
		m.setMemoryError(fmt.Errorf("未知 memory 命令: %s", redact.Text(fields[1])))
	}
	return true
}

func (m *Model) setMemoryNotice(message string) {
	m.status.Notice = redact.Text(message)
	m.status.Error = nil
}

func (m *Model) setMemoryError(err error) {
	m.status.Notice = ""
	if err == nil {
		m.status.Error = nil
		return
	}
	m.status.Error = fmt.Errorf("%s", redact.Text(err.Error()))
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
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.close()
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
			text := sanitizeInput(m.input.Value())
			if text == "" {
				m.status.Error = nil
				return m, nil
			}
			if text == "/compact" {
				if m.conversation == nil {
					m.status.Notice = "当前没有会话需要压缩"
					m.status.Error = nil
					m.input.Clear()
					return m, nil
				}
				result, err := m.orchestrator.CompactContext(context.Background(), m.conversation)
				if err != nil {
					m.status.Error = err
					m.status.Notice = ""
					return m, nil
				}
				m.messages.SetMessages(m.conversation.Messages)
				m.status.Notice = compactNotice(result.Changed, result.Externalized, result.Summarized)
				m.status.Error = nil
				m.input.Clear()
				return m, nil
			}
			if strings.HasPrefix(text, "/memory") && m.handleMemoryCommand(text) {
				m.input.Clear()
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
			m.status.Notice = ""
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
