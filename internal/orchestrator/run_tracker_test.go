package orchestrator

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/provider"
	"xagent/internal/skill"
)

type orderedSaveStore struct {
	conversation.Store
	entered   chan struct{}
	release   chan struct{}
	completed atomic.Bool
	once      sync.Once
	traceMu   sync.Mutex
	trace     []string
}

func newOrderedSaveStore() *orderedSaveStore {
	return &orderedSaveStore{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *orderedSaveStore) Save(ctx context.Context, _ *conversation.Conversation) (conversation.SaveResult, error) {
	s.record("save-enter")
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		s.record("save-exit")
		s.completed.Store(true)
		return conversation.SaveResult{Kind: conversation.SaveSnapshot}, nil
	case <-ctx.Done():
		return conversation.SaveResult{}, ctx.Err()
	}
}

func (s *orderedSaveStore) record(event string) {
	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	s.trace = append(s.trace, event)
}

func (s *orderedSaveStore) events() []string {
	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	return append([]string(nil), s.trace...)
}

func drainRunEvents(stream <-chan events.Event) {
	for range stream {
	}
}

func requireRunTrackerReleased(t *testing.T, orch *Orchestrator) {
	t.Helper()
	if got := orch.runs.count(); got != 0 {
		t.Fatalf("active runs after exit = %d, want 0", got)
	}
	if err := orch.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after exit: %v", err)
	}
}

func requireRunTrackerBusyGeneration(t *testing.T, orch *Orchestrator) {
	t.Helper()
	orch.runs.mu.Lock()
	active := orch.runs.active
	idle := orch.runs.idle
	orch.runs.mu.Unlock()
	if active == 0 {
		t.Fatal("run tracker is idle while a run side effect is blocked")
	}
	select {
	case <-idle:
		t.Fatal("busy run generation exposes a closed idle channel")
	default:
	}
}

type blockingBeginTurnRuntime struct {
	hook.Runtime
	entered chan hook.ExecutionKind
	release chan struct{}
	once    sync.Once
}

func newBlockingBeginTurnRuntime() *blockingBeginTurnRuntime {
	return &blockingBeginTurnRuntime{
		Runtime: hook.Noop(),
		entered: make(chan hook.ExecutionKind, 1),
		release: make(chan struct{}),
	}
}

func (r *blockingBeginTurnRuntime) BeginTurn(_ context.Context, sessionID string, kind hook.ExecutionKind, mode hook.HookMode) hook.ExecutionRef {
	r.entered <- kind
	<-r.release
	return hook.ExecutionRef{SessionID: sessionID, ExecutionID: "blocked-execution", TurnID: "blocked-turn", Kind: kind, Mode: mode}
}

func (r *blockingBeginTurnRuntime) unblock() {
	r.once.Do(func() { close(r.release) })
}

func waitForBeginTurn(t *testing.T, runtime *blockingBeginTurnRuntime) hook.ExecutionKind {
	t.Helper()
	select {
	case kind := <-runtime.entered:
		return kind
	case <-time.After(time.Second):
		t.Fatal("BeginTurn hook was not reached")
		return ""
	}
}

func requireWaitIdleBusy(t *testing.T, orch *Orchestrator) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := orch.WaitIdle(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitIdle while BeginTurn is blocked = %v, want context deadline", err)
	}
}

func TestWaitIdleTracksConcurrentRuns(t *testing.T) {
	tracker := newRunTracker()
	endFirst := tracker.begin()
	endSecond := tracker.begin()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tracker.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait while busy = %v, want context.Canceled", err)
	}

	endFirst()
	if got := tracker.count(); got != 1 {
		t.Fatalf("active after first end = %d, want 1", got)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := tracker.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait before final end = %v, want context.Canceled", err)
	}

	endSecond()
	endSecond()
	if err := tracker.wait(context.Background()); err != nil {
		t.Fatalf("wait after final end: %v", err)
	}
	if got := tracker.count(); got != 0 {
		t.Fatalf("active after duplicate end = %d, want 0", got)
	}
}

func TestWaitIdleUsesBusyGenerations(t *testing.T) {
	tracker := newRunTracker()
	if err := tracker.wait(context.Background()); err != nil {
		t.Fatalf("initial wait: %v", err)
	}
	end := tracker.begin()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tracker.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("new busy generation reused closed idle channel: %v", err)
	}
	end()
	if err := tracker.wait(context.Background()); err != nil {
		t.Fatalf("wait after generation ended: %v", err)
	}
}

func TestSendRequestRegistersBeforeBeginTurnHook(t *testing.T) {
	runtime := newBlockingBeginTurnRuntime()
	t.Cleanup(runtime.unblock)
	providerImpl := &fakeProvider{events: [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: testSafeText("done")},
		{Type: provider.StreamEventDone},
	}}}
	orch := NewWithOptions(OrchestratorOptions{Provider: providerImpl, Hooks: runtime})
	conv := conversation.NewConversation("tracked-main", time.Now())

	type sendResult struct {
		stream <-chan events.Event
		err    error
	}
	resultCh := make(chan sendResult, 1)
	go func() {
		stream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "hello", Mode: RunModeDefault})
		resultCh <- sendResult{stream: stream, err: err}
	}()

	if kind := waitForBeginTurn(t, runtime); kind != hook.ExecutionMain {
		t.Fatalf("BeginTurn kind = %s, want main", kind)
	}
	if got := orch.runs.count(); got != 1 {
		t.Fatalf("active runs while BeginTurn is blocked = %d, want 1", got)
	}
	if len(conv.Messages) != 0 {
		t.Fatalf("Conversation mutated before blocked BeginTurn returned: %#v", conv.Messages)
	}
	requireWaitIdleBusy(t, orch)

	runtime.unblock()
	result := <-resultCh
	if result.err != nil {
		t.Fatal(result.err)
	}
	for event := range result.stream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	if err := orch.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after main turn: %v", err)
	}
}

func TestSendSkillRegistersOuterRunBeforeIsolatedTurn(t *testing.T) {
	runtime := newBlockingBeginTurnRuntime()
	t.Cleanup(runtime.unblock)
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{
		"tracked.md": `---
name: tracked
description: Tracker barrier fixture
mode: isolated
history: 0
---
TRACK {{args}}
`,
	}, [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: testSafeText("isolated done")},
		{Type: provider.StreamEventDone},
	}})
	orch.hooks = runtime

	stream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "tracked", Raw: "/tracked", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	go func() {
		for event := range stream {
			if event.Type == events.Error {
				drained <- event.Err
				return
			}
		}
		drained <- nil
	}()

	if kind := waitForBeginTurn(t, runtime); kind != hook.ExecutionIsolatedSkill {
		t.Fatalf("BeginTurn kind = %s, want isolated_skill", kind)
	}
	if got := orch.runs.count(); got < 1 {
		t.Fatalf("isolated run was not tracked while BeginTurn was blocked: %d", got)
	}
	requireWaitIdleBusy(t, orch)

	runtime.unblock()
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if err := orch.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after isolated turn: %v", err)
	}
}

func TestOuterRunRegistrationReleasesOnSynchronousErrors(t *testing.T) {
	orch := NewWithOptions(OrchestratorOptions{})
	if _, err := orch.SendRequest(context.Background(), nil, RunRequest{UserText: "hello", Mode: RunModeDefault}); err == nil {
		t.Fatal("SendRequest unexpectedly accepted a nil Conversation")
	}
	if got := orch.runs.count(); got != 0 {
		t.Fatalf("SendRequest synchronous error leaked %d active runs", got)
	}

	conv := conversation.NewConversation("sync-error", time.Now())
	if _, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{Name: "missing"}, skill.NewActivity(), RunModeDefault); err == nil {
		t.Fatal("SendSkill unexpectedly accepted a missing Skill manager")
	}
	if got := orch.runs.count(); got != 0 {
		t.Fatalf("SendSkill synchronous error leaked %d active runs", got)
	}
	if err := orch.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle after synchronous errors: %v", err)
	}
}

func TestRunTrackerCoversMainAndIndependentExits(t *testing.T) {
	t.Run("main registers before turn hooks", func(t *testing.T) {
		runtime := newBlockingBeginTurnRuntime()
		t.Cleanup(runtime.unblock)
		providerImpl := &fakeProvider{events: [][]provider.StreamEvent{{
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("done")},
			{Type: provider.StreamEventDone},
		}}}
		orch := NewWithOptions(OrchestratorOptions{Provider: providerImpl, Hooks: runtime})
		conv := conversation.NewConversation("tracker-main-registration", time.Now())
		type sendResult struct {
			stream <-chan events.Event
			err    error
		}
		resultCh := make(chan sendResult, 1)
		go func() {
			stream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "main", Mode: RunModeDefault})
			resultCh <- sendResult{stream: stream, err: err}
		}()

		if kind := waitForBeginTurn(t, runtime); kind != hook.ExecutionMain {
			t.Fatalf("BeginTurn kind = %s, want main", kind)
		}
		requireRunTrackerBusyGeneration(t, orch)
		if len(conv.Messages) != 0 {
			t.Fatalf("Conversation mutated before blocked BeginTurn returned: %#v", conv.Messages)
		}
		runtime.unblock()
		result := <-resultCh
		if result.err != nil {
			t.Fatal(result.err)
		}
		drainRunEvents(result.stream)
		requireRunTrackerReleased(t, orch)
	})

	t.Run("independent registers before isolated turn hooks", func(t *testing.T) {
		runtime := newBlockingBeginTurnRuntime()
		t.Cleanup(runtime.unblock)
		orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{
			"tracked.md": `---
name: tracked
description: Run tracker registration fixture
mode: isolated
history: 0
---
TRACK
`,
		}, [][]provider.StreamEvent{{
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("isolated done")},
			{Type: provider.StreamEventDone},
		}})
		orch.hooks = runtime
		stream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
			Name: "tracked", Raw: "/tracked", Origin: skill.OriginSlash,
		}, activity, RunModeDefault)
		if err != nil {
			t.Fatal(err)
		}
		drained := make(chan struct{})
		go func() {
			drainRunEvents(stream)
			close(drained)
		}()

		if kind := waitForBeginTurn(t, runtime); kind != hook.ExecutionIsolatedSkill {
			t.Fatalf("BeginTurn kind = %s, want isolated_skill", kind)
		}
		requireRunTrackerBusyGeneration(t, orch)
		runtime.unblock()
		<-drained
		requireRunTrackerReleased(t, orch)
	})

	t.Run("synchronous failures", func(t *testing.T) {
		orch := NewWithOptions(OrchestratorOptions{})
		if _, err := orch.SendRequest(context.Background(), nil, RunRequest{UserText: "main", Mode: RunModeDefault}); err == nil {
			t.Fatal("SendRequest unexpectedly accepted a nil Conversation")
		}
		if _, err := orch.RunIndependent(context.Background(), IndependentRequest{}, make(chan events.Event)); err == nil {
			t.Fatal("RunIndependent unexpectedly accepted an empty request")
		}
		conv := conversation.NewConversation("tracker-sync-failure", time.Now())
		if _, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{Name: "missing"}, skill.NewActivity(), RunModeDefault); err == nil {
			t.Fatal("SendSkill unexpectedly accepted a missing Skill manager")
		}
		requireRunTrackerReleased(t, orch)
	})

	t.Run("main success", func(t *testing.T) {
		providerImpl := &fakeProvider{events: [][]provider.StreamEvent{{
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("done")},
			{Type: provider.StreamEventDone},
		}}}
		orch := NewWithOptions(OrchestratorOptions{Provider: providerImpl})
		conv := conversation.NewConversation("tracker-main-success", time.Now())
		stream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "main", Mode: RunModeDefault})
		if err != nil {
			t.Fatal(err)
		}
		drainRunEvents(stream)
		requireRunTrackerReleased(t, orch)
	})

	t.Run("main provider error", func(t *testing.T) {
		orch := NewWithOptions(OrchestratorOptions{Provider: &immediateProviderError{err: errors.New("provider failed")}})
		conv := conversation.NewConversation("tracker-main-error", time.Now())
		stream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "main", Mode: RunModeDefault})
		if err != nil {
			t.Fatal(err)
		}
		drainRunEvents(stream)
		requireRunTrackerReleased(t, orch)
	})

	t.Run("main cancellation", func(t *testing.T) {
		providerImpl := newCancelDuringStreamStartProvider()
		orch := NewWithOptions(OrchestratorOptions{Provider: providerImpl})
		conv := conversation.NewConversation("tracker-main-cancel", time.Now())
		ctx, cancel := context.WithCancel(context.Background())
		stream, err := orch.SendRequest(ctx, conv, RunRequest{UserText: "main", Mode: RunModeDefault})
		if err != nil {
			t.Fatal(err)
		}
		drained := make(chan struct{})
		go func() {
			drainRunEvents(stream)
			close(drained)
		}()
		<-providerImpl.entered
		cancel()
		<-drained
		requireRunTrackerReleased(t, orch)
	})

	for _, testCase := range []struct {
		name     string
		events   [][]provider.StreamEvent
		provider provider.Provider
		cancel   bool
	}{
		{
			name: "independent success",
			events: [][]provider.StreamEvent{{
				{Type: provider.StreamEventTextDelta, Delta: testSafeText("isolated done")},
				{Type: provider.StreamEventDone},
			}},
		},
		{name: "independent provider error", provider: &immediateProviderError{err: errors.New("provider failed")}},
		{name: "independent cancellation", provider: newCancelThenCloseProvider(), cancel: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{
				"tracked.md": `---
name: tracked
description: Run tracker exit fixture
mode: isolated
history: 0
---
TRACK
`,
			}, testCase.events)
			if testCase.provider != nil {
				orch.provider = testCase.provider
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, _, err := orch.SendSkill(ctx, conv, skill.Invocation{
				Name: "tracked", Raw: "/tracked", Origin: skill.OriginSlash,
			}, activity, RunModeDefault)
			if err != nil {
				t.Fatal(err)
			}
			drained := make(chan struct{})
			go func() {
				drainRunEvents(stream)
				close(drained)
			}()
			if testCase.cancel {
				providerImpl := testCase.provider.(*cancelThenCloseProvider)
				<-providerImpl.entered
				cancel()
			}
			<-drained
			requireRunTrackerReleased(t, orch)
		})
	}
}

func TestWaitIdleReturnsAfterOrderedSave(t *testing.T) {
	t.Run("main request", func(t *testing.T) {
		store := newOrderedSaveStore()
		providerImpl := &fakeProvider{events: [][]provider.StreamEvent{{
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("saved reply")},
			{Type: provider.StreamEventDone},
		}}}
		orch := NewWithOptions(OrchestratorOptions{Provider: providerImpl, Store: store})
		conv := conversation.NewConversation("ordered-save-main", time.Now())
		stream, err := orch.SendRequest(context.Background(), conv, RunRequest{
			UserText: "save",
			Mode:     RunModeDefault,
			Profile:  skill.ExecutionProfile{Model: "test", Persist: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		requireWaitIdleAfterStoreRelease(t, orch, store, stream)
	})

	t.Run("independent request", func(t *testing.T) {
		orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{
			"save.md": `---
name: save
description: Ordered independent save fixture
mode: isolated
history: 0
---
SAVE
`,
		}, [][]provider.StreamEvent{{
			{Type: provider.StreamEventTextDelta, Delta: testSafeText("saved isolated reply")},
			{Type: provider.StreamEventDone},
		}})
		store := newOrderedSaveStore()
		orch.store = store
		stream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
			Name: "save", Raw: "/save", Origin: skill.OriginSlash,
		}, activity, RunModeDefault)
		if err != nil {
			t.Fatal(err)
		}
		requireWaitIdleAfterStoreRelease(t, orch, store, stream)
	})
}

func requireWaitIdleAfterStoreRelease(
	t *testing.T,
	orch *Orchestrator,
	store *orderedSaveStore,
	stream <-chan events.Event,
) {
	t.Helper()
	drained := make(chan struct{})
	go func() {
		drainRunEvents(stream)
		close(drained)
	}()

	<-store.entered
	if store.completed.Load() {
		t.Fatal("save completed before its release barrier")
	}
	if got := orch.runs.count(); got != 1 {
		t.Fatalf("active runs while ordered save is blocked = %d, want 1", got)
	}
	requireRunTrackerBusyGeneration(t, orch)

	waitResult := make(chan error, 1)
	go func() {
		err := orch.WaitIdle(context.Background())
		store.record("wait-return")
		waitResult <- err
	}()
	close(store.release)
	if err := <-waitResult; err != nil {
		t.Fatalf("WaitIdle after ordered save: %v", err)
	}
	if !store.completed.Load() {
		t.Fatal("WaitIdle returned before ordered save completed")
	}
	if got, want := store.events(), []string{"save-enter", "save-exit", "wait-return"}; !slices.Equal(got, want) {
		t.Fatalf("ordered save trace = %v, want %v", got, want)
	}
	<-drained
	requireRunTrackerReleased(t, orch)
}
