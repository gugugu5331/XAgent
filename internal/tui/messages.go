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
	tools           []events.ToolDisplay
	assistantBuffer strings.Builder
	thinkingBuffer  strings.Builder
	showThinking    bool
}

func NewMessagesView(showThinking bool) MessagesView {
	return MessagesView{showThinking: showThinking}
}

func (v *MessagesView) SetMessages(messages []conversation.Message) {
	v.messages = append([]conversation.Message(nil), messages...)
	v.tools = nil
}

func (v *MessagesView) AppendUser(text string) {
	v.messages = append(v.messages, conversation.Message{Role: conversation.RoleUser, Content: text})
	v.assistantBuffer.Reset()
	v.thinkingBuffer.Reset()
}

func (v *MessagesView) AppendAssistantDelta(text string) {
	v.assistantBuffer.WriteString(text)
}

func (v *MessagesView) AppendThinkingDelta(text string) {
	if v.showThinking {
		v.thinkingBuffer.WriteString(text)
	}
}

func (v *MessagesView) UpsertTool(tool events.ToolDisplay) {
	for index := range v.tools {
		if v.tools[index].CallID == tool.CallID {
			if strings.TrimSpace(tool.Arguments) == "" {
				tool.Arguments = v.tools[index].Arguments
			}
			v.tools[index] = tool
			return
		}
	}
	v.tools = append(v.tools, tool)
}

func (v *MessagesView) CommitAssistant() {
	if v.thinkingBuffer.Len() > 0 {
		v.messages = append(v.messages, conversation.Message{Role: conversation.RoleThinking, Content: v.thinkingBuffer.String()})
	}
	if v.assistantBuffer.Len() > 0 {
		v.messages = append(v.messages, conversation.Message{Role: conversation.RoleAssistant, Content: v.assistantBuffer.String()})
	}
	v.assistantBuffer.Reset()
	v.thinkingBuffer.Reset()
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
	for _, tool := range v.tools {
		b.WriteString(renderToolDisplay(tool))
		b.WriteString("\n\n")
	}
	if v.thinkingBuffer.Len() > 0 {
		b.WriteString(renderMessage(conversation.RoleThinking, v.thinkingBuffer.String()))
		b.WriteString("\n\n")
	}
	if v.assistantBuffer.Len() > 0 {
		b.WriteString(renderMessage(conversation.RoleAssistant, v.assistantBuffer.String()))
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
	if strings.TrimSpace(tool.Summary) != "" {
		line += " — " + tool.Summary
	}
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	if tool.Status == events.ToolDisplayError || tool.Status == events.ToolDisplayDenied {
		style = style.Foreground(lipgloss.Color("9"))
	}
	return style.Render(line)
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

func toolDisplayFromHistory(messages []conversation.Message, result conversation.Message) events.ToolDisplay {
	display := events.ToolDisplay{
		CallID:  result.ToolCallID,
		Name:    result.ToolName,
		Summary: result.ToolResultSummary,
		Status:  events.ToolDisplaySuccess,
	}
	if result.ToolResultStatus != "success" {
		display.Status = events.ToolDisplayError
	}
	if result.ToolResultStatus == "denied" {
		display.Status = events.ToolDisplayDenied
	}
	for _, message := range messages {
		if message.Role == conversation.RoleToolCall && message.ToolCallID == result.ToolCallID {
			display.Arguments = message.RawToolArguments
			break
		}
	}
	return display
}
