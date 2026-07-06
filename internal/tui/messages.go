package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"xagent/internal/conversation"
	"xagent/internal/events"
)

type MessagesView struct {
	messages        []conversation.Message
	assistantBuffer string
	thinkingBuffer  string
	showThinking    bool
}

func NewMessagesView(showThinking bool) MessagesView {
	return MessagesView{showThinking: showThinking}
}

func (v *MessagesView) SetMessages(messages []conversation.Message) {
	v.messages = append([]conversation.Message(nil), messages...)
}

func (v *MessagesView) AppendUser(text string) {
	v.messages = append(v.messages, conversation.Message{Role: conversation.RoleUser, Content: text})
	v.assistantBuffer = ""
	v.thinkingBuffer = ""
}

func (v *MessagesView) AppendAssistantDelta(text string) {
	v.assistantBuffer += text
}

func (v *MessagesView) AppendThinkingDelta(text string) {
	if v.showThinking {
		v.thinkingBuffer += text
	}
}

func (v *MessagesView) UpsertTool(tool events.ToolDisplay) {
	for index := range v.messages {
		message := &v.messages[index]
		if message.Role == conversation.RoleToolResult && message.ToolCallID == tool.CallID {
			if strings.TrimSpace(tool.Arguments) == "" {
				tool.Arguments = toolArgumentsForCall(v.messages, tool.CallID)
			}
			applyToolDisplay(message, tool)
			return
		}
	}
	v.CommitAssistant()
	v.messages = append(v.messages, toolResultMessage(tool))
}

func (v *MessagesView) CommitAssistant() {
	if v.thinkingBuffer != "" {
		v.messages = append(v.messages, conversation.Message{Role: conversation.RoleThinking, Content: v.thinkingBuffer})
	}
	if v.assistantBuffer != "" {
		v.messages = append(v.messages, conversation.Message{Role: conversation.RoleAssistant, Content: v.assistantBuffer})
	}
	v.assistantBuffer = ""
	v.thinkingBuffer = ""
}

func (v MessagesView) View() string {
	var b strings.Builder
	for _, message := range v.messages {
		if message.Role == conversation.RoleToolCall || message.Role == conversation.RoleToolResult {
			if message.Role == conversation.RoleToolResult {
				b.WriteString(renderToolDisplay(toolDisplayFromHistory(v.messages, message)))
				b.WriteString("\n\n")
			}
			continue
		}
		b.WriteString(renderMessage(message.Role, message.Content))
		b.WriteString("\n\n")
	}
	if v.thinkingBuffer != "" {
		b.WriteString(renderMessage(conversation.RoleThinking, v.thinkingBuffer))
		b.WriteString("\n\n")
	}
	if v.assistantBuffer != "" {
		b.WriteString(renderMessage(conversation.RoleAssistant, v.assistantBuffer))
		b.WriteString("\n\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderMessage(role conversation.MessageRole, content string) string {
	label := string(role)
	switch role {
	case conversation.RoleUser:
		label = "You"
	case conversation.RoleAssistant:
		label = "XAgent"
	case conversation.RoleThinking:
		label = "Thinking"
	}
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Width(88)
	return style.Render(label + "\n" + content)
}

func renderToolDisplay(tool events.ToolDisplay) string {
	line := fmt.Sprintf("● %s(%s)", tool.Name, summarizeArguments(tool.Arguments))
	status := toolStatusText(tool.Status)
	if strings.TrimSpace(tool.Summary) != "" {
		status = tool.Summary
	}
	if strings.TrimSpace(status) != "" {
		line += " — " + status
	}
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	if tool.Status == events.ToolDisplayError || tool.Status == events.ToolDisplayDenied || tool.Status == events.ToolDisplayCancelled {
		style = style.Foreground(lipgloss.Color("9"))
	}
	return style.Render(line)
}

func toolStatusText(status events.ToolDisplayStatus) string {
	switch status {
	case events.ToolDisplayPending:
		return "准备执行"
	case events.ToolDisplayWaitingConfirmation:
		return "等待确认"
	case events.ToolDisplayRunning:
		return "执行中"
	case events.ToolDisplaySuccess:
		return "完成"
	case events.ToolDisplayError:
		return "失败"
	case events.ToolDisplayDenied:
		return "已拒绝"
	case events.ToolDisplayCancelled:
		return "已取消"
	default:
		return ""
	}
}

func summarizeArguments(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	var args map[string]any
	if json.Unmarshal([]byte(text), &args) == nil {
		for _, key := range []string{"path", "command", "pattern"} {
			if value, ok := args[key].(string); ok {
				return value
			}
		}
	}
	if len(text) > 80 {
		return text[:80] + "..."
	}
	return text
}

func toolResultMessage(tool events.ToolDisplay) conversation.Message {
	message := conversation.Message{Role: conversation.RoleToolResult, ToolCallID: tool.CallID, ToolName: tool.Name}
	applyToolDisplay(&message, tool)
	return message
}

func applyToolDisplay(message *conversation.Message, tool events.ToolDisplay) {
	message.ToolName = tool.Name
	message.ToolResultSummary = tool.Summary
	message.ToolResultStatus = string(toolStatusResult(tool.Status))
	message.RawToolArguments = tool.Arguments
	message.Content = tool.Summary
}

func toolArgumentsForCall(messages []conversation.Message, callID string) string {
	for _, message := range messages {
		if message.ToolCallID == callID {
			if strings.TrimSpace(message.RawToolArguments) != "" {
				return message.RawToolArguments
			}
		}
	}
	return ""
}

func toolStatusResult(status events.ToolDisplayStatus) string {
	switch status {
	case events.ToolDisplaySuccess:
		return "success"
	case events.ToolDisplayError:
		return "error"
	case events.ToolDisplayDenied:
		return "denied"
	case events.ToolDisplayCancelled:
		return "cancelled"
	default:
		return string(status)
	}
}

func toolDisplayStatus(status string) events.ToolDisplayStatus {
	switch status {
	case "pending":
		return events.ToolDisplayPending
	case "waiting_confirmation":
		return events.ToolDisplayWaitingConfirmation
	case "running":
		return events.ToolDisplayRunning
	case "error":
		return events.ToolDisplayError
	case "denied":
		return events.ToolDisplayDenied
	case "cancelled":
		return events.ToolDisplayCancelled
	default:
		return events.ToolDisplaySuccess
	}
}

func toolDisplayFromHistory(messages []conversation.Message, result conversation.Message) events.ToolDisplay {
	display := events.ToolDisplay{
		CallID:    result.ToolCallID,
		Name:      result.ToolName,
		Arguments: result.RawToolArguments,
		Summary:   result.ToolResultSummary,
		Status:    toolDisplayStatus(result.ToolResultStatus),
	}
	for _, message := range messages {
		if message.Role == conversation.RoleToolCall && message.ToolCallID == result.ToolCallID {
			display.Arguments = message.RawToolArguments
			break
		}
	}
	return display
}
