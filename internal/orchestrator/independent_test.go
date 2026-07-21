package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/memory"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

const isolatedReviewSkill = `---
name: review
description: Review in isolation
allowed_tools: [Read, Glob, Grep, Bash]
mode: isolated
history: 1
---
ISOLATED REVIEW {{args}}
`

type gatedSkillProvider struct {
	release chan struct{}
}

func (*gatedSkillProvider) Name() string { return "gated-skill" }

func (p *gatedSkillProvider) StreamChat(ctx context.Context, _ provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	out := make(chan provider.StreamEvent)
	go func() {
		defer close(out)
		select {
		case out <- provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: strings.Repeat("实时进度", 24)}:
		case <-ctx.Done():
			return
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return
		}
		select {
		case out <- provider.StreamEvent{Type: provider.StreamEventDone}:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

type failingIndependentProvider struct {
	calls atomic.Int32
}

func (*failingIndependentProvider) Name() string { return "failing-independent" }

func (p *failingIndependentProvider) StreamChat(context.Context, provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.calls.Add(1)
	return nil, errors.New("independent provider failed")
}

type contextBoundIndependentProvider struct {
	calls   atomic.Int32
	started chan struct{}
	stopped chan struct{}
}

func newContextBoundIndependentProvider() *contextBoundIndependentProvider {
	return &contextBoundIndependentProvider{started: make(chan struct{}, 1), stopped: make(chan struct{}, 1)}
}

func (*contextBoundIndependentProvider) Name() string { return "context-bound-independent" }

func (p *contextBoundIndependentProvider) StreamChat(ctx context.Context, _ provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.calls.Add(1)
	select {
	case p.started <- struct{}{}:
	default:
	}
	out := make(chan provider.StreamEvent)
	go func() {
		defer close(out)
		<-ctx.Done()
		select {
		case p.stopped <- struct{}{}:
		default:
		}
	}()
	return out, nil
}

func TestExecuteIsolatedSkillCommandReturnsOnlySummary(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: "review summary"},
		{Type: provider.StreamEventDone},
	}})
	conversation.AppendUserMessage(conv, "previous request")
	conversation.AppendAssistantMessage(conv, "previous answer")

	eventStream, prepared, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Args: "workspace", Raw: "/review workspace", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Mode != skill.ModeIsolated {
		t.Fatalf("unexpected mode: %s", prepared.Mode)
	}
	var transientText strings.Builder
	var finalText bool
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
		if event.Type != events.TextDelta {
			continue
		}
		if event.Transient {
			if event.IndependentID == "" {
				t.Fatal("transient independent event is missing its identifier")
			}
			transientText.WriteString(event.Text)
		} else if event.Text == "review summary" {
			finalText = true
		}
	}
	if transientText.String() != "review summary" || !finalText {
		t.Fatalf("missing transient or final text: transient=%q final=%v", transientText.String(), finalText)
	}
	requests := scripted.Requests()
	if len(requests) != 1 {
		t.Fatalf("expected one provider call without second summarizer, got %d", len(requests))
	}
	if !systemBlocksContain(requests[0].DynamicSystem, "ISOLATED REVIEW workspace") {
		t.Fatalf("isolated SOP missing: %#v", requests[0].DynamicSystem)
	}
	if len(requests[0].Messages) != 3 || requests[0].Messages[0].Content != "previous request" || requests[0].Messages[2].Content != "/review workspace" {
		t.Fatalf("unexpected independent history: %#v", requests[0].Messages)
	}
	if len(conv.Messages) != 4 || conv.Messages[2].Content != "/review workspace" || conv.Messages[3].Content != "review summary" {
		t.Fatalf("unexpected main history: %#v", conv.Messages)
	}
	for _, message := range conv.Messages {
		if message.Role == conversation.RoleToolCall || message.Role == conversation.RoleToolResult || strings.Contains(message.Content, "ISOLATED REVIEW") {
			t.Fatalf("independent internals leaked into main history: %#v", conv.Messages)
		}
	}
	if len(activity.Snapshot().Active) != 0 {
		t.Fatalf("isolated skill polluted main activity: %#v", activity.Snapshot())
	}
}

func TestIndependentTextIsVisibleBeforeProviderCompletes(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, nil)
	gated := &gatedSkillProvider{release: make(chan struct{})}
	orch.provider = gated
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-eventStream:
			if !ok {
				if !released {
					t.Fatal("independent output completed without a live transient text event")
				}
				return
			}
			if event.Type == events.Error {
				t.Fatal(event.Err)
			}
			if event.Type == events.TextDelta && event.Transient && event.Text != "" && !released {
				close(gated.release)
				released = true
			}
		case <-timer.C:
			if !released {
				close(gated.release)
			}
			t.Fatal("independent text was buffered until provider completion")
		}
	}
}

func TestIndependentDoesNotInheritMainSharedActivity(t *testing.T) {
	orch, conv, mainActivity, scripted := newSkillRuntimeFixture(t, map[string]string{
		"review.md": isolatedReviewSkill,
		"shared.md": `---
name: shared
description: Shared workflow
mode: shared
---
MAIN SHARED SOP
`,
	}, [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: "isolated result"},
		{Type: provider.StreamEventDone},
	}})
	if _, err := orch.PrepareSkill(skill.Invocation{Name: "shared", Origin: skill.OriginSlash}, mainActivity); err != nil {
		t.Fatal(err)
	}
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, mainActivity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	requests := scripted.Requests()
	if len(requests) != 1 || !systemBlocksContain(requests[0].DynamicSystem, "ISOLATED REVIEW") || systemBlocksContain(requests[0].DynamicSystem, "MAIN SHARED SOP") {
		t.Fatalf("isolated request inherited the main shared activity: %#v", requests)
	}
	if active := mainActivity.Snapshot().Active; len(active) != 1 || active[0].Name != "shared" {
		t.Fatalf("isolated run mutated the main activity: %#v", active)
	}
}

func TestIndependentModelOverrideDoesNotLeakToMainRequests(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"modeled.md": `---
name: modeled
description: Isolated model override
mode: isolated
history: 0
model: child-model
---
MODELED CHILD
`}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: "child result"}, {Type: provider.StreamEventDone}},
		{{Type: provider.StreamEventTextDelta, Delta: "main result"}, {Type: provider.StreamEventDone}},
	})
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "modeled", Raw: "/modeled", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	mainStream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "ordinary request", Mode: RunModeDefault, Activity: activity})
	if err != nil {
		t.Fatal(err)
	}
	for event := range mainStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	requests := scripted.Requests()
	if len(requests) != 2 || requests[0].Model != "child-model" || requests[1].Model != "default-model" {
		t.Fatalf("isolated model leaked into main requests: %#v", requests)
	}
}

func TestAgentTriggeredIsolatedSkillEndsParentLoop(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: "parent draft must disappear"}, {Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "load-review", Name: tool.LoadSkillToolName, ArgumentsJSON: `{"name":"review","args":"changes"}`}}},
		{{Type: provider.StreamEventTextDelta, Delta: "agent review summary"}, {Type: provider.StreamEventDone}},
	})

	eventStream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "review this", Mode: RunModeDefault, Activity: activity})
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	if len(scripted.Requests()) != 2 {
		t.Fatalf("expected parent plus child provider calls only, got %d", len(scripted.Requests()))
	}
	if len(conv.Messages) != 2 || conv.Messages[0].Content != "review this" || conv.Messages[1].Content != "agent review summary" {
		t.Fatalf("unexpected main history: %#v", conv.Messages)
	}
	for _, message := range conv.Messages {
		if message.ToolName == tool.LoadSkillToolName {
			t.Fatalf("isolated system tool call leaked into main history: %#v", conv.Messages)
		}
	}
}

func TestIndependentUsesOnlyCompletedChildIterationAsSummary(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: "draft before tool"}, {Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "read", Name: "Read", ArgumentsJSON: `{"path":"missing.txt"}`}}},
		{{Type: provider.StreamEventTextDelta, Delta: "final child summary"}, {Type: provider.StreamEventDone}},
	})

	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	if len(scripted.Requests()) != 2 {
		t.Fatalf("expected child tool iteration and final iteration, got %d", len(scripted.Requests()))
	}
	if len(conv.Messages) != 2 || conv.Messages[1].Content != "final child summary" {
		t.Fatalf("intermediate child text was mistaken for the summary: %#v", conv.Messages)
	}
}

func TestRunIndependentCancellationDoesNotRequireOutputConsumer(t *testing.T) {
	orch, conv, _, scripted := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: "streaming child output"}, {Type: provider.StreamEventDone}},
	})
	definition, ok := orch.skillManager.Resolve("review")
	if !ok {
		t.Fatal("review skill not found")
	}
	activity := skill.NewActivity()
	activated, err := activity.Activate(definition, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := orch.RunIndependent(ctx, IndependentRequest{
			Invocation: skill.PreparedInvocation{Definition: definition, Activated: activated, Activity: activity, Mode: skill.ModeIsolated, History: 1},
			Main:       conv,
			Mode:       RunModeDefault,
		}, make(chan events.Event))
		done <- runErr
	}()
	deadline := time.Now().Add(time.Second)
	for len(scripted.Requests()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case runErr := <-done:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("independent run deadlocked after cancellation with no output consumer")
	}
}

func TestIndependentProviderFailureDoesNotCreateSummary(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, nil)
	failing := &failingIndependentProvider{}
	orch.provider = failing
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var sawError, sawSuccess bool
	for event := range eventStream {
		sawError = sawError || event.Type == events.Error
		sawSuccess = sawSuccess || event.Type == events.Done || event.Type == events.TextDelta && !event.Transient
	}
	if !sawError || sawSuccess {
		t.Fatalf("provider failure events reported the wrong terminal state: error=%v success=%v", sawError, sawSuccess)
	}
	assertIndependentHasNoSuccessSummary(t, conv, "/review")
	if got := failing.calls.Load(); got != 1 {
		t.Fatalf("provider failure made %d calls, want 1", got)
	}
}

func TestIndependentTimeoutStopsProviderWithoutSummary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	assertIndependentContextStop(t, ctx, nil)
}

func TestIndependentCancelStopsProviderWithoutSummary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	assertIndependentContextStop(t, ctx, cancel)
}

func assertIndependentContextStop(t *testing.T, ctx context.Context, stop func()) {
	t.Helper()
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, nil)
	blocking := newContextBoundIndependentProvider()
	orch.provider = blocking
	eventStream, _, err := orch.SendSkill(ctx, conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	outcome := make(chan bool, 1)
	go func() {
		sawSuccess := false
		for event := range eventStream {
			sawSuccess = sawSuccess || event.Type == events.Done || event.Type == events.TextDelta && !event.Transient
		}
		outcome <- sawSuccess
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("independent Provider did not start")
	}
	if stop != nil {
		stop()
	}
	select {
	case sawSuccess := <-outcome:
		if sawSuccess {
			t.Fatal("context termination emitted a successful independent summary")
		}
	case <-time.After(time.Second):
		t.Fatal("independent event stream did not stop after context termination")
	}
	select {
	case <-blocking.stopped:
	case <-time.After(time.Second):
		t.Fatal("Provider continued running after independent context termination")
	}
	assertIndependentHasNoSuccessSummary(t, conv, "/review")
	if got := blocking.calls.Load(); got != 1 {
		t.Fatalf("context-terminated run made %d Provider calls, want 1", got)
	}
}

func assertIndependentHasNoSuccessSummary(t *testing.T, conv *conversation.Conversation, raw string) {
	t.Helper()
	if len(conv.Messages) != 1 || conv.Messages[0].Role != conversation.RoleUser || conv.Messages[0].Content != raw {
		t.Fatalf("failed independent run wrote a success summary: %#v", conv.Messages)
	}
}

func TestIndependentStreamingRedactionSpansProviderChunks(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{
			{Type: provider.StreamEventTextDelta, Delta: "api_key "},
			{Type: provider.StreamEventTextDelta, Delta: "= split-secret;\n-----BEGIN PRIVATE "},
			{Type: provider.StreamEventTextDelta, Delta: "KEY-----\nSUPERSECRETBASE64\n"},
			{Type: provider.StreamEventTextDelta, Delta: "-----END PRIVATE KEY-----\nreview complete"},
			{Type: provider.StreamEventDone},
		},
	})
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
		output.WriteString(event.Text)
	}
	for _, secret := range []string{"split-secret", "SUPERSECRETBASE64"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("streaming output leaked %q across provider chunks: %q", secret, output.String())
		}
		if len(conv.Messages) > 1 && strings.Contains(conv.Messages[1].Content, secret) {
			t.Fatalf("main summary leaked %q: %#v", secret, conv.Messages)
		}
	}
	if !strings.Contains(output.String(), "[redacted]") {
		t.Fatalf("expected redaction marker in output: %q", output.String())
	}
}

func TestIndependentUsesRuntimeRedactorAcrossPromptEventsAndHistory(t *testing.T) {
	const opaqueSecret = "opaque-runtime-value"
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"secure.md": `---
name: secure
description: Workflow containing opaque-runtime-value
mode: isolated
history: 0
---
Never expose opaque-runtime-value or {{args}}.
`}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: "result contains opaque-runtime-value"}, {Type: provider.StreamEventDone}},
	})
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(opaqueSecret)
	orch.redact = runtimeRedactor.Text
	orch.redactionLookbehind = max(64, runtimeRedactor.MaxSecretBytes())

	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "secure", Args: opaqueSecret, Raw: "/secure " + opaqueSecret, Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var visible strings.Builder
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
		visible.WriteString(event.Text)
		if event.Tool != nil {
			visible.WriteString(event.Tool.Arguments)
			visible.WriteString(event.Tool.Summary)
		}
	}
	if strings.Contains(visible.String(), opaqueSecret) {
		t.Fatalf("runtime secret leaked in events: %q", visible.String())
	}
	for _, message := range conv.Messages {
		if strings.Contains(message.Content, opaqueSecret) || strings.Contains(message.RawToolArguments, opaqueSecret) {
			t.Fatalf("runtime secret leaked in main history: %#v", conv.Messages)
		}
	}
	for _, request := range scripted.Requests() {
		for _, block := range append(append([]provider.SystemBlock(nil), request.StableSystem...), request.DynamicSystem...) {
			if strings.Contains(block.Content, opaqueSecret) {
				t.Fatalf("runtime secret leaked in provider prompt: %q", block.Content)
			}
		}
		for _, message := range request.Messages {
			if strings.Contains(message.Content, opaqueSecret) {
				t.Fatalf("runtime secret leaked in provider history: %#v", request.Messages)
			}
		}
	}
}

type failingSaveStore struct {
	conversation.ConversationStore
}

func (failingSaveStore) Save(context.Context, *conversation.Conversation) error {
	return errors.New("save failed")
}

type countingSaveStore struct {
	conversation.ConversationStore
	saves int
}

func (s *countingSaveStore) Save(context.Context, *conversation.Conversation) error {
	s.saves++
	return nil
}

type countingMemory struct {
	updates int
}

func (m *countingMemory) UpdateAsync(memory.UpdateInput) {
	m.updates++
}

func TestRunIndependentDoesNotPersistOrMutateMainConversation(t *testing.T) {
	orch, conv, _, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: "standalone result"}, {Type: provider.StreamEventDone}},
	})
	conversation.AppendUserMessage(conv, "existing request")
	conversation.AppendAssistantMessage(conv, "existing answer")
	before := cloneMessages(conv.Messages)
	store := &countingSaveStore{ConversationStore: orch.store}
	memoryRecorder := &countingMemory{}
	orch.store = store
	orch.memory = memoryRecorder
	prepared, err := orch.PrepareSkill(skill.Invocation{Name: "review", Origin: skill.OriginSlash}, skill.NewActivity())
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan events.Event, 32)
	result, err := orch.RunIndependent(context.Background(), IndependentRequest{
		Invocation: prepared,
		Main:       conv,
		Mode:       RunModeDefault,
	}, out)
	if err != nil || result.FinalText != "standalone result" {
		t.Fatalf("unexpected independent result: %#v %v", result, err)
	}
	if store.saves != 0 || memoryRecorder.updates != 0 {
		t.Fatalf("temporary run produced side effects: saves=%d memory=%d", store.saves, memoryRecorder.updates)
	}
	if !reflect.DeepEqual(conv.Messages, before) {
		t.Fatalf("temporary run mutated main messages: before=%#v after=%#v", before, conv.Messages)
	}
}

func TestIsolatedSummaryRollsBackWhenMainSaveFails(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventTextDelta, Delta: "summary that must roll back"}, {Type: provider.StreamEventDone}},
	})
	orch.store = failingSaveStore{ConversationStore: orch.store}
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for event := range eventStream {
		if event.Type == events.Error {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("expected save failure event")
	}
	if len(conv.Messages) != 1 || conv.Messages[0].Content != "/review" {
		t.Fatalf("failed summary save left a hidden assistant message: %#v", conv.Messages)
	}
}

func TestIndependentNestedIsolatedSkillIsRecoverable(t *testing.T) {
	orch, conv, _, scripted := newSkillRuntimeFixture(t, map[string]string{
		"review.md": isolatedReviewSkill,
		"other.md": `---
name: other
description: Other isolated workflow
mode: isolated
history: 0
---
OTHER
`,
	}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "nested", Name: tool.LoadSkillToolName, ArgumentsJSON: `{"name":"other"}`}}},
		{{Type: provider.StreamEventTextDelta, Delta: "handled nested rejection"}, {Type: provider.StreamEventDone}},
	})
	definition, ok := orch.skillManager.Resolve("review")
	if !ok {
		t.Fatal("review skill not found")
	}
	temporaryActivity := skill.NewActivity()
	activated, err := temporaryActivity.Activate(definition, "")
	if err != nil {
		t.Fatal(err)
	}
	prepared := skill.PreparedInvocation{Definition: definition, Activated: activated, Mode: skill.ModeIsolated, History: 1}
	out := make(chan events.Event, 32)
	result, err := orch.RunIndependent(context.Background(), IndependentRequest{
		Invocation: prepared,
		Main:       conv,
		Mode:       RunModeDefault,
	}, out)
	close(out)
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalText != "handled nested rejection" || len(scripted.Requests()) != 2 {
		t.Fatalf("unexpected nested result=%#v calls=%d", result, len(scripted.Requests()))
	}
	var recoverable bool
	for event := range out {
		if event.Tool != nil && event.Tool.Name == tool.LoadSkillToolName && event.Tool.Recoverable {
			recoverable = true
		}
	}
	if !recoverable {
		t.Fatal("nested isolated load did not return a recoverable tool error")
	}
}

func TestIndependentSharedLoadStaysInTemporaryActivity(t *testing.T) {
	orch, conv, mainActivity, scripted := newSkillRuntimeFixture(t, map[string]string{
		"review.md": isolatedReviewSkill,
		"helper.md": `---
name: helper
description: Temporary shared helper
mode: shared
---
TEMPORARY HELPER SOP
`,
	}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "helper", Name: tool.LoadSkillToolName, ArgumentsJSON: `{"name":"helper"}`}}},
		{{Type: provider.StreamEventTextDelta, Delta: "temporary helper result"}, {Type: provider.StreamEventDone}},
	})
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, mainActivity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	requests := scripted.Requests()
	if len(requests) != 2 || !systemBlocksContain(requests[1].DynamicSystem, "TEMPORARY HELPER SOP") || !systemBlocksContain(requests[1].DynamicSystem, "ISOLATED REVIEW") {
		t.Fatalf("shared helper did not activate in the child context: %#v", requests)
	}
	if len(mainActivity.Snapshot().Active) != 0 {
		t.Fatalf("temporary shared helper polluted main activity: %#v", mainActivity.Snapshot())
	}
	if len(conv.Messages) != 2 || conv.Messages[1].Content != "temporary helper result" {
		t.Fatalf("temporary tool history leaked into main conversation: %#v", conv.Messages)
	}
}

func TestIndependentEmptyFinalReplyDoesNotCreateSummary(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventDone}},
	})
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for event := range eventStream {
		if event.Type == events.Error {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("empty independent reply did not report a failure")
	}
	if len(conv.Messages) != 1 || conv.Messages[0].Content != "/review" {
		t.Fatalf("empty independent reply created a success summary: %#v", conv.Messages)
	}
}

func TestIndependentSkillReusesPermissionConfirmation(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "bash", Name: "Bash", ArgumentsJSON: `{"command":"printf ok"}`}}},
		{{Type: provider.StreamEventTextDelta, Delta: "permission-aware summary"}, {Type: provider.StreamEventDone}},
	})
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "review", Raw: "/review", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var sawConfirmation bool
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
		if event.Confirmation != nil {
			if !event.Transient || event.IndependentID == "" {
				t.Fatalf("independent confirmation was not marked transient: %#v", event)
			}
			sawConfirmation = true
			event.Confirmation.Decision <- events.ToolConfirmationDecision{Action: events.PermissionAllowOnce, Allowed: true}
		}
	}
	if !sawConfirmation {
		t.Fatal("dangerous independent tool did not request confirmation")
	}
	if len(scripted.Requests()) != 2 || len(conv.Messages) != 2 || conv.Messages[1].Content != "permission-aware summary" {
		t.Fatalf("unexpected independent permission flow: calls=%d history=%#v", len(scripted.Requests()), conv.Messages)
	}
}

func TestIndependentPermissionDenialAndCancelRemainRecoverable(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		action     events.PermissionAction
		wantStatus events.ToolDisplayStatus
	}{
		{name: "deny", action: events.PermissionDeny, wantStatus: events.ToolDisplayDenied},
		{name: "cancel", action: events.PermissionCancel, wantStatus: events.ToolDisplayCancelled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"review.md": isolatedReviewSkill}, [][]provider.StreamEvent{
				{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "bash", Name: "Bash", ArgumentsJSON: `{"command":"printf blocked"}`}}},
				{{Type: provider.StreamEventTextDelta, Delta: "handled permission decision"}, {Type: provider.StreamEventDone}},
			})
			eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
				Name: "review", Raw: "/review", Origin: skill.OriginSlash,
			}, activity, RunModeDefault)
			if err != nil {
				t.Fatal(err)
			}
			var sawDecision bool
			for event := range eventStream {
				if event.Type == events.Error {
					t.Fatal(event.Err)
				}
				if event.Confirmation != nil {
					event.Confirmation.Decision <- events.ToolConfirmationDecision{Action: testCase.action}
				}
				if event.Type == events.ToolDenied && event.Tool != nil && event.Tool.Status == testCase.wantStatus {
					sawDecision = true
				}
			}
			if !sawDecision || len(conv.Messages) != 2 || conv.Messages[1].Content != "handled permission decision" {
				t.Fatalf("permission decision did not preserve recoverable semantics: saw=%v history=%#v", sawDecision, conv.Messages)
			}
		})
	}
}
