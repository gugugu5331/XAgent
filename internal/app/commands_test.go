package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/memory"
	"xagent/internal/orchestrator"
	"xagent/internal/provider"
	"xagent/internal/tool"
)

type recordingCommandProvider struct {
	mu       sync.Mutex
	requests []provider.ChatRequest
}

func (p *recordingCommandProvider) Name() string { return "recording" }

func (p *recordingCommandProvider) StreamChat(_ context.Context, request provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	events := make(chan provider.StreamEvent, 1)
	events <- provider.StreamEvent{Type: provider.StreamEventDone}
	close(events)
	return events, nil
}

func (p *recordingCommandProvider) Requests() []provider.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.ChatRequest(nil), p.requests...)
}

func newCommandTestModel(t *testing.T, provider provider.Provider) Model {
	t.Helper()
	cfg := testAppConfig()
	cfg.UI.StartMode = config.StartModeNew
	registry, err := tool.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("create tool registry: %v", err)
	}
	executor := tool.NewExecutor(registry, t.TempDir(), time.Second, 1024)
	return New(Deps{
		Config:    cfg,
		Provider:  provider,
		Store:     &fakeRecoveringStore{},
		Resources: fakeResources{},
		Registry:  registry,
		Executor:  executor,
	})
}

func runCommandInput(t *testing.T, model Model, input string) Model {
	t.Helper()
	model.input.SetValue(input)
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	for steps := 0; cmd != nil && steps < 20; steps++ {
		msg := cmd()
		if msg == nil {
			break
		}
		updated, cmd = model.Update(msg)
		model = updated.(Model)
	}
	if model.streaming {
		t.Fatal("input did not finish streaming")
	}
	return model
}

func TestUnknownCommandDoesNotCallProvider(t *testing.T) {
	provider := &recordingCommandProvider{}
	model := newCommandTestModel(t, provider)
	model = runCommandInput(t, model, "/unknown")
	if model.status.Error == nil || !strings.Contains(model.status.Error.Error(), "/help") {
		t.Fatalf("unknown command did not show help guidance: %v", model.status.Error)
	}
	if got := len(provider.Requests()); got != 0 {
		t.Fatalf("unknown command made %d provider requests", got)
	}
}

func TestPlainInputUsesAgentPath(t *testing.T) {
	provider := &recordingCommandProvider{}
	model := newCommandTestModel(t, provider)
	model = runCommandInput(t, model, "普通问题")
	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("plain input made %d requests", len(requests))
	}
	if len(model.conversation.Messages) == 0 || model.conversation.Messages[0].Content != "普通问题" {
		t.Fatalf("plain input was not saved: %#v", model.conversation.Messages)
	}
}

func TestCommandCompletionSingleAndExact(t *testing.T) {
	provider := &recordingCommandProvider{}
	model := newCommandTestModel(t, provider)

	model.input.SetValue("/cl")
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	if cmd != nil || model.input.Value() != "/clear" || model.commandMenu.Visible {
		t.Fatalf("single completion failed: input=%q menu=%#v", model.input.Value(), model.commandMenu)
	}

	model.input.SetValue("/p")
	updated, cmd = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	if cmd != nil || model.input.Value() != "/plan" {
		t.Fatalf("exact alias completion failed: %q", model.input.Value())
	}
	if model.mode != orchestrator.RunModeDefault || len(provider.Requests()) != 0 {
		t.Fatal("completion executed the command")
	}
}

func TestCommandCompletionMultipleUsesMenuAndEnterOnlyWritesInput(t *testing.T) {
	provider := &recordingCommandProvider{}
	model := newCommandTestModel(t, provider)
	model.input.SetValue("/c")
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	if !model.commandMenu.Visible || !strings.Contains(model.commandMenu.View(), "/clear") || !strings.Contains(model.commandMenu.View(), "/compact") {
		t.Fatalf("multiple completion menu missing choices: %q", model.commandMenu.View())
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if cmd != nil || model.input.Value() != "/compact" || model.commandMenu.Visible {
		t.Fatalf("menu acceptance should only write input: input=%q", model.input.Value())
	}
	if len(provider.Requests()) != 0 {
		t.Fatal("menu acceptance called provider")
	}

	model.input.SetValue("/c")
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)
	if model.commandMenu.Visible || model.input.Value() != "/c" {
		t.Fatal("escape did not close the menu while preserving input")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	model = updated.(Model)
	if model.commandMenu.Visible || model.input.Value() != "/cx" {
		t.Fatalf("ordinary input did not close menu and continue editing: %q", model.input.Value())
	}
}

func TestCompactCommandHandlesActiveAndMissingConversation(t *testing.T) {
	provider := &recordingCommandProvider{}
	disabled := false
	cfg := testAppConfig()
	cfg.UI.StartMode = config.StartModeNew
	manager := contextmgr.New(provider, t.TempDir(), config.ContextConfig{Enabled: &disabled})
	model := New(Deps{Config: cfg, Provider: provider, Store: &fakeRecoveringStore{}, Resources: fakeResources{}, ContextManager: manager})
	model = runCommandInput(t, model, "/ctx")
	if model.status.Error != nil || !strings.Contains(model.status.Notice, "无需压缩") {
		t.Fatalf("active compact did not report its result: notice=%q error=%v", model.status.Notice, model.status.Error)
	}
	model.conversation = nil
	model = runCommandInput(t, model, "/compact")
	if model.status.Error != nil || !strings.Contains(model.status.Notice, "没有会话") {
		t.Fatalf("missing-conversation compact did not return locally: notice=%q error=%v", model.status.Notice, model.status.Error)
	}
	if len(provider.Requests()) != 0 {
		t.Fatal("compact with disabled context unexpectedly called provider")
	}
}

func TestSessionAndStatusCommandsUseCurrentModelState(t *testing.T) {
	provider := &recordingCommandProvider{}
	model := newCommandTestModel(t, provider)
	model.status.InputTokens = 11
	model.status.OutputTokens = 7
	model.status.CacheCreationInputTokens = 3
	model.status.CacheReadInputTokens = 2
	model.status.MCP = "ready=1"
	model.lastError = errors.New("token=secret-value")

	model = runCommandInput(t, model, "/session")
	for _, want := range []string{"id=new", "messages=0", "mode=default", "streaming=false"} {
		if !strings.Contains(model.status.Notice, want) {
			t.Fatalf("session status missing %q: %q", want, model.status.Notice)
		}
	}

	model = runCommandInput(t, model, "/status")
	for _, want := range []string{"provider=recording", "model=fake", "mode=default", "tokens=11_in/7_out", "cache=3_create/2_read", "mcp=ready=1"} {
		if !strings.Contains(model.status.Notice, want) {
			t.Fatalf("runtime status missing %q: %q", want, model.status.Notice)
		}
	}
	if strings.Contains(model.status.Notice, "secret-value") || len(provider.Requests()) != 0 {
		t.Fatalf("status leaked secret or called provider: %q", model.status.Notice)
	}
	if model.lastError == nil {
		t.Fatal("status command discarded the stored recent error")
	}
	model.conversation.ID = "token=secret-session"
	model = runCommandInput(t, model, "/sess")
	if strings.Contains(model.status.Notice, "secret-session") {
		t.Fatalf("session status leaked a sensitive ID: %q", model.status.Notice)
	}
}

func TestMemoryAndPermissionCommandsReportReadOnlyState(t *testing.T) {
	provider := &recordingCommandProvider{}
	root := t.TempDir()
	manager := memory.NewManager(memory.ManagerOptions{
		UserDir:    filepath.Join(root, "user"),
		ProjectDir: filepath.Join(root, "project-token=secret-path"),
	})
	userNote := memory.NewNote(memory.NoteUserPreference, memory.ScopeUser, "user", "password=hunter2", "test", time.Unix(1, 0))
	projectNote := memory.NewNote(memory.NoteProjectKnowledge, memory.ScopeProject, "project", "api_key=secret-key", "test", time.Unix(2, 0))
	if err := manager.SaveNote(userNote); err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveNote(projectNote); err != nil {
		t.Fatal(err)
	}
	cfg := testAppConfig()
	cfg.UI.StartMode = config.StartModeNew
	model := New(Deps{Config: cfg, Provider: provider, Store: &fakeRecoveringStore{}, Resources: fakeResources{}, Memory: manager})

	model = runCommandInput(t, model, "/mem")
	if !strings.Contains(model.status.Notice, "user=on(1)") || !strings.Contains(model.status.Notice, "project=on(1)") {
		t.Fatalf("memory summary does not reflect indexes: %q", model.status.Notice)
	}
	for _, secret := range []string{"hunter2", "secret-key", "secret-path"} {
		if strings.Contains(model.status.Notice, secret) {
			t.Fatalf("memory summary leaked %q: %q", secret, model.status.Notice)
		}
	}

	before := model.orchestrator.PermissionStatus()
	model = runCommandInput(t, model, "/perm")
	after := model.orchestrator.PermissionStatus()
	if before != after || !strings.Contains(model.status.Notice, "mode=default") {
		t.Fatalf("permission command changed state: before=%#v after=%#v notice=%q", before, after, model.status.Notice)
	}
	if len(provider.Requests()) != 0 {
		t.Fatal("memory or permission status called provider")
	}
}

func TestClearCommandOnlyClearsDisplayAndReloadRestoresHistory(t *testing.T) {
	provider := &recordingCommandProvider{}
	model := newCommandTestModel(t, provider)
	conversation.AppendUserMessage(model.conversation, "保留的历史")
	model.messages.SetMessages(model.conversation.Messages)
	originalCount := len(model.conversation.Messages)
	store := model.deps.Store.(*fakeRecoveringStore)
	store.conversation = model.conversation

	model = runCommandInput(t, model, "/clear")
	if model.messages.View() != "" || len(model.conversation.Messages) != originalCount || !model.messagesCleared {
		t.Fatalf("clear changed conversation or left display content: messages=%q conversation=%#v", model.messages.View(), model.conversation.Messages)
	}
	model = runCommandInput(t, model, "/compact")
	if model.messages.View() != "" {
		t.Fatal("compact rehydrated cleared history")
	}
	model.loadConversation(model.conversation.ID)
	if !strings.Contains(model.messages.View(), "保留的历史") || model.messagesCleared {
		t.Fatal("loading conversation did not restore visible history")
	}
}

func TestCommandModePlanPersistsUntilDoAndResetsForConversation(t *testing.T) {
	provider := &recordingCommandProvider{}
	model := newCommandTestModel(t, provider)
	model = runCommandInput(t, model, "/plan")
	if model.mode != orchestrator.RunModePlan || model.status.Mode != "plan" || !strings.Contains(model.status.View(), "[PLAN]") {
		t.Fatalf("plan mode was not reflected immediately: %#v", model.status)
	}
	model = runCommandInput(t, model, "先分析")
	model = runCommandInput(t, model, "继续分析")
	requests := provider.Requests()
	if len(requests) != 2 || !requestContainsDynamic(requests[0], "当前请求处于 Plan Mode") || !requestContainsDynamic(requests[1], "当前请求处于 Plan Mode") {
		t.Fatalf("plan mode did not persist across requests: %#v", requests)
	}

	model = runCommandInput(t, model, "/do")
	if model.mode != orchestrator.RunModeDefault || !strings.Contains(model.status.View(), "[DEFAULT]") {
		t.Fatal("do command did not restore default mode")
	}
	model = runCommandInput(t, model, "现在执行")
	requests = provider.Requests()
	if len(requests) != 3 || requestContainsDynamic(requests[2], "当前请求处于 Plan Mode") {
		t.Fatal("default request retained plan mode")
	}

	model = runCommandInput(t, model, "/plan")
	model.startNewConversation()
	if model.mode != orchestrator.RunModeDefault || model.status.Mode != "default" {
		t.Fatal("new conversation did not reset mode")
	}
	model = runCommandInput(t, model, "/plan")
	store := model.deps.Store.(*fakeRecoveringStore)
	store.conversation = conversation.NewConversation("other", time.Unix(3, 0))
	model.loadConversation("other")
	if model.mode != orchestrator.RunModeDefault || model.status.Mode != "default" {
		t.Fatal("loading another conversation did not reset mode")
	}
	restarted := newCommandTestModel(t, provider)
	if restarted.mode != orchestrator.RunModeDefault || restarted.status.Mode != "default" {
		t.Fatal("reconstructing app did not reset mode")
	}
}

func TestReviewCommandIsNoLongerHardCoded(t *testing.T) {
	provider := &recordingCommandProvider{}
	model := newCommandTestModel(t, provider)
	model = runCommandInput(t, model, "/review")
	if model.status.Error == nil || !strings.Contains(model.status.Error.Error(), "/help") {
		t.Fatalf("missing review Skill did not behave as an unknown command: %v", model.status.Error)
	}
	if len(provider.Requests()) != 0 {
		t.Fatal("hard-coded review still reached the provider")
	}
}

func TestLocalCommandsDoNotCallProvider(t *testing.T) {
	provider := &recordingCommandProvider{}
	model := newCommandTestModel(t, provider)
	for _, input := range []string{
		"/help", "/h", "/?", "/HeLp", "/compact", "/ctx", "/clear", "/cls", "/plan", "/p", "/do", "/d",
		"/session", "/sess", "/memory", "/mem", "/permission", "/perm", "/status", "/st", "/status extra",
		"/permissions status", "/mcp status", "/diagnostics", "/unknown",
	} {
		model = runCommandInput(t, model, input)
	}
	if got := len(provider.Requests()); got != 0 {
		t.Fatalf("local commands made %d provider requests", got)
	}
}

func requestContainsDynamic(request provider.ChatRequest, want string) bool {
	for _, block := range request.DynamicSystem {
		if strings.Contains(block.Content, want) {
			return true
		}
	}
	return false
}
