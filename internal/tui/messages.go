package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

type MessagesView struct {
	messages        []renderedMessage
	assistantBuffer string
	thinkingBuffer  string
	transient       []transientTrace
	showThinking    bool
	viewport        viewport.Model
	viewportSet     bool
	renderWidth     int
}

type renderedMessage struct {
	role    string
	content redact.SafeText
	tool    events.ToolDisplay
	isTool  bool
}

type transientTrace struct {
	independentID   string
	entries         []transientEntry
	assistantBuffer string
	thinkingBuffer  string
}

type transientEntry struct {
	role    string
	content string
	tool    events.ToolDisplay
	isTool  bool
}

func NewMessagesView(showThinking bool) MessagesView {
	return MessagesView{showThinking: showThinking, renderWidth: 88}
}

func (v *MessagesView) SetMessages(messages []conversation.Message) {
	v.messages = projectConversationMessages(messages)
	v.assistantBuffer = ""
	v.thinkingBuffer = ""
	v.transient = nil
	v.refreshViewport(true)
}

func (v *MessagesView) Clear() {
	v.messages = nil
	v.assistantBuffer = ""
	v.thinkingBuffer = ""
	v.transient = nil
	v.refreshViewport(true)
}

func (v *MessagesView) AppendUser(text string) {
	follow := v.shouldFollowBottom()
	v.cloneMessages()
	v.messages = append(v.messages, renderedMessage{role: string(conversation.RoleUser), content: safeDisplayText(text)})
	v.assistantBuffer = ""
	v.thinkingBuffer = ""
	v.refreshViewport(follow)
}

func (v *MessagesView) AppendAssistantDelta(text string) {
	follow := v.shouldFollowBottom()
	v.assistantBuffer += text
	v.refreshViewport(follow)
}

func (v *MessagesView) AppendThinkingDelta(text string) {
	if v.showThinking {
		follow := v.shouldFollowBottom()
		v.thinkingBuffer += text
		v.refreshViewport(follow)
	}
}

// AppendTransientAssistantDelta appends streamed assistant text to an
// independent execution trace without adding it to persistent messages.
func (v *MessagesView) AppendTransientAssistantDelta(independentID string, text string) {
	follow := v.shouldFollowBottom()
	trace := v.transientTrace(independentID)
	trace.assistantBuffer += text
	v.refreshViewport(follow)
}

// AppendTransientThinkingDelta appends streamed thinking to an independent
// execution trace when thinking output is enabled.
func (v *MessagesView) AppendTransientThinkingDelta(independentID string, text string) {
	if !v.showThinking {
		return
	}
	follow := v.shouldFollowBottom()
	trace := v.transientTrace(independentID)
	trace.thinkingBuffer += text
	v.refreshViewport(follow)
}

// UpsertTransientTool updates a temporary tool row by CallID. The row is
// rendered live but never converted into a conversation message.
func (v *MessagesView) UpsertTransientTool(independentID string, tool events.ToolDisplay) {
	follow := v.shouldFollowBottom()
	tool = cloneToolDisplay(tool)
	trace := v.transientTrace(independentID)
	for index := range trace.entries {
		entry := &trace.entries[index]
		if !entry.isTool || entry.tool.CallID != tool.CallID {
			continue
		}
		if strings.TrimSpace(tool.Name) == "" {
			tool.Name = entry.tool.Name
		}
		if strings.TrimSpace(tool.Arguments.Text()) == "" {
			tool.Arguments = entry.tool.Arguments
		}
		entry.tool = tool
		v.refreshViewport(follow)
		return
	}
	commitTransientTrace(trace)
	trace.entries = append(trace.entries, transientEntry{tool: tool, isTool: true})
	v.refreshViewport(follow)
}

// ClearTransient removes the live trace for one independent execution. It
// does not alter persistent messages or the main assistant buffers.
func (v *MessagesView) ClearTransient(independentID string) {
	if len(v.transient) == 0 {
		return
	}
	follow := v.shouldFollowBottom()
	v.cloneTransient()
	for index := range v.transient {
		if v.transient[index].independentID != independentID {
			continue
		}
		copy(v.transient[index:], v.transient[index+1:])
		v.transient[len(v.transient)-1] = transientTrace{}
		v.transient = v.transient[:len(v.transient)-1]
		v.refreshViewport(follow)
		return
	}
}

func (v *MessagesView) UpsertTool(tool events.ToolDisplay) {
	follow := v.shouldFollowBottom()
	tool = cloneToolDisplay(tool)
	v.cloneMessages()
	for index := range v.messages {
		message := &v.messages[index]
		if message.isTool && message.tool.CallID == tool.CallID {
			if strings.TrimSpace(tool.Name) == "" {
				tool.Name = message.tool.Name
			}
			if strings.TrimSpace(tool.Arguments.Text()) == "" {
				tool.Arguments = message.tool.Arguments
			}
			message.tool = tool
			v.refreshViewport(follow)
			return
		}
	}
	v.commitAssistantBuffers()
	v.messages = append(v.messages, renderedMessage{tool: tool, isTool: true})
	v.refreshViewport(follow)
}

// UpsertToolResult accepts only the projected UserView for result-bearing
// fields. The preview and artifact metadata remain in display-only state and
// are never copied into a conversation.Message.
func (v *MessagesView) UpsertToolResult(callID, name string, arguments redact.SafeText, view tool.UserView) {
	v.UpsertTool(events.ToolResultDisplayFromUserView(callID, name, arguments, view))
}

func (v *MessagesView) CommitAssistant() {
	follow := v.shouldFollowBottom()
	v.commitAssistantBuffers()
	v.refreshViewport(follow)
}

func (v *MessagesView) commitAssistantBuffers() {
	v.cloneMessages()
	if v.thinkingBuffer != "" {
		v.messages = append(v.messages, renderedMessage{role: string(conversation.RoleThinking), content: safeDisplayText(v.thinkingBuffer)})
	}
	if v.assistantBuffer != "" {
		v.messages = append(v.messages, renderedMessage{role: string(conversation.RoleAssistant), content: safeDisplayText(v.assistantBuffer)})
	}
	v.assistantBuffer = ""
	v.thinkingBuffer = ""
}

func (v *MessagesView) cloneMessages() {
	if len(v.messages) == 0 {
		return
	}
	messages := append([]renderedMessage(nil), v.messages...)
	for index := range messages {
		if messages[index].isTool {
			messages[index].tool = cloneToolDisplay(messages[index].tool)
		}
	}
	v.messages = messages
}

func (v MessagesView) View() string {
	if v.viewportSet {
		if v.viewport.Width <= 0 || v.viewport.Height <= 0 {
			return ""
		}
		return v.viewport.View()
	}
	return v.renderContent()
}

func (v MessagesView) renderContent() string {
	var b strings.Builder
	for _, message := range v.messages {
		if message.isTool {
			b.WriteString(renderToolDisplay(message.tool, v.renderWidth))
			b.WriteString("\n\n")
			continue
		}
		b.WriteString(renderMessage(message.role, message.content.Text(), v.renderWidth))
		b.WriteString("\n\n")
	}
	if v.thinkingBuffer != "" {
		b.WriteString(renderMessage(string(conversation.RoleThinking), v.thinkingBuffer, v.renderWidth))
		b.WriteString("\n\n")
	}
	if v.assistantBuffer != "" {
		b.WriteString(renderMessage(string(conversation.RoleAssistant), v.assistantBuffer, v.renderWidth))
		b.WriteString("\n\n")
	}
	for _, trace := range v.transient {
		for _, entry := range trace.entries {
			if entry.isTool {
				b.WriteString(renderToolDisplay(entry.tool, v.renderWidth))
			} else {
				b.WriteString(renderMessage(entry.role, entry.content, v.renderWidth))
			}
			b.WriteString("\n\n")
		}
		if trace.thinkingBuffer != "" {
			b.WriteString(renderMessage(string(conversation.RoleThinking), trace.thinkingBuffer, v.renderWidth))
			b.WriteString("\n\n")
		}
		if trace.assistantBuffer != "" {
			b.WriteString(renderMessage(string(conversation.RoleAssistant), trace.assistantBuffer, v.renderWidth))
			b.WriteString("\n\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// SetRegion applies the unified layout region. A user-controlled top-line
// offset is retained; a bottom anchor remains pinned while text reflows.
func (v *MessagesView) SetRegion(region Region) {
	follow := v.shouldFollowBottom()
	offset := v.viewport.YOffset
	v.viewport.Width = nonNegative(region.Width)
	v.viewport.Height = nonNegative(region.Height)
	v.viewport.YPosition = nonNegative(region.Y)
	v.renderWidth = nonNegative(region.Width)
	v.viewportSet = true
	v.viewport.SetContent(v.renderContent())
	if follow {
		v.viewport.GotoBottom()
	} else {
		v.viewport.SetYOffset(offset)
	}
}

func (v *MessagesView) Update(msg tea.Msg) tea.Cmd {
	if !v.viewportSet {
		return nil
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.Type {
		case tea.KeyHome:
			v.viewport.GotoTop()
			return nil
		case tea.KeyEnd:
			v.viewport.GotoBottom()
			return nil
		}
	}
	var cmd tea.Cmd
	v.viewport, cmd = v.viewport.Update(msg)
	return cmd
}

func (v MessagesView) AtBottom() bool {
	return !v.viewportSet || v.viewport.AtBottom()
}

func (v MessagesView) YOffset() int {
	if !v.viewportSet {
		return 0
	}
	return v.viewport.YOffset
}

func (v MessagesView) TotalLineCount() int {
	if !v.viewportSet {
		return strings.Count(v.renderContent(), "\n") + 1
	}
	return v.viewport.TotalLineCount()
}

func (v MessagesView) VisibleLineCount() int {
	if !v.viewportSet {
		return v.TotalLineCount()
	}
	return v.viewport.VisibleLineCount()
}

func (v MessagesView) shouldFollowBottom() bool {
	return !v.viewportSet || v.viewport.AtBottom()
}

func (v *MessagesView) refreshViewport(followBottom bool) {
	if !v.viewportSet {
		return
	}
	offset := v.viewport.YOffset
	v.viewport.SetContent(v.renderContent())
	if followBottom {
		v.viewport.GotoBottom()
	} else {
		v.viewport.SetYOffset(offset)
	}
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
		for entryIndex := range traces[index].entries {
			if traces[index].entries[entryIndex].isTool {
				traces[index].entries[entryIndex].tool = cloneToolDisplay(traces[index].entries[entryIndex].tool)
			}
		}
	}
	v.transient = traces
}

func commitTransientTrace(trace *transientTrace) {
	if trace.thinkingBuffer != "" {
		trace.entries = append(trace.entries, transientEntry{role: string(conversation.RoleThinking), content: trace.thinkingBuffer})
	}
	if trace.assistantBuffer != "" {
		trace.entries = append(trace.entries, transientEntry{role: string(conversation.RoleAssistant), content: trace.assistantBuffer})
	}
	trace.assistantBuffer = ""
	trace.thinkingBuffer = ""
}

func renderMessage(role string, content string, width int) string {
	if width <= 0 {
		return ""
	}
	label := role
	switch role {
	case string(conversation.RoleUser):
		label = "You"
	case string(conversation.RoleAssistant):
		label = "XAgent"
	case string(conversation.RoleThinking):
		label = "Thinking"
	}
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	frameWidth := style.GetHorizontalFrameSize()
	if width <= frameWidth {
		return ansi.Truncate(label+" "+content, width, "")
	}
	innerWidth := width - frameWidth
	return style.Width(innerWidth).Render(ansi.Hardwrap(label+"\n"+content, innerWidth, true))
}

func renderToolDisplay(tool events.ToolDisplay, width int) string {
	if width <= 0 {
		return ""
	}
	line := fmt.Sprintf("● %s(%s)", tool.Name, summarizeArguments(tool.Arguments.Text()))
	status := toolStatusText(tool.Status)
	if strings.TrimSpace(tool.Summary.Text()) != "" {
		status = tool.Summary.Text()
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
	return style.Render(ansi.Hardwrap(line, width, true))
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
	if strings.TrimSpace(tool.Stdout.Text()) != "" {
		details = append(details, "stdout: "+previewText(tool.Stdout.Text(), 160))
	}
	if strings.TrimSpace(tool.Stderr.Text()) != "" {
		details = append(details, "stderr: "+previewText(tool.Stderr.Text(), 160))
	}
	if tool.Artifact != nil {
		createdAtUnixNano := int64(0)
		if !tool.Artifact.CreatedAt.IsZero() {
			createdAtUnixNano = tool.Artifact.CreatedAt.UnixNano()
		}
		artifactError := redact.SafeText{}
		if !tool.Artifact.Available {
			artifactError = safeDisplayText("artifact 无法打开，请确认 ID 后重试")
		}
		artifactView := NewArtifactView(ArtifactViewSpec{
			ID: tool.Artifact.ID, Bytes: tool.Artifact.Bytes, CreatedAtUnixNano: createdAtUnixNano,
			Available: tool.Artifact.Available, Complete: tool.Artifact.Complete,
			TruncationReason: tool.TruncationReason, Error: artifactError,
		})
		details = append(details, "artifact: "+artifactView.MetadataLine())
		if intent, ok := artifactView.OpenIntent(); ok {
			details = append(details, "open: /artifact "+intent.ID())
		}
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

func toolDisplayStatus(state tool.ExecutionState, status tool.ResultStatus) events.ToolDisplayStatus {
	switch state {
	case tool.Prepared:
		return events.ToolDisplayPending
	case tool.Running:
		return events.ToolDisplayRunning
	case tool.Rejected:
		return events.ToolDisplayDenied
	case tool.CancelledBeforeStart, tool.CancelledAfterStart:
		return events.ToolDisplayCancelled
	}
	switch status {
	case tool.StatusError, tool.StatusTimeout:
		return events.ToolDisplayError
	case tool.StatusDenied:
		return events.ToolDisplayDenied
	case tool.StatusSuccess:
		return events.ToolDisplaySuccess
	default:
		return events.ToolDisplayPending
	}
}

func toolDisplayFromHistory(messages []conversation.Message, result conversation.Message) events.ToolDisplay {
	if result.Tool == nil {
		return events.ToolDisplay{}
	}
	state := result.Tool
	display := events.ToolDisplay{
		CallID:           state.CallID,
		Name:             state.Name,
		Arguments:        state.ArgumentsJSON,
		Summary:          state.Summary,
		Status:           toolDisplayStatus(state.State, state.Status),
		Truncated:        state.Truncated,
		TruncationReason: state.TruncationReason,
	}
	if state.Error != nil {
		display.ErrorCode = state.Error.Code
		display.Stderr = state.Error.Message
		display.Recoverable = state.Error.Recoverable
	}
	if state.Artifact != nil {
		display.Artifact = &events.ArtifactRef{
			ID:        state.Artifact.ID,
			Bytes:     state.Artifact.Bytes,
			Available: state.Artifact.Available,
			Complete:  state.Artifact.Complete,
			CreatedAt: state.Artifact.CreatedAt,
		}
	}
	for _, message := range messages {
		if message.Role == conversation.RoleToolCall && message.Tool != nil && message.Tool.CallID == state.CallID {
			display.Arguments = message.Tool.ArgumentsJSON
			break
		}
	}
	return display
}

func safeDisplayText(value string) redact.SafeText {
	return redact.NewRuntimeRedactor().Redact(value)
}

func projectConversationMessages(messages []conversation.Message) []renderedMessage {
	projected := make([]renderedMessage, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case conversation.RoleToolCall:
			continue
		case conversation.RoleToolResult:
			if message.Tool == nil {
				continue
			}
			projected = append(projected, renderedMessage{tool: toolDisplayFromHistory(messages, message), isTool: true})
		default:
			projected = append(projected, renderedMessage{role: string(message.Role), content: message.Content})
		}
	}
	return projected
}

func cloneToolDisplay(display events.ToolDisplay) events.ToolDisplay {
	cloned := display
	if display.Artifact != nil {
		artifact := *display.Artifact
		cloned.Artifact = &artifact
	}
	return cloned
}
