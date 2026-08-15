package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/artifact"
	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/memory"
	"xagent/internal/orchestrator"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/tool"
	"xagent/internal/tui"
)

func TestArtifactOpenIsLocalUserOnly(t *testing.T) {
	t.Run("slash command reaches one explicit bounded local read without provider", func(t *testing.T) {
		id := strings.Repeat("a", 64)
		raw := "safe first line\ntoken=raw-artifact-canary"
		opener := &recordingArtifactUserReader{
			content: raw,
			ref: artifact.Ref{ID: id, Bytes: int64(len(raw)), CreatedAt: time.Unix(10, 20),
				Available: true, Complete: true},
		}
		provider := &recordingCommandProvider{}
		model := newCommandTestModel(t, provider)
		model.deps.Artifacts = opener
		model.deps.RuntimeRedactor = redact.NewRuntimeRedactor()
		metadata := tui.NewArtifactView(tui.ArtifactViewSpec{
			ID: id, Bytes: int64(len(raw)), Available: true, Complete: true,
		})
		if rendered := model.View() + metadata.MetadataLine(); opener.calls != 0 || strings.Contains(rendered, raw) {
			t.Fatalf("ordinary rendering opened or exposed artifact: calls=%d view=%q", opener.calls, rendered)
		}

		model = runCommandInput(t, model, "/artifact "+id)
		if opener.calls != 1 || opener.lastID != id || opener.lastReader == nil || !opener.lastReader.closed {
			t.Fatalf("explicit command did not perform one closed read: calls=%d id=%q reader=%#v", opener.calls, opener.lastID, opener.lastReader)
		}
		if got := len(provider.Requests()); got != 0 {
			t.Fatalf("local artifact command made %d provider requests", got)
		}
		if model.artifactView == nil {
			t.Fatal("explicit command did not publish the dedicated safe page")
		}
		opened := model.View()
		if strings.Contains(opened, "raw-artifact-canary") || !strings.Contains(opened, "[redacted]") ||
			!strings.Contains(opened, "safe first line") || !strings.Contains(opened, id) {
			t.Fatalf("dedicated page was not safely projected: %q", opened)
		}
		updated, quitCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
		model = updated.(Model)
		if model.artifactView != nil || model.artifactCancel != nil || quitCmd == nil {
			t.Fatalf("quit retained artifact state: view=%v cancel=%v cmd=%v", model.artifactView != nil, model.artifactCancel != nil, quitCmd != nil)
		}
		if _, ok := quitCmd().(tea.QuitMsg); !ok {
			t.Fatal("artifact page q did not quit after closing local state")
		}
	})

	t.Run("pagination is bounded and every page reader closes", func(t *testing.T) {
		id := strings.Repeat("b", 64)
		raw := strings.Repeat("x", int(artifactPageSize*2+7))
		opener := &recordingArtifactUserReader{content: raw, ref: artifact.Ref{
			ID: id, Bytes: int64(len(raw)), CreatedAt: time.Unix(30, 40), Available: true, Complete: true,
		}}
		intent, _ := tui.NewArtifactOpenIntent(id, 0)
		first, err := openArtifactForUser(context.Background(), opener, intent, redact.NewRuntimeRedactor())
		if err != nil || first.ConsumedBytes() != artifactPageSize || !first.HasMore() {
			t.Fatalf("first page = consumed:%d more:%t err:%v", first.ConsumedBytes(), first.HasMore(), err)
		}
		secondIntent, ok := first.NextIntent()
		if !ok || secondIntent.Offset() != artifactPageSize {
			t.Fatalf("next intent = %#v ok=%t", secondIntent, ok)
		}
		second, err := openArtifactForUser(context.Background(), opener, secondIntent, redact.NewRuntimeRedactor())
		if err != nil || second.ConsumedBytes() != artifactPageSize || !second.HasMore() {
			t.Fatalf("second page = consumed:%d more:%t err:%v", second.ConsumedBytes(), second.HasMore(), err)
		}
		thirdIntent, _ := second.NextIntent()
		third, err := openArtifactForUser(context.Background(), opener, thirdIntent, redact.NewRuntimeRedactor())
		if err != nil || third.ConsumedBytes() != 7 || third.HasMore() || opener.calls != 3 {
			t.Fatalf("third page = consumed:%d more:%t calls:%d err:%v", third.ConsumedBytes(), third.HasMore(), opener.calls, err)
		}
		for index, reader := range opener.readers {
			if !reader.closed || reader.maxReadRequest > int(artifactPageSize+1) {
				t.Fatalf("reader %d unbounded or unclosed: closed=%t max=%d", index, reader.closed, reader.maxReadRequest)
			}
		}
	})

	t.Run("invalid input refs and IO failures close and expose one fixed error", func(t *testing.T) {
		id := strings.Repeat("c", 64)
		validRef := artifact.Ref{ID: id, Bytes: 4, CreatedAt: time.Unix(50, 60), Available: true, Complete: true}
		intent, _ := tui.NewArtifactOpenIntent(id, 0)
		for _, test := range []struct {
			name       string
			opener     *recordingArtifactUserReader
			cancelOpen bool
		}{
			{name: "raw error", opener: &recordingArtifactUserReader{err: errors.New("open /private/artifacts/raw.artifact token=secret")}},
			{name: "reader plus error", opener: &recordingArtifactUserReader{content: "data", ref: validRef, err: errors.New("secret"), readerWithError: true}},
			{name: "nil reader", opener: &recordingArtifactUserReader{ref: validRef, nilReader: true}},
			{name: "mismatched id", opener: &recordingArtifactUserReader{
				content: "data", ref: artifact.Ref{ID: strings.Repeat("d", 64), Bytes: 4, CreatedAt: time.Unix(1, 0), Available: true},
			}},
			{name: "unavailable ref", opener: &recordingArtifactUserReader{
				content: "data", ref: artifact.Ref{ID: id, Bytes: 4, CreatedAt: time.Unix(1, 0), Available: false},
			}},
			{name: "zero created at", opener: &recordingArtifactUserReader{content: "data", ref: artifact.Ref{ID: id, Bytes: 4, Available: true}}},
			{name: "negative bytes", opener: &recordingArtifactUserReader{content: "data", ref: artifact.Ref{ID: id, Bytes: -1, CreatedAt: time.Unix(1, 0), Available: true}}},
			{name: "short reader", opener: &recordingArtifactUserReader{content: "bad", ref: validRef}},
			{name: "read error", opener: &recordingArtifactUserReader{content: "data", ref: validRef, readErr: errors.New("read /private/raw token=secret")}},
			{name: "close error", opener: &recordingArtifactUserReader{content: "data", ref: validRef, closeErr: errors.New("close /private/raw token=secret")}},
			{name: "post open cancel", opener: &recordingArtifactUserReader{content: "data", ref: validRef}, cancelOpen: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				ctx := context.Background()
				if test.cancelOpen {
					cancelCtx, cancel := context.WithCancel(context.Background())
					test.opener.afterOpen = cancel
					ctx = cancelCtx
				}
				view, err := openArtifactForUser(ctx, test.opener, intent, redact.NewRuntimeRedactor())
				if err == nil || test.opener.calls != 1 {
					t.Fatalf("invalid open did not fail closed: calls=%d err=%v", test.opener.calls, err)
				}
				output := view.View()
				for _, forbidden := range []string{"secret", "/private/", ".artifact", "data", "bad"} {
					if strings.Contains(output, forbidden) {
						t.Fatalf("safe error view leaked %q: %q", forbidden, output)
					}
				}
				if !strings.Contains(output, "artifact 无法读取，请确认 ID 后重试") {
					t.Fatalf("safe error view is not actionable: %q", output)
				}
				if test.opener.lastReader != nil && !test.opener.lastReader.closed {
					t.Fatal("rejected reader was not closed")
				}
			})
		}

		for _, invalid := range []string{"", strings.Repeat("A", 64), "/private/artifacts/raw.artifact", strings.Repeat("a", 63)} {
			opener := &recordingArtifactUserReader{}
			controller := &commandController{model: &Model{deps: Deps{Artifacts: opener}}}
			if err := controller.OpenArtifact(invalid); err == nil || controller.cmd != nil || opener.calls != 0 {
				t.Fatalf("invalid command ID was accepted: %q err=%v calls=%d", invalid, err, opener.calls)
			}
		}
	})

	t.Run("Esc and Model Close actively unblock an attached reader exactly once", func(t *testing.T) {
		for _, action := range []string{"esc", "close"} {
			t.Run(action, func(t *testing.T) {
				id := strings.Repeat("e", 64)
				gate := newGateArtifactReader()
				opener := &gateArtifactUserReader{reader: gate, ref: artifact.Ref{
					ID: id, Bytes: 1, CreatedAt: time.Unix(70, 80), Available: true, Complete: true,
				}}
				intent, _ := tui.NewArtifactOpenIntent(id, 0)
				model := Model{deps: Deps{Artifacts: opener}, lifecycle: newLifecycleState()}
				updated, cmd := model.Update(intent)
				model = updated.(Model)
				result := make(chan tea.Msg, 1)
				go func() { result <- cmd() }()
				<-gate.started
				if action == "esc" {
					updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
					model = updated.(Model)
				} else if err := model.Close(context.Background()); err != nil {
					t.Fatalf("Model.Close: %v", err)
				}
				select {
				case msg := <-result:
					if msg != nil {
						t.Fatalf("canceled read published %#v", msg)
					}
				case <-time.After(time.Second):
					t.Fatal("closing the artifact lease did not unblock Read")
				}
				if got := gate.CloseCount(); got != 1 {
					t.Fatalf("reader Close count = %d, want 1", got)
				}
				if model.artifactCancel != nil || model.artifactGeneration == 0 {
					t.Fatalf("close retained loading state or zero generation: %#v", model)
				}
			})
		}
	})

	t.Run("generation rejects a delayed result with the same ID and offset", func(t *testing.T) {
		id := strings.Repeat("f", 64)
		opener := &recordingArtifactUserReader{content: "first!", ref: artifact.Ref{
			ID: id, Bytes: 6, CreatedAt: time.Unix(90, 1), Available: true, Complete: true,
		}}
		intent, _ := tui.NewArtifactOpenIntent(id, 0)
		model := Model{deps: Deps{Artifacts: opener}, lifecycle: newLifecycleState()}
		updated, firstCmd := model.Update(intent)
		model = updated.(Model)
		delayed := firstCmd()
		opener.content = "second"
		updated, secondCmd := model.Update(intent)
		model = updated.(Model)
		updated, _ = model.Update(secondCmd())
		model = updated.(Model)
		if model.artifactView == nil || model.artifactView.Page().Text() != "second" {
			t.Fatalf("current generation missing: %#v", model.artifactView)
		}
		updated, _ = model.Update(delayed)
		model = updated.(Model)
		if model.artifactView.Page().Text() != "second" {
			t.Fatalf("delayed same-key result replaced current page: %q", model.artifactView.Page().Text())
		}
	})

	t.Run("a later page failure is visible and clears its loading cancel", func(t *testing.T) {
		id := strings.Repeat("1", 64)
		intent, _ := tui.NewArtifactOpenIntent(id, artifactPageSize)
		model := Model{deps: Deps{Artifacts: &recordingArtifactUserReader{
			err: errors.New("open /private/deep-page token=secret"),
		}}, lifecycle: newLifecycleState()}
		updated, cmd := model.Update(intent)
		model = updated.(Model)
		result := cmd().(artifactViewMsg)
		if result.RequestID != id || result.Offset != artifactPageSize || result.Generation == 0 {
			t.Fatalf("later-page failure lost request identity: %#v", result)
		}
		updated, _ = model.Update(result)
		model = updated.(Model)
		if model.artifactView == nil || !strings.Contains(model.artifactView.View(), "artifact 无法读取，请确认 ID 后重试") || model.artifactCancel != nil {
			t.Fatalf("later-page failure was hidden or retained loading state: view=%#v cancel=%v", model.artifactView, model.artifactCancel != nil)
		}
	})
}

type gateArtifactUserReader struct {
	reader *gateArtifactReader
	ref    artifact.Ref
}

func (reader *gateArtifactUserReader) OpenForUser(context.Context, string) (io.ReadCloser, artifact.Ref, error) {
	return reader.reader, reader.ref, nil
}

type gateArtifactReader struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	mu        sync.Mutex
	closes    int
}

func newGateArtifactReader() *gateArtifactReader {
	return &gateArtifactReader{started: make(chan struct{}), closed: make(chan struct{})}
}

func (reader *gateArtifactReader) Read([]byte) (int, error) {
	reader.startOnce.Do(func() { close(reader.started) })
	<-reader.closed
	return 0, errors.New("reader closed")
}

func (reader *gateArtifactReader) Close() error {
	reader.closeOnce.Do(func() {
		reader.mu.Lock()
		reader.closes++
		reader.mu.Unlock()
		close(reader.closed)
	})
	return nil
}

func (reader *gateArtifactReader) CloseCount() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.closes
}

type recordingArtifactUserReader struct {
	calls           int
	lastID          string
	content         string
	ref             artifact.Ref
	err             error
	nilReader       bool
	readerWithError bool
	readErr         error
	closeErr        error
	afterOpen       func()
	lastReader      *trackingArtifactReader
	readers         []*trackingArtifactReader
}

func (reader *recordingArtifactUserReader) OpenForUser(_ context.Context, id string) (io.ReadCloser, artifact.Ref, error) {
	reader.calls++
	reader.lastID = id
	if reader.err != nil && !reader.readerWithError {
		return nil, artifact.Ref{}, reader.err
	}
	if reader.nilReader {
		return nil, reader.ref, reader.err
	}
	reader.lastReader = &trackingArtifactReader{reader: strings.NewReader(reader.content), readErr: reader.readErr, closeErr: reader.closeErr}
	reader.readers = append(reader.readers, reader.lastReader)
	if reader.afterOpen != nil {
		reader.afterOpen()
	}
	return reader.lastReader, reader.ref, reader.err
}

type trackingArtifactReader struct {
	reader         io.Reader
	readErr        error
	closeErr       error
	closed         bool
	maxReadRequest int
}

func (reader *trackingArtifactReader) Read(buffer []byte) (int, error) {
	if len(buffer) > reader.maxReadRequest {
		reader.maxReadRequest = len(buffer)
	}
	if reader.readErr != nil {
		return 0, reader.readErr
	}
	return reader.reader.Read(buffer)
}

func (reader *trackingArtifactReader) Close() error {
	reader.closed = true
	return reader.closeErr
}

type recordingCommandProvider struct {
	mu       sync.Mutex
	requests []provider.ChatRequest
	tracker  appTestStreamTracker
}

func (p *recordingCommandProvider) Name() string { return "recording" }

func (p *recordingCommandProvider) StreamChat(_ context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	return newAppTestChatStream([]provider.StreamEvent{{Type: provider.StreamEventDone}}, &p.tracker), nil
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
		Store:     &fakeConversationStore{},
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
	if len(model.conversation.Messages) == 0 || model.conversation.Messages[0].Content.Text() != "普通问题" {
		t.Fatalf("plain input was not saved: %#v", model.conversation.Messages)
	}
	if got := provider.tracker.CloseCalls(); got != 1 {
		t.Fatalf("plain input stream close calls = %d, want 1", got)
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
	cfg := testAppConfig()
	cfg.UI.StartMode = config.StartModeNew
	manager, err := contextmgr.New(provider, appTestContextManagerOptions(false))
	if err != nil {
		t.Fatal(err)
	}
	model := New(Deps{Config: cfg, Provider: provider, Store: &fakeConversationStore{}, Resources: fakeResources{}, ContextManager: manager})
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
	model := New(Deps{Config: cfg, Provider: provider, Store: &fakeConversationStore{}, Resources: fakeResources{}, Memory: manager})

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
	store := model.deps.Store.(*fakeConversationStore)
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
	store := model.deps.Store.(*fakeConversationStore)
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

func TestHelpAndREADMEMatchPublicMetadata(t *testing.T) {
	model := newCommandTestModel(t, &recordingCommandProvider{})
	catalog := model.ensureCommandRegistry().HelpCatalog()
	model = runCommandInput(t, model, "/help")
	help := model.status.Notice
	readmeBytes, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(readmeBytes)

	for _, item := range catalog.Commands {
		for _, output := range []struct {
			name  string
			value string
		}{{name: "help", value: help}, {name: "README", value: readme}} {
			for _, want := range append([]string{"/" + item.Name, item.Description, item.Usage}, slashAliasesForAppTest(item.Aliases)...) {
				if !strings.Contains(output.value, want) {
					t.Fatalf("%s missing public command metadata %q for /%s", output.name, want, item.Name)
				}
			}
		}
	}
	for _, binding := range catalog.Bindings {
		for _, output := range []struct {
			name  string
			value string
		}{{name: "help", value: help}, {name: "README", value: readme}} {
			for _, want := range []string{string(binding.Context), binding.Key, binding.Description} {
				if !strings.Contains(output.value, want) {
					t.Fatalf("%s missing shortcut metadata %q for %s/%s", output.name, want, binding.Context, binding.Key)
				}
			}
		}
	}
	for _, entry := range catalog.Entries {
		if !strings.Contains(help, entry.Description) || !strings.Contains(readme, entry.Description) {
			t.Fatalf("help or README missing %q entry: help=%q", entry.Kind, entry.Description)
		}
	}
	for _, hidden := range []string{"/diagnostics", "/permissions", "/mcp", "/rv"} {
		if strings.Contains(help, hidden) || strings.Contains(readme, "`"+hidden) {
			t.Fatalf("hidden compatibility command %q leaked into help or README", hidden)
		}
	}
}

func TestStatusOutputsBoundedActionableDiagnostics(t *testing.T) {
	provider := &recordingCommandProvider{}
	model := newCommandTestModel(t, provider)
	secret := "status-secret-canary"
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(secret)
	model.deps.Redact = redactor.Text
	model.status.Provider = strings.Repeat("provider", 500)
	model.status.Model = "model\x1b]52;c;clipboard\a"
	model.status.MCP = strings.Repeat("mcp", 500)
	model.lastError = errors.New("retry configuration token=" + secret + strings.Repeat("x", 5000))
	model.diagnostics.Add(diagnostics.New("status_probe", diagnostics.SeverityWarning, "check connection token="+secret))

	model = runCommandInput(t, model, "/status")
	output := model.status.Notice
	if len(output) == 0 || len(output) > 2048 {
		t.Fatalf("status output is empty or unbounded: bytes=%d", len(output))
	}
	for _, want := range []string{"provider=", "model=", "mcp=", "recent_error=", "diagnostics=", "status_probe", "action="} {
		if !strings.Contains(output, want) {
			t.Fatalf("status output is not actionable; missing %q: %q", want, output)
		}
	}
	if strings.Contains(output, secret) || strings.ContainsAny(output, "\x1b\a") || strings.Contains(output, "52;c;clipboard") {
		t.Fatalf("status output leaked secret or terminal controls: %q", output)
	}
	if got := len(provider.Requests()); got != 0 {
		t.Fatalf("/status called provider %d times", got)
	}
}

func slashAliasesForAppTest(aliases []string) []string {
	result := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		result = append(result, "/"+alias)
	}
	return result
}

func requestContainsDynamic(request provider.ChatRequest, want string) bool {
	for _, block := range request.DynamicSystem {
		if strings.Contains(block.Content.Text(), want) {
			return true
		}
	}
	return false
}
