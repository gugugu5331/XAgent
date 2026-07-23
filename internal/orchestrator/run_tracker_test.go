package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/provider"
	"xagent/internal/skill"
)

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
		{Type: provider.StreamEventTextDelta, Delta: "done"},
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
		{Type: provider.StreamEventTextDelta, Delta: "isolated done"},
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
