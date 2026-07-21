package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"xagent/internal/command"
	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/orchestrator"
	"xagent/internal/permission"
	"xagent/internal/redact"
	"xagent/internal/skill"
	"xagent/internal/tui"
)

type screen string

const (
	screenList screen = "list"
	screenChat screen = "chat"
)

type RequestSession struct {
	Cancel       context.CancelFunc
	StartedAt    time.Time
	Timeout      time.Duration
	Independent  bool
	TransientIDs []string
}

type Model struct {
	deps            Deps
	orchestrator    *orchestrator.Orchestrator
	screen          screen
	list            list.Model
	input           tui.Input
	messages        tui.MessagesView
	status          tui.Status
	conversation    *conversation.Conversation
	request         *RequestSession
	diagnostics     *diagnostics.Collector
	streaming       bool
	confirmation    *Event
	commandRegistry *command.Registry
	baseCommands    []command.Definition
	commandMenu     tui.CommandMenu
	skillActivity   *skill.Activity
	skillGeneration uint64
	skillNotice     string
	mode            orchestrator.RunMode
	lastError       error
	messagesCleared bool
}

func New(deps Deps) Model {
	ctx := context.Background()
	if deps.CommandRegistry == nil {
		deps.CommandRegistry = command.MustNew(command.Builtins()...)
	}
	if deps.Diagnostics == nil {
		deps.Diagnostics = diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redact.Text})
	}
	conversations, _ := deps.Store.List(ctx)
	orchOptions := orchestrator.OrchestratorOptions{
		Provider:            deps.Provider,
		Store:               deps.Store,
		Resources:           deps.Resources,
		Thinking:            deps.Config.LLM.Thinking,
		Registry:            deps.Registry,
		Executor:            deps.Executor,
		ContextManager:      deps.ContextManager,
		Diagnostics:         deps.Diagnostics,
		Agent:               deps.Config.Agent,
		SkillManager:        deps.SkillManager,
		DefaultModel:        deps.Config.LLM.Model,
		Redact:              deps.Redact,
		RedactionLookbehind: deps.RedactionLookbehind,
	}
	if deps.SessionContext != nil {
		orchOptions.SessionContext = deps.SessionContext
	}
	if deps.Memory != nil {
		orchOptions.Memory = deps.Memory
	}
	orch := orchestrator.NewWithOptions(orchOptions)
	if mode, ok := permission.ParseMode(deps.Config.Permission.Mode); ok {
		orch.SetPermissionMode(mode)
	}
	model := Model{
		deps:            deps,
		orchestrator:    orch,
		screen:          screenList,
		list:            tui.NewConversationList(conversations),
		input:           tui.NewInput(deps.Resources.UILabel("input_prompt")),
		messages:        tui.NewMessagesView(deps.Config.LLM.Thinking.Show),
		diagnostics:     deps.Diagnostics,
		commandRegistry: deps.CommandRegistry,
		baseCommands:    deps.CommandRegistry.Definitions(),
		skillActivity:   skill.NewActivity(),
		mode:            orchestrator.RunModeDefault,
		status: tui.Status{
			Mode:     string(orchestrator.RunModeDefault),
			Provider: deps.Provider.Name(),
			Model:    deps.Config.LLM.Model,
		},
	}
	model.installInitialSkillCommands()
	model.refreshMCPStatus()
	model.syncMCPDiagnostics()
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
	if menu := m.commandMenu.View(); menu != "" {
		return fmt.Sprintf("%s\n\n%s\n%s\n%s\n%s", content, m.status.View(), m.input.Text.View(), menu, footer)
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
	m.resetCommandState()
	m.screen = screenChat
}

func (m *Model) loadConversation(id string) {
	conv, report, err := m.recoverOrLoadConversation(id)
	if err != nil {
		m.status.Error = err
		return
	}
	m.conversation = conv
	m.messages.SetMessages(conv.Messages)
	m.resetCommandState()
	m.status.Notice = recoveryNotice(report)
	m.screen = screenChat
}

func (m *Model) resetCommandState() {
	if m.skillActivity != nil {
		m.skillActivity.Clear()
	}
	m.mode = orchestrator.RunModeDefault
	m.status.Mode = string(orchestrator.RunModeDefault)
	m.skillActivity = skill.NewActivity()
	m.status.ActiveSkills = ""
	m.status.RequestModel = ""
	m.commandMenu.Close()
	m.messagesCleared = false
}

func (m *Model) recoverOrLoadConversation(id string) (*conversation.Conversation, conversation.RecoveryReport, error) {
	ctx := context.Background()
	if recovering, ok := m.deps.Store.(conversation.RecoveringStore); ok {
		conv, report, err := recovering.Recover(ctx, id)
		return conv, report, err
	}
	conv, err := m.deps.Store.Load(ctx, id)
	return conv, conversation.RecoveryReport{}, err
}

func recoveryNotice(report conversation.RecoveryReport) string {
	parts := []string{}
	if report.SkippedLines > 0 {
		parts = append(parts, fmt.Sprintf("跳过 %d 行损坏会话记录", report.SkippedLines))
	}
	if report.TruncatedFromMessage >= 0 {
		parts = append(parts, fmt.Sprintf("从第 %d 条未闭合工具调用处截断", report.TruncatedFromMessage))
	}
	if report.TimeGapReminder {
		parts = append(parts, "已插入会话时间跨度提醒")
	}
	for _, diagnostic := range report.Diagnostics {
		text := strings.TrimSpace(diagnostic.Safe(redact.Text).Text())
		if text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "恢复提示: " + strings.Join(parts, "；")
}

func (m *Model) close() {
	var closeErrors []string
	if m.conversation != nil {
		_ = m.deps.Store.Save(context.Background(), m.conversation)
	}
	if m.skillActivity != nil {
		m.skillActivity.Clear()
	}
	if m.deps.Closer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := m.deps.Closer.Close(ctx); err != nil {
			closeErrors = append(closeErrors, "MCP close: "+redact.Text(err.Error()))
		}
	}
	if len(closeErrors) > 0 {
		m.status.Error = fmt.Errorf("%s", strings.Join(closeErrors, "; "))
	}
	m.refreshMCPStatus()
}

func (m *Model) refreshMCPStatus() {
	if m.deps.MCPStatus != nil {
		m.status.MCP = m.deps.MCPStatus.StatusLine()
	}
}

func (m *Model) syncMCPDiagnostics() {
	if m.diagnostics == nil || m.deps.MCPStatus == nil {
		return
	}
	for _, item := range m.deps.MCPStatus.Diagnostics() {
		m.diagnostics.Add(diagnostics.New("mcp_status", diagnostics.SeverityWarning, item.Message).WithSource(item.Server))
	}
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
