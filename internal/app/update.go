package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/command"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/memory"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/tui"
)

func (m *Model) sendConfirmation(action events.PermissionAction, allowed bool) {
	if m == nil || m.confirmation == nil || m.confirmation.Confirmation == nil || m.confirmationResolver == nil {
		return
	}
	request := m.confirmation.Confirmation
	if !m.confirmationResolver.ResolveToolConfirmation(events.ToolConfirmationDecision{
		ConfirmationID: request.ConfirmationID,
		CallID:         request.CallID,
		Allowed:        allowed,
		Action:         action,
	}) {
		return
	}
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

// navigationIntentMsg is the capability-free handoff from command/key input to
// the staged navigation transaction. T4.7 and later tasks own its side effects.
type navigationIntentMsg struct {
	Intent    command.IntentKind
	SessionID string
}

func navigationIntentCmd(intent command.IntentKind, sessionID string) tea.Cmd {
	return func() tea.Msg {
		return navigationIntentMsg{Intent: intent, SessionID: sessionID}
	}
}

// HandleIntent lets slash commands use the same value-only handoff as
// shortcuts. It deliberately performs no Store, Orchestrator, or screen work.
func (c *commandController) HandleIntent(intent command.IntentKind) error {
	if _, err := navigationKind(intent, ""); err != nil {
		return err
	}
	c.cmd = navigationIntentCmd(intent, "")
	return nil
}

// handleEscape resolves Esc from the command metadata for the current request
// context. Cancellation never emits a navigation intent; only an idle chat can
// request the sessions screen.
func (m *Model) handleEscape() (bool, tea.Cmd) {
	waitingConfirmation := m.confirmation != nil && m.confirmation.Confirmation != nil
	if !waitingConfirmation && !m.streaming {
		switch m.screen {
		case screenTaskDetail:
			m.screen = screenTasks
			return true, nil
		case screenTasks:
			m.screen = screenChat
			return true, nil
		}
	}
	shortcutContext := command.ShortcutChatIdle
	switch {
	case waitingConfirmation:
		shortcutContext = command.ShortcutChatConfirmation
	case m.streaming:
		shortcutContext = command.ShortcutChatStreaming
	case m.screen != screenChat:
		return false, nil
	}

	intent, ok := m.ensureCommandRegistry().IntentForShortcut(shortcutContext, "esc")
	if !ok {
		return false, nil
	}
	switch intent {
	case command.IntentCancel:
		if m.request != nil || m.streaming {
			m.cancelRequest("正在取消请求，等待当前轮次收尾")
		} else if waitingConfirmation {
			m.sendConfirmation(events.PermissionCancel, false)
		}
		return true, nil
	case command.IntentShowSessions:
		return true, navigationIntentCmd(intent, "")
	default:
		return false, nil
	}
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

// artifactViewMsg is the only artifact load result allowed across Bubble Tea.
// ArtifactView contains redacted SafeText and bounded metadata, never a reader
// or raw artifact bytes.
type artifactViewMsg struct {
	RequestID  string
	Offset     int64
	Generation uint64
	View       tui.ArtifactView
}

func artifactOpenCmd(
	ctx context.Context,
	artifacts ArtifactUserReader,
	intent tui.ArtifactOpenIntent,
	runtimeRedactor *redact.RuntimeRedactor,
	lease *artifactReadLease,
	generation uint64,
) tea.Cmd {
	return func() tea.Msg {
		view, err := openArtifactForUser(ctx, artifacts, intent, runtimeRedactor, lease)
		if errors.Is(err, errArtifactOpenCanceled) {
			return nil
		}
		return artifactViewMsg{
			RequestID: intent.ID(), Offset: intent.Offset(), Generation: generation, View: view,
		}
	}
}

func (m *Model) beginArtifactOpen(intent tui.ArtifactOpenIntent) tea.Cmd {
	if m.artifactCancel != nil {
		m.artifactCancel()
	}
	generation := m.advanceArtifactGeneration()
	ctx, cancel := context.WithCancel(context.Background())
	lease := newArtifactReadLease()
	m.artifactCancel = func() {
		cancel()
		lease.Cancel()
	}
	m.artifactExpectedID = intent.ID()
	m.artifactExpectedPage = intent.Offset()
	return artifactOpenCmd(ctx, m.deps.Artifacts, intent, m.deps.RuntimeRedactor, lease, generation)
}

func (m *Model) closeArtifactView() {
	m.advanceArtifactGeneration()
	if m.artifactCancel != nil {
		m.artifactCancel()
	}
	m.artifactCancel = nil
	m.artifactExpectedID = ""
	m.artifactExpectedPage = 0
	m.artifactView = nil
}

func (m *Model) advanceArtifactGeneration() uint64 {
	m.artifactGeneration++
	if m.artifactGeneration == 0 {
		m.artifactGeneration = 1
	}
	return m.artifactGeneration
}

func (m *Model) applyWindowSize(size tea.WindowSizeMsg) {
	screen := tui.ScreenChat
	switch m.screen {
	case screenList:
		screen = tui.ScreenList
	case screenTasks:
		screen = tui.ScreenTasks
	case screenTaskDetail:
		screen = tui.ScreenTaskDetail
	}
	input := tui.LayoutInput{
		Terminal:         tui.Size{Width: size.Width, Height: size.Height},
		InputLines:       1,
		ShowConfirmation: m.confirmation != nil && m.confirmation.Confirmation != nil,
		ShowCommandMenu:  m.commandMenu.Visible,
		Screen:           string(screen),
	}
	preliminary := tui.ComputeLayout(input)
	input.InputLines = m.input.VisualLineCount(preliminary.Input.Width)
	layout := tui.ComputeLayout(input)

	m.status.SetRegion(layout.Status)
	if m.screen == screenList {
		m.taskRegion = tui.Region{}
		tui.ApplyConversationListLayout(&m.list, layout)
		return
	}
	if m.screen == screenTasks || m.screen == screenTaskDetail {
		m.taskRegion = layout.Main
		m.input.SetRegion(layout.Input)
		m.commandMenu.SetRegion(layout.CommandMenu)
		return
	}
	m.taskRegion = tui.Region{}
	m.messages.SetRegion(layout.Main)
	m.input.SetRegion(layout.Input)
	m.commandMenu.SetRegion(layout.CommandMenu)
	m.applyConfirmationLayout(layout.Confirmation)
}

func (m *Model) applyConfirmationLayout(region tui.Region) {
	if m.confirmation == nil || m.confirmation.Confirmation == nil {
		return
	}
	request := m.confirmation.Confirmation
	scopes := make([]ConfirmationScopeState, len(request.Scopes))
	for index, scope := range request.Scopes {
		scopes[index] = ConfirmationScopeState{
			Scope: scope.Scope, Available: scope.Available, Description: scope.Description,
		}
	}
	viewModel := newViewModel(RuntimeState{}, ConversationState{}, RequestState{Confirmation: &ConfirmationState{
		CallID: request.CallID, Name: request.Name, Prompt: request.Prompt, Target: request.Target,
		Risk: request.Risk, PermissionMode: request.PermissionMode, ScopePreview: request.ScopePreview,
		RuleLocation: request.RuleLocation, Scopes: scopes, Warning: request.Warning,
		RevokeHint: request.RevokeHint, AllowPermanent: request.AllowPermanent,
	}}, ConversationListState{}, screenChat)
	confirmation, present := viewModel.Request().Confirmation()
	if !present {
		return
	}
	panel := tui.NewConfirmationPanel(confirmation)
	panel.SetRegion(region)

	clonedEvent := events.Clone(*m.confirmation)
	clonedEvent.Confirmation.Prompt = redact.NewRuntimeRedactor().Redact(panel.View())
	m.confirmation = &clonedEvent
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.lifecycle == nil {
		m.lifecycle = newLifecycleState()
		if m.request != nil {
			m.lifecycle.begin(m.request)
		}
	}
	if !m.lifecycle.isAccepting() {
		return m, nil
	}
	request, _, _ := m.lifecycle.active()
	m.request = request
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		m.applyWindowSize(size)
		return m, nil
	}
	if intent, ok := msg.(tui.ArtifactOpenIntent); ok {
		return m, m.beginArtifactOpen(intent)
	}
	if loaded, ok := msg.(artifactViewMsg); ok {
		if loaded.Generation == 0 || loaded.Generation != m.artifactGeneration ||
			loaded.RequestID != m.artifactExpectedID || loaded.Offset != m.artifactExpectedPage {
			return m, nil
		}
		if m.artifactCancel != nil {
			m.artifactCancel()
		}
		m.artifactCancel = nil
		view := loaded.View
		m.artifactView = &view
		return m, nil
	}
	switch message := msg.(type) {
	case taskSubscribedMsg:
		return m, m.handleTaskSubscribed(message)
	case taskEventMsg:
		if diagnostic, reject := m.rejectTaskEventProjection(message); reject {
			m.publishTaskStreamError(subagent.SafeError(
				subagent.ErrInvalidTransition,
				redact.NewRuntimeRedactor().Redact(diagnostic),
				false,
			))
			m.taskEventCursor = message.event.Revision
			return m, waitTaskEvent(message.state, message.epoch, message.events)
		}
		return m, m.handleTaskEvent(message)
	case taskEventStreamClosedMsg:
		return m, m.handleTaskEventStreamClosed(message)
	case taskEventResyncMsg:
		return m, m.handleTaskEventResync(message)
	case taskEventRetryMsg:
		return m, m.handleTaskEventRetry(message)
	case taskForegroundOutcomeMsg:
		return m, m.handleTaskForegroundOutcome(message)
	}
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "esc" && (m.artifactView != nil || m.artifactCancel != nil) {
			m.closeArtifactView()
			return m, nil
		}
		if (msg.String() == "q" || msg.String() == "ctrl+c") && (m.artifactView != nil || m.artifactCancel != nil) {
			m.closeArtifactView()
			return m, tea.Quit
		}
		if m.artifactView != nil {
			switch msg.String() {
			case "pgdown", " ":
				if intent, ok := m.artifactView.NextIntent(); ok {
					return m, m.beginArtifactOpen(intent)
				}
				return m, nil
			case "pgup":
				if intent, ok := m.artifactView.PreviousIntent(); ok {
					return m, m.beginArtifactOpen(intent)
				}
				return m, nil
			}
		}
		if m.streaming && (msg.String() == "ctrl+c" || msg.String() == "q") {
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
		if msg.String() == "esc" {
			if handled, cmd := m.handleEscape(); handled {
				return m, cmd
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
				} else if !item.Available {
					m.status.Error = m.redactError(fmt.Errorf("该会话当前不可用，已保留当前页面"))
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

	if closed, ok := msg.(eventStreamClosedMsg); ok {
		if !m.finishEventEnvelope(closed.envelope) {
			return m, nil
		}
		m.finishRequestAfterStream()
		return m, nil
	}

	if eventMsg, ok := msg.(eventMsg); ok {
		if !m.acceptsEventEnvelope(eventMsg.envelope) {
			return m, nil
		}
		if eventMsg.event.Transient {
			m.trackTransientID(eventMsg.event.IndependentID)
		}
		switch eventMsg.event.Type {
		case EventUserSubmitted:
			if !eventMsg.event.Transient {
				if m.pendingUserProjection {
					m.pendingUserProjection = false
				} else {
					m.messages.AppendUser(eventMsg.event.Text.Text())
				}
			}
		case EventTextDelta:
			if eventMsg.event.Transient {
				m.messages.AppendTransientAssistantDelta(eventMsg.event.IndependentID, eventMsg.event.Text.Text())
			} else {
				if m.request != nil && m.request.Independent {
					m.clearRequestTransient()
				}
				m.messages.AppendAssistantDelta(eventMsg.event.Text.Text())
			}
		case EventThinkingDelta:
			if eventMsg.event.Transient {
				m.messages.AppendTransientThinkingDelta(eventMsg.event.IndependentID, eventMsg.event.Text.Text())
			} else {
				m.messages.AppendThinkingDelta(eventMsg.event.Text.Text())
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
				m.status.StopMessage = eventMsg.event.Progress.Message.Text()
			}
		case EventUsageUpdated:
			if eventMsg.event.Usage != nil {
				m.status.InputTokens += eventMsg.event.Usage.InputTokens
				m.status.OutputTokens += eventMsg.event.Usage.OutputTokens
				m.status.CacheCreationInputTokens += eventMsg.event.Usage.CacheCreationInputTokens
				m.status.CacheReadInputTokens += eventMsg.event.Usage.CacheReadInputTokens
			}
		case EventMainTraceReset:
			if m.conversation != nil && !m.messagesCleared {
				m.messages.SetMessages(m.conversation.Messages)
			}
		case EventDone:
			m.messages.CommitAssistant()
			if m.status.ShowResponseTimer {
				m.status.Duration = formatDuration(eventMsg.event.Duration)
			} else {
				m.status.Duration = 0
			}
			m.lifecycle.markTerminal()
			return m, listen(eventMsg.events, eventMsg.envelope)
		case EventError:
			m.messages.CommitAssistant()
			safe := m.redactError(eventMsg.event.Err)
			m.status.Error = safe
			m.lastError = safe
			m.lifecycle.markTerminal()
			return m, listen(eventMsg.events, eventMsg.envelope)
		}
		return m, listen(eventMsg.events, eventMsg.envelope)
	}

	if m.screen == screenList {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	if m.confirmation != nil {
		return m, nil
	}

	var cmd tea.Cmd
	m.input.Text, cmd = m.input.Text.Update(msg)
	return m, cmd
}

func (m *Model) rejectTaskEventProjection(message taskEventMsg) (string, bool) {
	if m == nil || message.state == nil || !message.state.accepts(message.epoch) {
		return "", false
	}
	event := message.event
	if event.Kind == subagent.EventGap || event.Revision == 0 || event.Revision <= m.taskEventCursor ||
		event.Sequence == 0 || event.TaskID == "" || event.ValidateOneOf() != nil || !validTaskEventPayload(event) {
		return "", false
	}
	if workspace, present := taskEventWorkspace(event); present {
		if _, valid := projectTaskWorkspace(workspace); !valid {
			return "任务工作区状态无效", true
		}
	}
	next, changesStatus := taskEventProjectedStatus(event)
	if !changesStatus {
		return "", false
	}
	current, found := m.currentProjectedTaskStatus(event.TaskID)
	if !found || current == next || subagent.CanTransition(current, next) {
		return "", false
	}
	return "任务状态事件顺序无效", true
}

func taskEventProjectedStatus(event subagent.Event) (subagent.Status, bool) {
	if event.Result != nil {
		return event.Result.Status, true
	}
	if event.Snapshot != nil {
		return event.Snapshot.Status, true
	}
	return "", false
}

func (m *Model) currentProjectedTaskStatus(taskID subagent.ID) (subagent.Status, bool) {
	var (
		status   subagent.Status
		revision uint64
		found    bool
	)
	for _, task := range m.taskListView.Tasks() {
		candidate := subagent.Status(task.Status())
		if task.ID() == string(taskID) && candidate.Valid() && (!found || task.Revision() >= revision) {
			status, revision, found = candidate, task.Revision(), true
		}
	}
	detail := m.taskDetailView.Task()
	candidate := subagent.Status(detail.Status())
	if detail.ID() == string(taskID) && candidate.Valid() && (!found || detail.Revision() >= revision) {
		status, found = candidate, true
	}
	return status, found
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
	m.pendingUserProjection = false
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
