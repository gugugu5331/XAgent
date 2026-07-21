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
	transient       []transientTrace
	showThinking    bool
}

type transientTrace struct {
	independentID   string
	entries         []transientEntry
	assistantBuffer string
	thinkingBuffer  string
}

type transientEntry struct {
	role    conversation.MessageRole
	content string
	tool    events.ToolDisplay
	isTool  bool
}

func NewMessagesView(showThinking bool) MessagesView {
	return MessagesView{showThinking: showThinking}
}

func (v *MessagesView) SetMessages(messages []conversation.Message) {
	v.messages = append([]conversation.Message(nil), messages...)
	v.assistantBuffer = ""
	v.thinkingBuffer = ""
	v.transient = nil
}

func (v *MessagesView) Clear() {
	v.messages = nil
	v.assistantBuffer = ""
	v.thinkingBuffer = ""
	v.transient = nil
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

// AppendTransientAssistantDelta appends streamed assistant text to an
// independent execution trace without adding it to persistent messages.
func (v *MessagesView) AppendTransientAssistantDelta(independentID string, text string) {
	trace := v.transientTrace(independentID)
	trace.assistantBuffer += text
}

// AppendTransientThinkingDelta appends streamed thinking to an independent
// execution trace when thinking output is enabled.
func (v *MessagesView) AppendTransientThinkingDelta(independentID string, text string) {
	if !v.showThinking {
		return
	}
	trace := v.transientTrace(independentID)
	trace.thinkingBuffer += text
}

// UpsertTransientTool updates a temporary tool row by CallID. The row is
// rendered live but never converted into a conversation message.
func (v *MessagesView) UpsertTransientTool(independentID string, tool events.ToolDisplay) {
	trace := v.transientTrace(independentID)
	for index := range trace.entries {
		entry := &trace.entries[index]
		if !entry.isTool || entry.tool.CallID != tool.CallID {
			continue
		}
		if strings.TrimSpace(tool.Name) == "" {
			tool.Name = entry.tool.Name
		}
		if strings.TrimSpace(tool.Arguments) == "" {
			tool.Arguments = entry.tool.Arguments
		}
		entry.tool = tool
		return
	}
	commitTransientTrace(trace)
	trace.entries = append(trace.entries, transientEntry{tool: tool, isTool: true})
}

// ClearTransient removes the live trace for one independent execution. It
// does not alter persistent messages or the main assistant buffers.
func (v *MessagesView) ClearTransient(independentID string) {
	if len(v.transient) == 0 {
		return
	}
	v.cloneTransient()
	for index := range v.transient {
		if v.transient[index].independentID != independentID {
			continue
		}
		copy(v.transient[index:], v.transient[index+1:])
		v.transient[len(v.transient)-1] = transientTrace{}
		v.transient = v.transient[:len(v.transient)-1]
		return
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
	for _, trace := range v.transient {
		for _, entry := range trace.entries {
			if entry.isTool {
				b.WriteString(renderToolDisplay(entry.tool))
			} else {
				b.WriteString(renderMessage(entry.role, entry.content))
			}
			b.WriteString("\n\n")
		}
		if trace.thinkingBuffer != "" {
			b.WriteString(renderMessage(conversation.RoleThinking, trace.thinkingBuffer))
			b.WriteString("\n\n")
		}
		if trace.assistantBuffer != "" {
			b.WriteString(renderMessage(conversation.RoleAssistant, trace.assistantBuffer))
			b.WriteString("\n\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (v *MessagesView) transientTrace(independentID string) *transientTrace {
	v.cloneTransient()
	for index := range v.transient {
		if v.transient[index].independentID == independentID {
			return &v.transient[index]
		}
	}
	v.transient = append(v.transient, transientTrace{independentID: independentID})
	return &v.transient[len(v.transient)-1]
}

func (v *MessagesView) cloneTransient() {
	if len(v.transient) == 0 {
		return
	}
	traces := make([]transientTrace, len(v.transient))
	copy(traces, v.transient)
	for index := range traces {
		traces[index].entries = append([]transientEntry(nil), traces[index].entries...)
	}
	v.transient = traces
}

func commitTransientTrace(trace *transientTrace) {
	if trace.thinkingBuffer != "" {
		trace.entries = append(trace.entries, transientEntry{role: conversation.RoleThinking, content: trace.thinkingBuffer})
	}
	if trace.assistantBuffer != "" {
		trace.entries = append(trace.entries, transientEntry{role: conversation.RoleAssistant, content: trace.assistantBuffer})
	}
	trace.assistantBuffer = ""
	trace.thinkingBuffer = ""
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
	if detail := renderToolDetails(tool); detail != "" {
		line += "\n" + detail
	}
	style := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	if tool.Status == events.ToolDisplayError || tool.Status == events.ToolDisplayDenied || tool.Status == events.ToolDisplayCancelled {
		style = style.Foreground(lipgloss.Color("9"))
	}
	return style.Render(line)
}

func renderToolDetails(tool events.ToolDisplay) string {
	var details []string
	if strings.TrimSpace(tool.ErrorCode) != "" {
		details = append(details, "error: "+tool.ErrorCode)
	}
	if tool.Truncated {
		details = append(details, "output truncated")
	}
	if tool.Recoverable {
		details = append(details, "recoverable")
	}
	if strings.TrimSpace(tool.Stdout) != "" {
		details = append(details, "stdout: "+previewText(tool.Stdout, 160))
	}
	if strings.TrimSpace(tool.Stderr) != "" {
		details = append(details, "stderr: "+previewText(tool.Stderr, 160))
	}
	if tool.ArtifactAvailable && strings.TrimSpace(tool.ArtifactID) != "" {
		details = append(details, fmt.Sprintf("artifact: %s (%d bytes)", tool.ArtifactID, tool.ArtifactBytes))
	}
	if len(details) == 0 {
		return ""
	}
	return "  " + strings.Join(details, "\n  ")
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

func previewText(value string, limit int) string {
	if len([]rune(value)) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "..."
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
	message.ToolErrorCode = tool.ErrorCode
	message.ToolResultTruncated = tool.Truncated
	message.ToolResultData = toolDisplayData(tool)
	message.ToolResultError = toolDisplayError(tool)
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
		CallID:            result.ToolCallID,
		Name:              result.ToolName,
		Arguments:         result.RawToolArguments,
		Summary:           result.ToolResultSummary,
		Status:            toolDisplayStatus(result.ToolResultStatus),
		ErrorCode:         result.ToolErrorCode,
		Truncated:         result.ToolResultTruncated,
		Stdout:            stringToolData(result.ToolResultData, "stdout"),
		Stderr:            stringToolData(result.ToolResultData, "stderr"),
		ArtifactID:        stringToolData(result.ToolResultData, "artifact_id"),
		ArtifactBytes:     int64ToolData(result.ToolResultData, "artifact_bytes"),
		ArtifactAvailable: boolToolData(result.ToolResultData, "artifact_available"),
		Recoverable:       recoverableToolError(result.ToolResultError),
	}
	for _, message := range messages {
		if message.Role == conversation.RoleToolCall && message.ToolCallID == result.ToolCallID {
			display.Arguments = message.RawToolArguments
			break
		}
	}
	return display
}

func toolDisplayData(tool events.ToolDisplay) json.RawMessage {
	data := map[string]any{}
	if strings.TrimSpace(tool.Stdout) != "" {
		data["stdout"] = tool.Stdout
	}
	if strings.TrimSpace(tool.Stderr) != "" {
		data["stderr"] = tool.Stderr
	}
	if strings.TrimSpace(tool.ArtifactID) != "" {
		data["artifact_id"] = tool.ArtifactID
	}
	if tool.ArtifactBytes > 0 {
		data["artifact_bytes"] = tool.ArtifactBytes
	}
	if tool.ArtifactAvailable {
		data["artifact_available"] = true
	}
	if len(data) == 0 {
		return nil
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	return encoded
}

func toolDisplayError(tool events.ToolDisplay) json.RawMessage {
	if strings.TrimSpace(tool.ErrorCode) == "" && !tool.Recoverable {
		return nil
	}
	encoded, err := json.Marshal(map[string]any{"code": tool.ErrorCode, "recoverable": tool.Recoverable})
	if err != nil {
		return nil
	}
	return encoded
}

func stringToolData(raw json.RawMessage, key string) string {
	var data map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &data) != nil {
		return ""
	}
	value, _ := data[key].(string)
	return value
}

func boolToolData(raw json.RawMessage, key string) bool {
	var data map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &data) != nil {
		return false
	}
	value, _ := data[key].(bool)
	return value
}

func int64ToolData(raw json.RawMessage, key string) int64 {
	var data map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &data) != nil {
		return 0
	}
	switch value := data[key].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	default:
		return 0
	}
}

func recoverableToolError(raw json.RawMessage) bool {
	var data map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &data) != nil {
		return false
	}
	value, _ := data["recoverable"].(bool)
	return value
}
