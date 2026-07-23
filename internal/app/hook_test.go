package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
	"xagent/internal/orchestrator"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/tui"
)

func TestLifecycleStateSharedAcrossCopies(t *testing.T) {
	cancelled := 0
	request := &RequestSession{Cancel: func() { cancelled++ }}
	model := Model{
		streaming: true,
		request:   request,
		input:     tui.NewInput(""),
		lifecycle: newLifecycleState(),
	}
	model.lifecycle.begin(request)
	copyOfModel := model

	copyOfModel.cancelRequest("canceling")
	active, canceling, terminal := model.lifecycle.active()
	if active != request || !canceling || terminal || cancelled != 1 {
		t.Fatalf("model copies did not share request state: active=%p canceling=%t terminal=%t cancelled=%d", active, canceling, terminal, cancelled)
	}
	model.finishRequestAfterStream()
	active, canceling, terminal = copyOfModel.lifecycle.active()
	if active != nil || canceling || terminal {
		t.Fatalf("finishing one copy did not clear shared lifecycle: active=%p canceling=%t terminal=%t", active, canceling, terminal)
	}
}

type noHookRuntimeCase struct {
	name    string
	runtime hook.Runtime
}

func noHookRuntimeCases(t *testing.T) []noHookRuntimeCase {
	t.Helper()
	emptyEngine, err := hook.NewEngine(hook.Snapshot{}, hook.EngineOptions{ProjectRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("create empty Hook engine: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if shutdownErr := emptyEngine.Shutdown(ctx); shutdownErr != nil {
			t.Errorf("shutdown empty Hook engine: %v", shutdownErr)
		}
	})
	return []noHookRuntimeCase{
		{name: "nil", runtime: nil},
		{name: "noop", runtime: hook.Noop()},
		{name: "empty-engine", runtime: emptyEngine},
	}
}

func TestNoHookSessionCompatibility(t *testing.T) {
	type snapshot struct {
		storeEvents      []string
		oldMessages      []string
		resumedMessages  []string
		oldJSON          string
		resumedJSON      string
		finalMode        orchestrator.RunMode
		finalStatusMode  string
		conversationNil  bool
		requestNil       bool
		streaming        bool
		activeSkillCount int
		diagnostics      int
		statusError      string
	}

	var baseline *snapshot
	conversationJSON := func(t *testing.T, value *conversation.Conversation) string {
		t.Helper()
		clone := *value
		clone.Messages = append([]conversation.Message(nil), value.Messages...)
		clone.CreatedAt = time.Time{}
		clone.UpdatedAt = time.Time{}
		for index := range clone.Messages {
			clone.Messages[index].CreatedAt = time.Time{}
		}
		data, err := json.Marshal(clone)
		if err != nil {
			t.Fatalf("marshal compatibility conversation: %v", err)
		}
		return string(data)
	}
	for _, item := range noHookRuntimeCases(t) {
		sequence := &eventSequence{}
		store := newLifecycleStore(sequence)
		oldConversation := conversation.NewConversation("old", time.Unix(10, 0))
		resumedConversation := conversation.NewConversation("resumed", time.Unix(20, 0))
		conversation.AppendAssistantMessage(resumedConversation, "restored answer")
		store.created = oldConversation
		store.loaded[resumedConversation.ID] = resumedConversation
		collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redact.Text})
		cfg := testAppConfig()
		cfg.UI.StartMode = config.StartModeNew
		model := New(Deps{
			Config: cfg, Provider: fakeProvider{}, Store: store, Resources: fakeResources{},
			Diagnostics: collector, Hooks: item.runtime,
		})
		conversation.AppendUserMessage(model.conversation, "saved before switch")
		model.loadConversation(resumedConversation.ID)
		conversation.AppendUserMessage(model.conversation, "saved before close")
		if err := model.Close(context.Background()); err != nil {
			t.Fatalf("%s App.Close: %v", item.name, err)
		}

		got := snapshot{
			storeEvents:      sequence.snapshot(),
			finalMode:        model.mode,
			finalStatusMode:  model.status.Mode,
			conversationNil:  model.conversation == nil,
			requestNil:       model.request == nil,
			streaming:        model.streaming || model.status.Streaming,
			activeSkillCount: len(model.skillActivity.Snapshot().Active),
			diagnostics:      collector.Count(),
		}
		if model.status.Error != nil {
			got.statusError = model.status.Error.Error()
		}
		for _, message := range oldConversation.Messages {
			got.oldMessages = append(got.oldMessages, string(message.Role)+":"+message.Content)
		}
		for _, message := range resumedConversation.Messages {
			got.resumedMessages = append(got.resumedMessages, string(message.Role)+":"+message.Content)
		}
		got.oldJSON = conversationJSON(t, oldConversation)
		got.resumedJSON = conversationJSON(t, resumedConversation)
		if baseline == nil {
			copy := got
			baseline = &copy
		} else if !reflect.DeepEqual(*baseline, got) {
			t.Fatalf("%s changed no-Hook session state:\nlegacy=%#v\nactual=%#v", item.name, *baseline, got)
		}
	}
	if baseline == nil || strings.Join(baseline.storeEvents, ",") != "create:old,load:resumed,save:old,save:resumed" ||
		strings.Join(baseline.oldMessages, ",") != "user:saved before switch" ||
		strings.Join(baseline.resumedMessages, ",") != "assistant:restored answer,user:saved before close" ||
		!strings.Contains(baseline.oldJSON, "saved before switch") || !strings.Contains(baseline.resumedJSON, "saved before close") ||
		baseline.finalMode != orchestrator.RunModeDefault || baseline.finalStatusMode != "default" ||
		!baseline.conversationNil || !baseline.requestNil || baseline.streaming || baseline.activeSkillCount != 0 ||
		baseline.diagnostics != 0 || baseline.statusError != "" {
		t.Fatalf("legacy session golden changed: %#v", baseline)
	}
}

func TestNoHookVisibleCompatibility(t *testing.T) {
	type snapshot struct {
		planStatus        string
		defaultStatus     string
		view              string
		messageView       string
		messages          []string
		requestText       string
		requestOrdered    int
		requestObserved   bool
		diagnostics       int
		finalConversation bool
		finalStreaming    bool
		finalMode         orchestrator.RunMode
	}

	var baseline *snapshot
	for _, item := range noHookRuntimeCases(t) {
		providerImpl := &skillAppProvider{}
		collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redact.Text})
		cfg := testAppConfig()
		cfg.UI.StartMode = config.StartModeNew
		model := New(Deps{
			Config: cfg, Provider: providerImpl, Store: &fakeRecoveringStore{}, Resources: fakeResources{},
			Diagnostics: collector, Hooks: item.runtime,
		})
		model = runCommandInput(t, model, "/plan")
		got := snapshot{planStatus: model.status.View()}
		model = runCommandInput(t, model, "visible question")
		// Compare semantic visibility rather than nondeterministic elapsed time.
		model.status.Duration = 0
		model = runCommandInput(t, model, "/do")
		got.defaultStatus = model.status.View()
		got.view = model.View()
		got.messageView = model.messages.View()
		got.diagnostics = collector.Count()
		for _, message := range model.conversation.Messages {
			got.messages = append(got.messages, string(message.Role)+":"+message.Content)
		}
		requests := providerImpl.Requests()
		if len(requests) != 1 {
			t.Fatalf("%s visible Provider requests = %d, want 1", item.name, len(requests))
		}
		got.requestText = visibleRequestText(requests[0])
		got.requestOrdered = len(requests[0].System)
		got.requestObserved = requests[0].Observer != nil
		if err := model.Close(context.Background()); err != nil {
			t.Fatalf("%s App.Close: %v", item.name, err)
		}
		got.finalConversation = model.conversation == nil
		got.finalStreaming = model.streaming || model.status.Streaming
		got.finalMode = model.mode

		if baseline == nil {
			copy := got
			baseline = &copy
		} else if !reflect.DeepEqual(*baseline, got) {
			t.Fatalf("%s changed no-Hook visible state:\nlegacy=%#v\nactual=%#v", item.name, *baseline, got)
		}
	}
	if baseline == nil || !strings.Contains(baseline.planStatus, "[PLAN]") || !strings.Contains(baseline.defaultStatus, "[DEFAULT]") ||
		!strings.Contains(baseline.messageView, "visible question") || !strings.Contains(baseline.messageView, "completed") ||
		strings.Join(baseline.messages, ",") != "user:visible question,assistant:completed" ||
		!strings.Contains(baseline.requestText, "visible question") || !strings.Contains(baseline.requestText, "Plan Mode") ||
		baseline.requestOrdered != 0 || baseline.requestObserved || baseline.diagnostics != 0 ||
		!baseline.finalConversation || baseline.finalStreaming || baseline.finalMode != orchestrator.RunModeDefault ||
		strings.Contains(strings.ToLower(baseline.view), "hook") {
		t.Fatalf("legacy visible golden changed: %#v", baseline)
	}
}

func TestSystemAndInitialSessionStart(t *testing.T) {
	t.Run("new", func(t *testing.T) {
		sequence := &eventSequence{}
		store := newLifecycleStore(sequence)
		store.created = conversation.NewConversation("created", time.Unix(1, 0))
		hooks := newRecordingHook(sequence, nil)
		cfg := testAppConfig()
		cfg.UI.StartMode = config.StartModeNew

		model := New(Deps{Config: cfg, Provider: fakeProvider{}, Store: store, Resources: fakeResources{}, Hooks: hooks})
		if model.conversation == nil || model.conversation.ID != "created" {
			t.Fatalf("new startup did not activate created session: %#v", model.conversation)
		}
		assertOrderedEvents(t, sequence.snapshot(), "system_start", "create:created", "session_start:created:new")
	})

	t.Run("resumed", func(t *testing.T) {
		sequence := &eventSequence{}
		store := newLifecycleStore(sequence)
		store.loaded["resume"] = conversation.NewConversation("resume", time.Unix(2, 0))
		hooks := newRecordingHook(sequence, nil)
		model := New(Deps{Config: testAppConfig(), Provider: fakeProvider{}, Store: store, Resources: fakeResources{}, Hooks: hooks})
		model.loadConversation("resume")

		if model.conversation == nil || model.conversation.ID != "resume" {
			t.Fatalf("load did not activate resumed session: %#v", model.conversation)
		}
		assertOrderedEvents(t, sequence.snapshot(), "system_start", "load:resume", "session_start:resume:resumed")
	})

	t.Run("failed candidate", func(t *testing.T) {
		sequence := &eventSequence{}
		store := newLifecycleStore(sequence)
		store.loadErrors["missing"] = errors.New("load failed")
		hooks := newRecordingHook(sequence, nil)
		model := New(Deps{Config: testAppConfig(), Provider: fakeProvider{}, Store: store, Resources: fakeResources{}, Hooks: hooks})
		model.loadConversation("missing")

		if model.conversation != nil || model.status.Error == nil {
			t.Fatalf("failed initial load changed active session: conversation=%#v err=%v", model.conversation, model.status.Error)
		}
		if got := countEventPrefix(sequence.snapshot(), "session_start:"); got != 0 {
			t.Fatalf("failed candidate emitted %d session starts: %v", got, sequence.snapshot())
		}
	})
}

func TestSessionSwitchTransaction(t *testing.T) {
	t.Run("waits for old turn then switches", func(t *testing.T) {
		sequence := &eventSequence{}
		store := newLifecycleStore(sequence)
		store.created = conversation.NewConversation("old", time.Unix(1, 0))
		store.loaded["next"] = conversation.NewConversation("next", time.Unix(2, 0))
		hooks := newRecordingHook(sequence, nil)
		provider := newBlockingLifecycleProvider()
		cfg := testAppConfig()
		cfg.UI.StartMode = config.StartModeNew
		model := New(Deps{Config: cfg, Provider: provider, Store: store, Resources: fakeResources{}, Hooks: hooks})

		events, err := model.orchestrator.SendRequest(context.Background(), model.conversation, orchestrator.RunRequest{UserText: "hold", Mode: orchestrator.RunModeDefault})
		if err != nil {
			t.Fatalf("start tracked request: %v", err)
		}
		drained := make(chan struct{})
		go func() {
			for range events {
			}
			close(drained)
		}()
		waitClosed(t, provider.started, "provider start")
		sequence.reset()

		switched := make(chan struct{})
		go func() {
			model.loadConversation("next")
			close(switched)
		}()
		waitClosed(t, store.loadObserved, "candidate load")
		select {
		case <-switched:
			t.Fatal("session switch completed before the old turn became idle")
		case <-time.After(30 * time.Millisecond):
		}
		if got := countEventPrefix(sequence.snapshot(), "save:"); got != 0 {
			t.Fatalf("old session saved before WaitIdle: %v", sequence.snapshot())
		}

		close(provider.release)
		waitClosed(t, drained, "request drain")
		waitClosed(t, switched, "session switch")
		if model.conversation == nil || model.conversation.ID != "next" {
			t.Fatalf("candidate not activated after idle: %#v", model.conversation)
		}
		assertOrderedPrefixes(t, sequence.snapshot(), "turn_end:", "save:old", "session_end:old:switch", "session_start:next:resumed")
	})

	t.Run("candidate failure preserves old state", func(t *testing.T) {
		sequence := &eventSequence{}
		store := newLifecycleStore(sequence)
		store.created = conversation.NewConversation("old", time.Unix(1, 0))
		store.loadErrors["bad"] = errors.New("candidate failed")
		hooks := newRecordingHook(sequence, nil)
		cfg := testAppConfig()
		cfg.UI.StartMode = config.StartModeNew
		model := New(Deps{Config: cfg, Provider: fakeProvider{}, Store: store, Resources: fakeResources{}, Hooks: hooks})
		old := model.conversation
		oldActivity := model.skillActivity
		model.mode = orchestrator.RunModePlan
		model.status.Mode = string(orchestrator.RunModePlan)
		model.status.ActiveSkills = "keep"
		sequence.reset()

		model.loadConversation("bad")
		if model.conversation != old || model.skillActivity != oldActivity || model.status.ActiveSkills != "keep" || model.mode != orchestrator.RunModePlan || !hooks.promptIsActive() {
			t.Fatalf("candidate failure mutated old state: conversation=%p activity=%p skills=%q mode=%s prompt=%t", model.conversation, model.skillActivity, model.status.ActiveSkills, model.mode, hooks.promptIsActive())
		}
		if countEventPrefix(sequence.snapshot(), "save:") != 0 || countEventPrefix(sequence.snapshot(), "session_end:") != 0 {
			t.Fatalf("candidate failure began old-session teardown: %v", sequence.snapshot())
		}
	})

	t.Run("save failure reports and continues", func(t *testing.T) {
		sequence := &eventSequence{}
		store := newLifecycleStore(sequence)
		store.created = conversation.NewConversation("old", time.Unix(1, 0))
		store.loaded["next"] = conversation.NewConversation("next", time.Unix(2, 0))
		store.saveErr = errors.New("password=save-secret")
		hooks := newRecordingHook(sequence, nil)
		cfg := testAppConfig()
		cfg.UI.StartMode = config.StartModeNew
		model := New(Deps{Config: cfg, Provider: fakeProvider{}, Store: store, Resources: fakeResources{}, Hooks: hooks})
		sequence.reset()

		model.loadConversation("next")
		if model.conversation == nil || model.conversation.ID != "next" || model.status.Error == nil {
			t.Fatalf("save failure did not continue switch/report error: conversation=%#v err=%v", model.conversation, model.status.Error)
		}
		if strings.Contains(model.status.Error.Error(), "save-secret") {
			t.Fatalf("save failure leaked sensitive detail: %v", model.status.Error)
		}
		assertOrderedEvents(t, sequence.snapshot(), "load:next", "save:old", "session_end:old:switch", "session_start:next:resumed")
	})
}

func TestAppCloseOrder(t *testing.T) {
	t.Run("active request order and repeated copies", testAppCloseActiveRequestOrder)
	t.Run("concurrent copies close once", testAppCloseConcurrentCopies)
}

func testAppCloseActiveRequestOrder(t *testing.T) {
	sequence := &eventSequence{}
	store := newLifecycleStore(sequence)
	store.created = conversation.NewConversation("active", time.Unix(1, 0))
	hooks := newRecordingHook(sequence, nil)
	provider := newBlockingLifecycleProvider()
	closer := &fakeCloser{}
	cfg := testAppConfig()
	cfg.UI.StartMode = config.StartModeNew
	model := New(Deps{Config: cfg, Provider: provider, Store: store, Resources: fakeResources{}, Hooks: hooks, Closer: closer})

	requestCtx, cancel := context.WithCancel(context.Background())
	events, err := model.orchestrator.SendRequest(requestCtx, model.conversation, orchestrator.RunRequest{UserText: "hold", Mode: orchestrator.RunModeDefault})
	if err != nil {
		t.Fatalf("start close-order request: %v", err)
	}
	model.beginRequest(cancel, events, "", false)
	drained := make(chan struct{})
	go func() {
		for range events {
		}
		close(drained)
	}()
	waitClosed(t, provider.started, "provider start")
	sequence.reset()
	copyOfModel := model

	if err := model.Close(context.Background()); err != nil {
		t.Fatalf("App.Close: %v", err)
	}
	waitClosed(t, drained, "canceled request drain")
	afterFirstClose := sequence.snapshot()
	if err := model.Close(context.Background()); err != nil {
		t.Fatalf("repeated App.Close: %v", err)
	}
	if err := copyOfModel.Close(context.Background()); err != nil {
		t.Fatalf("copied App.Close: %v", err)
	}

	eventsSeen := sequence.snapshot()
	assertOrderedPrefixes(t, eventsSeen, "turn_end:canceled", "save:active", "session_end:active:exit")
	if strings.Join(eventsSeen, "|") != strings.Join(afterFirstClose, "|") || countEventPrefix(eventsSeen, "session_end:") != 1 {
		t.Fatalf("App.Close was not idempotent: %v", eventsSeen)
	}
	if hooks.shutdownCount() != 0 || closer.closed {
		t.Fatalf("App.Close took process-resource ownership: hook shutdown=%d mcp closed=%t", hooks.shutdownCount(), closer.closed)
	}
	if model.conversation != nil || model.request != nil || model.streaming || model.status.Streaming || model.mode != orchestrator.RunModeDefault {
		t.Fatalf("App.Close did not clear app/session state: conversation=%p request=%p streaming=%t mode=%s", model.conversation, model.request, model.streaming, model.mode)
	}
}

func testAppCloseConcurrentCopies(t *testing.T) {
	sequence := &eventSequence{}
	store := newLifecycleStore(sequence)
	store.created = conversation.NewConversation("concurrent", time.Unix(1, 0))
	hooks := newRecordingHook(sequence, nil)
	closer := &fakeCloser{}
	cfg := testAppConfig()
	cfg.UI.StartMode = config.StartModeNew
	model := New(Deps{Config: cfg, Provider: fakeProvider{}, Store: store, Resources: fakeResources{}, Hooks: hooks, Closer: closer})
	first, second := model, model
	sequence.reset()

	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	for _, candidate := range []*Model{&first, &second} {
		candidate := candidate
		go func() {
			defer wait.Done()
			<-start
			errorsSeen <- candidate.Close(context.Background())
		}()
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent App.Close: %v", err)
		}
	}
	eventsSeen := sequence.snapshot()
	if countEventPrefix(eventsSeen, "save:concurrent") != 1 || countEventPrefix(eventsSeen, "session_end:concurrent:exit") != 1 {
		t.Fatalf("concurrent Model copies performed duplicate close work: %v", eventsSeen)
	}
	if hooks.shutdownCount() != 0 || closer.closed {
		t.Fatalf("concurrent App.Close took process-resource ownership: hook shutdown=%d mcp closed=%t", hooks.shutdownCount(), closer.closed)
	}
}

func TestLocalCommandHookExclusion(t *testing.T) {
	model, hooks, provider := newHookCommandModel(t, nil)
	hooks.setPromptActive(true)

	cases := []struct {
		input string
		want  []string
	}{
		{input: "/clear"},
		{input: "/plan"},
		{input: "/do"},
		{input: "/compact", want: []string{"compact_before:manual", "compact_after:success"}},
		{input: "/diagnostics"},
	}
	for _, tc := range cases {
		hooks.resetEvents()
		model = runCommandInput(t, model, tc.input)
		got := hooks.lifecycleEvents()
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Fatalf("%s emitted wrong Hook lifecycle: got=%v want=%v", tc.input, got, tc.want)
		}
	}
	if !hooks.promptIsActive() {
		t.Fatal("/clear cleared Hook-owned Prompt state")
	}
	if len(provider.Requests()) != 0 || len(model.conversation.Messages) != 0 {
		t.Fatalf("local command reached model/history: requests=%d messages=%#v", len(provider.Requests()), model.conversation.Messages)
	}
}

func TestHookDiagnosticIsolation(t *testing.T) {
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redact.Text})
	model, hooks, provider := newHookCommandModel(t, collector)
	hooks.failCompact = true

	model = runCommandInput(t, model, "/compact")
	if collector.Count() != 1 || model.status.Error != nil {
		t.Fatalf("fail-open Hook diagnostic changed command outcome: diagnostics=%d err=%v", collector.Count(), model.status.Error)
	}
	if strings.Contains(model.messages.View(), "hook_action_failed") || len(model.conversation.Messages) != 0 || len(provider.Requests()) != 0 {
		t.Fatalf("Hook diagnostic leaked before explicit diagnostics command: view=%q messages=%#v requests=%d", model.messages.View(), model.conversation.Messages, len(provider.Requests()))
	}

	model = runCommandInput(t, model, "/diagnostics")
	if !strings.Contains(model.status.Notice, "hook_action_failed") || strings.Contains(model.status.Notice, "hook-secret") {
		t.Fatalf("diagnostics command did not expose only redacted local failure: %q", model.status.Notice)
	}
	model = runCommandInput(t, model, "ordinary request")
	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("ordinary request count=%d", len(requests))
	}
	wire := visibleRequestText(requests[0])
	if strings.Contains(wire, "hook_action_failed") || strings.Contains(wire, "hook-secret") {
		t.Fatalf("Hook diagnostic leaked into next Provider request: %q", wire)
	}
	for _, message := range model.conversation.Messages {
		if strings.Contains(message.Content, "hook_action_failed") || strings.Contains(message.Content, "hook-secret") {
			t.Fatalf("Hook diagnostic leaked into Conversation: %#v", model.conversation.Messages)
		}
	}
}

func TestRuntimeRedactorCoversEveryAppStatusBoundary(t *testing.T) {
	const secret = "opaque-app-status-canary-9f31"
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(secret)
	model := Model{
		screen: screenChat,
		input:  tui.NewInput(""),
		deps: Deps{
			Redact:    runtimeRedactor.Text,
			MCPStatus: &fakeCloser{status: "mcp failure " + secret},
		},
	}

	controller := &commandController{model: &model}
	controller.DisplayNotice("notice " + secret)
	if strings.Contains(model.status.Notice, secret) {
		t.Fatalf("notice leaked runtime secret: %q", model.status.Notice)
	}
	controller.DisplayError(errors.New("command failure " + secret))
	if model.status.Error == nil || strings.Contains(model.status.Error.Error(), secret) {
		t.Fatalf("command error leaked runtime secret: %v", model.status.Error)
	}

	model.refreshMCPStatus()
	if strings.Contains(model.status.MCP, secret) {
		t.Fatalf("MCP status leaked runtime secret: %q", model.status.MCP)
	}
	recovery := recoveryNotice(conversation.RecoveryReport{Diagnostics: []diagnostics.Diagnostic{
		diagnostics.New("recovery", diagnostics.SeverityWarning, "bad record "+secret),
	}}, model.redactText)
	if strings.Contains(recovery, secret) {
		t.Fatalf("recovery notice leaked runtime secret: %q", recovery)
	}

	updated, _ := model.Update(eventMsg{
		event:  Event{Type: EventError, Err: errors.New("provider failure " + secret)},
		events: make(chan Event),
	})
	model = updated.(Model)
	if model.status.Error == nil || strings.Contains(model.status.Error.Error(), secret) || model.lastError == nil || strings.Contains(model.lastError.Error(), secret) {
		t.Fatalf("stream error leaked runtime secret: status=%v last=%v", model.status.Error, model.lastError)
	}
}

func newHookCommandModel(t *testing.T, collector *diagnostics.Collector) (Model, *recordingHook, *recordingCommandProvider) {
	t.Helper()
	if collector == nil {
		collector = diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: redact.Text})
	}
	sequence := &eventSequence{}
	hooks := newRecordingHook(sequence, collector)
	store := newLifecycleStore(sequence)
	store.created = conversation.NewConversation("commands", time.Unix(1, 0))
	provider := &recordingCommandProvider{}
	enabled := true
	manager := contextmgr.New(provider, t.TempDir(), config.ContextConfig{
		Enabled:             &enabled,
		RecentKeepMessages:  100,
		SummaryFailureLimit: 3,
	})
	cfg := testAppConfig()
	cfg.UI.StartMode = config.StartModeNew
	model := New(Deps{
		Config: cfg, Provider: provider, Store: store, Resources: fakeResources{},
		ContextManager: manager, Diagnostics: collector, Hooks: hooks,
	})
	hooks.resetEvents()
	return model, hooks, provider
}

type eventSequence struct {
	mu     sync.Mutex
	events []string
}

func (s *eventSequence) add(event string) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func (s *eventSequence) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *eventSequence) reset() {
	s.mu.Lock()
	s.events = nil
	s.mu.Unlock()
}

type lifecycleStore struct {
	mu           sync.Mutex
	sequence     *eventSequence
	created      *conversation.Conversation
	loaded       map[string]*conversation.Conversation
	loadErrors   map[string]error
	saveErr      error
	loadObserved chan struct{}
	loadOnce     sync.Once
}

func newLifecycleStore(sequence *eventSequence) *lifecycleStore {
	return &lifecycleStore{
		sequence: sequence, loaded: map[string]*conversation.Conversation{}, loadErrors: map[string]error{},
		loadObserved: make(chan struct{}),
	}
}

func (s *lifecycleStore) List(context.Context) ([]conversation.Conversation, error) { return nil, nil }

func (s *lifecycleStore) Load(_ context.Context, id string) (*conversation.Conversation, error) {
	s.sequence.add("load:" + id)
	s.loadOnce.Do(func() { close(s.loadObserved) })
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadErrors[id]; err != nil {
		return nil, err
	}
	conv := s.loaded[id]
	if conv == nil {
		return nil, fmt.Errorf("conversation %s not found", id)
	}
	return conv, nil
}

func (s *lifecycleStore) Save(_ context.Context, conv *conversation.Conversation) error {
	id := "<nil>"
	if conv != nil {
		id = conv.ID
	}
	s.sequence.add("save:" + id)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveErr
}

func (s *lifecycleStore) Create(context.Context) (*conversation.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.created == nil {
		return nil, errors.New("create failed")
	}
	s.sequence.add("create:" + s.created.ID)
	return s.created, nil
}

type recordingHook struct {
	hook.Runtime
	mu            sync.Mutex
	sequence      *eventSequence
	diagnostics   *diagnostics.Collector
	events        []string
	promptActive  bool
	failCompact   bool
	shutdownCalls int
	turn          int
}

func newRecordingHook(sequence *eventSequence, collector *diagnostics.Collector) *recordingHook {
	return &recordingHook{Runtime: hook.Noop(), sequence: sequence, diagnostics: collector}
}

func (r *recordingHook) record(event string) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	if r.sequence != nil {
		r.sequence.add(event)
	}
}

func (r *recordingHook) SystemStart(context.Context) { r.record("system_start") }

func (r *recordingHook) Shutdown(context.Context) error {
	r.mu.Lock()
	r.shutdownCalls++
	r.mu.Unlock()
	r.record("system_stop")
	return nil
}

func (r *recordingHook) SessionStart(_ context.Context, id string, state hook.SessionState) {
	r.mu.Lock()
	r.promptActive = true
	r.mu.Unlock()
	r.record("session_start:" + id + ":" + string(state))
}

func (r *recordingHook) SessionEnd(_ context.Context, id string, reason hook.SessionEndReason) {
	r.record("session_end:" + id + ":" + string(reason))
	r.mu.Lock()
	r.promptActive = false
	r.mu.Unlock()
}

func (r *recordingHook) BeginTurn(_ context.Context, sessionID string, kind hook.ExecutionKind, mode hook.HookMode) hook.ExecutionRef {
	r.mu.Lock()
	r.turn++
	turn := r.turn
	r.mu.Unlock()
	r.record("turn_start:" + string(kind))
	return hook.ExecutionRef{SessionID: sessionID, ExecutionID: fmt.Sprintf("execution-%d", turn), TurnID: fmt.Sprintf("turn-%d", turn), Kind: kind, Mode: mode}
}

func (r *recordingHook) EndTurn(_ context.Context, _ hook.ExecutionRef, status hook.TurnStatus, _ string) {
	r.record("turn_end:" + string(status))
}

func (r *recordingHook) BeginMessage(ctx context.Context, ref hook.ExecutionRef, role hook.MessageRole, content string) hook.MessageToken {
	r.record("message_before:" + string(role))
	return r.Runtime.BeginMessage(ctx, ref, role, content)
}

func (r *recordingHook) EndMessage(ctx context.Context, token hook.MessageToken) {
	r.record("message_after")
	r.Runtime.EndMessage(ctx, token)
}

func (r *recordingHook) BeforeCompact(ctx context.Context, binding hook.CompactBinding, input hook.CompactInput) hook.CompactToken {
	r.record("compact_before:" + string(input.Reason))
	r.mu.Lock()
	fail := r.failCompact
	collector := r.diagnostics
	r.mu.Unlock()
	if fail && collector != nil {
		collector.Add(diagnostics.New("hook_action_failed", diagnostics.SeverityWarning, "token=hook-secret"))
	}
	return r.Runtime.BeforeCompact(ctx, binding, input)
}

func (r *recordingHook) AfterCompact(ctx context.Context, token hook.CompactToken, output hook.CompactOutput) {
	r.record("compact_after:" + string(output.Status))
	r.Runtime.AfterCompact(ctx, token, output)
}

func (r *recordingHook) lifecycleEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *recordingHook) resetEvents() {
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
}

func (r *recordingHook) setPromptActive(active bool) {
	r.mu.Lock()
	r.promptActive = active
	r.mu.Unlock()
}

func (r *recordingHook) promptIsActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.promptActive
}

func (r *recordingHook) shutdownCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shutdownCalls
}

type blockingLifecycleProvider struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
}

func newBlockingLifecycleProvider() *blockingLifecycleProvider {
	return &blockingLifecycleProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (*blockingLifecycleProvider) Name() string { return "blocking" }

func (p *blockingLifecycleProvider) StreamChat(ctx context.Context, request provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.startedOnce.Do(func() { close(p.started) })
	if request.Observer != nil {
		request.Observer.MarkSent()
	}
	out := make(chan provider.StreamEvent)
	go func() {
		defer close(out)
		if request.Observer != nil {
			defer request.Observer.Finish(true)
		}
		select {
		case <-ctx.Done():
			return
		case <-p.release:
		}
		select {
		case <-ctx.Done():
		case out <- provider.StreamEvent{Type: provider.StreamEventDone}:
		}
	}()
	return out, nil
}

func waitClosed(t *testing.T, channel <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func assertOrderedEvents(t *testing.T, events []string, want ...string) {
	t.Helper()
	position := 0
	for _, event := range events {
		if position < len(want) && event == want[position] {
			position++
		}
	}
	if position != len(want) {
		t.Fatalf("events are not ordered: got=%v want subsequence=%v", events, want)
	}
}

func assertOrderedPrefixes(t *testing.T, events []string, want ...string) {
	t.Helper()
	position := 0
	for _, event := range events {
		if position < len(want) && strings.HasPrefix(event, want[position]) {
			position++
		}
	}
	if position != len(want) {
		t.Fatalf("event prefixes are not ordered: got=%v want subsequence=%v", events, want)
	}
}

func countEventPrefix(events []string, prefix string) int {
	count := 0
	for _, event := range events {
		if strings.HasPrefix(event, prefix) {
			count++
		}
	}
	return count
}

func visibleRequestText(request provider.ChatRequest) string {
	parts := []string{request.SystemPrompt}
	for _, block := range request.System {
		parts = append(parts, block.Name, block.Content)
	}
	for _, block := range request.StableSystem {
		parts = append(parts, block.Name, block.Content)
	}
	for _, block := range request.DynamicSystem {
		parts = append(parts, block.Name, block.Content)
	}
	for _, message := range request.Messages {
		parts = append(parts, message.Content, message.ToolResultContent)
	}
	return strings.Join(parts, "\n")
}
