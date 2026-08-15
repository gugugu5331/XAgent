package app

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/artifact"
	"xagent/internal/command"
	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/redact"
	"xagent/internal/tool"
	"xagent/internal/tui"
)

func TestEscapeCancelsBeforeNavigating(t *testing.T) {
	conversationSentinel := &conversation.Conversation{ID: "active-conversation"}

	t.Run("streaming request must finish before navigation", func(t *testing.T) {
		cancelled := 0
		model := Model{
			screen:       screenChat,
			conversation: conversationSentinel,
			request:      &RequestSession{Cancel: func() { cancelled++ }},
			streaming:    true,
			input:        tui.NewInput(""),
			status:       tui.Status{Streaming: true},
		}

		updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
		model = updated.(Model)
		if cmd != nil || cancelled != 1 {
			t.Fatalf("first Esc = (cmd=%v cancelled=%d), want cancellation only", cmd, cancelled)
		}
		if model.screen != screenChat || model.conversation != conversationSentinel || model.request == nil || !model.streaming {
			t.Fatalf("first Esc navigated or cleared the active request: screen=%q conversation=%p request=%p streaming=%t", model.screen, model.conversation, model.request, model.streaming)
		}

		updated, cmd = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
		model = updated.(Model)
		if cmd != nil || cancelled != 1 || model.screen != screenChat || model.conversation != conversationSentinel {
			t.Fatalf("repeated Esc before cleanup navigated or canceled twice: cmd=%v cancelled=%d", cmd, cancelled)
		}

		updated, cmd = model.Update(testEventStreamClosedMessage(t, &model))
		model = updated.(Model)
		if cmd != nil || model.request != nil || model.streaming || model.status.Streaming {
			t.Fatalf("stream cleanup did not reach idle state: request=%p streaming=%t status=%#v", model.request, model.streaming, model.status)
		}

		updated, cmd = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
		model = updated.(Model)
		assertNavigationIntent(t, cmd, command.IntentShowSessions, "")
		if model.screen != screenChat || model.conversation != conversationSentinel {
			t.Fatalf("intent production committed navigation early: screen=%q conversation=%p", model.screen, model.conversation)
		}
	})

	t.Run("waiting confirmation cancels without answering or navigating", func(t *testing.T) {
		cancelled := 0
		resolver := &recordingConfirmationResolver{}
		confirmation := &Event{Confirmation: &events.ToolConfirmationRequest{ConfirmationID: "confirmation-1", CallID: "call"}}
		model := Model{
			screen:               screenChat,
			conversation:         conversationSentinel,
			request:              &RequestSession{Cancel: func() { cancelled++ }},
			streaming:            true,
			input:                tui.NewInput(""),
			confirmation:         confirmation,
			confirmationResolver: resolver,
			status:               tui.Status{Streaming: true, WaitingConfirmation: true},
		}

		updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
		model = updated.(Model)
		if cmd != nil || cancelled != 1 || model.confirmation != confirmation || !model.status.WaitingConfirmation {
			t.Fatalf("confirmation Esc did not preserve state until cleanup: cmd=%v cancelled=%d model=%#v", cmd, cancelled, model.status)
		}
		if len(resolver.decisions) != 0 {
			t.Fatalf("request cancellation forged permission decisions: %#v", resolver.decisions)
		}
		if model.screen != screenChat || model.conversation != conversationSentinel {
			t.Fatal("confirmation Esc navigated before request cleanup")
		}

		updated, _ = model.Update(testEventStreamClosedMessage(t, &model))
		model = updated.(Model)
		updated, cmd = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
		model = updated.(Model)
		assertNavigationIntent(t, cmd, command.IntentShowSessions, "")
	})

	t.Run("idle chat navigates on first Esc", func(t *testing.T) {
		model := Model{screen: screenChat, conversation: conversationSentinel, input: tui.NewInput("")}
		updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
		model = updated.(Model)
		assertNavigationIntent(t, cmd, command.IntentShowSessions, "")
		if model.screen != screenChat || model.conversation != conversationSentinel {
			t.Fatalf("idle Esc committed before the navigation transaction: screen=%q conversation=%p", model.screen, model.conversation)
		}
	})

	t.Run("slash command uses the same intent handoff", func(t *testing.T) {
		model := Model{screen: screenChat, conversation: conversationSentinel, input: tui.NewInput("")}
		model.input.SetValue("/sessions")
		updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
		model = updated.(Model)
		assertNavigationIntent(t, cmd, command.IntentShowSessions, "")
		if model.status.Error != nil || model.screen != screenChat || model.conversation != conversationSentinel {
			t.Fatalf("/sessions bypassed the intent boundary: error=%v screen=%q conversation=%p", model.status.Error, model.screen, model.conversation)
		}
	})
}

func assertNavigationIntent(t *testing.T, cmd tea.Cmd, want command.IntentKind, wantSessionID string) {
	t.Helper()
	if cmd == nil {
		t.Fatal("navigation intent command is nil")
	}
	message, ok := cmd().(navigationIntentMsg)
	if !ok || message.Intent != want || message.SessionID != wantSessionID {
		t.Fatalf("navigation intent = %#v, want intent=%q session=%q", message, want, wantSessionID)
	}
}

func TestNavigationDropsStaleGeneration(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	secretSessionID := "session-secret-navigation-8d12"
	redactor.RegisterSecret(secretSessionID)
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      8,
		MaxItemBytes:  512,
		MaxTotalBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	navigation := newNavigationState(sink, redactor)

	first, err := navigation.beginNavigation(command.IntentOpenConversation, secretSessionID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := navigation.beginNavigation(command.IntentShowSessions, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation == 0 || second.Generation != first.Generation+1 {
		t.Fatalf("navigation generations are not strictly increasing: first=%d second=%d", first.Generation, second.Generation)
	}
	if first.Kind != NavigationOpenConversation || first.SessionID != secretSessionID {
		t.Fatalf("first request lost its bound intent/session: %#v", first)
	}
	if second.Kind != NavigationShowSessions || second.SessionID != "" {
		t.Fatalf("second request has unexpected metadata: %#v", second)
	}

	type appSnapshot struct {
		screen       screen
		conversation ConversationState
		request      RequestState
	}
	state := appSnapshot{
		screen: screenChat,
		conversation: ConversationState{
			ActiveID:        "active-sentinel",
			Mode:            "plan",
			Skills:          []string{"review"},
			SkillGeneration: 91,
			Input:           redactor.Redact("input-sentinel"),
			Messages:        []redact.SafeText{redactor.Redact("message-sentinel")},
			Notice:          redactor.Redact("notice-sentinel"),
		},
		request: RequestState{
			Generation:   91,
			Duration:     3 * time.Second,
			Tokens:       Usage{InputTokens: 11, OutputTokens: 7},
			Cache:        CacheUsage{CacheCreationInputTokens: 5, CacheReadInputTokens: 3},
			StopReason:   "sentinel",
			TransientIDs: []string{"transient-sentinel"},
		},
	}
	want := state
	want.conversation.Skills = append([]string(nil), state.conversation.Skills...)
	want.conversation.Messages = append([]redact.SafeText(nil), state.conversation.Messages...)
	want.request.TransientIDs = append([]string(nil), state.request.TransientIDs...)

	committed := navigation.commitNavigation(first, func() {
		state.screen = screenList
		state.conversation = ConversationState{ActiveID: secretSessionID}
		state.request = RequestState{Generation: first.Generation}
	})
	if committed {
		t.Fatal("stale navigation result was committed")
	}
	if !reflect.DeepEqual(state, want) {
		t.Fatalf("stale navigation changed App state: got=%#v want=%#v", state, want)
	}

	items := sink.Snapshot().Items()
	if len(items) != 1 || items[0].Count != 1 {
		t.Fatalf("stale navigation diagnostics = %#v, want one bounded record", items)
	}
	diagnostic := items[0].Diagnostic
	if diagnostic.Code != staleNavigationDiagnosticCode || diagnostic.Source != staleNavigationDiagnosticSource || diagnostic.Hint != staleNavigationDiagnosticHint || diagnostic.Severity != diagnostics.SeverityWarning {
		t.Fatalf("unexpected stale navigation diagnostic: %#v", diagnostic)
	}
	retained := strings.Join([]string{diagnostic.Code, diagnostic.Source, diagnostic.Hint, diagnostic.Message.Text()}, " ")
	if strings.Contains(retained, secretSessionID) || diagnostic.Message.Text() != "" {
		t.Fatalf("stale diagnostic retained navigation content: %#v", diagnostic)
	}

	if !navigation.commitNavigation(second, func() { state.screen = screenList }) {
		t.Fatal("current navigation generation was rejected")
	}
	if state.screen != screenList {
		t.Fatal("current navigation commit did not publish its result")
	}

	t.Run("monotonic generation survives request and session resets", testNavigationGenerationSurvivesResets)
	t.Run("invalid metadata and exhaustion are atomic", testNavigationInvalidMetadataAndExhaustion)
	t.Run("complete identity and duplicate results are rejected", testNavigationRejectsMismatchedAndDuplicateResults)
	t.Run("stale diagnostic flood is bounded and content free", testNavigationBoundsStaleDiagnosticFlood)
	t.Run("concurrent allocation and commit remain serialized", testNavigationConcurrentAllocationAndCommit)
}

func TestNavigationRejectsInvalidMetadataAndExhaustionWithoutReplacement(t *testing.T) {
	testNavigationInvalidMetadataAndExhaustion(t)
}

func testNavigationInvalidMetadataAndExhaustion(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      8,
		MaxItemBytes:  512,
		MaxTotalBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	navigation := newNavigationState(sink, redactor)
	current, err := navigation.beginNavigation(command.IntentShowSessions, "")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		intent    command.IntentKind
		sessionID string
		want      error
	}{
		{intent: command.IntentCancel, want: errNavigationIntentInvalid},
		{intent: command.IntentQuit, want: errNavigationIntentInvalid},
		{intent: command.IntentOpenConversation, want: errNavigationSessionIDRequired},
		{intent: command.IntentNewConversation, sessionID: "unexpected", want: errNavigationSessionIDUnexpected},
		{intent: command.IntentShowSessions, sessionID: "unexpected", want: errNavigationSessionIDUnexpected},
	}
	for _, test := range tests {
		if _, got := navigation.beginNavigation(test.intent, test.sessionID); !errors.Is(got, test.want) {
			t.Fatalf("beginNavigation(%q, %q) error = %v, want %v", test.intent, test.sessionID, got, test.want)
		}
		if navigation.current != current || navigation.sequence != current.Generation {
			t.Fatalf("invalid navigation partially replaced current request: %#v", navigation.current)
		}
	}

	navigation.sequence = math.MaxUint64
	if _, got := navigation.beginNavigation(command.IntentNewConversation, ""); !errors.Is(got, errNavigationGenerationExhausted) {
		t.Fatalf("exhausted navigation error = %v, want %v", got, errNavigationGenerationExhausted)
	}
	if navigation.current != current || navigation.sequence != math.MaxUint64 {
		t.Fatalf("generation exhaustion partially replaced current request: %#v", navigation.current)
	}
	if _, got := navigation.beginNavigation(command.IntentCancel, ""); !errors.Is(got, errNavigationIntentInvalid) {
		t.Fatalf("invalid intent at exhaustion error = %v, want %v", got, errNavigationIntentInvalid)
	}
	published := 0
	if !navigation.commitNavigation(current, func() { published++ }) || published != 1 {
		t.Fatal("generation exhaustion poisoned the last valid current request")
	}
	if navigation.current != (NavigationRequest{}) || navigation.sequence != math.MaxUint64 {
		t.Fatalf("settling the final current request changed the sequence: current=%#v sequence=%d", navigation.current, navigation.sequence)
	}
}

func testNavigationGenerationSurvivesResets(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	navigation := newNavigationState(newNavigationTestSink(t, redactor), redactor)
	runtime := RuntimeState{}
	conversationState := ConversationState{ActiveID: "first"}
	requestState := runtime.newRequestState()

	first, err := navigation.beginNavigation(command.IntentNewConversation, "")
	if err != nil {
		t.Fatal(err)
	}
	runtime.resetRequest(&conversationState, &requestState)
	second, err := navigation.beginNavigation(command.IntentShowSessions, "")
	if err != nil {
		t.Fatal(err)
	}
	activity := &navigationTestActivity{}
	runtime.resetConversation(&conversationState, &requestState, "second", activity)
	third, err := navigation.beginNavigation(command.IntentOpenConversation, "opaque-session")
	if err != nil {
		t.Fatal(err)
	}

	requests := []NavigationRequest{first, second, third}
	wantKinds := []NavigationKind{NavigationNewConversation, NavigationShowSessions, NavigationOpenConversation}
	wantSessions := []string{"", "", "opaque-session"}
	for index, got := range requests {
		wantGeneration := uint64(index + 1)
		if got.Generation != wantGeneration || got.Kind != wantKinds[index] || got.SessionID != wantSessions[index] {
			t.Fatalf("navigation request %d = %#v, want generation=%d kind=%q session=%q", index, got, wantGeneration, wantKinds[index], wantSessions[index])
		}
	}
	if runtime.RequestSequence != 3 || requestState.Generation != 3 || activity.clears != 1 {
		t.Fatalf("request/session resets did not execute independently: runtime=%#v request=%#v clears=%d", runtime, requestState, activity.clears)
	}
}

func testNavigationRejectsMismatchedAndDuplicateResults(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	secretSessionID := "session-secret-navigation-mismatch-61af"
	redactor.RegisterSecret(secretSessionID)
	sink := newNavigationTestSink(t, redactor)
	navigation := newNavigationState(sink, redactor)
	stale, err := navigation.beginNavigation(command.IntentOpenConversation, secretSessionID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := navigation.beginNavigation(command.IntentShowSessions, "")
	if err != nil {
		t.Fatal(err)
	}

	state := newNavigationAppSnapshot(redactor)
	want := cloneNavigationAppSnapshot(state)
	var callbackCalls atomic.Int64
	publish := func() {
		callbackCalls.Add(1)
		state.runtime = RuntimeState{RequestSequence: 999}
		state.screen = screenList
		state.conversation = ConversationState{ActiveID: secretSessionID}
		state.request = RequestState{Generation: current.Generation}
	}
	rejected := []NavigationRequest{
		{},
		{Generation: current.Generation + 1, Kind: current.Kind},
		{Generation: current.Generation, Kind: NavigationNewConversation},
		{Generation: current.Generation, Kind: current.Kind, SessionID: secretSessionID},
		stale,
	}
	for _, request := range rejected {
		if navigation.commitNavigation(request, publish) {
			t.Fatalf("rejected request was committed: %#v", request)
		}
	}
	if callbackCalls.Load() != 0 || !reflect.DeepEqual(state, want) {
		t.Fatalf("rejected result partially published App state: calls=%d got=%#v want=%#v", callbackCalls.Load(), state, want)
	}

	if !navigation.commitNavigation(current, publish) || callbackCalls.Load() != 1 {
		t.Fatal("current navigation request did not publish exactly once")
	}
	published := cloneNavigationAppSnapshot(state)
	if navigation.commitNavigation(current, publish) {
		t.Fatal("duplicate navigation result was committed")
	}
	if callbackCalls.Load() != 1 || !reflect.DeepEqual(state, published) {
		t.Fatal("duplicate navigation result changed App state")
	}
	next, err := navigation.beginNavigation(command.IntentNewConversation, "")
	if err != nil {
		t.Fatal(err)
	}
	if next.Generation != current.Generation+1 {
		t.Fatalf("generation was reused after successful commit: got=%d want=%d", next.Generation, current.Generation+1)
	}

	items := sink.Snapshot().Items()
	if len(items) != 1 || items[0].Count != uint64(len(rejected)+1) {
		t.Fatalf("rejected navigation diagnostics = %#v", items)
	}
	assertSafeStaleNavigationDiagnostic(t, items[0].Diagnostic, secretSessionID)
}

func testNavigationBoundsStaleDiagnosticFlood(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	secretSessionID := "navigation-diagnostic-canary-9b72"
	redactor.RegisterSecret(secretSessionID)
	sink := newLimitedNavigationTestSink(t, redactor, 1, 512)
	navigation := newNavigationState(sink, redactor)
	current, err := navigation.beginNavigation(command.IntentOpenConversation, secretSessionID)
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 1000
	for index := uint64(0); index < attempts; index++ {
		request := NavigationRequest{
			Generation: current.Generation + index + 1,
			Kind:       NavigationOpenConversation,
			SessionID:  secretSessionID,
		}
		if navigation.commitNavigation(request, func() { t.Fatal("stale flood callback ran") }) {
			t.Fatal("stale flood result was committed")
		}
	}

	snapshot := sink.Snapshot()
	items := snapshot.Items()
	if len(items) != 1 || items[0].Count != attempts || snapshot.Dropped() != 0 {
		t.Fatalf("bounded stale diagnostics = %#v dropped=%d, want one aggregated item", items, snapshot.Dropped())
	}
	if snapshot.Bytes() <= 0 || snapshot.Bytes() > 512 {
		t.Fatalf("stale diagnostic bytes = %d, want 1..512", snapshot.Bytes())
	}
	assertSafeStaleNavigationDiagnostic(t, items[0].Diagnostic, secretSessionID)

	items[0].Count = 1
	items[0].Diagnostic.Code = "mutated"
	immutable := sink.Snapshot().Items()
	if len(immutable) != 1 || immutable[0].Count != attempts || immutable[0].Diagnostic.Code != staleNavigationDiagnosticCode {
		t.Fatal("diagnostic snapshot exposed mutable sink storage")
	}
}

func testNavigationConcurrentAllocationAndCommit(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	sink := newLimitedNavigationTestSink(t, redactor, 1, 512)
	navigation := newNavigationState(sink, redactor)
	const workers = 64
	startAllocation := make(chan struct{})
	requests := make(chan NavigationRequest, workers)
	errorsSeen := make(chan error, workers)
	var allocationWait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		allocationWait.Add(1)
		go func() {
			defer allocationWait.Done()
			<-startAllocation
			request, err := navigation.beginNavigation(command.IntentNewConversation, "")
			if err != nil {
				errorsSeen <- err
				return
			}
			requests <- request
		}()
	}
	close(startAllocation)
	allocationWait.Wait()
	close(requests)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent allocation failed: %v", err)
	}

	allocated := make([]NavigationRequest, 0, workers)
	for request := range requests {
		allocated = append(allocated, request)
	}
	if len(allocated) != workers {
		t.Fatalf("allocated request count = %d, want %d", len(allocated), workers)
	}
	sort.Slice(allocated, func(left, right int) bool {
		return allocated[left].Generation < allocated[right].Generation
	})
	for index, request := range allocated {
		wantGeneration := uint64(index + 1)
		if request.Generation != wantGeneration {
			t.Fatalf("allocated generation %d = %d, want %d", index, request.Generation, wantGeneration)
		}
	}

	current := allocated[len(allocated)-1]
	commitRequests := append([]NavigationRequest(nil), allocated...)
	commitRequests = append(commitRequests,
		current,
		NavigationRequest{},
		NavigationRequest{Generation: current.Generation + 1, Kind: current.Kind},
		NavigationRequest{Generation: current.Generation, Kind: NavigationShowSessions},
	)
	startCommit := make(chan struct{})
	var commitWait sync.WaitGroup
	var successfulCommits atomic.Int64
	var publications atomic.Int64
	for _, request := range commitRequests {
		request := request
		commitWait.Add(1)
		go func() {
			defer commitWait.Done()
			<-startCommit
			if navigation.commitNavigation(request, func() { publications.Add(1) }) {
				successfulCommits.Add(1)
			}
		}()
	}
	close(startCommit)
	commitWait.Wait()

	if successfulCommits.Load() != 1 || publications.Load() != 1 {
		t.Fatalf("concurrent commits: successes=%d publications=%d, want 1/1", successfulCommits.Load(), publications.Load())
	}
	if navigation.current != (NavigationRequest{}) || navigation.sequence != workers {
		t.Fatalf("concurrent final navigation state = current=%#v sequence=%d", navigation.current, navigation.sequence)
	}
	items := sink.Snapshot().Items()
	if len(items) != 1 || items[0].Count != uint64(len(commitRequests)-1) {
		t.Fatalf("concurrent rejected diagnostics = %#v", items)
	}
}

type navigationTestActivity struct {
	clears int
}

func (activity *navigationTestActivity) Clear() {
	activity.clears++
}

type navigationAppSnapshot struct {
	runtime      RuntimeState
	screen       screen
	conversation ConversationState
	request      RequestState
}

func newNavigationAppSnapshot(redactor *redact.RuntimeRedactor) navigationAppSnapshot {
	return navigationAppSnapshot{
		runtime: RuntimeState{RequestSequence: 91},
		screen:  screenChat,
		conversation: ConversationState{
			ActiveID:        "active-sentinel",
			Mode:            "plan",
			Skills:          []string{"review"},
			SkillGeneration: 91,
			Input:           redactor.Redact("input-sentinel"),
			Messages:        []redact.SafeText{redactor.Redact("message-sentinel")},
			Notice:          redactor.Redact("notice-sentinel"),
		},
		request: RequestState{
			Generation: 91,
			Duration:   3 * time.Second,
			Tokens:     Usage{InputTokens: 11, OutputTokens: 7},
			Cache:      CacheUsage{CacheCreationInputTokens: 5, CacheReadInputTokens: 3},
			StopReason: "sentinel",
			LastError: &diagnostics.SafeError{
				Code:        "sentinel",
				Source:      "test",
				Message:     redactor.Redact("safe-error-sentinel"),
				Recoverable: true,
			},
			Confirmation: &ConfirmationState{
				CallID:         "call-sentinel",
				Name:           "tool-sentinel",
				Prompt:         redactor.Redact("prompt-sentinel"),
				Risk:           "high",
				PermissionMode: "strict",
				ScopePreview:   redactor.Redact("scope-sentinel"),
				Warning:        redactor.Redact("warning-sentinel"),
				RevokeHint:     redactor.Redact("revoke-sentinel"),
				AllowPermanent: true,
			},
			TransientIDs: []string{"transient-sentinel"},
		},
	}
}

func cloneNavigationAppSnapshot(source navigationAppSnapshot) navigationAppSnapshot {
	clone := source
	clone.conversation.Skills = append([]string(nil), source.conversation.Skills...)
	clone.conversation.Messages = append([]redact.SafeText(nil), source.conversation.Messages...)
	clone.request.TransientIDs = append([]string(nil), source.request.TransientIDs...)
	if source.request.LastError != nil {
		lastError := *source.request.LastError
		clone.request.LastError = &lastError
	}
	if source.request.Confirmation != nil {
		confirmation := *source.request.Confirmation
		clone.request.Confirmation = &confirmation
	}
	return clone
}

func newLimitedNavigationTestSink(t *testing.T, redactor *redact.RuntimeRedactor, maxItems, maxTotalBytes int64) *diagnostics.Sink {
	t.Helper()
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      maxItems,
		MaxItemBytes:  512,
		MaxTotalBytes: maxTotalBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sink
}

func assertSafeStaleNavigationDiagnostic(t *testing.T, diagnostic diagnostics.SafeDiagnostic, forbidden string) {
	t.Helper()
	if diagnostic.Code != staleNavigationDiagnosticCode || diagnostic.Source != staleNavigationDiagnosticSource || diagnostic.Hint != staleNavigationDiagnosticHint || diagnostic.Severity != diagnostics.SeverityWarning {
		t.Fatalf("unexpected stale navigation diagnostic: %#v", diagnostic)
	}
	retained := strings.Join([]string{diagnostic.Code, diagnostic.Source, diagnostic.Hint, diagnostic.Message.Text()}, " ")
	if strings.Contains(retained, forbidden) || diagnostic.Message.Text() != "" {
		t.Fatalf("stale diagnostic retained navigation content: %#v", diagnostic)
	}
}

func TestNavigationSaveFailurePreservesActiveConversation(t *testing.T) {
	for _, test := range []struct {
		name          string
		waitErr       error
		saveErr       error
		wantCode      string
		wantTrace     []string
		wantSaveCalls int
	}{
		{
			name:      "WaitIdle failure stops before Save",
			waitErr:   errors.New("wait failed with navigation-secret-43bc"),
			wantCode:  waitNavigationErrorCode,
			wantTrace: []string{"wait"},
		},
		{
			name:          "Save failure preserves every visible state boundary",
			saveErr:       errors.New("save failed with navigation-secret-43bc"),
			wantCode:      saveNavigationErrorCode,
			wantTrace:     []string{"wait", "save"},
			wantSaveCalls: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			redactor := redact.NewRuntimeRedactor()
			const secret = "navigation-secret-43bc"
			redactor.RegisterSecret(secret)
			sink := newNavigationTestSink(t, redactor)
			navigation := newNavigationState(sink, redactor)
			request, err := navigation.beginNavigation(command.IntentShowSessions, "")
			if err != nil {
				t.Fatal(err)
			}

			type visibleState struct {
				screen       screen
				conversation ConversationState
				messages     []redact.SafeText
				hookEvents   []string
			}
			state := visibleState{
				screen: screenChat,
				conversation: ConversationState{
					ActiveID: "active-conversation",
					Mode:     "plan",
					Skills:   []string{"review"},
					Messages: []redact.SafeText{redactor.Redact("conversation-message")},
				},
				messages:   []redact.SafeText{redactor.Redact("visible-message")},
				hookEvents: []string{"session_start:active-conversation"},
			}
			want := state
			want.conversation.Skills = append([]string(nil), state.conversation.Skills...)
			want.conversation.Messages = append([]redact.SafeText(nil), state.conversation.Messages...)
			want.messages = append([]redact.SafeText(nil), state.messages...)
			want.hookEvents = append([]string(nil), state.hookEvents...)
			active := &conversation.Conversation{ID: "active-conversation"}
			trace := []string{}
			waiter := &navigationWaiterFake{trace: &trace, err: test.waitErr}
			saver := &navigationSaverFake{trace: &trace, err: test.saveErr}

			prepared, safeErr := navigation.prepareNavigation(context.Background(), request, waiter, saver, active)
			if prepared.ready || safeErr == nil || safeErr.Code != test.wantCode || !safeErr.Recoverable {
				t.Fatalf("failed preparation = (%#v, %#v), want recoverable %q", prepared, safeErr, test.wantCode)
			}
			if safeErr.Message.Text() == "" || strings.Contains(safeErr.Message.Text(), secret) {
				t.Fatalf("unsafe navigation error: %#v", safeErr)
			}
			if !reflect.DeepEqual(trace, test.wantTrace) || saver.calls != test.wantSaveCalls {
				t.Fatalf("navigation order = %#v save_calls=%d, want %#v/%d", trace, saver.calls, test.wantTrace, test.wantSaveCalls)
			}
			if saver.calls > 0 && saver.active != active {
				t.Fatalf("Save received wrong active Conversation: %p want %p", saver.active, active)
			}
			if !reflect.DeepEqual(state, want) {
				t.Fatalf("failed navigation changed visible state: got=%#v want=%#v", state, want)
			}
			items := sink.Snapshot().Items()
			if len(items) != 1 || items[0].Diagnostic.Code != test.wantCode || strings.Contains(items[0].Diagnostic.Message.Text(), secret) {
				t.Fatalf("failure diagnostic is missing or unsafe: %#v", items)
			}
		})
	}

	t.Run("superseded WaitIdle result cannot Save or display its error", func(t *testing.T) {
		redactor := redact.NewRuntimeRedactor()
		sink := newNavigationTestSink(t, redactor)
		navigation := newNavigationState(sink, redactor)
		stale, err := navigation.beginNavigation(command.IntentShowSessions, "")
		if err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		trace := []string{}
		waiter := &navigationWaiterFake{trace: &trace, err: errors.New("obsolete wait error"), entered: entered, release: release}
		saver := &navigationSaverFake{trace: &trace}
		type outcome struct {
			prepared navigationPreparation
			err      *diagnostics.SafeError
		}
		finished := make(chan outcome, 1)
		go func() {
			prepared, safeErr := navigation.prepareNavigation(context.Background(), stale, waiter, saver, &conversation.Conversation{ID: "active"})
			finished <- outcome{prepared: prepared, err: safeErr}
		}()
		<-entered
		current, err := navigation.beginNavigation(command.IntentNewConversation, "")
		if err != nil {
			t.Fatal(err)
		}
		close(release)
		result := <-finished
		if result.prepared.ready || result.err != nil || saver.calls != 0 {
			t.Fatalf("superseded preparation leaked result: prepared=%#v error=%#v save_calls=%d", result.prepared, result.err, saver.calls)
		}
		if !navigation.isCurrentNavigation(current) {
			t.Fatal("superseded preparation disturbed the current navigation")
		}
		items := sink.Snapshot().Items()
		if len(items) != 1 || items[0].Diagnostic.Code != staleNavigationDiagnosticCode {
			t.Fatalf("superseded preparation diagnostic = %#v", items)
		}
	})
}

func TestNavigationSkipsSaveWithoutActiveConversation(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	sink := newNavigationTestSink(t, redactor)
	navigation := newNavigationState(sink, redactor)
	request, err := navigation.beginNavigation(command.IntentShowSessions, "")
	if err != nil {
		t.Fatal(err)
	}
	trace := []string{}
	waiter := &navigationWaiterFake{trace: &trace}
	saver := &navigationSaverFake{trace: &trace}

	prepared, safeErr := navigation.prepareNavigation(context.Background(), request, waiter, saver, nil)
	if safeErr != nil || !prepared.ready || prepared.saved || prepared.request != request {
		t.Fatalf("cold-start preparation = (%#v, %#v), want ready without Save", prepared, safeErr)
	}
	if !reflect.DeepEqual(trace, []string{"wait"}) || saver.calls != 0 || saver.active != nil {
		t.Fatalf("cold-start navigation attempted Save: trace=%#v calls=%d active=%p", trace, saver.calls, saver.active)
	}
	if len(sink.Snapshot().Items()) != 0 {
		t.Fatalf("successful cold-start preparation produced diagnostics: %#v", sink.Snapshot().Items())
	}

	t.Run("active success is WaitIdle then Save", func(t *testing.T) {
		second, beginErr := navigation.beginNavigation(command.IntentNewConversation, "")
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		activeTrace := []string{}
		active := &conversation.Conversation{ID: "active"}
		activeSaver := &navigationSaverFake{
			trace:  &activeTrace,
			result: conversation.SaveResult{Kind: conversation.SaveBatch},
		}
		result, prepareErr := navigation.prepareNavigation(context.Background(), second, &navigationWaiterFake{trace: &activeTrace}, activeSaver, active)
		if prepareErr != nil || !result.ready || !result.saved || result.request != second || result.saveResult.Kind != conversation.SaveBatch {
			t.Fatalf("active preparation = (%#v, %#v), want saved result", result, prepareErr)
		}
		if !reflect.DeepEqual(activeTrace, []string{"wait", "save"}) || activeSaver.active != active {
			t.Fatalf("active preparation order/argument = %#v/%p", activeTrace, activeSaver.active)
		}
	})
}

func TestNavigationBuildsCompleteCandidateWithoutEarlyCommit(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	const candidateSecret = "navigation-candidate-secret-7f31"
	redactor.RegisterSecret(candidateSecret)

	t.Run("List preserves the active state and owns every mutable result", func(t *testing.T) {
		sink := newNavigationTestSink(t, redactor)
		navigation := newNavigationState(sink, redactor)
		request, err := navigation.beginNavigation(command.IntentShowSessions, "")
		if err != nil {
			t.Fatal(err)
		}
		prepared := navigationPreparation{
			request:    request,
			ready:      true,
			saved:      true,
			saveResult: conversation.SaveResult{Kind: conversation.SaveBatch, Persisted: conversation.PersistedState{Revision: 9}},
		}
		current := navigationViewState{
			screen: screenChat,
			conversation: ConversationState{
				ActiveID:        "active",
				Mode:            "plan",
				Skills:          []string{"review"},
				SkillGeneration: 17,
				Input:           redactor.Redact("draft"),
				Messages:        []redact.SafeText{redactor.Redact("state-message")},
				Notice:          redactor.Redact("notice"),
			},
			messages: []redact.SafeText{redactor.Redact("visible-message")},
			active: &conversation.Conversation{
				ID:       "active",
				Messages: []conversation.Message{{Content: redactor.Redact("active-message"), Tool: &conversation.ToolState{Name: "active-tool"}}},
			},
		}
		wantCurrent := cloneNavigationViewStateForTest(current)
		hooks := []string{"session_start:active"}
		wantHooks := append([]string(nil), hooks...)
		listResult := conversation.ListResult{
			Entries: []conversation.ListEntry{{
				Summary:   conversation.ConversationSummary{ID: "listed", Title: redactor.Redact("Listed"), MessageCount: 3},
				Available: true,
				Recovery: conversation.RecoveryReport{Diagnostics: []diagnostics.Diagnostic{
					diagnostics.New("entry_partial", diagnostics.SeverityWarning, "safe").WithAttributes(map[string]string{"kind": "partial"}),
				}},
			}},
			Truncated:    true,
			ScannedFiles: 4,
			ScannedBytes: 512,
			Diagnostics: []diagnostics.Diagnostic{
				diagnostics.New("list_partial", diagnostics.SeverityWarning, "safe").WithAttributes(map[string]string{"scope": "partial"}),
			},
		}
		store := &navigationCandidateStoreFake{listResult: listResult}

		candidate, safeErr := navigation.buildNavigationCandidate(context.Background(), prepared, store, current)
		if safeErr != nil || candidate == nil {
			t.Fatalf("List candidate = (%#v, %#v), want complete candidate", candidate, safeErr)
		}
		got := candidate.snapshot()
		if store.listCalls != 1 || store.createCalls != 0 || store.loadCalls != 0 || !reflect.DeepEqual(store.trace, []string{"list"}) {
			t.Fatalf("List selected wrong Store operation: %#v calls=%d/%d/%d", store.trace, store.listCalls, store.createCalls, store.loadCalls)
		}
		if got.request != request || got.screen != screenList || !got.saved || got.saveResult.Persisted.Revision != 9 || got.created || got.changesActive {
			t.Fatalf("List candidate lost transaction metadata: %#v", got)
		}
		if !reflect.DeepEqual(got.conversation, current.conversation) || !reflect.DeepEqual(got.messages, current.messages) || !reflect.DeepEqual(got.listResult, listResult) {
			t.Fatalf("List candidate is incomplete: %#v", got)
		}
		if got.active == current.active || !reflect.DeepEqual(got.active, current.active) {
			t.Fatalf("List active Conversation is aliased or changed: got=%p current=%p", got.active, current.active)
		}
		if candidate.trackedActive != current.active {
			t.Fatal("List candidate lost the current Store-tracked Conversation pointer")
		}
		if !reflect.DeepEqual(current, wantCurrent) || !reflect.DeepEqual(hooks, wantHooks) {
			t.Fatalf("candidate construction committed App or Hook state early: current=%#v hooks=%#v", current, hooks)
		}

		current.conversation.Skills[0] = "mutated-source"
		current.messages[0] = redactor.Redact("mutated-source")
		current.active.Messages[0].Tool.Name = "mutated-source"
		listResult.Entries[0].Summary.ID = "mutated-source"
		listResult.Entries[0].Recovery.Diagnostics[0].Attributes["kind"] = "mutated-source"
		listResult.Diagnostics[0].Attributes["scope"] = "mutated-source"
		got.conversation.Skills[0] = "mutated-snapshot"
		got.messages[0] = redactor.Redact("mutated-snapshot")
		got.active.Messages[0].Tool.Name = "mutated-snapshot"
		got.listResult.Entries[0].Recovery.Diagnostics[0].Attributes["kind"] = "mutated-snapshot"
		got.listResult.Diagnostics[0].Attributes["scope"] = "mutated-snapshot"
		again := candidate.snapshot()
		if again.conversation.Skills[0] != "review" || again.messages[0].Text() != "visible-message" ||
			again.active.Messages[0].Tool.Name != "active-tool" || again.listResult.Entries[0].Summary.ID != "listed" ||
			again.listResult.Entries[0].Recovery.Diagnostics[0].Attributes["kind"] != "partial" ||
			again.listResult.Diagnostics[0].Attributes["scope"] != "partial" {
			t.Fatalf("CommitCandidate retained a mutable alias: %#v", again)
		}
	})

	t.Run("Create produces a complete clean chat candidate", func(t *testing.T) {
		sink := newNavigationTestSink(t, redactor)
		navigation := newNavigationState(sink, redactor)
		request, err := navigation.beginNavigation(command.IntentNewConversation, "")
		if err != nil {
			t.Fatal(err)
		}
		created := &conversation.Conversation{
			ID:       "created",
			Title:    redactor.Redact("New"),
			Messages: []conversation.Message{{Role: conversation.RoleAssistant, Content: redactor.Redact("welcome")}},
		}
		store := &navigationCandidateStoreFake{createResult: created}
		current := navigationViewState{screen: screenChat, conversation: ConversationState{ActiveID: "preserved", Skills: []string{"review"}}, messages: []redact.SafeText{redactor.Redact("visible")}, active: &conversation.Conversation{ID: "preserved"}}
		wantCurrent := cloneNavigationViewStateForTest(current)
		hooks := []string{"session_start:preserved"}
		wantHooks := append([]string(nil), hooks...)
		candidate, safeErr := navigation.buildNavigationCandidate(context.Background(), navigationPreparation{request: request, ready: true}, store, current)
		if safeErr != nil || candidate == nil {
			t.Fatalf("Create candidate = (%#v, %#v)", candidate, safeErr)
		}
		got := candidate.snapshot()
		if !reflect.DeepEqual(store.trace, []string{"create"}) || got.request != request || got.screen != screenChat || !got.created || !got.changesActive || got.saved ||
			got.active == created || got.active.ID != "created" || got.conversation.ActiveID != "created" ||
			len(got.conversation.Messages) != 1 || got.conversation.Messages[0].Text() != "welcome" ||
			len(got.messages) != 1 || got.messages[0].Text() != "welcome" || got.recovery.Status != conversation.RecoveryClean {
			t.Fatalf("incomplete Create candidate: trace=%#v candidate=%#v", store.trace, got)
		}
		if !reflect.DeepEqual(current, wantCurrent) || !reflect.DeepEqual(hooks, wantHooks) {
			t.Fatal("Create candidate modified App or Hook state before commit")
		}
		created.Messages[0].Content = redactor.Redact("mutated")
		if candidate.snapshot().active.Messages[0].Content.Text() != "welcome" {
			t.Fatal("Create candidate aliases Store Conversation")
		}
		if candidate.trackedActive != created || created.Messages[0].Content.Text() != "mutated" {
			t.Fatal("Create candidate lost Store pointer identity or modified it before commit")
		}
	})

	t.Run("Load binds persisted recovery and requested session", func(t *testing.T) {
		sink := newNavigationTestSink(t, redactor)
		navigation := newNavigationState(sink, redactor)
		request, err := navigation.beginNavigation(command.IntentOpenConversation, "loaded")
		if err != nil {
			t.Fatal(err)
		}
		compressionAt := time.Unix(1710000000, 0).UTC()
		wantCompressionAt := compressionAt
		loaded := &conversation.Conversation{
			ID: "loaded",
			Messages: []conversation.Message{{
				Role:    conversation.RoleUser,
				Content: redactor.Redact("history"),
				Tool: &conversation.ToolState{
					Name:     "nested-tool",
					Artifact: &artifact.Ref{ID: "artifact-opaque", Bytes: 7, Available: true, Complete: true},
					Error:    &tool.SafeError{Code: "nested_error", Message: redactor.Redact("safe nested error"), Recoverable: true},
				},
			}},
			Context: &conversation.ContextMetadata{
				Summary:           redactor.Redact("summary"),
				LastCompressionAt: &compressionAt,
			},
		}
		loadResult := conversation.LoadResult{
			Conversation: loaded,
			Available:    true,
			Persisted:    conversation.PersistedState{Revision: 23, MessageCount: 1},
			Recovery: conversation.RecoveryReport{
				Status:            conversation.RecoveryPartial,
				LastValidRevision: 23,
				SkippedRecords:    1,
				Diagnostics: []diagnostics.Diagnostic{
					diagnostics.New("recovered", diagnostics.SeverityWarning, "safe").WithAttributes(map[string]string{"revision": "23"}),
				},
			},
		}
		store := &navigationCandidateStoreFake{loadResult: loadResult}
		current := navigationViewState{screen: screenChat, conversation: ConversationState{ActiveID: "preserved", Skills: []string{"review"}}, messages: []redact.SafeText{redactor.Redact("visible")}, active: &conversation.Conversation{ID: "preserved"}}
		wantCurrent := cloneNavigationViewStateForTest(current)
		hooks := []string{"session_start:preserved"}
		wantHooks := append([]string(nil), hooks...)
		candidate, safeErr := navigation.buildNavigationCandidate(context.Background(), navigationPreparation{request: request, ready: true, saved: true}, store, current)
		if safeErr != nil || candidate == nil {
			t.Fatalf("Load candidate = (%#v, %#v)", candidate, safeErr)
		}
		got := candidate.snapshot()
		if !reflect.DeepEqual(store.trace, []string{"load:loaded"}) || got.request != request || got.screen != screenChat || got.created || !got.changesActive || !got.saved ||
			got.active == loaded || got.conversation.ActiveID != "loaded" || len(got.messages) != 1 || got.messages[0].Text() != "history" ||
			got.persisted.Revision != 23 || !reflect.DeepEqual(got.recovery, loadResult.Recovery) {
			t.Fatalf("incomplete Load candidate: trace=%#v candidate=%#v", store.trace, got)
		}
		if !reflect.DeepEqual(current, wantCurrent) || !reflect.DeepEqual(hooks, wantHooks) {
			t.Fatal("Load candidate modified App or Hook state before commit")
		}
		loaded.Messages[0].Content = redactor.Redact("mutated")
		loaded.Messages[0].Tool.Artifact.ID = "mutated"
		loaded.Messages[0].Tool.Error.Code = "mutated"
		*loaded.Context.LastCompressionAt = loaded.Context.LastCompressionAt.Add(time.Hour)
		loadResult.Recovery.Diagnostics[0].Attributes["revision"] = "mutated"
		frozen := candidate.snapshot()
		if candidate.trackedActive != loaded || loaded.Messages[0].Content.Text() != "mutated" {
			t.Fatal("Load candidate lost Store pointer identity or modified it before commit")
		}
		if frozen.active.Messages[0].Content.Text() != "history" || frozen.active.Messages[0].Tool.Artifact.ID != "artifact-opaque" ||
			frozen.active.Messages[0].Tool.Error.Code != "nested_error" || !frozen.active.Context.LastCompressionAt.Equal(wantCompressionAt) ||
			frozen.recovery.Diagnostics[0].Attributes["revision"] != "23" {
			t.Fatalf("Load candidate retained a nested mutable alias: %#v", frozen)
		}
	})

	t.Run("Store and validation failures never produce a candidate", func(t *testing.T) {
		for _, test := range []struct {
			name          string
			intent        command.IntentKind
			session       string
			store         navigationCandidateStore
			preparedReady bool
			wantCode      string
		}{
			{name: "List error", intent: command.IntentShowSessions, store: &navigationCandidateStoreFake{listErr: errors.New("list " + candidateSecret)}, preparedReady: true, wantCode: listNavigationErrorCode},
			{name: "Create error", intent: command.IntentNewConversation, store: &navigationCandidateStoreFake{createErr: errors.New("create " + candidateSecret)}, preparedReady: true, wantCode: createNavigationErrorCode},
			{name: "Create nil", intent: command.IntentNewConversation, store: &navigationCandidateStoreFake{}, preparedReady: true, wantCode: createNavigationErrorCode},
			{name: "Create blank ID", intent: command.IntentNewConversation, store: &navigationCandidateStoreFake{createResult: &conversation.Conversation{ID: "   "}}, preparedReady: true, wantCode: createNavigationErrorCode},
			{name: "Load error", intent: command.IntentOpenConversation, session: "wanted", store: &navigationCandidateStoreFake{loadErr: errors.New("load " + candidateSecret)}, preparedReady: true, wantCode: loadNavigationErrorCode},
			{name: "Load unavailable", intent: command.IntentOpenConversation, session: "wanted", store: &navigationCandidateStoreFake{loadResult: conversation.LoadResult{Available: false}}, preparedReady: true, wantCode: loadNavigationErrorCode},
			{name: "Load nil", intent: command.IntentOpenConversation, session: "wanted", store: &navigationCandidateStoreFake{loadResult: conversation.LoadResult{Available: true}}, preparedReady: true, wantCode: loadNavigationErrorCode},
			{name: "Load mismatched ID", intent: command.IntentOpenConversation, session: "wanted", store: &navigationCandidateStoreFake{loadResult: conversation.LoadResult{Available: true, Conversation: &conversation.Conversation{ID: "other"}}}, preparedReady: true, wantCode: loadNavigationErrorCode},
			{name: "Store unavailable", intent: command.IntentShowSessions, store: nil, preparedReady: true, wantCode: candidateNavigationErrorCode},
			{name: "Preparation not ready", intent: command.IntentShowSessions, store: &navigationCandidateStoreFake{}, preparedReady: false, wantCode: candidateNavigationErrorCode},
		} {
			t.Run(test.name, func(t *testing.T) {
				sink := newNavigationTestSink(t, redactor)
				navigation := newNavigationState(sink, redactor)
				request, err := navigation.beginNavigation(test.intent, test.session)
				if err != nil {
					t.Fatal(err)
				}
				current := navigationViewState{
					screen:       screenChat,
					conversation: ConversationState{ActiveID: "preserved", Skills: []string{"review"}, Messages: []redact.SafeText{redactor.Redact("state")}},
					messages:     []redact.SafeText{redactor.Redact("visible")},
					active:       &conversation.Conversation{ID: "preserved", Messages: []conversation.Message{{Content: redactor.Redact("history")}}},
				}
				wantCurrent := cloneNavigationViewStateForTest(current)
				hooks := []string{"session_start:preserved"}
				wantHooks := append([]string(nil), hooks...)
				candidate, safeErr := navigation.buildNavigationCandidate(context.Background(), navigationPreparation{request: request, ready: test.preparedReady}, test.store, current)
				if candidate != nil || safeErr == nil || safeErr.Code != test.wantCode || !safeErr.Recoverable || strings.Contains(safeErr.Message.Text(), candidateSecret) {
					t.Fatalf("failure result = (%#v, %#v), want safe %q without candidate", candidate, safeErr, test.wantCode)
				}
				if !navigation.isCurrentNavigation(request) || !reflect.DeepEqual(current, wantCurrent) || !reflect.DeepEqual(hooks, wantHooks) {
					t.Fatalf("failure changed current navigation/App/Hook state: current=%#v hooks=%#v", current, hooks)
				}
				items := sink.Snapshot().Items()
				if len(items) != 1 || items[0].Diagnostic.Code != test.wantCode || strings.Contains(items[0].Diagnostic.Message.Text(), candidateSecret) {
					t.Fatalf("failure diagnostic is missing or unsafe: %#v", items)
				}
			})
		}
	})

	t.Run("superseded Store result cannot become a CommitCandidate", func(t *testing.T) {
		for _, test := range []struct {
			name      string
			intent    command.IntentKind
			sessionID string
			fail      bool
		}{
			{name: "List success", intent: command.IntentShowSessions},
			{name: "Create success", intent: command.IntentNewConversation},
			{name: "Load success", intent: command.IntentOpenConversation, sessionID: "loaded"},
			{name: "Load error", intent: command.IntentOpenConversation, sessionID: "loaded", fail: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				sink := newNavigationTestSink(t, redactor)
				navigation := newNavigationState(sink, redactor)
				stale, err := navigation.beginNavigation(test.intent, test.sessionID)
				if err != nil {
					t.Fatal(err)
				}
				entered := make(chan struct{})
				release := make(chan struct{})
				store := &navigationCandidateStoreFake{
					createResult: &conversation.Conversation{ID: "created"},
					listResult:   conversation.ListResult{Entries: []conversation.ListEntry{}},
					loadResult:   conversation.LoadResult{Available: true, Conversation: &conversation.Conversation{ID: "loaded"}},
				}
				switch test.intent {
				case command.IntentShowSessions:
					store.listEntered, store.listRelease = entered, release
				case command.IntentNewConversation:
					store.createEntered, store.createRelease = entered, release
				case command.IntentOpenConversation:
					store.loadEntered, store.loadRelease = entered, release
					if test.fail {
						store.loadErr = errors.New("obsolete " + candidateSecret)
					}
				}
				currentView := navigationViewState{
					screen:       screenChat,
					conversation: ConversationState{ActiveID: "preserved", Skills: []string{"review"}},
					messages:     []redact.SafeText{redactor.Redact("visible")},
					active:       &conversation.Conversation{ID: "preserved"},
				}
				wantView := cloneNavigationViewStateForTest(currentView)
				hooks := []string{"session_start:preserved"}
				wantHooks := append([]string(nil), hooks...)
				type outcome struct {
					candidate *CommitCandidate
					err       *diagnostics.SafeError
				}
				finished := make(chan outcome, 1)
				go func() {
					candidate, safeErr := navigation.buildNavigationCandidate(context.Background(), navigationPreparation{request: stale, ready: true}, store, currentView)
					finished <- outcome{candidate: candidate, err: safeErr}
				}()
				<-entered
				current, err := navigation.beginNavigation(command.IntentShowSessions, "")
				if err != nil {
					t.Fatal(err)
				}
				close(release)
				result := <-finished
				if result.candidate != nil || result.err != nil || !navigation.isCurrentNavigation(current) {
					t.Fatalf("stale Store result escaped: result=%#v current=%#v", result, current)
				}
				if !reflect.DeepEqual(currentView, wantView) || !reflect.DeepEqual(hooks, wantHooks) {
					t.Fatal("stale Store result changed App or Hook state")
				}
				items := sink.Snapshot().Items()
				if len(items) != 1 || items[0].Diagnostic.Code != staleNavigationDiagnosticCode || strings.Contains(items[0].Diagnostic.Message.Text(), candidateSecret) {
					t.Fatalf("stale candidate diagnostic = %#v", items)
				}
			})
		}
	})
}

func TestListFailureAndPartialResultsRemainVisible(t *testing.T) {
	const secret = "list-view-secret-4e91"
	redactor := redact.NewRuntimeRedactor()
	redactor.RegisterSecret(secret)

	t.Run("top-level List failure publishes SafeError and preserves trusted page", func(t *testing.T) {
		sink := newNavigationTestSink(t, redactor)
		navigation := newNavigationState(sink, redactor)
		request, err := navigation.beginNavigation(command.IntentShowSessions, "")
		if err != nil {
			t.Fatal(err)
		}
		current := navigationViewState{
			screen:       screenChat,
			conversation: ConversationState{ActiveID: "active", Mode: "plan", Messages: []redact.SafeText{redactor.Redact("state")}},
			messages:     []redact.SafeText{redactor.Redact("visible")},
			active:       &conversation.Conversation{ID: "active"},
		}
		wantCurrent := cloneNavigationViewStateForTest(current)
		candidate, safeErr := navigation.buildNavigationCandidate(
			context.Background(), navigationPreparation{request: request, ready: true},
			&navigationCandidateStoreFake{
				listResult: conversation.ListResult{Entries: []conversation.ListEntry{{Summary: conversation.ConversationSummary{ID: "untrusted", Title: redactor.Redact("Untrusted")}, Available: true}}},
				listErr:    errors.New("permission denied " + secret),
			}, current,
		)
		if candidate != nil || safeErr == nil || safeErr.Code != listNavigationErrorCode || safeErr.Source != staleNavigationDiagnosticSource ||
			!safeErr.Recoverable || safeErr.Message.Text() == "" || strings.Contains(safeErr.Message.Text(), secret) {
			t.Fatalf("List failure = (%#v, %#v), want fixed recoverable SafeError", candidate, safeErr)
		}
		if !reflect.DeepEqual(current, wantCurrent) {
			t.Fatalf("List failure changed candidate input state: got=%#v want=%#v", current, wantCurrent)
		}

		screenState := screenChat
		conversationState := cloneNavigationConversationState(current.conversation)
		messageView := cloneNavigationSafeTexts(current.messages)
		active := current.active
		trustedList := ConversationListState{Entries: []ConversationListEntryState{{ID: "trusted", Title: redactor.Redact("Trusted"), Available: true, Selectable: true}}}
		wantConversation := cloneNavigationConversationState(conversationState)
		wantMessages := cloneNavigationSafeTexts(messageView)
		wantList := cloneConversationListState(trustedList)
		requestState := RequestState{Generation: 9, StopReason: "preserved"}
		if !navigation.publishNavigationListFailure(request, safeErr, &requestState) {
			t.Fatal("current List SafeError was not published")
		}
		if requestState.LastError == nil || requestState.LastError.Code != listNavigationErrorCode || strings.Contains(requestState.LastError.Message.Text(), secret) ||
			screenState != screenChat || !reflect.DeepEqual(conversationState, wantConversation) || !reflect.DeepEqual(messageView, wantMessages) ||
			active != current.active || !reflect.DeepEqual(trustedList, wantList) || requestState.Generation != 9 || requestState.StopReason != "preserved" {
			t.Fatalf("List error was hidden or replaced trusted state: screen=%q conversation=%#v messages=%#v active=%p list=%#v request=%#v", screenState, conversationState, messageView, active, trustedList, requestState)
		}

		cfg := testAppConfig()
		cfg.UI.StartMode = config.StartModeNew
		legacyStore := &fakeConversationStore{
			listResult: conversation.ListResult{Entries: []conversation.ListEntry{{Summary: conversation.ConversationSummary{ID: "untrusted", Title: redactor.Redact("Untrusted")}, Available: true}}},
			listErr:    errors.New("legacy " + secret),
		}
		legacy := New(Deps{Config: cfg, Provider: fakeProvider{name: "fake"}, Store: legacyStore, Resources: fakeResources{}, RuntimeRedactor: redactor})
		legacySafe, ok := legacy.status.Error.(*diagnostics.SafeError)
		if !ok || legacySafe.Code != listNavigationErrorCode || legacy.screen != screenList || legacy.conversation != nil || strings.Contains(legacySafe.Message.Text(), secret) || len(legacy.list.Items()) != 1 {
			t.Fatalf("legacy List failure was not a visible SafeError: error=%#v screen=%q conversation=%#v", legacy.status.Error, legacy.screen, legacy.conversation)
		}
	})

	t.Run("partial and placeholder entries become safe selectable view data", func(t *testing.T) {
		partialTitle := redact.NewRuntimeRedactor().Redact("Partial " + secret)
		result := conversation.ListResult{
			Truncated:    true,
			ScannedFiles: 7,
			ScannedBytes: 4096,
			Entries: []conversation.ListEntry{
				{
					Summary:   conversation.ConversationSummary{ID: "partial", Title: partialTitle, UpdatedAt: time.Unix(20, 0), MessageCount: 3},
					Available: true,
					Recovery: conversation.RecoveryReport{
						Status: conversation.RecoveryPartial, LastValidRevision: 12, SkippedRecords: 2,
						Diagnostics: []diagnostics.Diagnostic{diagnostics.New("partial_record", diagnostics.SeverityWarning, "recovered "+secret)},
					},
				},
				{
					Summary:   conversation.ConversationSummary{ID: "placeholder", Title: redactor.Redact("Unavailable"), UpdatedAt: time.Unix(10, 0)},
					Available: false,
					Recovery: conversation.RecoveryReport{
						Status:      conversation.RecoveryPlaceholder,
						Diagnostics: []diagnostics.Diagnostic{diagnostics.New("placeholder_record", diagnostics.SeverityWarning, "unavailable "+secret)},
					},
				},
			},
			Diagnostics: []diagnostics.Diagnostic{diagnostics.New("scan_partial", diagnostics.SeverityWarning, "scan "+secret)},
		}
		sink := newNavigationTestSink(t, redactor)
		navigation := newNavigationState(sink, redactor)
		request, err := navigation.beginNavigation(command.IntentShowSessions, "")
		if err != nil {
			t.Fatal(err)
		}
		candidate, safeErr := navigation.buildNavigationCandidate(
			context.Background(), navigationPreparation{request: request, ready: true},
			&navigationCandidateStoreFake{listResult: result}, navigationViewState{},
		)
		if safeErr != nil || candidate == nil {
			t.Fatalf("partial List candidate = (%#v, %#v)", candidate, safeErr)
		}
		projected := candidate.snapshot().listState
		if !projected.Truncated || projected.ScannedFiles != 7 || projected.ScannedBytes != 4096 || len(projected.Entries) != 2 ||
			!projected.Entries[0].Available || !projected.Entries[0].Selectable || projected.Entries[0].RecoveryStatus != conversation.RecoveryPartial ||
			projected.Entries[1].Available || projected.Entries[1].Selectable || projected.Entries[1].RecoveryStatus != conversation.RecoveryPlaceholder ||
			!strings.Contains(projected.Notice.Text(), "部分结果") || !strings.Contains(projected.Entries[0].RecoveryNotice.Text(), "revision 12") {
			t.Fatalf("partial List projection lost trusted metadata: %#v", projected)
		}
		retained := projected.Notice.Text() + projected.Entries[0].Title.Text() + projected.Entries[0].RecoveryNotice.Text() + projected.Entries[1].RecoveryNotice.Text()
		if strings.Contains(retained, secret) {
			t.Fatalf("partial List projection leaked raw content: %q", retained)
		}

		screenState := screenChat
		conversationState := ConversationState{ActiveID: "active", Mode: "plan"}
		messageView := []redact.SafeText{redactor.Redact("visible")}
		active := &conversation.Conversation{ID: "active"}
		published := ConversationListState{Entries: []ConversationListEntryState{{ID: "old"}}}
		wantConversation := cloneNavigationConversationState(conversationState)
		wantMessages := cloneNavigationSafeTexts(messageView)
		if !navigation.commitNavigationCandidate(context.Background(), candidate, navigationCommitTarget{
			screen: &screenState, conversation: &conversationState, messages: &messageView, active: &active, list: &published,
		}, nil) {
			t.Fatal("partial List candidate was not atomically published")
		}
		if screenState != screenList || !reflect.DeepEqual(published, projected) || !reflect.DeepEqual(conversationState, wantConversation) ||
			!reflect.DeepEqual(messageView, wantMessages) || active.ID != "active" {
			t.Fatalf("partial List publication lost data or changed active session: screen=%q list=%#v conversation=%#v messages=%#v active=%#v", screenState, published, conversationState, messageView, active)
		}
		published.Entries[0].ID = "mutated"
		if candidate.snapshot().listState.Entries[0].ID != "partial" {
			t.Fatal("published List state aliases immutable CommitCandidate")
		}
	})
}

type navigationCandidateStoreFake struct {
	trace         []string
	createCalls   int
	listCalls     int
	loadCalls     int
	createResult  *conversation.Conversation
	createErr     error
	listResult    conversation.ListResult
	listErr       error
	loadResult    conversation.LoadResult
	loadErr       error
	createEntered chan struct{}
	createRelease <-chan struct{}
	listEntered   chan struct{}
	listRelease   <-chan struct{}
	loadEntered   chan struct{}
	loadRelease   <-chan struct{}
}

func cloneNavigationViewStateForTest(source navigationViewState) navigationViewState {
	clone := source
	clone.conversation = cloneNavigationConversationState(source.conversation)
	clone.messages = cloneNavigationSafeTexts(source.messages)
	clone.active = cloneNavigationConversation(source.active)
	return clone
}

func (fake *navigationCandidateStoreFake) Create(context.Context) (*conversation.Conversation, error) {
	fake.trace = append(fake.trace, "create")
	fake.createCalls++
	if fake.createEntered != nil {
		close(fake.createEntered)
	}
	if fake.createRelease != nil {
		<-fake.createRelease
	}
	return fake.createResult, fake.createErr
}

func (fake *navigationCandidateStoreFake) List(context.Context) (conversation.ListResult, error) {
	fake.trace = append(fake.trace, "list")
	fake.listCalls++
	if fake.listEntered != nil {
		close(fake.listEntered)
	}
	if fake.listRelease != nil {
		<-fake.listRelease
	}
	return fake.listResult, fake.listErr
}

func (fake *navigationCandidateStoreFake) Load(_ context.Context, id string) (conversation.LoadResult, error) {
	fake.trace = append(fake.trace, "load:"+id)
	fake.loadCalls++
	if fake.loadEntered != nil {
		close(fake.loadEntered)
	}
	if fake.loadRelease != nil {
		<-fake.loadRelease
	}
	return fake.loadResult, fake.loadErr
}

type navigationWaiterFake struct {
	trace   *[]string
	err     error
	entered chan struct{}
	release <-chan struct{}
}

func (fake *navigationWaiterFake) WaitIdle(context.Context) error {
	*fake.trace = append(*fake.trace, "wait")
	if fake.entered != nil {
		close(fake.entered)
	}
	if fake.release != nil {
		<-fake.release
	}
	return fake.err
}

type navigationSaverFake struct {
	trace  *[]string
	result conversation.SaveResult
	err    error
	calls  int
	active *conversation.Conversation
}

func (fake *navigationSaverFake) Save(_ context.Context, active *conversation.Conversation) (conversation.SaveResult, error) {
	*fake.trace = append(*fake.trace, "save")
	fake.calls++
	fake.active = active
	return fake.result, fake.err
}

func newNavigationTestSink(t *testing.T, redactor *redact.RuntimeRedactor) *diagnostics.Sink {
	t.Helper()
	sink, err := diagnostics.NewBoundedSink(diagnostics.BoundedSinkOptions{
		Redactor:      redactor,
		MaxItems:      8,
		MaxItemBytes:  512,
		MaxTotalBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sink
}
