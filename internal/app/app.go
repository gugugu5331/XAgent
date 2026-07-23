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
	"xagent/internal/hook"
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
	lifecycle       *lifecycleState
	hooks           hook.Runtime
}

func New(deps Deps) Model {
	ctx := context.Background()
	if deps.CommandRegistry == nil {
		deps.CommandRegistry = command.MustNew(command.Builtins()...)
	}
	if deps.Diagnostics == nil {
		redactor := deps.Redact
		if redactor == nil {
			redactor = redact.Text
		}
		deps.Diagnostics = diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redactor})
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
		Hooks:               deps.Hooks,
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
	hooks := deps.Hooks
	if hooks == nil {
		hooks = hook.Noop()
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
		lifecycle:       newLifecycleState(),
		hooks:           hooks,
		status: tui.Status{
			Mode:     string(orchestrator.RunModeDefault),
			Provider: deps.Provider.Name(),
			Model:    deps.Config.LLM.Model,
		},
	}
	model.installInitialSkillCommands()
	model.refreshMCPStatus()
	model.syncMCPDiagnostics()
	model.hooks.SystemStart(context.Background())
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
		m.status.Error = m.redactError(err)
		return
	}
	m.transitionConversation(conv, conversation.RecoveryReport{}, true)
}

func (m *Model) loadConversation(id string) {
	conv, report, err := m.recoverOrLoadConversation(id)
	if err != nil {
		m.status.Error = m.redactError(err)
		return
	}
	m.transitionConversation(conv, report, false)
}

// transitionConversation activates an already-created candidate. Candidate
// creation/recovery happens before this method so a failed candidate never
// mutates the current session.
func (m *Model) transitionConversation(candidate *conversation.Conversation, report conversation.RecoveryReport, created bool) {
	if candidate == nil {
		m.status.Error = m.redactError(fmt.Errorf("候选会话不能为空"))
		return
	}
	var transitionErrors []string
	if m.conversation != nil {
		if m.orchestrator != nil {
			if err := m.orchestrator.WaitIdle(context.Background()); err != nil {
				m.status.Error = m.redactError(err)
				return
			}
		}
		if m.deps.Store != nil {
			if err := m.deps.Store.Save(context.Background(), m.conversation); err != nil {
				transitionErrors = append(transitionErrors, "旧会话保存: "+m.redactText(err.Error()))
			}
		}
		m.hookRuntime().SessionEnd(context.Background(), m.conversation.ID, hook.SessionEndSwitch)
	}
	m.resetCommandState()
	m.conversation = candidate
	if created {
		m.messages.SetMessages(nil)
	} else {
		m.messages.SetMessages(candidate.Messages)
	}
	m.status.Notice = recoveryNotice(report, m.redactText)
	m.screen = screenChat
	state := hook.SessionResumed
	if created {
		state = hook.SessionNew
	}
	m.hookRuntime().SessionStart(context.Background(), candidate.ID, state)
	if len(transitionErrors) > 0 {
		m.status.Error = m.redactError(fmt.Errorf("%s", strings.Join(transitionErrors, "; ")))
	} else {
		m.status.Error = nil
	}
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

func (m *Model) hookRuntime() hook.Runtime {
	if m == nil || m.hooks == nil {
		return hook.Noop()
	}
	return m.hooks
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

func recoveryNotice(report conversation.RecoveryReport, redactor func(string) string) string {
	if redactor == nil {
		redactor = redact.Text
	}
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
		text := strings.TrimSpace(diagnostic.Safe(redactor).Text())
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
	_ = m.Close(context.Background())
}

// Close owns only the App/session lifecycle. Process resources such as Hook
// workers and MCP transports are closed by the main coordinator after this
// method returns.
func (m *Model) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if m.lifecycle == nil {
		m.lifecycle = newLifecycleState()
		if m.request != nil {
			m.lifecycle.begin(m.request)
		}
	}
	return m.lifecycle.close(func() error {
		if request, _, _ := m.lifecycle.active(); request != nil {
			m.request = request
		}
		m.lifecycle.cancel()
		var closeErrors []string
		if m.orchestrator != nil {
			if err := m.orchestrator.WaitIdle(ctx); err != nil {
				closeErrors = append(closeErrors, "等待 Agent 收尾: "+m.redactText(err.Error()))
			}
		}
		if m.conversation != nil && m.deps.Store != nil {
			if err := m.deps.Store.Save(ctx, m.conversation); err != nil {
				closeErrors = append(closeErrors, "会话保存: "+m.redactText(err.Error()))
			}
		}
		if m.conversation != nil {
			m.hookRuntime().SessionEnd(context.Background(), m.conversation.ID, hook.SessionEndExit)
		}
		if m.skillActivity != nil {
			m.skillActivity.Clear()
		}
		m.mode = orchestrator.RunModeDefault
		m.status.Mode = string(orchestrator.RunModeDefault)
		m.status.ActiveSkills = ""
		m.status.RequestModel = ""
		m.clearRequestTransient()
		m.request = nil
		m.streaming = false
		m.status.Streaming = false
		m.status.WaitingConfirmation = false
		m.confirmation = nil
		m.conversation = nil
		m.lifecycle.finish()
		if len(closeErrors) == 0 {
			return nil
		}
		err := m.redactError(fmt.Errorf("%s", strings.Join(closeErrors, "; ")))
		m.status.Error = err
		return err
	})
}

func (m *Model) refreshMCPStatus() {
	if m.deps.MCPStatus != nil {
		m.status.MCP = m.redactText(m.deps.MCPStatus.StatusLine())
	}
}

func (m *Model) redactText(value string) string {
	if m != nil && m.deps.Redact != nil {
		return m.deps.Redact(value)
	}
	return redact.Text(value)
}

func (m *Model) redactError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", m.redactText(err.Error()))
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
			return eventStreamClosedMsg{}
		}
		return eventMsg{event: event, events: events}
	}
}

type eventMsg struct {
	event  Event
	events <-chan Event
}

type eventStreamClosedMsg struct{}

func sanitizeInput(value string) string {
	return strings.TrimSpace(value)
}

func formatDuration(duration time.Duration) time.Duration {
	return duration.Round(time.Millisecond)
}
