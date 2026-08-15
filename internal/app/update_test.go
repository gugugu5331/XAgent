package app

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

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

func TestLoadConversationUsesStoreResultAndShowsDiagnostics(t *testing.T) {
	conv := conversation.NewConversation("session", time.Date(2026, 7, 6, 10, 0, 0, 0, time.UTC))
	conversation.AppendUserMessage(conv, "hello")
	store := &fakeConversationStore{
		conversation: conv,
		report: conversation.RecoveryReport{
			Status:            conversation.RecoveryPartial,
			LastValidRevision: 1,
			SkippedRecords:    2,
			Diagnostics: []diagnostics.Diagnostic{
				diagnostics.New("jsonl_recovery_bad_line", diagnostics.SeverityWarning, "token=secret"),
			},
		},
	}
	model := Model{deps: Deps{Store: store}}
	model.loadConversation("session")
	if !store.loaded {
		t.Fatal("expected Store.Load to be used")
	}
	if model.conversation == nil || model.conversation.ID != "session" || model.screen != screenChat {
		t.Fatalf("conversation not loaded: %#v", model)
	}
	if !strings.Contains(model.status.Notice, "跳过 2 条") || !strings.Contains(model.status.Notice, "jsonl_recovery_bad_line") {
		t.Fatalf("recovery notice missing details: %q", model.status.Notice)
	}
	if strings.Contains(model.status.Notice, "secret") {
		t.Fatalf("recovery notice leaked secret: %q", model.status.Notice)
	}
}

func TestConversationListErrorIsVisibleAndBlocksAutomaticNewSession(t *testing.T) {
	cfg := testAppConfig()
	cfg.UI.StartMode = config.StartModeNew
	store := &fakeConversationStore{listErr: errors.New("list token=secret failed")}

	model := New(Deps{
		Config: cfg, Provider: fakeProvider{name: "fake"}, Store: store,
		Resources: fakeResources{}, Redact: redact.Text,
	})

	if model.screen != screenList || model.conversation != nil {
		t.Fatalf("untrusted list result changed page: screen=%s conversation=%#v", model.screen, model.conversation)
	}
	if model.status.Error == nil || strings.Contains(model.status.Error.Error(), "secret") {
		t.Fatalf("list error was hidden or unsafe: %v", model.status.Error)
	}
	if got := store.calls; len(got) != 1 || got[0] != "list" {
		t.Fatalf("list failure continued startup navigation: %#v", got)
	}
	if got := len(model.list.Items()); got != 1 {
		t.Fatalf("list error was presented as trusted history: %d items", got)
	}
}

func TestConversationListKeepsPartialAndPlaceholderEntries(t *testing.T) {
	runtimeRedactor := redact.NewRuntimeRedactor()
	store := &fakeConversationStore{listResult: conversation.ListResult{
		Truncated: true,
		Entries: []conversation.ListEntry{
			{
				Summary:   conversation.ConversationSummary{ID: "partial", Title: runtimeRedactor.Redact("Partial"), UpdatedAt: time.Unix(2, 0)},
				Available: true,
				Recovery:  conversation.RecoveryReport{Status: conversation.RecoveryPartial, LastValidRevision: 2},
			},
			{
				Summary:   conversation.ConversationSummary{ID: "placeholder", Title: runtimeRedactor.Redact("Unavailable"), UpdatedAt: time.Unix(1, 0)},
				Available: false,
				Recovery:  conversation.RecoveryReport{Status: conversation.RecoveryPlaceholder},
			},
		},
	}}
	model := New(Deps{Config: testAppConfig(), Provider: fakeProvider{name: "fake"}, Store: store, Resources: fakeResources{}})

	if got := len(model.list.Items()); got != 3 {
		t.Fatalf("partial list entries were discarded: %d items", got)
	}
	if !strings.Contains(model.status.Notice, "部分结果") {
		t.Fatalf("truncated list did not show a notice: %q", model.status.Notice)
	}
	placeholder, ok := model.list.Items()[2].(tui.ConversationItem)
	if !ok || placeholder.Available || placeholder.RecoveryStatus != string(conversation.RecoveryPlaceholder) {
		t.Fatalf("placeholder metadata was lost: %#v", model.list.Items()[2])
	}

	model.list.Select(2)
	updated, _ := model.Update(keyMsg("enter"))
	model = updated.(Model)
	if model.screen != screenList || store.loaded {
		t.Fatalf("placeholder selection attempted navigation: screen=%s loaded=%t", model.screen, store.loaded)
	}
}

func TestConversationNavigationSaveFailureDoesNotCreateOrLoad(t *testing.T) {
	current := conversation.NewConversation("current", time.Unix(1, 0))
	store := &fakeConversationStore{conversation: current, saveErr: errors.New("save failed")}
	model := Model{deps: Deps{Store: store}, conversation: current, screen: screenChat}

	model.startNewConversation()
	if model.conversation != current || model.screen != screenChat || model.status.Error == nil {
		t.Fatalf("save failure changed active session: conversation=%p screen=%s error=%v", model.conversation, model.screen, model.status.Error)
	}
	if got := store.calls; len(got) != 1 || got[0] != "save" {
		t.Fatalf("candidate was created after save failure: %#v", got)
	}

	store.calls = nil
	model.loadConversation("next")
	if model.conversation != current || model.screen != screenChat {
		t.Fatalf("save failure changed active session during load")
	}
	if got := store.calls; len(got) != 1 || got[0] != "save" {
		t.Fatalf("candidate was loaded after save failure: %#v", got)
	}
}

func TestConversationNavigationCandidateFailurePreservesCurrentSession(t *testing.T) {
	current := conversation.NewConversation("current", time.Unix(1, 0))
	store := &fakeConversationStore{conversation: current, loadErr: errors.New("load failed")}
	model := Model{deps: Deps{Store: store}, conversation: current, screen: screenChat}

	model.loadConversation("next")
	if model.conversation != current || model.screen != screenChat || model.status.Error == nil {
		t.Fatalf("load failure changed active session: conversation=%p screen=%s error=%v", model.conversation, model.screen, model.status.Error)
	}
	if got := store.calls; len(got) != 2 || got[0] != "save" || got[1] != "load" {
		t.Fatalf("navigation order = %#v, want save then load", got)
	}

	store.calls = nil
	store.loadErr = nil
	store.unavailable = true
	model.loadConversation("current")
	if model.conversation != current || model.screen != screenChat || model.status.Error == nil {
		t.Fatalf("unavailable load result changed active session")
	}
	if got := store.calls; len(got) != 2 || got[0] != "save" || got[1] != "load" {
		t.Fatalf("unavailable navigation order = %#v", got)
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

func TestCancelWaitsForTurnEnd(t *testing.T) {
	cancelled := 0
	confirmation := &Event{Confirmation: &events.ToolConfirmationRequest{CallID: "call"}}
	model := Model{
		screen:       screenChat,
		streaming:    true,
		request:      &RequestSession{Cancel: func() { cancelled++ }},
		input:        tui.NewInput(""),
		confirmation: confirmation,
		status:       tui.Status{Streaming: true, WaitingConfirmation: true, RequestModel: "model"},
	}

	updated, cmd := model.Update(keyMsg("esc"))
	model = updated.(Model)
	if cmd != nil || cancelled != 1 || model.request == nil || !model.streaming || !model.status.Streaming {
		t.Fatalf("cancel cleared the request before stream close: cancelled=%d request=%p streaming=%t", cancelled, model.request, model.streaming)
	}
	if model.confirmation != confirmation || !model.status.WaitingConfirmation || model.input.Text.Focused() {
		t.Fatalf("cancel changed confirmation/input before turn cleanup: confirmation=%p status=%#v", model.confirmation, model.status)
	}
	updated, cmd = model.Update(keyMsg("q"))
	model = updated.(Model)
	if cmd != nil || cancelled != 1 || model.request == nil {
		t.Fatalf("repeated cancel quit early or invoked cancel twice: cancelled=%d request=%p cmd=%v", cancelled, model.request, cmd)
	}

	updated, cmd = model.Update(testEventMessage(t, &model, events.Event{Type: events.Done}, closedEvents()))
	model = updated.(Model)
	if cmd == nil || model.request == nil || !model.streaming {
		t.Fatalf("terminal event cleared request before channel close: request=%p streaming=%t cmd=%v", model.request, model.streaming, cmd)
	}
	updated, cmd = model.Update(cmd())
	model = updated.(Model)
	if cmd != nil || model.request != nil || model.streaming || model.status.Streaming || model.status.WaitingConfirmation || model.confirmation != nil || !model.input.Text.Focused() {
		t.Fatalf("stream close did not finish request cleanup: %#v", model.status)
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
		{Type: events.ThinkingDelta, Text: appSafeText("temporary thinking"), Transient: true, IndependentID: "run-1"},
		{Type: events.TextDelta, Text: appSafeText("temporary text"), Transient: true, IndependentID: "run-1"},
		{Type: events.ToolRunning, Transient: true, IndependentID: "run-1", Tool: &events.ToolDisplay{
			CallID: "call-1", Name: "Read", Arguments: appSafeText(`{"path":"main.go"}`), Status: events.ToolDisplayRunning,
		}},
	}
	for _, event := range eventsToApply {
		updated, _ := model.Update(testEventMessage(t, &model, event, closedEvents()))
		model = updated.(Model)
	}
	for _, want := range []string{"temporary thinking", "temporary text", "● Read(main.go)"} {
		if !strings.Contains(model.messages.View(), want) {
			t.Fatalf("live transient view missing %q: %q", want, model.messages.View())
		}
	}

	updated, _ := model.Update(testEventMessage(t, &model, events.Event{Type: events.TextDelta, Text: appSafeText("final summary")}, closedEvents()))
	model = updated.(Model)
	if strings.Contains(model.messages.View(), "temporary") || strings.Contains(model.messages.View(), "● Read") || !strings.Contains(model.messages.View(), "final summary") {
		t.Fatalf("final summary did not replace transient trace: %q", model.messages.View())
	}
	updated, cmd := model.Update(testEventMessage(t, &model, events.Event{Type: events.Done}, closedEvents()))
	model = updated.(Model)
	model = finishClosedEventStream(t, model, cmd)
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
	updated, _ := model.Update(testEventMessage(t, &model, events.Event{Type: events.TextDelta, Text: appSafeText("temporary"), Transient: true, IndependentID: "run-1"}, closedEvents()))
	model = updated.(Model)
	updated, cmd := model.Update(testEventMessage(t, &model, events.Event{Type: events.Error, Err: appSafeError("provider_failed", "failed")}, closedEvents()))
	model = updated.(Model)
	if !strings.Contains(model.messages.View(), "temporary") || model.request == nil {
		t.Fatalf("terminal error cleared transient state before stream close: messages=%q request=%p", model.messages.View(), model.request)
	}
	model = finishClosedEventStream(t, model, cmd)
	if strings.Contains(model.messages.View(), "temporary") || model.request != nil || model.status.RequestModel != "" || model.status.Error == nil {
		t.Fatalf("failure retained transient state: messages=%q model=%#v", model.messages.View(), model)
	}

	cancelled := false
	model = newModel(func() { cancelled = true })
	updated, _ = model.Update(testEventMessage(t, &model, events.Event{Type: events.TextDelta, Text: appSafeText("cancel me"), Transient: true, IndependentID: "run-2"}, closedEvents()))
	model = updated.(Model)
	updated, _ = model.Update(keyMsg("esc"))
	model = updated.(Model)
	if !cancelled || !strings.Contains(model.messages.View(), "cancel me") || model.request == nil || model.status.RequestModel == "" {
		t.Fatalf("cancel cleared transient state too early: cancelled=%t messages=%q request=%p", cancelled, model.messages.View(), model.request)
	}
	updated, _ = model.Update(testEventStreamClosedMessage(t, &model))
	model = updated.(Model)
	if strings.Contains(model.messages.View(), "cancel me") || model.request != nil || model.status.RequestModel != "" {
		t.Fatalf("stream close retained canceled state: messages=%q model=%#v", model.messages.View(), model)
	}
}

func TestMainTraceResetDropsParentIterationBuffers(t *testing.T) {
	conv := &conversation.Conversation{Messages: []conversation.Message{{Role: conversation.RoleUser, Content: redact.NewRuntimeRedactor().Redact("review this")}}}
	model := Model{
		screen: screenChat, streaming: true, input: tui.NewInput(""), messages: tui.NewMessagesView(true),
		conversation: conv, request: &RequestSession{Cancel: func() {}}, status: tui.Status{Streaming: true},
	}
	for _, event := range []events.Event{
		{Type: events.ThinkingDelta, Text: appSafeText("parent thinking")},
		{Type: events.TextDelta, Text: appSafeText("parent preamble")},
		{Type: events.MainTraceReset},
		{Type: events.TextDelta, Text: appSafeText("isolated summary")},
	} {
		updated, _ := model.Update(testEventMessage(t, &model, event, closedEvents()))
		model = updated.(Model)
	}
	updated, _ := model.Update(testEventMessage(t, &model, events.Event{Type: events.Done}, closedEvents()))
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
	if !cancelled || !model.streaming || model.request == nil || !strings.Contains(model.status.Notice, "取消") {
		t.Fatalf("streaming ctrl+c did not cancel request: cancelled=%v model=%#v", cancelled, model)
	}
}

func TestNewSyncsMCPDiagnosticsToCollector(t *testing.T) {
	store := &fakeConversationStore{}
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
	deps := Deps{Config: testAppConfig(), Provider: fakeProvider{name: "fake"}, Store: &fakeConversationStore{}, Resources: fakeResources{}, Registry: registry, Executor: executor}
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
	msg := testEventMessage(t, &model, events.Event{Type: events.UsageUpdated, Usage: &events.UsageDisplay{
		InputTokens:              1,
		OutputTokens:             2,
		CacheCreationInputTokens: 3,
		CacheReadInputTokens:     4,
	}}, closedEvents())
	updated, _ := model.Update(msg)
	model = updated.(Model)
	msg = testEventMessage(t, &model, events.Event{Type: events.UsageUpdated, Usage: &events.UsageDisplay{
		InputTokens:              10,
		OutputTokens:             20,
		CacheCreationInputTokens: 30,
		CacheReadInputTokens:     40,
	}}, closedEvents())
	updated, _ = model.Update(msg)
	model = updated.(Model)
	if model.status.InputTokens != 11 || model.status.OutputTokens != 22 || model.status.CacheCreationInputTokens != 33 || model.status.CacheReadInputTokens != 44 {
		t.Fatalf("usage was not accumulated: %#v", model.status)
	}
}

func TestStaleRequestEventCannotCrossResetBoundary(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      2,
		MaxItemBytes:  256,
		MaxTotalBytes: 512,
	})
	if err != nil {
		t.Fatalf("new bounded event diagnostic sink: %v", err)
	}
	boundary := newEventBoundaryState(sink)
	runtimeState := RuntimeState{RequestSequence: 1}
	conversationState := ConversationState{ActiveID: "old-conversation", Mode: "plan", SkillGeneration: 1}
	requestState := RequestState{Generation: 1}

	textCandidate, ok := sealStateEvent(requestState, conversationState, events.Event{
		Type: events.TextDelta, Text: redactor.Redact("current text"),
	})
	if !ok || !boundary.applyStateEvent(textCandidate, &runtimeState, &conversationState, &requestState) {
		t.Fatal("matching conversation event was not applied")
	}
	transientCandidate, ok := sealStateEvent(requestState, conversationState, events.Event{
		Type: events.TextDelta, Text: redactor.Redact("temporary text"), Transient: true, IndependentID: "run-1",
	})
	if !ok || !boundary.applyStateEvent(transientCandidate, &runtimeState, &conversationState, &requestState) {
		t.Fatal("matching transient event was not applied")
	}
	usage := &events.UsageDisplay{InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: 3, CacheReadInputTokens: 4}
	usageCandidate, ok := sealStateEvent(requestState, conversationState, events.Event{Type: events.UsageUpdated, Usage: usage})
	if !ok {
		t.Fatal("matching usage event was not sealed")
	}
	usage.InputTokens = 999
	if !boundary.applyStateEvent(usageCandidate, &runtimeState, &conversationState, &requestState) ||
		len(conversationState.Messages) != 1 || conversationState.Messages[0].Text() != "current text" ||
		requestState.Tokens != (Usage{InputTokens: 1, OutputTokens: 2}) ||
		requestState.Cache != (CacheUsage{CacheCreationInputTokens: 3, CacheReadInputTokens: 4}) ||
		!reflect.DeepEqual(requestState.TransientIDs, []string{"run-1"}) {
		t.Fatalf("matching safe events were not projected from their sealed snapshots: conversation=%#v request=%#v", conversationState, requestState)
	}
	matchingEvents := []events.Event{
		{Type: events.Done, Duration: 2 * time.Second},
		{Type: events.AgentProgressed, Progress: &events.AgentProgress{StopReason: "complete"}},
		{Type: events.Error, Err: &diagnostics.SafeError{Code: "safe_error", Source: "test", Message: redactor.Redact("safe error"), Recoverable: true}},
		{Type: events.ToolWaitingConfirmation, Confirmation: &events.ToolConfirmationRequest{
			ConfirmationID: "confirmation-1", CallID: "call-1", Name: "Bash", Prompt: redactor.Redact("confirm"),
		}},
	}
	for _, event := range matchingEvents {
		candidate, sealed := sealStateEvent(requestState, conversationState, event)
		if !sealed || !boundary.applyStateEvent(candidate, &runtimeState, &conversationState, &requestState) {
			t.Fatalf("matching request event %q was not applied", event.Type)
		}
	}
	if requestState.Duration != 2*time.Second || requestState.StopReason != "complete" || requestState.LastError == nil || requestState.LastError.Code != "safe_error" ||
		requestState.Confirmation == nil || requestState.Confirmation.CallID != "call-1" {
		t.Fatalf("matching request event matrix was not projected: %#v", requestState)
	}

	canary := "stale-event-secret-canary"
	staleRequestCandidates := sealEventBoundaryTestMatrix(t, requestState, conversationState, redactor, canary+"-request")
	runtimeState.resetRequest(&conversationState, &requestState)
	wantRuntime := runtimeState
	wantConversation := cloneNavigationConversationState(conversationState)
	wantRequest := cloneEventTestRequestState(requestState)
	for _, candidate := range staleRequestCandidates {
		if boundary.applyStateEvent(candidate, &runtimeState, &conversationState, &requestState) {
			t.Fatal("old generation event crossed request reset")
		}
	}
	if !reflect.DeepEqual(runtimeState, wantRuntime) || !reflect.DeepEqual(conversationState, wantConversation) || !reflect.DeepEqual(requestState, wantRequest) {
		t.Fatalf("old generation matrix changed state after request reset: runtime=%#v conversation=%#v request=%#v", runtimeState, conversationState, requestState)
	}

	staleConversationCandidates := sealEventBoundaryTestMatrix(t, requestState, conversationState, redactor, canary+"-conversation")
	activity := &recordingSkillActivity{}
	runtimeState.resetConversation(&conversationState, &requestState, "new-conversation", activity)
	wantRuntime = runtimeState
	wantConversation = cloneNavigationConversationState(conversationState)
	wantRequest = cloneEventTestRequestState(requestState)
	for _, candidate := range staleConversationCandidates {
		if boundary.applyStateEvent(candidate, &runtimeState, &conversationState, &requestState) {
			t.Fatal("old session event crossed conversation reset")
		}
	}
	if !reflect.DeepEqual(runtimeState, wantRuntime) || !reflect.DeepEqual(conversationState, wantConversation) || !reflect.DeepEqual(requestState, wantRequest) || activity.clearCalls != 1 {
		t.Fatalf("old session matrix changed state after conversation reset: runtime=%#v conversation=%#v request=%#v clears=%d", runtimeState, conversationState, requestState, activity.clearCalls)
	}

	snapshot := sink.Snapshot()
	items := snapshot.Items()
	if len(items) != 1 || items[0].Count != 12 || items[0].Diagnostic.Code != staleRequestEventDiagnosticCode ||
		items[0].Diagnostic.Source != staleRequestEventDiagnosticSource || items[0].Diagnostic.Message.Text() != "" ||
		strings.Contains(items[0].Diagnostic.Message.Text(), canary) {
		t.Fatalf("stale diagnostics are not bounded and content-free: items=%#v dropped=%d", items, snapshot.Dropped())
	}

	currentCandidate, ok := sealStateEvent(requestState, conversationState, events.Event{Type: events.TextDelta, Text: redactor.Redact("new current event")})
	if !ok || !boundary.applyStateEvent(currentCandidate, &runtimeState, &conversationState, &requestState) ||
		len(conversationState.Messages) != 1 || conversationState.Messages[0].Text() != "new current event" {
		t.Fatalf("current event was not applied after conversation reset: conversation=%#v request=%#v", conversationState, requestState)
	}
}

func sealEventBoundaryTestMatrix(
	t *testing.T,
	request RequestState,
	conversation ConversationState,
	redactor *redact.RuntimeRedactor,
	canary string,
) []*sealedEventEnvelope {
	t.Helper()
	eventsToSeal := []events.Event{
		{Type: events.Done, Duration: 99 * time.Second},
		{Type: events.UsageUpdated, Usage: &events.UsageDisplay{
			InputTokens: 100, OutputTokens: 200, CacheCreationInputTokens: 300, CacheReadInputTokens: 400,
		}},
		{Type: events.AgentProgressed, Progress: &events.AgentProgress{StopReason: "stale-stop", Message: redactor.Redact(canary)}},
		{Type: events.Error, Err: &diagnostics.SafeError{Code: "stale_error", Source: "test", Message: redactor.Redact(canary), Recoverable: true}},
		{Type: events.ToolWaitingConfirmation, Confirmation: &events.ToolConfirmationRequest{
			ConfirmationID: "stale-confirmation", CallID: "stale-call", Name: "Bash", Prompt: redactor.Redact(canary),
		}},
		{Type: events.TextDelta, Text: redactor.Redact(canary), Transient: true, IndependentID: "stale-run"},
	}
	sealed := make([]*sealedEventEnvelope, len(eventsToSeal))
	for index, event := range eventsToSeal {
		candidate, ok := sealStateEvent(request, conversation, event)
		if !ok {
			t.Fatalf("could not seal stale event %q", event.Type)
		}
		sealed[index] = candidate
	}
	return sealed
}

func cloneEventTestRequestState(source RequestState) RequestState {
	clone := source
	clone.LastError = cloneAppSafeError(source.LastError)
	if source.Confirmation != nil {
		confirmationClone := *source.Confirmation
		clone.Confirmation = &confirmationClone
	}
	clone.TransientIDs = append([]string(nil), source.TransientIDs...)
	return clone
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
			resolver := &recordingConfirmationResolver{}
			request := &events.ToolConfirmationRequest{ConfirmationID: "confirmation-1", CallID: "call", AllowPermanent: tc.action == events.PermissionAllowPermanent}
			model := Model{confirmation: &Event{Confirmation: request}, confirmationResolver: resolver}
			updated, _ := model.Update(keyMsg(tc.key))
			model = updated.(Model)
			if model.confirmation != nil || model.status.WaitingConfirmation {
				t.Fatalf("confirmation not cleared: %#v", model)
			}
			if len(resolver.decisions) != 1 {
				t.Fatalf("decision count = %d, want 1", len(resolver.decisions))
			}
			decision := resolver.decisions[0]
			if decision.Action != tc.action || decision.Allowed != tc.allowed {
				t.Fatalf("unexpected decision: %#v", decision)
			}
			if decision.ConfirmationID != request.ConfirmationID || decision.CallID != request.CallID {
				t.Fatalf("decision identity = %#v, want confirmation and call identity", decision)
			}
		})
	}
}

func TestPermanentConfirmationKeyIgnoredWhenDisabled(t *testing.T) {
	resolver := &recordingConfirmationResolver{}
	model := Model{confirmation: &Event{Confirmation: &events.ToolConfirmationRequest{ConfirmationID: "confirmation-1", CallID: "call", AllowPermanent: false}}, confirmationResolver: resolver}
	updated, _ := model.Update(keyMsg("p"))
	model = updated.(Model)
	if model.confirmation == nil {
		t.Fatal("confirmation should remain active when permanent allow is disabled")
	}
	if len(resolver.decisions) != 0 {
		t.Fatalf("unexpected decision sent: %#v", resolver.decisions)
	}
}

type recordingConfirmationResolver struct {
	decisions []events.ToolConfirmationDecision
}

func (resolver *recordingConfirmationResolver) ResolveToolConfirmation(decision events.ToolConfirmationDecision) bool {
	resolver.decisions = append(resolver.decisions, decision)
	return true
}

func TestQuitDefersProcessResourceClose(t *testing.T) {
	closer := &fakeCloser{}
	model := Model{deps: Deps{Closer: closer}}
	updated, cmd := model.Update(keyMsg("q"))
	model = updated.(Model)
	if cmd == nil {
		t.Fatal("expected quit command")
	}
	if closer.closed {
		t.Fatal("quit key must not close process-owned resources")
	}
}

func TestAppCloseDoesNotOwnMCPCloser(t *testing.T) {
	closer := &fakeCloser{status: "1 ready, 1 failed", err: errors.New("close failed")}
	model := Model{deps: Deps{Closer: closer, MCPStatus: closer}}
	if err := model.Close(context.Background()); err != nil {
		t.Fatalf("process-owned closer error leaked into App.Close: %v", err)
	}
	if closer.closed {
		t.Fatal("App.Close closed the process-owned MCP resource")
	}
}

func TestWindowSizePrecedesInputDispatch(t *testing.T) {
	model := newCommandTestModel(t, &recordingCommandProvider{})
	model.input.SetValue(strings.Repeat("界", 90))
	model.commandMenu.Open([]tui.CommandMenuItem{
		{Name: "one", Description: "first"},
		{Name: "two", Description: "second"},
		{Name: "three", Description: "third"},
		{Name: "four", Description: "fourth"},
	})
	redactor := redact.NewRuntimeRedactor()
	model.confirmation = &Event{Type: EventToolWaitingConfirmation, Confirmation: &events.ToolConfirmationRequest{
		ConfirmationID: "confirmation-19", CallID: "call-19", Name: "Bash",
		Prompt: redactor.Redact("legacy prompt must be replaced by the structured panel"),
		Target: redactor.Redact("Bash(command=go test ./...)"), Risk: "high", PermissionMode: "default",
		ScopePreview: redactor.Redact("exact command"),
		Scopes: []events.ConfirmationScopeDisplay{
			{Scope: "once", Available: true}, {Scope: "session", Available: true},
		},
		RuleLocation: redactor.Redact(".xagent/permissions.local.yaml"),
		Warning:      redactor.Redact("review command"), RevokeHint: redactor.Redact("remove rule"),
	}}

	updated, cmd := model.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	model = updated.(Model)
	if cmd != nil {
		t.Fatal("window resize emitted an input command")
	}
	if model.input.Value() != strings.Repeat("界", 90) || model.status.Notice != "" {
		t.Fatalf("window resize was dispatched as input: value=%q notice=%q", model.input.Value(), model.status.Notice)
	}
	if model.input.Height() != tui.CompactInputMaxHeight {
		t.Fatalf("compact resize applied input height %d, want %d", model.input.Height(), tui.CompactInputMaxHeight)
	}
	if lines := strings.Count(model.commandMenu.View(), "\n") + 1; lines > 3 {
		t.Fatalf("compact command menu used %d rows, want at most 3", lines)
	}
	compactPanel := model.confirmation.Confirmation.Prompt.Text()
	for _, want := range []string{"权限确认", "n拒绝", "Esc取消"} {
		if !strings.Contains(compactPanel, want) {
			t.Fatalf("compact confirmation omitted %q: %q", want, compactPanel)
		}
	}
	if strings.Contains(compactPanel, "legacy prompt") {
		t.Fatalf("resize retained the pre-layout prompt: %q", compactPanel)
	}

	updated, cmd = model.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	model = updated.(Model)
	if cmd != nil || model.input.Height() != 2 {
		t.Fatalf("wide resize did not recompute wrapped input: height=%d cmd=%v", model.input.Height(), cmd)
	}
	for _, line := range strings.Split(model.confirmation.Confirmation.Prompt.Text(), "\n") {
		if width := ansi.StringWidth(line); width > 120 {
			t.Fatalf("wide confirmation line exceeded real terminal width: %d", width)
		}
	}
}

func finishClosedEventStream(t *testing.T, model Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("terminal event did not continue listening for stream close")
	}
	updated, next := model.Update(cmd())
	if next != nil {
		t.Fatalf("stream close unexpectedly scheduled another command: %v", next)
	}
	return updated.(Model)
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

type fakeConversationStore struct {
	conversation *conversation.Conversation
	report       conversation.RecoveryReport
	loaded       bool
	unavailable  bool
	listResult   conversation.ListResult
	listErr      error
	loadErr      error
	saveErr      error
	createErr    error
	calls        []string
}

func (s *fakeConversationStore) List(ctx context.Context) (conversation.ListResult, error) {
	s.calls = append(s.calls, "list")
	if s.listErr != nil || len(s.listResult.Entries) > 0 || s.listResult.Truncated || len(s.listResult.Diagnostics) > 0 {
		return s.listResult, s.listErr
	}
	if s.conversation == nil {
		return conversation.ListResult{}, nil
	}
	return conversation.ListResult{Entries: []conversation.ListEntry{{
		Summary: conversation.ConversationSummary{
			ID:           s.conversation.ID,
			Title:        s.conversation.Title,
			UpdatedAt:    s.conversation.UpdatedAt,
			MessageCount: len(s.conversation.Messages),
		},
		Available: true,
		Recovery:  s.report,
	}}}, nil
}

func (s *fakeConversationStore) Load(ctx context.Context, id string) (conversation.LoadResult, error) {
	s.calls = append(s.calls, "load")
	s.loaded = true
	if s.loadErr != nil {
		return conversation.LoadResult{}, s.loadErr
	}
	return conversation.LoadResult{
		Conversation: s.conversation,
		Available:    s.conversation != nil && !s.unavailable,
		Recovery:     s.report,
	}, nil
}

func (s *fakeConversationStore) Save(ctx context.Context, value *conversation.Conversation) (conversation.SaveResult, error) {
	s.calls = append(s.calls, "save")
	if s.saveErr != nil {
		return conversation.SaveResult{}, s.saveErr
	}
	return conversation.SaveResult{Kind: conversation.SaveNoop}, nil
}

func (s *fakeConversationStore) Create(ctx context.Context) (*conversation.Conversation, error) {
	s.calls = append(s.calls, "create")
	if s.createErr != nil {
		return nil, s.createErr
	}
	return conversation.NewConversation("new", time.Now()), nil
}

func (s *fakeConversationStore) Maintain(context.Context) (conversation.MaintenanceResult, error) {
	s.calls = append(s.calls, "maintain")
	return conversation.MaintenanceResult{}, nil
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

type fakeProvider struct {
	name    string
	tracker *appTestStreamTracker
}

func (f fakeProvider) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake"
}

func (f fakeProvider) StreamChat(ctx context.Context, req provider.ChatRequest) (provider.ChatStream, error) {
	return newAppTestChatStream(nil, f.tracker), nil
}

type fakeResources struct{}

func (fakeResources) SystemPrompt() string      { return "" }
func (fakeResources) UILabel(key string) string { return key }

func testAppConfig() *config.AppConfig {
	return &config.AppConfig{LLM: config.LLMConfig{Model: "fake", RequestTimeoutMS: 1000}, UI: config.UIConfig{StartMode: config.StartModeList}}
}

func testEventMessage(t *testing.T, model *Model, event events.Event, stream <-chan Event) eventMsg {
	t.Helper()
	return eventMsg{event: event, events: stream, envelope: ensureTestEventEnvelope(t, model)}
}

func testEventStreamClosedMessage(t *testing.T, model *Model) eventStreamClosedMsg {
	t.Helper()
	return eventStreamClosedMsg{envelope: ensureTestEventEnvelope(t, model)}
}

func ensureTestEventEnvelope(t *testing.T, model *Model) eventEnvelope {
	t.Helper()
	if model.conversation == nil {
		model.conversation = &conversation.Conversation{ID: "test-conversation"}
	} else if strings.TrimSpace(model.conversation.ID) == "" {
		model.conversation.ID = "test-conversation"
	}
	if model.eventBoundary == nil {
		model.eventBoundary = newDefaultEventBoundaryState(redact.NewRuntimeRedactor())
	}
	if model.request != nil && model.request.Generation != 0 && model.request.ConversationID == model.conversation.ID {
		return eventEnvelope{Generation: model.request.Generation, ConversationID: model.request.ConversationID}
	}
	envelope, err := model.eventBoundary.begin(model.conversation.ID)
	if err != nil {
		t.Fatalf("begin test event boundary: %v", err)
	}
	if model.request == nil {
		model.request = &RequestSession{}
	}
	model.request.Generation = envelope.Generation
	model.request.ConversationID = envelope.ConversationID
	if model.lifecycle != nil {
		model.lifecycle.replace(model.request)
	}
	return envelope
}

func closedEvents() <-chan Event {
	ch := make(chan Event)
	close(ch)
	return ch
}
