package app

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
	"xagent/internal/tui"
)

func TestRuntimeConversationRequestStateAreSeparated(t *testing.T) {
	t.Run("approved field ownership", func(t *testing.T) {
		assertStateFields(t, reflect.TypeOf(RuntimeState{}), []stateField{
			{Name: "RequestSequence", Type: reflect.TypeOf(uint64(0))},
			{Name: "ShowResponseTimer", Type: reflect.TypeOf(false)},
			{Name: "StartMode", Type: reflect.TypeOf("")},
		})
		assertStateFields(t, reflect.TypeOf(ConversationState{}), []stateField{
			{Name: "ActiveID", Type: reflect.TypeOf("")},
			{Name: "Mode", Type: reflect.TypeOf("")},
			{Name: "Skills", Type: reflect.TypeOf([]string(nil))},
			{Name: "SkillGeneration", Type: reflect.TypeOf(uint64(0))},
			{Name: "Input", Type: reflect.TypeOf(redact.SafeText{})},
			{Name: "Messages", Type: reflect.TypeOf([]redact.SafeText(nil))},
			{Name: "Notice", Type: reflect.TypeOf(redact.SafeText{})},
		})
		assertStateFields(t, reflect.TypeOf(RequestState{}), []stateField{
			{Name: "Generation", Type: reflect.TypeOf(uint64(0))},
			{Name: "Duration", Type: reflect.TypeOf(time.Duration(0))},
			{Name: "Tokens", Type: reflect.TypeOf(Usage{})},
			{Name: "Cache", Type: reflect.TypeOf(CacheUsage{})},
			{Name: "StopReason", Type: reflect.TypeOf("")},
			{Name: "LastError", Type: reflect.TypeOf((*diagnostics.SafeError)(nil))},
			{Name: "Confirmation", Type: reflect.TypeOf((*ConfirmationState)(nil))},
			{Name: "TransientIDs", Type: reflect.TypeOf([]string(nil))},
		})
	})

	t.Run("request generations belong to runtime and survive request replacement", func(t *testing.T) {
		runtime := RuntimeState{}
		conversation := ConversationState{ActiveID: "session-7", Mode: "plan", Skills: []string{"review"}}

		first := runtime.newRequestState()
		first.Duration = 3 * time.Second
		first.Tokens = Usage{InputTokens: 13, OutputTokens: 21}
		first.Cache = CacheUsage{CacheCreationInputTokens: 8, CacheReadInputTokens: 5}
		first.StopReason = "end_turn"
		first.TransientIDs = []string{"transient-1"}

		first = RequestState{}
		second := runtime.newRequestState()

		if !reflect.DeepEqual(first, RequestState{}) {
			t.Fatalf("replaced request state = %#v, want zero value", first)
		}
		if runtime.RequestSequence != 2 || second.Generation != 2 {
			t.Fatalf("runtime sequence = %d, request generation = %d; want 2, 2", runtime.RequestSequence, second.Generation)
		}
		if conversation.ActiveID != "session-7" || conversation.Mode != "plan" || !reflect.DeepEqual(conversation.Skills, []string{"review"}) {
			t.Fatalf("request replacement changed conversation state: %#v", conversation)
		}
	})

	t.Run("request state carries only safe display data", func(t *testing.T) {
		const canary = "t41-secret-canary"
		redactor := redact.NewRuntimeRedactor()
		redactor.RegisterSecret(canary)
		safe := redactor.Redact("safe after " + canary)

		request := RequestState{
			Generation: 34,
			Duration:   2500 * time.Millisecond,
			Tokens:     Usage{InputTokens: 144, OutputTokens: 55},
			Cache: CacheUsage{
				CacheCreationInputTokens: 89,
				CacheReadInputTokens:     34,
			},
			StopReason: "max_tokens",
			LastError: &diagnostics.SafeError{
				Code:        "provider_failed",
				Source:      "provider",
				Message:     safe,
				Recoverable: true,
			},
			Confirmation: &ConfirmationState{
				CallID:         "call-8",
				Name:           "Bash",
				Prompt:         safe,
				Target:         safe,
				Risk:           "high",
				PermissionMode: "default",
				ScopePreview:   safe,
				RuleLocation:   safe,
				Scopes: []ConfirmationScopeState{{
					Scope: "once", Available: true, Description: safe,
				}},
				Warning:        safe,
				RevokeHint:     safe,
				AllowPermanent: true,
			},
			TransientIDs: []string{"transient-8"},
		}

		if request.Generation != 34 || request.Duration != 2500*time.Millisecond ||
			request.Tokens.InputTokens != 144 || request.Tokens.OutputTokens != 55 ||
			request.Cache.CacheCreationInputTokens != 89 || request.Cache.CacheReadInputTokens != 34 ||
			request.StopReason != "max_tokens" || request.LastError == nil || request.Confirmation == nil ||
			!reflect.DeepEqual(request.TransientIDs, []string{"transient-8"}) {
			t.Fatalf("request state lost typed values: %#v", request)
		}
		for name, value := range map[string]string{
			"error":         request.LastError.Message.Text(),
			"prompt":        request.Confirmation.Prompt.Text(),
			"target":        request.Confirmation.Target.Text(),
			"scope preview": request.Confirmation.ScopePreview.Text(),
			"rule location": request.Confirmation.RuleLocation.Text(),
			"scope detail":  request.Confirmation.Scopes[0].Description.Text(),
			"warning":       request.Confirmation.Warning.Text(),
			"revoke hint":   request.Confirmation.RevokeHint.Text(),
		} {
			if strings.Contains(value, canary) {
				t.Fatalf("%s retained raw canary", name)
			}
		}

		for _, stateType := range []reflect.Type{
			reflect.TypeOf(RuntimeState{}),
			reflect.TypeOf(ConversationState{}),
			reflect.TypeOf(RequestState{}),
		} {
			assertStateHasNoRawPayload(t, stateType, map[reflect.Type]bool{})
		}
	})

	t.Run("generation allocation fails closed before reuse", func(t *testing.T) {
		runtime := RuntimeState{RequestSequence: math.MaxUint64}
		defer func() {
			if recover() == nil {
				t.Fatal("exhausted request sequence did not fail closed")
			}
			if runtime.RequestSequence != math.MaxUint64 {
				t.Fatalf("exhausted sequence changed to %d", runtime.RequestSequence)
			}
		}()
		_ = runtime.newRequestState()
	})
}

func TestViewModelIsSafeAndImmutable(t *testing.T) {
	const canary = "t412-view-model-secret"
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(canary)
	safe := redactor.Redact("visible " + canary)
	updatedAt := time.Date(2026, time.August, 3, 16, 0, 0, 0, time.UTC)

	runtimeState := RuntimeState{RequestSequence: 51, ShowResponseTimer: true, StartMode: config.StartModeList}
	conversationState := ConversationState{
		ActiveID: "conversation-51", Mode: "plan", Skills: []string{"review"}, SkillGeneration: 51,
		Input: safe, Messages: []redact.SafeText{safe}, Notice: safe,
	}
	requestState := RequestState{
		Generation: 51, Duration: 1250 * time.Millisecond,
		Tokens:     Usage{InputTokens: 13, OutputTokens: 8},
		Cache:      CacheUsage{CacheCreationInputTokens: 5, CacheReadInputTokens: 3},
		StopReason: "end_turn",
		LastError:  &diagnostics.SafeError{Code: "safe_error", Source: "app", Message: safe, Recoverable: true},
		Confirmation: &ConfirmationState{
			CallID: "call-51", Name: "Bash", Prompt: safe, Risk: "high", PermissionMode: "default",
			Target: safe, ScopePreview: safe, RuleLocation: safe,
			Scopes:  []ConfirmationScopeState{{Scope: "once", Available: true, Description: safe}},
			Warning: safe, RevokeHint: safe, AllowPermanent: true,
		},
		TransientIDs: []string{"transient-51"},
	}
	listState := ConversationListState{
		Entries: []ConversationListEntryState{{
			ID: "conversation-51", Title: safe, UpdatedAt: updatedAt, MessageCount: 7,
			Available: true, Selectable: true, RecoveryStatus: conversation.RecoveryPartial, RecoveryNotice: safe,
		}},
		Truncated: true, ScannedFiles: 11, ScannedBytes: 4096, Notice: safe,
	}

	view := newViewModel(runtimeState, conversationState, requestState, listState, screenChat)

	conversationState.ActiveID = "mutated"
	conversationState.Skills[0] = "mutated"
	conversationState.Messages[0] = redact.SafeText{}
	requestState.TransientIDs[0] = "mutated"
	requestState.LastError.Code = "mutated"
	requestState.Confirmation.CallID = "mutated"
	requestState.Confirmation.Scopes[0].Scope = "mutated"
	requestState.Confirmation.Scopes[0].Description = redact.SafeText{}
	listState.Entries[0].ID = "mutated"
	listState.Entries[0].Title = redact.SafeText{}

	conversationView := view.Conversation()
	requestView := view.Request()
	sessionsView := view.Sessions()
	entries := sessionsView.Entries()
	lastError, hasError := requestView.LastError()
	confirmationView, hasConfirmation := requestView.Confirmation()
	confirmationScopes := confirmationView.Scopes()
	if view.Generation() != 51 || view.RuntimeSequence() != 51 || view.Screen() != tui.ScreenChat ||
		conversationView.ActiveID() != "conversation-51" || conversationView.Mode() != "plan" ||
		requestView.Duration() != 1250*time.Millisecond || requestView.InputTokens() != 13 || requestView.OutputTokens() != 8 ||
		requestView.CacheCreated() != 5 || requestView.CacheRead() != 3 || requestView.StopReason() != "end_turn" ||
		!hasError || lastError.Code() != "safe_error" || lastError.Source() != "app" || !lastError.Recoverable() ||
		!hasConfirmation || confirmationView.CallID() != "call-51" || confirmationView.Name() != "Bash" ||
		confirmationView.Risk() != "high" || confirmationView.PermissionMode() != "default" || !confirmationView.AllowPermanent() ||
		len(confirmationScopes) != 1 || confirmationScopes[0].Scope() != "once" || !confirmationScopes[0].Available() ||
		confirmationScopes[0].Description().Text() != safe.Text() ||
		len(entries) != 1 || entries[0].ID() != "conversation-51" || entries[0].UpdatedAtUnixMilli() != updatedAt.UnixMilli() ||
		entries[0].MessageCount() != 7 || !entries[0].Available() || !entries[0].Selectable() ||
		entries[0].RecoveryStatus() != string(conversation.RecoveryPartial) || !sessionsView.Truncated() ||
		sessionsView.ScannedFiles() != 11 || sessionsView.ScannedBytes() != 4096 {
		t.Fatalf("ViewModel projection changed or aliased App state: view=%#v entries=%#v", view, entries)
	}

	skills := conversationView.Skills()
	messages := conversationView.Messages()
	lines := view.Lines()
	transientIDs := requestView.TransientIDs()
	confirmationScopes[0] = tui.ConfirmationScopeView{}
	skills[0] = "renderer mutation"
	messages[0] = redact.SafeText{}
	lines[0] = redact.SafeText{}
	transientIDs[0] = "renderer mutation"
	entries[0] = tui.SessionListEntry{}
	if conversationView.Skills()[0] != "review" || conversationView.Messages()[0].Text() != safe.Text() ||
		view.Lines()[0].Text() != safe.Text() || requestView.TransientIDs()[0] != "transient-51" ||
		confirmationView.Scopes()[0].Scope() != "once" || confirmationView.Scopes()[0].Description().Text() != safe.Text() ||
		sessionsView.Entries()[0].ID() != "conversation-51" {
		t.Fatal("ViewModel exposed mutable backing storage")
	}

	for name, value := range map[string]string{
		"input": conversationView.Input().Text(), "message": conversationView.Messages()[0].Text(),
		"notice": conversationView.Notice().Text(), "error": lastError.Message().Text(),
		"confirmation": confirmationView.Prompt().Text(), "target": confirmationView.Target().Text(),
		"scope": confirmationView.ScopePreview().Text(), "rule location": confirmationView.RuleLocation().Text(),
		"scope detail": confirmationView.Scopes()[0].Description().Text(),
		"warning":      confirmationView.Warning().Text(), "revoke": confirmationView.RevokeHint().Text(),
		"list title": sessionsView.Entries()[0].Title().Text(), "list recovery": sessionsView.Entries()[0].RecoveryNotice().Text(),
		"list notice": sessionsView.Notice().Text(),
	} {
		if value != safe.Text() {
			t.Fatalf("ViewModel %s = %q, want safe projection %q", name, value, safe.Text())
		}
		if strings.Contains(value, canary) {
			t.Fatalf("ViewModel %s retained raw canary", name)
		}
	}

	emptyRequest := newViewModel(RuntimeState{}, ConversationState{}, RequestState{}, ConversationListState{}, screenList).Request()
	if _, present := emptyRequest.LastError(); present {
		t.Fatal("zero request projected a present error")
	}
	if _, present := emptyRequest.Confirmation(); present {
		t.Fatal("zero request projected a present confirmation")
	}
}

func TestConfirmationScopeProjectionDefensivelyCopiesEveryAppBoundary(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	description := redactor.Redact("allow only this call")
	source := Event{Confirmation: &events.ToolConfirmationRequest{
		Scopes: []events.ConfirmationScopeDisplay{{Scope: "once", Available: true, Description: description}},
	}}

	sealed := cloneAppEvent(source)
	source.Confirmation.Scopes[0].Scope = "source-mutated"
	if sealed.Confirmation.Scopes[0].Scope != "once" {
		t.Fatal("sealed event aliased the source confirmation scopes")
	}

	states := confirmationScopeStates(sealed.Confirmation.Scopes)
	sealed.Confirmation.Scopes[0].Scope = "event-mutated"
	if states[0].Scope != "once" || states[0].Description.Text() != description.Text() {
		t.Fatal("App confirmation state aliased the sealed event scopes")
	}

	specs := confirmationScopeViewSpecs(states)
	states[0].Scope = "state-mutated"
	if specs[0].Scope != "once" || specs[0].Description.Text() != description.Text() {
		t.Fatal("TUI confirmation spec aliased App confirmation state")
	}

	for _, test := range []struct {
		name   string
		source []events.ConfirmationScopeDisplay
	}{
		{name: "nil", source: nil},
		{name: "explicit empty", source: []events.ConfirmationScopeDisplay{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cloned := cloneConfirmationScopeDisplays(test.source)
			projected := confirmationScopeStates(test.source)
			if (cloned == nil) != (test.source == nil) || (projected == nil) != (test.source == nil) {
				t.Fatalf("nil/empty distinction changed: cloned=%#v projected=%#v", cloned, projected)
			}
		})
	}
}

func TestRequestAndConversationResetBoundaries(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	providerSentinel := &struct{ name string }{name: "provider"}
	mcpSentinel := &struct{ name string }{name: "mcp"}
	skills := []string{"review", "test"}
	messages := []redact.SafeText{redactor.Redact("safe message")}
	conversation := ConversationState{
		ActiveID:        "conversation-old",
		Mode:            "plan",
		Skills:          skills,
		SkillGeneration: 40,
		Input:           redactor.Redact("draft input"),
		Messages:        messages,
		Notice:          redactor.Redact("safe notice"),
	}
	runtime := RuntimeState{RequestSequence: 40}
	runtimeAddress := &runtime
	request := RequestState{
		Generation:   40,
		Duration:     9 * time.Second,
		Tokens:       Usage{InputTokens: 8, OutputTokens: 13},
		Cache:        CacheUsage{CacheCreationInputTokens: 21, CacheReadInputTokens: 34},
		StopReason:   "end_turn",
		LastError:    &diagnostics.SafeError{Code: "provider_failed", Message: redactor.Redact("safe error")},
		Confirmation: &ConfirmationState{CallID: "call-old", Name: "Bash", Prompt: redactor.Redact("safe prompt")},
		TransientIDs: []string{"old-transient"},
	}
	oldTransientIDs := request.TransientIDs

	runtime.resetRequest(&conversation, &request)

	if &runtime != runtimeAddress || runtime.RequestSequence != 41 {
		t.Fatalf("request reset rebuilt or cleared runtime: address=%p want=%p state=%#v", &runtime, runtimeAddress, runtime)
	}
	if !reflect.DeepEqual(request, RequestState{Generation: 41}) {
		t.Fatalf("request reset retained request-scoped state: %#v", request)
	}
	if conversation.ActiveID != "conversation-old" || conversation.Mode != "plan" ||
		!reflect.DeepEqual(conversation.Skills, []string{"review", "test"}) ||
		conversation.Input.Text() != "draft input" || len(conversation.Messages) != 1 ||
		conversation.Messages[0].Text() != "safe message" || conversation.Notice.Text() != "safe notice" {
		t.Fatalf("request reset changed conversation state: %#v", conversation)
	}
	if conversation.SkillGeneration != request.Generation {
		t.Fatalf("preserved Skill Activity generation = %d, want %d", conversation.SkillGeneration, request.Generation)
	}
	oldTransientIDs[0] = "late-request-write"
	if request.TransientIDs != nil {
		t.Fatalf("new request aliases replaced transient IDs: %#v", request.TransientIDs)
	}
	if providerSentinel.name != "provider" || mcpSentinel.name != "mcp" {
		t.Fatalf("request reset changed runtime owners: provider=%#v mcp=%#v", providerSentinel, mcpSentinel)
	}

	request.Duration = time.Second
	request.Tokens = Usage{InputTokens: 1, OutputTokens: 2}
	request.Cache = CacheUsage{CacheCreationInputTokens: 3, CacheReadInputTokens: 5}
	request.StopReason = "max_tokens"
	request.LastError = &diagnostics.SafeError{Code: "late", Message: redactor.Redact("late safe error")}
	request.Confirmation = &ConfirmationState{CallID: "call-late", Prompt: redactor.Redact("late safe prompt")}
	request.TransientIDs = []string{"late-transient"}
	oldSkills := conversation.Skills
	oldMessages := conversation.Messages

	activity := &recordingSkillActivity{}
	runtime.resetConversation(&conversation, &request, "conversation-new", activity)

	if &runtime != runtimeAddress || runtime.RequestSequence != 42 {
		t.Fatalf("conversation reset rebuilt or cleared runtime: address=%p want=%p state=%#v", &runtime, runtimeAddress, runtime)
	}
	if !reflect.DeepEqual(request, RequestState{Generation: 42}) {
		t.Fatalf("conversation reset retained request-scoped state: %#v", request)
	}
	if conversation.ActiveID != "conversation-new" || conversation.Mode != "" || conversation.Skills != nil ||
		conversation.Input.Text() != "" || conversation.Messages != nil || conversation.Notice.Text() != "" {
		t.Fatalf("conversation reset retained conversation-scoped state: %#v", conversation)
	}
	if conversation.SkillGeneration != request.Generation {
		t.Fatalf("cleared conversation Skill generation = %d, want %d", conversation.SkillGeneration, request.Generation)
	}
	if activity.clearCalls != 1 {
		t.Fatalf("conversation reset Clear calls = %d, want 1", activity.clearCalls)
	}
	oldSkills[0] = "late-skill-write"
	oldMessages[0] = redactor.Redact("late-message-write")
	if conversation.Skills != nil || conversation.Messages != nil {
		t.Fatalf("new conversation aliases replaced views: skills=%#v messages=%#v", conversation.Skills, conversation.Messages)
	}
	if providerSentinel.name != "provider" || mcpSentinel.name != "mcp" {
		t.Fatalf("conversation reset changed runtime owners: provider=%#v mcp=%#v", providerSentinel, mcpSentinel)
	}
}

func TestTimerAndUsageRespectResetAndConfig(t *testing.T) {
	disabled := runtimeStateFromResolvedUI(config.UIConfig{
		ShowResponseTimer: false,
		StartMode:         config.StartModeList,
	})
	request := RequestState{
		Generation:   7,
		Duration:     1750 * time.Millisecond,
		Tokens:       Usage{InputTokens: 13, OutputTokens: 21},
		Cache:        CacheUsage{CacheCreationInputTokens: 8, CacheReadInputTokens: 5},
		StopReason:   "completed",
		LastError:    &diagnostics.SafeError{Code: "old_error"},
		Confirmation: &ConfirmationState{CallID: "old-confirmation"},
		TransientIDs: []string{"old-transient"},
	}

	disabledView := newViewModel(disabled, ConversationState{}, request, ConversationListState{}, screenChat).Request()
	if disabledView.Duration() != 0 {
		t.Fatalf("disabled timer projected duration %s", disabledView.Duration())
	}
	if disabledView.InputTokens() != 13 || disabledView.OutputTokens() != 21 ||
		disabledView.CacheCreated() != 8 || disabledView.CacheRead() != 5 {
		t.Fatalf("disabling timer removed usage: %#v", disabledView)
	}

	enabled := runtimeStateFromResolvedUI(config.UIConfig{
		ShowResponseTimer: true,
		StartMode:         config.StartModeList,
	})
	enabledView := newViewModel(enabled, ConversationState{}, request, ConversationListState{}, screenChat).Request()
	if enabledView.Duration() != 1750*time.Millisecond {
		t.Fatalf("enabled timer duration = %s", enabledView.Duration())
	}

	conversationState := ConversationState{ActiveID: "session-a", Mode: "plan"}
	disabled.RequestSequence = request.Generation
	disabled.resetRequest(&conversationState, &request)
	if !reflect.DeepEqual(request, RequestState{Generation: 8}) {
		t.Fatalf("new request retained request state: %#v", request)
	}
	if disabled.ShowResponseTimer || disabled.StartMode != config.StartModeList {
		t.Fatalf("request reset changed resolved UI config: %#v", disabled)
	}

	model := Model{
		status: tui.Status{
			ShowResponseTimer:        false,
			Duration:                 3 * time.Second,
			InputTokens:              34,
			OutputTokens:             55,
			CacheCreationInputTokens: 89,
			CacheReadInputTokens:     144,
			StopReason:               "completed",
			Error:                    errStatusResetTest{},
		},
		lastError:    errStatusResetTest{},
		confirmation: &Event{},
	}
	model.resetCommandState()
	if model.status.Duration != 0 || model.status.InputTokens != 0 || model.status.OutputTokens != 0 ||
		model.status.CacheCreationInputTokens != 0 || model.status.CacheReadInputTokens != 0 ||
		model.status.StopReason != "" || model.status.Error != nil || model.lastError != nil || model.confirmation != nil {
		t.Fatalf("conversation reset retained old request presentation: status=%#v last=%v confirmation=%#v", model.status, model.lastError, model.confirmation)
	}
	if model.status.ShowResponseTimer {
		t.Fatal("conversation reset overwrote explicit false timer config")
	}

	requestModel := Model{
		deps:         Deps{Config: testAppConfig()},
		conversation: &conversation.Conversation{ID: "session-request"},
		input:        tui.NewInput("prompt"),
		status: tui.Status{
			ShowResponseTimer:        true,
			Duration:                 5 * time.Second,
			InputTokens:              89,
			OutputTokens:             144,
			CacheCreationInputTokens: 233,
			CacheReadInputTokens:     377,
			StopReason:               "completed",
			Error:                    errStatusResetTest{},
		},
		lastError:    errStatusResetTest{},
		confirmation: &Event{},
	}
	if cmd := requestModel.beginRequest(func() {}, closedEvents(), "", false); cmd == nil {
		t.Fatal("new request did not produce an event listener")
	}
	if requestModel.status.Duration != 0 || requestModel.status.InputTokens != 0 || requestModel.status.OutputTokens != 0 ||
		requestModel.status.CacheCreationInputTokens != 0 || requestModel.status.CacheReadInputTokens != 0 ||
		requestModel.status.StopReason != "" || requestModel.status.Error != nil || requestModel.lastError != nil || requestModel.confirmation != nil {
		t.Fatalf("new request retained old presentation: status=%#v last=%v confirmation=%#v", requestModel.status, requestModel.lastError, requestModel.confirmation)
	}
	if !requestModel.status.ShowResponseTimer || !requestModel.status.Streaming {
		t.Fatalf("new request changed config or did not enter streaming: %#v", requestModel.status)
	}

	t.Run("resolved timer controls App status and Done publication", func(t *testing.T) {
		const duration = 1750*time.Millisecond + 600*time.Microsecond
		for _, test := range []struct {
			name         string
			show         bool
			wantDuration time.Duration
		}{
			{name: "disabled"},
			{name: "enabled", show: true, wantDuration: 1751 * time.Millisecond},
		} {
			t.Run(test.name, func(t *testing.T) {
				cfg := testAppConfig()
				cfg.UI.ShowResponseTimer = test.show
				model := New(Deps{
					Config: cfg, Provider: fakeProvider{name: "fake"}, Store: &fakeConversationStore{}, Resources: fakeResources{},
				})
				if model.runtime.ShowResponseTimer != test.show || model.status.ShowResponseTimer != test.show {
					t.Fatalf("resolved timer was not copied into App state: runtime=%#v status=%#v", model.runtime, model.status)
				}
				model.conversation = conversation.NewConversation("timer-session", time.Unix(1, 0))
				model.screen = screenChat
				if cmd := model.beginRequest(func() {}, closedEvents(), "", false); cmd == nil {
					t.Fatal("beginRequest did not return a listener")
				}
				updated, _ := model.Update(testEventMessage(t, &model, events.Event{Type: events.Done, Duration: duration}, closedEvents()))
				model = updated.(Model)
				if model.status.Duration != test.wantDuration || model.status.ShowResponseTimer != test.show {
					t.Fatalf("Done duration = %s (show=%t), want %s (show=%t)", model.status.Duration, model.status.ShowResponseTimer, test.wantDuration, test.show)
				}
			})
		}
	})

	t.Run("main beginRequest resets live presentation and consumes synchronized user once", func(t *testing.T) {
		cfg := testAppConfig()
		cfg.UI.ShowResponseTimer = true
		model := New(Deps{
			Config: cfg, Provider: fakeProvider{name: "fake"}, Store: &fakeConversationStore{}, Resources: fakeResources{},
		})
		model.screen = screenChat
		model.conversation = conversation.NewConversation("main-request", time.Unix(2, 0))
		conversation.AppendAssistantMessage(model.conversation, "kept history")
		conversation.AppendUserMessage(model.conversation, "synchronized user")
		seedT420RequestPresentation(&model)

		if cmd := model.beginRequest(func() {}, closedEvents(), "current-model", false); cmd == nil {
			t.Fatal("main beginRequest did not return a listener")
		}
		assertT420RequestPresentationReset(t, model, true)
		view := model.messages.View()
		for _, stale := range []string{"stale assistant buffer", "stale thinking buffer", "stale transient buffer", "stale transient tool"} {
			if strings.Contains(view, stale) {
				t.Fatalf("beginRequest retained %q in MessagesView: %q", stale, view)
			}
		}
		if strings.Count(view, "kept history") != 1 || strings.Count(view, "synchronized user") != 1 {
			t.Fatalf("beginRequest did not project synchronized conversation exactly once: %q", view)
		}

		updated, _ := model.Update(testEventMessage(t, &model, events.Event{
			Type: events.UserSubmitted, Text: appSafeText("synchronized user"),
		}, closedEvents()))
		model = updated.(Model)
		if got := strings.Count(model.messages.View(), "synchronized user"); got != 1 {
			t.Fatalf("UserSubmitted was rendered %d times: %q", got, model.messages.View())
		}
	})

	t.Run("new request preserves explicit clear until conversation reload", func(t *testing.T) {
		cfg := testAppConfig()
		model := New(Deps{
			Config: cfg, Provider: fakeProvider{name: "fake"}, Store: &fakeConversationStore{}, Resources: fakeResources{},
		})
		model.screen = screenChat
		model.conversation = conversation.NewConversation("cleared-request", time.Unix(2, 0))
		conversation.AppendUserMessage(model.conversation, "hidden prior history")
		model.messages.SetMessages(model.conversation.Messages)
		model.messages.Clear()
		model.messagesCleared = true
		conversation.AppendUserMessage(model.conversation, "visible current user")

		if cmd := model.beginRequest(func() {}, closedEvents(), "current-model", false); cmd == nil {
			t.Fatal("cleared beginRequest did not return a listener")
		}
		if model.messages.View() != "" || !model.messagesCleared || model.pendingUserProjection {
			t.Fatalf("beginRequest restored explicitly cleared history: view=%q cleared=%t pending=%t", model.messages.View(), model.messagesCleared, model.pendingUserProjection)
		}

		updated, _ := model.Update(testEventMessage(t, &model, events.Event{
			Type: events.UserSubmitted, Text: appSafeText("visible current user"),
		}, closedEvents()))
		model = updated.(Model)
		if view := model.messages.View(); strings.Contains(view, "hidden prior history") || strings.Count(view, "visible current user") != 1 {
			t.Fatalf("cleared request projected the wrong history: %q", view)
		}

		updated, _ = model.Update(testEventMessage(t, &model, events.Event{Type: events.MainTraceReset}, closedEvents()))
		model = updated.(Model)
		if view := model.messages.View(); strings.Contains(view, "hidden prior history") || strings.Count(view, "visible current user") != 1 {
			t.Fatalf("MainTraceReset restored explicitly cleared history: %q", view)
		}
	})

	t.Run("successful conversation switch resets the same live presentation", func(t *testing.T) {
		cfg := testAppConfig()
		cfg.UI.ShowResponseTimer = true
		store := &fakeConversationStore{}
		model := New(Deps{
			Config: cfg, Provider: fakeProvider{name: "fake"}, Store: store, Resources: fakeResources{},
		})
		store.calls = nil
		model.screen = screenChat
		model.conversation = conversation.NewConversation("old-session", time.Unix(3, 0))
		conversation.AppendUserMessage(model.conversation, "old session message")
		seedT420RequestPresentation(&model)

		model.startNewConversation()
		if !reflect.DeepEqual(store.calls, []string{"save", "create"}) || model.conversation == nil || model.conversation.ID != "new" || model.screen != screenChat {
			t.Fatalf("switch did not commit once: calls=%#v conversation=%#v screen=%q", store.calls, model.conversation, model.screen)
		}
		assertT420RequestPresentationReset(t, model, false)
		if strings.Contains(model.messages.View(), "old session message") || strings.Contains(model.messages.View(), "stale") {
			t.Fatalf("conversation switch retained old MessagesView state: %q", model.messages.View())
		}
	})

	t.Run("old envelope cannot refill a reset request", func(t *testing.T) {
		cfg := testAppConfig()
		cfg.UI.ShowResponseTimer = true
		model := New(Deps{
			Config: cfg, Provider: fakeProvider{name: "fake"}, Store: &fakeConversationStore{}, Resources: fakeResources{},
		})
		model.screen = screenChat
		model.conversation = conversation.NewConversation("generation-session", time.Unix(4, 0))
		if cmd := model.beginRequest(func() {}, closedEvents(), "first", false); cmd == nil {
			t.Fatal("first request did not return a listener")
		}
		oldEnvelope := eventEnvelope{Generation: model.request.Generation, ConversationID: model.request.ConversationID}
		if cmd := model.beginRequest(func() {}, closedEvents(), "second", false); cmd == nil {
			t.Fatal("second request did not return a listener")
		}
		if model.request.Generation == oldEnvelope.Generation {
			t.Fatalf("request generation was reused: %#v", oldEnvelope)
		}

		canary := appSafeText("stale generation canary")
		for _, event := range []events.Event{
			{Type: events.Done, Duration: 9 * time.Second},
			{Type: events.UsageUpdated, Usage: &events.UsageDisplay{InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: 3, CacheReadInputTokens: 4}},
			{Type: events.AgentProgressed, Progress: &events.AgentProgress{Iteration: 5, Max: 6, StopReason: "stale-stop", Message: canary}},
			{Type: events.Error, Err: appSafeError("stale_error", "stale generation canary")},
			{Type: events.ToolWaitingConfirmation, Confirmation: &events.ToolConfirmationRequest{ConfirmationID: "stale-confirmation", CallID: "stale-call", Prompt: canary}},
			{Type: events.TextDelta, Text: canary, Transient: true, IndependentID: "stale-transient"},
		} {
			updated, cmd := model.Update(eventMsg{event: event, events: closedEvents(), envelope: oldEnvelope})
			model = updated.(Model)
			if cmd != nil {
				t.Fatalf("stale %q event scheduled another stream read", event.Type)
			}
		}
		assertT420RequestPresentationReset(t, model, true)
		if model.status.RequestModel != "second" || strings.Contains(model.messages.View(), "stale generation canary") {
			t.Fatalf("old envelope refilled current state: status=%#v messages=%q", model.status, model.messages.View())
		}
	})
}

func TestStartModeFollowsResolvedConfig(t *testing.T) {
	for _, test := range []struct {
		name       string
		startMode  string
		wantScreen screen
		wantActive bool
		wantCalls  []string
	}{
		{name: "list", startMode: config.StartModeList, wantScreen: screenList, wantCalls: []string{"list"}},
		{name: "new", startMode: config.StartModeNew, wantScreen: screenChat, wantActive: true, wantCalls: []string{"list", "create"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testAppConfig()
			cfg.UI.StartMode = test.startMode
			store := &fakeConversationStore{}
			model := New(Deps{
				Config: cfg, Provider: fakeProvider{name: "fake"}, Store: store, Resources: fakeResources{},
			})
			if model.runtime.StartMode != test.startMode || model.screen != test.wantScreen ||
				(model.conversation != nil) != test.wantActive || !reflect.DeepEqual(store.calls, test.wantCalls) {
				t.Fatalf("resolved start mode %q produced runtime=%#v screen=%q conversation=%#v calls=%#v, want calls=%#v", test.startMode, model.runtime, model.screen, model.conversation, store.calls, test.wantCalls)
			}
		})
	}

	t.Run("App does not maintain a second default", func(t *testing.T) {
		runtime := runtimeStateFromResolvedUI(config.UIConfig{})
		if runtime.StartMode != "" || runtime.ShowResponseTimer {
			t.Fatalf("App reinterpreted unresolved UI config: %#v", runtime)
		}
	})
}

type errStatusResetTest struct{}

func (errStatusResetTest) Error() string { return "old request error" }

func seedT420RequestPresentation(model *Model) {
	model.input = tui.NewInput("prompt")
	model.input.Text.SetValue("stale input")
	model.messages = tui.NewMessagesView(true)
	if model.conversation != nil {
		model.messages.SetMessages(model.conversation.Messages)
	}
	model.messages.AppendAssistantDelta("stale assistant buffer")
	model.messages.AppendThinkingDelta("stale thinking buffer")
	model.messages.AppendTransientAssistantDelta("old-independent", "stale transient buffer")
	model.messages.UpsertTransientTool("old-independent", events.ToolDisplay{
		CallID: "stale-call", Name: "stale transient tool", Arguments: appSafeText("{}"), Status: events.ToolDisplayRunning,
	})
	model.status.Duration = 5 * time.Second
	model.status.InputTokens = 89
	model.status.OutputTokens = 144
	model.status.CacheCreationInputTokens = 34
	model.status.CacheReadInputTokens = 55
	model.status.StopReason = "completed"
	model.status.StopMessage = "stale stop message"
	model.status.Error = errStatusResetTest{}
	model.status.WaitingConfirmation = true
	model.status.AgentIteration = 7
	model.status.AgentMaxIterations = 8
	model.status.Notice = "stale notice"
	model.lastError = errStatusResetTest{}
	model.confirmation = &Event{Confirmation: &events.ToolConfirmationRequest{
		ConfirmationID: "stale-confirmation", CallID: "stale-call", Prompt: appSafeText("stale confirmation"),
	}}
	model.streaming = true
	model.request = &RequestSession{
		Cancel: func() {}, Generation: 99, ConversationID: "old-request", TransientIDs: []string{"old-independent"},
	}
}

func assertT420RequestPresentationReset(t *testing.T, model Model, streaming bool) {
	t.Helper()
	if model.status.Duration != 0 || model.status.InputTokens != 0 || model.status.OutputTokens != 0 ||
		model.status.CacheCreationInputTokens != 0 || model.status.CacheReadInputTokens != 0 ||
		model.status.StopReason != "" || model.status.StopMessage != "" || model.status.Error != nil ||
		model.status.WaitingConfirmation || model.status.AgentIteration != 0 || model.status.AgentMaxIterations != 0 ||
		model.status.Notice != "" || model.lastError != nil || model.confirmation != nil {
		t.Fatalf("request presentation was not reset: status=%#v lastError=%v confirmation=%#v", model.status, model.lastError, model.confirmation)
	}
	if model.streaming != streaming || model.status.Streaming != streaming {
		t.Fatalf("streaming state = model:%t status:%t, want %t", model.streaming, model.status.Streaming, streaming)
	}
	if model.request != nil && len(model.request.TransientIDs) != 0 {
		t.Fatalf("request retained transient IDs: %#v", model.request.TransientIDs)
	}
}

func TestRequestGenerationNeverReusedAcrossReset(t *testing.T) {
	runtime := RuntimeState{}
	conversation := ConversationState{ActiveID: "conversation-a"}
	request := RequestState{}
	want := []uint64{1, 2, 3, 4}
	got := make([]uint64, 0, len(want))

	runtime.resetRequest(&conversation, &request)
	got = append(got, request.Generation)
	runtime.resetRequest(&conversation, &request)
	got = append(got, request.Generation)
	activity := &recordingSkillActivity{}
	runtime.resetConversation(&conversation, &request, "conversation-b", activity)
	got = append(got, request.Generation)
	runtime.resetRequest(&conversation, &request)
	got = append(got, request.Generation)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generations across resets = %v, want %v", got, want)
	}
	if runtime.RequestSequence != want[len(want)-1] {
		t.Fatalf("runtime sequence = %d, want %d", runtime.RequestSequence, want[len(want)-1])
	}
	if activity.clearCalls != 1 {
		t.Fatalf("conversation reset Clear calls = %d, want 1", activity.clearCalls)
	}

	exhausted := RuntimeState{RequestSequence: math.MaxUint64}
	exhaustedConversation := ConversationState{
		ActiveID:        "conversation-live",
		Mode:            "plan",
		Skills:          []string{"keep"},
		SkillGeneration: math.MaxUint64,
	}
	exhaustedRequest := RequestState{Generation: math.MaxUint64, Duration: time.Second}
	exhaustedActivity := &recordingSkillActivity{}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("exhausted conversation reset did not fail closed")
			}
		}()
		exhausted.resetConversation(&exhaustedConversation, &exhaustedRequest, "conversation-never", exhaustedActivity)
	}()
	if exhausted.RequestSequence != math.MaxUint64 ||
		exhaustedConversation.ActiveID != "conversation-live" ||
		exhaustedConversation.Mode != "plan" ||
		!reflect.DeepEqual(exhaustedConversation.Skills, []string{"keep"}) ||
		exhaustedRequest.Generation != math.MaxUint64 || exhaustedRequest.Duration != time.Second ||
		exhaustedActivity.clearCalls != 0 {
		t.Fatalf("exhausted reset partially mutated state: runtime=%#v conversation=%#v request=%#v Clear=%d", exhausted, exhaustedConversation, exhaustedRequest, exhaustedActivity.clearCalls)
	}
}

func TestRequestResetPreservesSkillAndSessionResetClearsIt(t *testing.T) {
	runtime := RuntimeState{}
	conversation := ConversationState{ActiveID: "conversation-a"}
	request := RequestState{}

	runtime.resetRequest(&conversation, &request)
	firstGeneration := request.Generation
	skills := []string{"review"}
	if !conversation.applySkillSnapshot(firstGeneration, skills) {
		t.Fatal("current request Skill snapshot was rejected")
	}
	skills[0] = "caller-mutated"
	if !reflect.DeepEqual(conversation.Skills, []string{"review"}) {
		t.Fatalf("Skill snapshot was not copied: %v", conversation.Skills)
	}

	runtime.resetRequest(&conversation, &request)
	secondGeneration := request.Generation
	if !reflect.DeepEqual(conversation.Skills, []string{"review"}) {
		t.Fatalf("request reset cleared Skill Activity: %v", conversation.Skills)
	}
	if conversation.applySkillSnapshot(firstGeneration, []string{"stale"}) {
		t.Fatal("old-generation Skill snapshot was accepted after request reset")
	}
	if !reflect.DeepEqual(conversation.Skills, []string{"review"}) {
		t.Fatalf("old-generation Skill snapshot refilled state: %v", conversation.Skills)
	}
	if !conversation.applySkillSnapshot(secondGeneration, []string{"review", "test"}) {
		t.Fatal("current-generation Skill snapshot was rejected")
	}

	activity := &recordingSkillActivity{}
	runtime.resetConversation(&conversation, &request, "conversation-b", activity)
	if conversation.Skills != nil {
		t.Fatalf("conversation reset retained Skill Activity: %v", conversation.Skills)
	}
	if conversation.applySkillSnapshot(secondGeneration, []string{"late-session-snapshot"}) {
		t.Fatal("old-session Skill snapshot was accepted")
	}
	if conversation.Skills != nil {
		t.Fatalf("old-session Skill snapshot refilled cleared state: %v", conversation.Skills)
	}
	if conversation.applySkillSnapshot(0, []string{"zero-generation"}) {
		t.Fatal("zero-generation Skill snapshot was accepted")
	}
	fresh := []string{"fresh-session"}
	if !conversation.applySkillSnapshot(request.Generation, fresh) {
		t.Fatal("current new-session Skill snapshot was rejected")
	}
	fresh[0] = "caller-mutated"
	if !reflect.DeepEqual(conversation.Skills, []string{"fresh-session"}) {
		t.Fatalf("new-session Skill snapshot was not copied: %v", conversation.Skills)
	}
	if activity.clearCalls != 1 {
		t.Fatalf("conversation reset Clear calls = %d, want 1", activity.clearCalls)
	}
}

type recordingSkillActivity struct {
	clearCalls int
}

func (activity *recordingSkillActivity) Clear() {
	activity.clearCalls++
}

type stateField struct {
	Name string
	Type reflect.Type
}

func assertStateFields(t *testing.T, stateType reflect.Type, want []stateField) {
	t.Helper()
	if stateType.NumField() != len(want) {
		t.Fatalf("%s field count = %d, want %d", stateType.Name(), stateType.NumField(), len(want))
	}
	for index, expected := range want {
		field := stateType.Field(index)
		if field.Name != expected.Name || field.Type != expected.Type {
			t.Fatalf("%s field %d = %s %v, want %s %v", stateType.Name(), index, field.Name, field.Type, expected.Name, expected.Type)
		}
	}
}

func assertStateHasNoRawPayload(t *testing.T, valueType reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if valueType == nil || seen[valueType] {
		return
	}
	seen[valueType] = true

	safeErrorType := reflect.TypeOf((*diagnostics.SafeError)(nil))
	safeTextType := reflect.TypeOf(redact.SafeText{})
	if valueType == safeErrorType || valueType == safeTextType {
		return
	}
	if valueType == reflect.TypeOf(json.RawMessage{}) {
		t.Fatalf("state contains raw JSON payload type %v", valueType)
	}
	if valueType.Implements(reflect.TypeOf((*error)(nil)).Elem()) {
		t.Fatalf("state contains ordinary error type %v", valueType)
	}

	switch valueType.Kind() {
	case reflect.Interface:
		t.Fatalf("state contains unbounded interface type %v", valueType)
	case reflect.Pointer, reflect.Slice, reflect.Array:
		if valueType.Kind() == reflect.Slice && valueType.Elem().Kind() == reflect.Uint8 {
			t.Fatalf("state contains raw byte payload type %v", valueType)
		}
		assertStateHasNoRawPayload(t, valueType.Elem(), seen)
	case reflect.Struct:
		for index := 0; index < valueType.NumField(); index++ {
			field := valueType.Field(index)
			lowerName := strings.ToLower(field.Name)
			if strings.Contains(lowerName, "payload") || strings.HasPrefix(lowerName, "raw") {
				t.Fatalf("state field %s.%s can hold a raw payload", valueType.Name(), field.Name)
			}
			assertStateHasNoRawPayload(t, field.Type, seen)
		}
	}
}
