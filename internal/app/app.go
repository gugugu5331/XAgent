package app

import (
	"context"
	"errors"
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
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/orchestrator"
	"xagent/internal/permission"
	"xagent/internal/redact"
	"xagent/internal/skill"
	"xagent/internal/tui"
)

type screen string

const (
	screenList       screen = "list"
	screenChat       screen = "chat"
	screenTasks      screen = "tasks"
	screenTaskDetail screen = "task_detail"
)

// ConversationListEntryState is the safe App projection of one Store list
// entry. Recovery diagnostics are reduced to SafeText before this value can be
// published to a ViewModel.
type ConversationListEntryState struct {
	ID             string
	Title          redact.SafeText
	UpdatedAt      time.Time
	MessageCount   int
	Available      bool
	Selectable     bool
	RecoveryStatus conversation.RecoveryStatus
	RecoveryNotice redact.SafeText
}

// ConversationListState contains a trusted complete or partial list result.
// It intentionally excludes raw diagnostics and Store-owned domain objects.
type ConversationListState struct {
	Entries      []ConversationListEntryState
	Truncated    bool
	ScannedFiles int
	ScannedBytes int64
	Notice       redact.SafeText
}

type RequestSession struct {
	Cancel         context.CancelFunc
	StartedAt      time.Time
	Timeout        time.Duration
	Independent    bool
	TransientIDs   []string
	Generation     uint64
	ConversationID string
}

type Model struct {
	deps           Deps
	runtimeOptions RuntimeOptions
	runtime        RuntimeState
	orchestrator   *orchestrator.Orchestrator
	// lifecycleWaiter is a deliberately narrow Close seam. Production models
	// bind the concrete Orchestrator; tests and future state-driven adapters
	// may provide only WaitIdle without widening App's runtime capability.
	lifecycleWaiter       navigationWaiter
	screen                screen
	list                  list.Model
	input                 tui.Input
	messages              tui.MessagesView
	status                tui.Status
	conversation          *conversation.Conversation
	request               *RequestSession
	diagnostics           *diagnostics.Collector
	streaming             bool
	confirmation          *Event
	commandRegistry       *command.Registry
	baseCommands          []command.Definition
	commandMenu           tui.CommandMenu
	skillActivity         *skill.Activity
	skillGeneration       uint64
	skillNotice           string
	mode                  orchestrator.RunMode
	lastError             error
	messagesCleared       bool
	pendingUserProjection bool
	lifecycle             *lifecycleState
	eventBoundary         *eventBoundaryState
	confirmationResolver  interface {
		ResolveToolConfirmation(events.ToolConfirmationDecision) bool
	}
	hooks                hook.Runtime
	artifactView         *tui.ArtifactView
	artifactCancel       context.CancelFunc
	artifactExpectedID   string
	artifactExpectedPage int64
	artifactGeneration   uint64
	// Task views are detached, capability-free TUI projections. The App never
	// retains TaskManager snapshots or callable task objects in display state.
	taskListView    tui.TaskListView
	taskDetailView  tui.TaskDetailView
	taskRegion      tui.Region
	taskEvents      *taskEventStreamState
	taskEventCursor uint64
}

func New(deps Deps) Model {
	return newModel(deps, deps.RuntimeOptions)
}

// NewWithOptions constructs an App with explicit lifecycle ownership inputs.
// The compatibility New constructor remains the single default path and uses
// the RuntimeOptions carried by Deps (if any).
func NewWithOptions(deps Deps, options RuntimeOptions) Model {
	return newModel(deps, options)
}

func newModel(deps Deps, runtimeOptions RuntimeOptions) Model {
	ctx := context.Background()
	runtimeOptions = normalizeRuntimeOptions(runtimeOptions)
	eventBoundary := newRuntimeEventBoundaryState(&runtimeOptions, deps.RuntimeRedactor)
	runtimeState := runtimeStateFromResolvedUI(deps.Config.UI)
	initialScreen := screen(runtimeState.StartMode)
	if runtimeState.StartMode == config.StartModeNew {
		// A new session is published as chat only after Create succeeds. Until
		// then the trusted list remains the visible fallback.
		initialScreen = screenList
	}
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
	listResult, listErr := deps.Store.List(ctx)
	trustedListResult := listResult
	if listErr != nil {
		trustedListResult = conversation.ListResult{}
	}
	orchOptions := orchestrator.OrchestratorOptions{
		Provider:             deps.Provider,
		Store:                deps.Store,
		Resources:            deps.Resources,
		Thinking:             deps.Config.LLM.Thinking,
		Registry:             deps.Registry,
		Executor:             deps.Executor,
		CleanupTimeout:       runtimeOptions.CleanupTimeout,
		LifecycleDiagnostics: runtimeOptions.Diagnostics,
		ContextManager:       deps.ContextManager,
		SkillHistoryPolicy:   deps.SkillHistoryPolicy,
		RequestBudgeter:      deps.RequestBudgeter,
		Diagnostics:          deps.Diagnostics,
		Agent:                deps.Config.Agent,
		SkillManager:         deps.SkillManager,
		DefaultModel:         deps.Config.LLM.Model,
		RuntimeRedactor:      deps.RuntimeRedactor,
		Redact:               deps.Redact,
		RedactionLookbehind:  deps.RedactionLookbehind,
		Hooks:                deps.Hooks,
	}
	if deps.SessionContext != nil {
		orchOptions.SessionContext = deps.SessionContext
	}
	if deps.Memory != nil {
		orchOptions.Memory = deps.Memory
	}
	orch := deps.ExistingOrchestrator
	if orch == nil {
		orch = orchestrator.NewWithOptions(orchOptions)
	}
	if mode, ok := permission.ParseMode(deps.Config.Permission.Mode); ok {
		orch.SetPermissionMode(mode)
	}
	hooks := deps.Hooks
	if hooks == nil {
		hooks = hook.Noop()
	}
	status := tui.Status{
		Mode:              string(orchestrator.RunModeDefault),
		Provider:          deps.Provider.Name(),
		Model:             deps.Config.LLM.Model,
		ShowResponseTimer: runtimeState.ShowResponseTimer,
	}
	if listErr != nil {
		runtimeRedactor := deps.RuntimeRedactor
		if runtimeRedactor == nil {
			runtimeRedactor = redact.NewRuntimeRedactor()
		}
		status.Error = &diagnostics.SafeError{
			Code:        listNavigationErrorCode,
			Source:      staleNavigationDiagnosticSource,
			Message:     runtimeRedactor.Redact("读取会话列表失败，请重试"),
			Recoverable: true,
		}
	} else {
		status.Notice = conversationListNotice(listResult, deps.Redact)
		deps.Diagnostics.Add(listResult.Diagnostics...)
	}
	model := Model{
		deps:                 deps,
		runtimeOptions:       runtimeOptions,
		runtime:              runtimeState,
		orchestrator:         orch,
		lifecycleWaiter:      orch,
		screen:               initialScreen,
		list:                 tui.NewConversationList(trustedListResult.Entries),
		input:                tui.NewInput(deps.Resources.UILabel("input_prompt")),
		messages:             tui.NewMessagesView(deps.Config.LLM.Thinking.Show),
		diagnostics:          deps.Diagnostics,
		commandRegistry:      deps.CommandRegistry,
		baseCommands:         deps.CommandRegistry.Definitions(),
		skillActivity:        skill.NewActivity(),
		mode:                 orchestrator.RunModeDefault,
		lifecycle:            newLifecycleState(),
		eventBoundary:        eventBoundary,
		confirmationResolver: orch,
		hooks:                hooks,
		status:               status,
	}
	if deps.Tasks != nil {
		model.taskEvents = newTaskEventStreamState()
	}
	model.lifecycle.bindModel(&model)
	model.installInitialSkillCommands()
	model.refreshMCPStatus()
	model.syncMCPDiagnostics()
	model.hooks.SystemStart(context.Background())
	if listErr == nil && runtimeState.StartMode == config.StartModeNew {
		model.startNewConversation()
	}
	return model
}

func (m Model) Init() tea.Cmd {
	if m.deps.Tasks == nil {
		return nil
	}
	state := m.taskEvents
	if state == nil {
		state = newTaskEventStreamState()
	}
	return subscribeTaskEvents(state, m.deps.Tasks, m.taskEventCursor)
}

func (m Model) View() string {
	if m.artifactView != nil {
		return m.artifactView.View() + "\n\nEsc 返回，PgDown/Space 下一页，PgUp 上一页"
	}
	if m.screen == screenList {
		return m.list.View() + "\n" + m.status.View() + "\n按 Enter 选择，q 退出"
	}
	if m.screen == screenTasks {
		screen := tui.NewTaskList(m.taskListView)
		if m.taskRegion.Width > 0 && m.taskRegion.Height > 0 {
			screen.SetRegion(m.taskRegion)
		}
		return fmt.Sprintf("%s\n%s\n%s\n%s", screen.View(), m.status.View(), m.input.Text.View(), "Enter 提交命令，Esc 返回对话，q/ctrl+c 退出")
	}
	if m.screen == screenTaskDetail {
		screen := tui.NewTaskDetail(m.taskDetailView)
		if m.taskRegion.Width > 0 && m.taskRegion.Height > 0 {
			screen.SetRegion(m.taskRegion)
		}
		return fmt.Sprintf("%s\n%s\n%s\n%s", screen.View(), m.status.View(), m.input.Text.View(), "Enter 提交命令，Esc 返回任务列表，q/ctrl+c 退出")
	}
	content := lipgloss.NewStyle().Padding(1, 2).Render(m.messages.View())
	footer := "Enter 提交，q/ctrl+c 退出"
	if m.confirmation != nil && m.confirmation.Confirmation != nil {
		footer = m.confirmation.Confirmation.Prompt.Text()
	}
	if menu := m.commandMenu.View(); menu != "" {
		return fmt.Sprintf("%s\n\n%s\n%s\n%s\n%s", content, m.status.View(), m.input.Text.View(), menu, footer)
	}
	return fmt.Sprintf("%s\n\n%s\n%s\n%s", content, m.status.View(), m.input.Text.View(), footer)
}

// newViewModel is the App-owned projection from the three state lifetimes and
// trusted session-list state into a detached, capability-free TUI snapshot.
func newViewModel(
	runtime RuntimeState,
	conversationState ConversationState,
	requestState RequestState,
	listState ConversationListState,
	currentScreen screen,
) tui.ViewModel {
	viewScreen := tui.ScreenList
	switch currentScreen {
	case screenChat:
		viewScreen = tui.ScreenChat
	case screenTasks:
		viewScreen = tui.ScreenTasks
	case screenTaskDetail:
		viewScreen = tui.ScreenTaskDetail
	}

	entries := make([]tui.SessionListEntrySpec, len(listState.Entries))
	for index, entry := range listState.Entries {
		entries[index] = tui.SessionListEntrySpec{
			ID: entry.ID, Title: entry.Title, UpdatedAtUnixMilli: entry.UpdatedAt.UnixMilli(),
			MessageCount: entry.MessageCount, Available: entry.Available, Selectable: entry.Selectable,
			RecoveryStatus: string(entry.RecoveryStatus), RecoveryNotice: entry.RecoveryNotice,
		}
	}

	requestView := tui.RequestViewSpec{
		InputTokens:  requestState.Tokens.InputTokens,
		OutputTokens: requestState.Tokens.OutputTokens,
		CacheCreated: requestState.Cache.CacheCreationInputTokens, CacheRead: requestState.Cache.CacheReadInputTokens,
		StopReason: requestState.StopReason, TransientIDs: append([]string(nil), requestState.TransientIDs...),
	}
	if runtime.ShowResponseTimer {
		requestView.Duration = requestState.Duration
	}
	if requestState.LastError != nil {
		requestView.LastError = tui.SafeErrorViewSpec{
			Present: true, Code: requestState.LastError.Code, Source: requestState.LastError.Source,
			Message: requestState.LastError.Message, Recoverable: requestState.LastError.Recoverable,
		}
	}
	if requestState.Confirmation != nil {
		confirmation := requestState.Confirmation
		requestView.Confirmation = tui.ConfirmationViewSpec{
			Present: true, CallID: confirmation.CallID, Name: confirmation.Name, Prompt: confirmation.Prompt,
			Target: confirmation.Target, Risk: confirmation.Risk, PermissionMode: confirmation.PermissionMode,
			ScopePreview: confirmation.ScopePreview, RuleLocation: confirmation.RuleLocation,
			Scopes:  confirmationScopeViewSpecs(confirmation.Scopes),
			Warning: confirmation.Warning, RevokeHint: confirmation.RevokeHint,
			AllowPermanent: confirmation.AllowPermanent,
		}
	}

	return tui.NewStateViewModel(tui.ViewModelSpec{
		Generation: requestState.Generation, RuntimeSequence: runtime.RequestSequence, Screen: viewScreen,
		Lines: append([]redact.SafeText(nil), conversationState.Messages...),
		Conversation: tui.ConversationViewSpec{
			ActiveID: conversationState.ActiveID, Mode: conversationState.Mode,
			Skills: append([]string(nil), conversationState.Skills...), Input: conversationState.Input,
			Messages: append([]redact.SafeText(nil), conversationState.Messages...), Notice: conversationState.Notice,
		},
		Request: requestView,
		Sessions: tui.SessionListViewSpec{
			Entries: entries, Truncated: listState.Truncated, ScannedFiles: listState.ScannedFiles,
			ScannedBytes: listState.ScannedBytes, Notice: listState.Notice,
		},
	})
}

func confirmationScopeViewSpecs(source []ConfirmationScopeState) []tui.ConfirmationScopeViewSpec {
	if source == nil {
		return nil
	}
	specs := make([]tui.ConfirmationScopeViewSpec, len(source))
	for index, scope := range source {
		specs[index] = tui.ConfirmationScopeViewSpec{
			Scope: scope.Scope, Available: scope.Available, Description: scope.Description,
		}
	}
	return specs
}

func (m *Model) startNewConversation() {
	if !m.acceptsIntent() {
		return
	}
	if !m.prepareConversationTransition() {
		return
	}
	conv, err := m.deps.Store.Create(context.Background())
	if err != nil {
		m.status.Error = m.redactError(err)
		return
	}
	if err := validateConversationCandidate(conv, ""); err != nil {
		m.status.Error = m.redactError(err)
		return
	}
	m.commitConversationTransition(conv, conversation.RecoveryReport{Status: conversation.RecoveryClean}, true)
}

func (m *Model) loadConversation(id string) {
	if !m.acceptsIntent() {
		return
	}
	if !m.prepareConversationTransition() {
		return
	}
	result, err := m.deps.Store.Load(context.Background(), id)
	if err != nil {
		m.status.Error = m.redactError(err)
		return
	}
	if !result.Available {
		m.status.Error = m.redactError(fmt.Errorf("该会话当前不可用，已保留原会话"))
		m.status.Notice = recoveryNotice(result.Recovery, m.redactText)
		return
	}
	if err := validateConversationCandidate(result.Conversation, id); err != nil {
		m.status.Error = m.redactError(err)
		return
	}
	m.commitConversationTransition(result.Conversation, result.Recovery, false)
}

// prepareConversationTransition completes every fallible operation concerning
// the current session before a candidate is created or loaded. A failure leaves
// the active session and every visible navigation state unchanged.
func (m *Model) prepareConversationTransition() bool {
	if m.orchestrator != nil {
		if err := m.orchestrator.WaitIdle(context.Background()); err != nil {
			m.status.Error = m.redactError(err)
			return false
		}
	}
	if m.conversation != nil && m.deps.Store != nil {
		if _, err := m.deps.Store.Save(context.Background(), m.conversation); err != nil {
			m.status.Error = m.redactError(fmt.Errorf("旧会话保存: %w", err))
			return false
		}
	}
	return true
}

// commitConversationTransition is the single publication point after the
// candidate has been created/loaded and validated.
func (m *Model) commitConversationTransition(candidate *conversation.Conversation, report conversation.RecoveryReport, created bool) {
	previous := m.conversation
	if previous != nil {
		m.hookRuntime().SessionEnd(context.Background(), previous.ID, hook.SessionEndSwitch)
	}
	m.conversation = candidate
	if m.lifecycle != nil {
		m.lifecycle.bindConversation(candidate)
	}
	if m.lifecycle != nil {
		m.lifecycle.bindConversation(candidate)
	}
	m.resetCommandState()
	m.status.Notice = recoveryNotice(report, m.redactText)
	m.screen = screenChat
	state := hook.SessionResumed
	if created {
		state = hook.SessionNew
	}
	m.hookRuntime().SessionStart(context.Background(), candidate.ID, state)
	m.status.Error = nil
}

func (m *Model) resetCommandState() {
	var messages []conversation.Message
	if m.conversation != nil {
		messages = m.conversation.Messages
	}
	m.resetRequestPresentation(messages, false)
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

// resetRequestPresentation is the single reset boundary shared by a new
// request and a conversation transition. Re-projecting authoritative
// conversation messages also drops every request-only assistant, thinking,
// tool, and independent trace buffer held by the TUI.
func (m *Model) resetRequestPresentation(messages []conversation.Message, pendingUserProjection bool) {
	m.clearRequestTransient()
	if m.messagesCleared && pendingUserProjection {
		// /clear hides prior history until an explicit conversation reload. A
		// new request must still discard every old buffer, but its
		// UserSubmitted event becomes the first newly visible message.
		m.messages.SetMessages(nil)
		pendingUserProjection = false
	} else {
		m.messages.SetMessages(messages)
	}
	if m.lifecycle != nil {
		m.lifecycle.finish()
	}
	m.request = nil
	m.streaming = false
	m.status.ResetRequest()
	m.lastError = nil
	m.confirmation = nil
	m.pendingUserProjection = pendingUserProjection
}

func (m *Model) hookRuntime() hook.Runtime {
	if m == nil || m.hooks == nil {
		return hook.Noop()
	}
	return m.hooks
}

func recoveryNotice(report conversation.RecoveryReport, redactor func(string) string) string {
	if redactor == nil {
		redactor = redact.Text
	}
	parts := []string{}
	if report.SkippedRecords > 0 {
		parts = append(parts, fmt.Sprintf("跳过 %d 条损坏会话记录", report.SkippedRecords))
	}
	if report.Status == conversation.RecoveryPartial && report.LastValidRevision > 0 {
		parts = append(parts, fmt.Sprintf("已恢复至 revision %d", report.LastValidRevision))
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

func conversationListNotice(result conversation.ListResult, redactor func(string) string) string {
	parts := make([]string, 0, len(result.Diagnostics)+1)
	if result.Truncated {
		parts = append(parts, "历史会话列表已达到扫描上限，当前显示可信的部分结果")
	}
	if redactor == nil {
		redactor = redact.Text
	}
	for _, diagnostic := range result.Diagnostics {
		text := strings.TrimSpace(diagnostic.Safe(redactor).Text())
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "；")
}

func newConversationListState(result conversation.ListResult, runtimeRedactor *redact.RuntimeRedactor) ConversationListState {
	if runtimeRedactor == nil {
		runtimeRedactor = redact.NewRuntimeRedactor()
	}
	state := ConversationListState{
		Entries:      make([]ConversationListEntryState, len(result.Entries)),
		Truncated:    result.Truncated,
		ScannedFiles: result.ScannedFiles,
		ScannedBytes: result.ScannedBytes,
		Notice:       runtimeRedactor.Redact(conversationListNotice(result, runtimeRedactor.Text)),
	}
	for index, entry := range result.Entries {
		selectable := entry.Available && entry.Recovery.Status != conversation.RecoveryPlaceholder
		state.Entries[index] = ConversationListEntryState{
			ID:             entry.Summary.ID,
			Title:          runtimeRedactor.Redact(entry.Summary.Title.Text()),
			UpdatedAt:      entry.Summary.UpdatedAt,
			MessageCount:   entry.Summary.MessageCount,
			Available:      entry.Available,
			Selectable:     selectable,
			RecoveryStatus: entry.Recovery.Status,
			RecoveryNotice: runtimeRedactor.Redact(recoveryNotice(entry.Recovery, runtimeRedactor.Text)),
		}
	}
	return state
}

func cloneConversationListState(source ConversationListState) ConversationListState {
	clone := source
	clone.Entries = append([]ConversationListEntryState(nil), source.Entries...)
	return clone
}

func validateConversationCandidate(candidate *conversation.Conversation, expectedID string) error {
	if candidate == nil || strings.TrimSpace(candidate.ID) == "" {
		return fmt.Errorf("候选会话无效")
	}
	if expectedID != "" && candidate.ID != expectedID {
		return fmt.Errorf("候选会话标识不匹配")
	}
	return nil
}

func redactWith(redactor func(string) string, value string) string {
	if redactor == nil {
		return redact.Text(value)
	}
	return redactor(value)
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
	// Snapshot and stop admission are one shared linearization point. Only the
	// first Close caller contributes Model fields; later Bubble Tea value copies
	// merely wait on the same cleanup result and never race the worker's state
	// reset.
	m.lifecycle.startCloseModel(m, func() error { return m.runCloseCleanup() })
	return m.lifecycle.waitClose(ctx)
}

// runCloseCleanup is the sole App close worker. It uses a context derived from
// Background, so cancellation of the caller that is waiting in Close cannot
// interrupt persistence or hook delivery. Every stage is attempted in order;
// a failed stage contributes a bounded diagnostic but never skips later work.
func (m *Model) runCloseCleanup() error {
	if m == nil {
		return nil
	}
	options := normalizeRuntimeOptions(m.runtimeOptions)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), options.CleanupTimeout)
	defer cancel()

	// Artifact viewers own a user read lease, not a process resource. Stop it
	// before waiting for request-owned streams so no new user read can race the
	// final state reset.
	m.closeArtifactView()

	snapshot := m.lifecycle.closeSnapshot()

	var closeErrors []string
	timedOut := false
	diagnosedTimeout := false
	recordFailure := func(label, code string, err error) {
		if err == nil {
			return
		}
		if errors.Is(err, errAppCleanupTimeout) || errors.Is(err, context.DeadlineExceeded) {
			if !diagnosedTimeout {
				diagnosedTimeout = true
				m.addCleanupDiagnostic(appCleanupTimeoutDiagnosticCode, "cleanup_timeout", errAppCleanupTimeout)
			}
			timedOut = true
			return
		}
		closeErrors = append(closeErrors, label+": "+m.redactText(err.Error()))
		m.addCleanupDiagnostic(code, "cleanup_failed", err)
	}

	// Task subscriptions and foreground waits are App-owned consumers. Stop
	// them before TaskManager shutdown; canceling these waits never calls the
	// task-level Cancel operation. TaskManager then stops admission and joins
	// every producer before ordinary interactive resources are released.
	if m.taskEvents != nil {
		m.taskEvents.close()
	}
	if m.deps.Tasks != nil {
		if timedOut {
			appCleanupFireAndForget(m.deps.Tasks.Shutdown)
		} else {
			err, hitDeadline := appCleanupCall(cleanupCtx, m.deps.Tasks.Shutdown)
			if hitDeadline {
				timedOut = true
			}
			recordFailure("等待子任务收尾", appTaskShutdownDiagnosticCode, err)
		}
	}

	// Stop/cancel is synchronous and precedes WaitIdle. A hostile Cancel
	// implementation must not prevent subsequent cleanup stages.
	if snapshot.request != nil {
		_, cancelErr := m.lifecycle.cancelSafely()
		recordFailure("取消当前请求", appWaitIdleDiagnosticCode, cancelErr)
	}

	if snapshot.waiter != nil {
		if timedOut {
			// The hard budget has already elapsed. Signal the owner with a
			// canceled context and move on without waiting for an uncooperative
			// implementation.
			appCleanupFireAndForget(snapshot.waiter.WaitIdle)
		} else {
			err, hitDeadline := appCleanupCall(cleanupCtx, snapshot.waiter.WaitIdle)
			if hitDeadline {
				timedOut = true
			}
			recordFailure("等待 Agent 收尾", appWaitIdleDiagnosticCode, err)
		}
	}

	// Save and SessionEnd are intentionally gated by the active conversation,
	// not by request state. A list-only/cold-start App must never call Save(nil)
	// or manufacture an exit event.
	active := snapshot.conversation
	if active != nil {
		if snapshot.store != nil {
			if timedOut {
				appCleanupFireAndForget(func(ctx context.Context) error {
					_, err := snapshot.store.Save(ctx, active)
					return err
				})
			} else {
				err, hitDeadline := appCleanupCall(cleanupCtx, func(ctx context.Context) error {
					_, err := snapshot.store.Save(ctx, active)
					return err
				})
				if hitDeadline {
					timedOut = true
				}
				recordFailure("会话保存", appSaveDiagnosticCode, err)
			}
		}
		if snapshot.hooks != nil {
			end := func(ctx context.Context) error {
				snapshot.hooks.SessionEnd(ctx, active.ID, hook.SessionEndExit)
				return nil
			}
			if timedOut {
				appCleanupFireAndForget(end)
			} else {
				err, hitDeadline := appCleanupCall(cleanupCtx, end)
				if hitDeadline {
					timedOut = true
				}
				// SessionEnd has no error return in HookLifecycle today, but keep
				// the stage through the same guarded path for panic/adapter seams.
				recordFailure("会话结束通知", appSaveDiagnosticCode, err)
			}
		}
	}

	// State publication is best effort and non-blocking. It always runs even
	// when persistence or orchestration failed, leaving all subsequent Model
	// copies with the shared lifecycle's closed marker.
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
	m.lifecycle.bindConversation(nil)

	if timedOut {
		// A timeout is the stable final result. Ordinary stage errors remain in
		// diagnostics and are included as a redacted joined error when present.
		if len(closeErrors) == 0 {
			return errAppCleanupTimeout
		}
		return errors.Join(errAppCleanupTimeout, m.appCleanupError(closeErrors))
	}
	return m.appCleanupError(closeErrors)
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

func listen(stream <-chan Event, envelope eventEnvelope) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-stream
		if !ok {
			return eventStreamClosedMsg{envelope: envelope}
		}
		return eventMsg{event: events.Clone(event), events: stream, envelope: envelope}
	}
}

type eventMsg struct {
	event    Event
	events   <-chan Event
	envelope eventEnvelope
}

type eventStreamClosedMsg struct {
	envelope eventEnvelope
}

func sanitizeInput(value string) string {
	return strings.TrimSpace(value)
}

func formatDuration(duration time.Duration) time.Duration {
	return duration.Round(time.Millisecond)
}
