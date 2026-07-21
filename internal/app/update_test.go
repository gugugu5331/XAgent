package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/mcpclient"
	"xagent/internal/memory"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/tool"
	"xagent/internal/tui"
)

func TestLoadConversationUsesRecoveringStoreAndShowsDiagnostics(t *testing.T) {
	conv := conversation.NewConversation("session", time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC))
	conversation.AppendUserMessage(conv, "hello")
	store := &fakeRecoveringStore{
		conversation: conv,
		report: conversation.RecoveryReport{
			SkippedLines:         2,
			TruncatedFromMessage: 1,
			TimeGapReminder:      true,
			Diagnostics: []diagnostics.Diagnostic{
				diagnostics.New("jsonl_recovery_bad_line", diagnostics.SeverityWarning, "token=secret"),
			},
		},
	}
	model := Model{deps: Deps{Store: store}}
	model.loadConversation("session")
	if !store.recovered {
		t.Fatal("expected Recover to be used")
	}
	if model.conversation == nil || model.conversation.ID != "session" || model.screen != screenChat {
		t.Fatalf("conversation not loaded: %#v", model)
	}
	if !strings.Contains(model.status.Notice, "跳过 2 行") || !strings.Contains(model.status.Notice, "jsonl_recovery_bad_line") {
		t.Fatalf("recovery notice missing details: %q", model.status.Notice)
	}
	if strings.Contains(model.status.Notice, "secret") {
		t.Fatalf("recovery notice leaked secret: %q", model.status.Notice)
	}
}

func TestDiagnosticsCommandDisplaysRedactedLocalDiagnostics(t *testing.T) {
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redact.Text})
	collector.Add(diagnostics.New("mcp_missing_env", diagnostics.SeverityWarning, "token=secret-token").WithSource("server"))
	model := Model{screen: screenChat, input: tui.NewInput(""), diagnostics: collector, deps: Deps{Diagnostics: collector}}
	model = submitLocalCommand(t, model, "/diagnostics")
	if !strings.Contains(model.status.Notice, "本地诊断") || !strings.Contains(model.status.Notice, "mcp_missing_env") {
		t.Fatalf("diagnostics notice missing details: %q", model.status.Notice)
	}
	if strings.Contains(model.status.Notice, "secret-token") {
		t.Fatalf("diagnostics command leaked secret: %q", model.status.Notice)
	}
}

func TestMemoryCommandsAreHandledLocally(t *testing.T) {
	tmp := t.TempDir()
	userDir := filepath.Join(tmp, "user")
	projectDir := filepath.Join(tmp, "project-token=sk-ant-secret")
	manager := memory.NewManager(memory.ManagerOptions{UserDir: userDir, ProjectDir: projectDir})
	note := memory.NewNote(memory.NoteProjectKnowledge, memory.ScopeProject, "API key token=sk-ant-secret", "project fact password=hunter2", "test", time.Unix(1, 0))
	if err := manager.SaveNote(note); err != nil {
		t.Fatalf("save note: %v", err)
	}

	model := Model{screen: screenChat, input: tui.NewInput(""), deps: Deps{Memory: manager}}

	model = submitMemoryCommand(t, model, "/memory status")
	if model.status.Error != nil {
		t.Fatalf("status command failed: %v", model.status.Error)
	}
	if !strings.Contains(model.status.Notice, "memory 状态") || strings.Contains(model.status.Notice, "sk-ant-secret") {
		t.Fatalf("status notice was not redacted or missing status: %q", model.status.Notice)
	}

	model = submitMemoryCommand(t, model, "/memory index")
	if model.status.Error != nil {
		t.Fatalf("index command failed: %v", model.status.Error)
	}
	if !strings.Contains(model.status.Notice, "索引: 1 条") || strings.Contains(model.status.Notice, "sk-ant-secret") || strings.Contains(model.status.Notice, "hunter2") {
		t.Fatalf("index notice was not redacted or missing entry: %q", model.status.Notice)
	}

	model = submitMemoryCommand(t, model, "/memory off")
	if model.status.Error != nil {
		t.Fatalf("off command failed: %v", model.status.Error)
	}
	if !manager.Status().ProjectDisabled {
		t.Fatalf("off command did not disable project memory")
	}

	model = submitMemoryCommand(t, model, "/memory rebuild project")
	if model.status.Error != nil {
		t.Fatalf("rebuild command failed: %v", model.status.Error)
	}
	if !strings.Contains(model.status.Notice, "索引已重建，1 条") {
		t.Fatalf("unexpected rebuild notice: %q", model.status.Notice)
	}

	model = submitMemoryCommand(t, model, "/memory delete project "+note.ID)
	if model.status.Error != nil {
		t.Fatalf("delete command failed: %v", model.status.Error)
	}
	index, err := manager.LoadIndex(memory.ScopeProject)
	if err != nil {
		t.Fatalf("load index after delete: %v", err)
	}
	if len(index.Entries) != 0 {
		t.Fatalf("delete command left %d entries", len(index.Entries))
	}

	model = submitMemoryCommand(t, model, "/memory rebuild nope")
	if model.status.Error == nil || !strings.Contains(model.status.Error.Error(), "user 或 project") {
		t.Fatalf("invalid scope did not produce local status error: %#v", model.status.Error)
	}
}

func TestModelTracksRequestSessionLifecycle(t *testing.T) {
	cancelled := false
	model := Model{streaming: true, request: &RequestSession{Cancel: func() { cancelled = true }}, input: tui.NewInput("")}
	updated, _ := model.Update(keyMsg("esc"))
	model = updated.(Model)
	if !cancelled || model.request != nil || model.streaming || model.status.Streaming || !model.input.Text.Focused() {
		t.Fatalf("request was not cancelled and cleared: cancelled=%v model=%#v", cancelled, model)
	}

	model = Model{streaming: true, request: &RequestSession{Cancel: func() {}}, input: tui.NewInput("")}
	updated, _ = model.Update(eventMsg{event: events.Event{Type: events.Done}, events: closedEvents()})
	model = updated.(Model)
	if model.request != nil || model.streaming || model.status.Streaming {
		t.Fatalf("done did not clear request session: %#v", model)
	}
}

func TestTransientEventLifecycle(t *testing.T) {
	model := Model{
		screen: screenChat, streaming: true, input: tui.NewInput(""), messages: tui.NewMessagesView(true),
		request: &RequestSession{Cancel: func() {}, Independent: true},
		status:  tui.Status{Streaming: true, RequestModel: "review-model", ActiveSkills: "review"},
	}
	model.messages.AppendUser("/review")
	eventsToApply := []events.Event{
		{Type: events.ThinkingDelta, Text: "temporary thinking", Transient: true, IndependentID: "run-1"},
		{Type: events.TextDelta, Text: "temporary text", Transient: true, IndependentID: "run-1"},
		{Type: events.ToolRunning, Transient: true, IndependentID: "run-1", Tool: &events.ToolDisplay{
			CallID: "call-1", Name: "Read", Arguments: `{"path":"main.go"}`, Status: events.ToolDisplayRunning,
		}},
	}
	for _, event := range eventsToApply {
		updated, _ := model.Update(eventMsg{event: event, events: closedEvents()})
		model = updated.(Model)
	}
	for _, want := range []string{"temporary thinking", "temporary text", "● Read(main.go)"} {
		if !strings.Contains(model.messages.View(), want) {
			t.Fatalf("live transient view missing %q: %q", want, model.messages.View())
		}
	}

	updated, _ := model.Update(eventMsg{event: events.Event{Type: events.TextDelta, Text: "final summary"}, events: closedEvents()})
	model = updated.(Model)
	if strings.Contains(model.messages.View(), "temporary") || strings.Contains(model.messages.View(), "● Read") || !strings.Contains(model.messages.View(), "final summary") {
		t.Fatalf("final summary did not replace transient trace: %q", model.messages.View())
	}
	updated, _ = model.Update(eventMsg{event: events.Event{Type: events.Done}, events: closedEvents()})
	model = updated.(Model)
	if model.request != nil || model.streaming || model.status.RequestModel != "" || model.status.ActiveSkills != "" {
		t.Fatalf("done did not restore request status: %#v", model)
	}
	if strings.Count(model.messages.View(), "final summary") != 1 {
		t.Fatalf("final summary was not committed exactly once: %q", model.messages.View())
	}
}

func TestTransientFailureAndCancelCleanup(t *testing.T) {
	newModel := func(cancel context.CancelFunc) Model {
		model := Model{
			screen: screenChat, streaming: true, input: tui.NewInput(""), messages: tui.NewMessagesView(false),
			request: &RequestSession{Cancel: cancel, Independent: true},
			status:  tui.Status{Streaming: true, RequestModel: "review-model"},
		}
		model.messages.AppendUser("/review")
		return model
	}

	model := newModel(func() {})
	updated, _ := model.Update(eventMsg{event: events.Event{Type: events.TextDelta, Text: "temporary", Transient: true, IndependentID: "run-1"}, events: closedEvents()})
	model = updated.(Model)
	updated, _ = model.Update(eventMsg{event: events.Event{Type: events.Error, Err: errors.New("failed")}, events: closedEvents()})
	model = updated.(Model)
	if strings.Contains(model.messages.View(), "temporary") || model.request != nil || model.status.RequestModel != "" || model.status.Error == nil {
		t.Fatalf("failure retained transient state: messages=%q model=%#v", model.messages.View(), model)
	}

	cancelled := false
	model = newModel(func() { cancelled = true })
	updated, _ = model.Update(eventMsg{event: events.Event{Type: events.TextDelta, Text: "cancel me", Transient: true, IndependentID: "run-2"}, events: closedEvents()})
	model = updated.(Model)
	updated, _ = model.Update(keyMsg("esc"))
	model = updated.(Model)
	if !cancelled || strings.Contains(model.messages.View(), "cancel me") || model.request != nil || model.status.RequestModel != "" {
		t.Fatalf("cancel retained transient state: cancelled=%t messages=%q model=%#v", cancelled, model.messages.View(), model)
	}
}

func TestMainTraceResetDropsParentIterationBuffers(t *testing.T) {
	conv := &conversation.Conversation{Messages: []conversation.Message{{Role: conversation.RoleUser, Content: "review this"}}}
	model := Model{
		screen: screenChat, streaming: true, input: tui.NewInput(""), messages: tui.NewMessagesView(true),
		conversation: conv, request: &RequestSession{Cancel: func() {}}, status: tui.Status{Streaming: true},
	}
	for _, event := range []events.Event{
		{Type: events.ThinkingDelta, Text: "parent thinking"},
		{Type: events.TextDelta, Text: "parent preamble"},
		{Type: events.MainTraceReset},
		{Type: events.TextDelta, Text: "isolated summary"},
	} {
		updated, _ := model.Update(eventMsg{event: event, events: closedEvents()})
		model = updated.(Model)
	}
	updated, _ := model.Update(eventMsg{event: events.Event{Type: events.Done}, events: closedEvents()})
	model = updated.(Model)
	output := model.messages.View()
	if strings.Contains(output, "parent preamble") || strings.Contains(output, "parent thinking") || strings.Count(output, "isolated summary") != 1 {
		t.Fatalf("main trace reset did not replace parent iteration: %q", output)
	}
}

func TestStreamingKeysCancelBeforeQuit(t *testing.T) {
	cancelled := false
	closer := &fakeCloser{}
	model := Model{streaming: true, request: &RequestSession{Cancel: func() { cancelled = true }}, input: tui.NewInput(""), deps: Deps{Closer: closer}}
	updated, cmd := model.Update(keyMsg("ctrl+c"))
	model = updated.(Model)
	if cmd != nil || closer.closed {
		t.Fatalf("ctrl+c during streaming should cancel, not quit: cmd=%v closer=%#v", cmd, closer)
	}
	if !cancelled || model.streaming || model.request != nil || !strings.Contains(model.status.Notice, "取消") {
		t.Fatalf("streaming ctrl+c did not cancel request: cancelled=%v model=%#v", cancelled, model)
	}
}

func TestNewSyncsMCPDiagnosticsToCollector(t *testing.T) {
	store := &fakeRecoveringStore{}
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redact.Text})
	status := &fakeCloser{summary: mcpclient.StatusSummary{Configured: 1, Failed: 1, Diagnostics: []mcpclient.Diagnostic{{Server: "srv", Message: "api_key=secret-key"}}}}
	deps := Deps{Config: testAppConfig(), Provider: fakeProvider{name: "fake"}, Store: store, Resources: fakeResources{}, Diagnostics: collector, MCPStatus: status}
	model := New(deps)
	if model.diagnostics.Count() != 1 {
		t.Fatalf("expected one MCP diagnostic in collector, got %d", model.diagnostics.Count())
	}
	items := model.diagnostics.List()
	if items[0].Code != "mcp_status" || items[0].Source != "srv" || strings.Contains(items[0].Message, "secret-key") {
		t.Fatalf("unexpected synced diagnostic: %#v", items[0])
	}
}

func TestMCPStatusCommandDisplaysRedactedDetails(t *testing.T) {
	status := &fakeCloser{summary: mcpclient.StatusSummary{Configured: 2, Ready: 1, Failed: 1, Diagnostics: []mcpclient.Diagnostic{{Server: "server", Message: "Authorization: Bearer abc123"}}}}
	model := Model{screen: screenChat, input: tui.NewInput(""), deps: Deps{MCPStatus: status}}
	model = submitLocalCommand(t, model, "/mcp status")
	if !strings.Contains(model.status.Notice, "本地 MCP 状态") || !strings.Contains(model.status.Notice, "failed=1") {
		t.Fatalf("mcp status missing summary: %q", model.status.Notice)
	}
	if strings.Contains(model.status.Notice, "abc123") {
		t.Fatalf("mcp status leaked secret: %q", model.status.Notice)
	}
}

func TestPermissionsStatusCommandHandledLocally(t *testing.T) {
	root := t.TempDir()
	registry, _ := tool.NewRegistry(root)
	executor := tool.NewExecutor(registry, root, time.Second, 1024)
	deps := Deps{Config: testAppConfig(), Provider: fakeProvider{name: "fake"}, Store: &fakeRecoveringStore{}, Resources: fakeResources{}, Registry: registry, Executor: executor}
	model := New(deps)
	model.screen = screenChat
	model.input = tui.NewInput("")
	model = submitLocalCommand(t, model, "/permissions status")
	if !strings.Contains(model.status.Notice, "本地权限状态") || !strings.Contains(model.status.Notice, "mode=default") {
		t.Fatalf("permissions status missing details: %q", model.status.Notice)
	}
}

func TestUsageUpdatesAccumulateCacheFields(t *testing.T) {
	model := Model{}
	msg := eventMsg{event: events.Event{Type: events.UsageUpdated, Usage: &events.UsageDisplay{
		InputTokens:              1,
		OutputTokens:             2,
		CacheCreationInputTokens: 3,
		CacheReadInputTokens:     4,
	}}, events: closedEvents()}
	updated, _ := model.Update(msg)
	model = updated.(Model)
	msg = eventMsg{event: events.Event{Type: events.UsageUpdated, Usage: &events.UsageDisplay{
		InputTokens:              10,
		OutputTokens:             20,
		CacheCreationInputTokens: 30,
		CacheReadInputTokens:     40,
	}}, events: closedEvents()}
	updated, _ = model.Update(msg)
	model = updated.(Model)
	if model.status.InputTokens != 11 || model.status.OutputTokens != 22 || model.status.CacheCreationInputTokens != 33 || model.status.CacheReadInputTokens != 44 {
		t.Fatalf("usage was not accumulated: %#v", model.status)
	}
}

func TestConfirmationKeysSendPermissionActions(t *testing.T) {
	cases := []struct {
		key     string
		action  events.PermissionAction
		allowed bool
	}{
		{key: "y", action: events.PermissionAllowOnce, allowed: true},
		{key: "s", action: events.PermissionAllowSession, allowed: true},
		{key: "p", action: events.PermissionAllowPermanent, allowed: true},
		{key: "n", action: events.PermissionDeny, allowed: false},
		{key: "esc", action: events.PermissionCancel, allowed: false},
		{key: "enter", action: events.PermissionDeny, allowed: false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			decisionCh := make(chan events.ToolConfirmationDecision, 1)
			request := &events.ToolConfirmationRequest{CallID: "call", Decision: decisionCh, AllowPermanent: tc.action == events.PermissionAllowPermanent}
			model := Model{confirmation: &Event{Confirmation: request}}
			updated, _ := model.Update(keyMsg(tc.key))
			model = updated.(Model)
			if model.confirmation != nil || model.status.WaitingConfirmation {
				t.Fatalf("confirmation not cleared: %#v", model)
			}
			decision := <-decisionCh
			if decision.Action != tc.action || decision.Allowed != tc.allowed {
				t.Fatalf("unexpected decision: %#v", decision)
			}
		})
	}
}

func TestPermanentConfirmationKeyIgnoredWhenDisabled(t *testing.T) {
	decisionCh := make(chan events.ToolConfirmationDecision, 1)
	model := Model{confirmation: &Event{Confirmation: &events.ToolConfirmationRequest{CallID: "call", Decision: decisionCh, AllowPermanent: false}}}
	updated, _ := model.Update(keyMsg("p"))
	model = updated.(Model)
	if model.confirmation == nil {
		t.Fatal("confirmation should remain active when permanent allow is disabled")
	}
	select {
	case decision := <-decisionCh:
		t.Fatalf("unexpected decision sent: %#v", decision)
	default:
	}
}

func TestQuitClosesDepsCloser(t *testing.T) {
	closer := &fakeCloser{}
	model := Model{deps: Deps{Closer: closer}}
	updated, cmd := model.Update(keyMsg("q"))
	model = updated.(Model)
	if cmd == nil {
		t.Fatal("expected quit command")
	}
	if !closer.closed {
		t.Fatal("expected closer to be called")
	}
}

func TestCloseReportsMCPStatusAndCloseError(t *testing.T) {
	closer := &fakeCloser{status: "1 ready, 1 failed", err: errors.New("close failed")}
	model := Model{deps: Deps{Closer: closer, MCPStatus: closer}}
	model.close()
	if !closer.closed {
		t.Fatal("expected closer to be called")
	}
	if model.status.MCP != "1 ready, 1 failed" {
		t.Fatalf("expected MCP status to refresh, got %q", model.status.MCP)
	}
	if model.status.Error == nil || model.status.Error.Error() != "MCP close: close failed" {
		t.Fatalf("expected close error to be visible, got %v", model.status.Error)
	}
}

func submitMemoryCommand(t *testing.T, model Model, command string) Model {
	t.Helper()
	return submitLocalCommand(t, model, command)
}

func submitLocalCommand(t *testing.T, model Model, command string) Model {
	t.Helper()
	model.input.Text.SetValue(command)
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("local command returned async command, likely reached orchestrator")
	}
	model = updated.(Model)
	if model.input.Value() != "" {
		t.Fatalf("local command input was not cleared: %q", model.input.Value())
	}
	return model
}

type fakeRecoveringStore struct {
	conversation *conversation.Conversation
	report       conversation.RecoveryReport
	recovered    bool
}

func (s *fakeRecoveringStore) List(ctx context.Context) ([]conversation.Conversation, error) {
	if s.conversation == nil {
		return nil, nil
	}
	return []conversation.Conversation{*s.conversation}, nil
}

func (s *fakeRecoveringStore) Load(ctx context.Context, id string) (*conversation.Conversation, error) {
	return s.conversation, nil
}

func (s *fakeRecoveringStore) Save(ctx context.Context, conversation *conversation.Conversation) error {
	s.conversation = conversation
	return nil
}

func (s *fakeRecoveringStore) Create(ctx context.Context) (*conversation.Conversation, error) {
	return conversation.NewConversation("new", time.Now()), nil
}

func (s *fakeRecoveringStore) Recover(ctx context.Context, id string) (*conversation.Conversation, conversation.RecoveryReport, error) {
	s.recovered = true
	return s.conversation, s.report, nil
}

func keyMsg(key string) tea.KeyMsg {
	switch key {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
}

type fakeCloser struct {
	closed  bool
	status  string
	summary mcpclient.StatusSummary
	err     error
}

func (f *fakeCloser) Close(ctx context.Context) error {
	f.closed = true
	return f.err
}

func (f *fakeCloser) StatusLine() string {
	return f.status
}

func (f *fakeCloser) Summary() mcpclient.StatusSummary {
	return f.summary
}

func (f *fakeCloser) Diagnostics() []mcpclient.Diagnostic {
	return f.summary.Diagnostics
}

type fakeProvider struct{ name string }

func (f fakeProvider) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake"
}

func (f fakeProvider) StreamChat(ctx context.Context, req provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	ch := make(chan provider.StreamEvent)
	close(ch)
	return ch, nil
}

type fakeResources struct{}

func (fakeResources) SystemPrompt() string      { return "" }
func (fakeResources) UILabel(key string) string { return key }

func testAppConfig() *config.AppConfig {
	return &config.AppConfig{LLM: config.LLMConfig{Model: "fake", RequestTimeoutMS: 1000}, UI: config.UIConfig{StartMode: config.StartModeList}}
}

func closedEvents() <-chan Event {
	ch := make(chan Event)
	close(ch)
	return ch
}
