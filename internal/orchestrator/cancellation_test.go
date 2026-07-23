package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/provider"
	"xagent/internal/skill"
)

type cancellationTurnEnd struct {
	kind   hook.ExecutionKind
	status hook.TurnStatus
	detail string
}

type cancellationHookRecorder struct {
	hook.Runtime
	mu      sync.Mutex
	nextID  int
	turnEnd chan cancellationTurnEnd
}

func newCancellationHookRecorder() *cancellationHookRecorder {
	return &cancellationHookRecorder{Runtime: hook.Noop(), turnEnd: make(chan cancellationTurnEnd, 8)}
}

func (r *cancellationHookRecorder) BeginTurn(_ context.Context, sessionID string, kind hook.ExecutionKind, mode hook.HookMode) hook.ExecutionRef {
	r.mu.Lock()
	r.nextID++
	id := r.nextID
	r.mu.Unlock()
	return hook.ExecutionRef{
		SessionID: sessionID, ExecutionID: fmt.Sprintf("cancel-execution-%d", id),
		TurnID: fmt.Sprintf("cancel-turn-%d", id), Kind: kind, Mode: mode,
	}
}

func (r *cancellationHookRecorder) EndTurn(_ context.Context, ref hook.ExecutionRef, status hook.TurnStatus, detail string) {
	r.turnEnd <- cancellationTurnEnd{kind: ref.Kind, status: status, detail: detail}
}

type cancelDuringStreamStartProvider struct {
	entered chan struct{}
	once    sync.Once
}

func newCancelDuringStreamStartProvider() *cancelDuringStreamStartProvider {
	return &cancelDuringStreamStartProvider{entered: make(chan struct{})}
}

func (*cancelDuringStreamStartProvider) Name() string { return "cancel-during-stream-start" }

func (p *cancelDuringStreamStartProvider) StreamChat(ctx context.Context, _ provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.once.Do(func() { close(p.entered) })
	<-ctx.Done()
	return nil, errors.New("provider returned after request cancellation")
}

type cancelThenCloseProvider struct {
	entered chan struct{}
	once    sync.Once
}

func newCancelThenCloseProvider() *cancelThenCloseProvider {
	return &cancelThenCloseProvider{entered: make(chan struct{})}
}

func (*cancelThenCloseProvider) Name() string { return "cancel-then-close" }

func (p *cancelThenCloseProvider) StreamChat(ctx context.Context, _ provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.once.Do(func() { close(p.entered) })
	out := make(chan provider.StreamEvent)
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}

type immediateProviderError struct{ err error }

func (*immediateProviderError) Name() string { return "immediate-provider-error" }
func (p *immediateProviderError) StreamChat(context.Context, provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	return nil, p.err
}

func TestMainTurnCancellationDuringProviderStartIsCanceled(t *testing.T) {
	providerImpl := newCancelDuringStreamStartProvider()
	hooks := newCancellationHookRecorder()
	orch := NewWithOptions(OrchestratorOptions{Provider: providerImpl, Hooks: hooks})
	conv := conversation.NewConversation("main-cancel-session", time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := orch.SendRequest(ctx, conv, RunRequest{UserText: "cancel while starting", Mode: RunModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	drained := drainCancellationEvents(stream)
	waitCancellationBarrier(t, providerImpl.entered, "main Provider start")
	cancel()
	waitCancellationBarrier(t, drained, "main event drain")
	ended := waitTurnEnd(t, hooks)
	if ended.kind != hook.ExecutionMain || ended.status != hook.TurnCanceled || ended.detail != "request canceled" {
		t.Fatalf("main turn end = %#v, want main/canceled", ended)
	}
}

func TestIsolatedTurnCancellationWinsProviderClose(t *testing.T) {
	orch, conv, activity, _ := newSkillRuntimeFixture(t, map[string]string{"cancel.md": `---
name: cancel
description: Cancellation lifecycle fixture
mode: isolated
history: 0
---
CANCEL TEST
`}, nil)
	providerImpl := newCancelThenCloseProvider()
	hooks := newCancellationHookRecorder()
	orch.provider = providerImpl
	orch.hooks = hooks
	ctx, cancel := context.WithCancel(context.Background())
	stream, _, err := orch.SendSkill(ctx, conv, skill.Invocation{
		Name: "cancel", Raw: "/cancel", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	drained := drainCancellationEvents(stream)
	waitCancellationBarrier(t, providerImpl.entered, "isolated Provider start")
	cancel()
	waitCancellationBarrier(t, drained, "isolated event drain")
	ended := waitTurnEnd(t, hooks)
	if ended.kind != hook.ExecutionIsolatedSkill || ended.status != hook.TurnCanceled || ended.detail != "request canceled" {
		t.Fatalf("isolated turn end = %#v, want isolated_skill/canceled", ended)
	}
}

func TestProviderErrorWithoutRequestCancellationRemainsError(t *testing.T) {
	wantErr := errors.New("real provider failure")
	hooks := newCancellationHookRecorder()
	orch := NewWithOptions(OrchestratorOptions{Provider: &immediateProviderError{err: wantErr}, Hooks: hooks})
	conv := conversation.NewConversation("provider-error-session", time.Now())
	stream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "fail normally", Mode: RunModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	ended := waitTurnEnd(t, hooks)
	if ended.kind != hook.ExecutionMain || ended.status != hook.TurnError || ended.detail != "agent run failed" {
		t.Fatalf("provider failure turn end = %#v, want main/error", ended)
	}
}

func TestCollectProviderStreamCancellationWinsClosedOrErrorRace(t *testing.T) {
	for _, mode := range []string{"closed", "error"} {
		t.Run(mode, func(t *testing.T) {
			for iteration := 0; iteration < 128; iteration++ {
				ctx, cancel := context.WithCancel(context.Background())
				stream := make(chan provider.StreamEvent, 1)
				if mode == "error" {
					stream <- provider.StreamEvent{Type: provider.StreamEventError, Err: errors.New("provider race failure")}
				}
				close(stream)
				cancel()
				_, reason, err := collectProviderStreamWithRedactor(ctx, stream, make(chan events.Event), nil, 64)
				if reason != StopReasonCancelled || !errors.Is(err, context.Canceled) {
					t.Fatalf("iteration %d: reason=%s err=%v, want canceled", iteration, reason, err)
				}
			}
		})
	}
}

func drainCancellationEvents(stream <-chan events.Event) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for range stream {
		}
		close(done)
	}()
	return done
}

func waitCancellationBarrier(t *testing.T, barrier <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-barrier:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func waitTurnEnd(t *testing.T, recorder *cancellationHookRecorder) cancellationTurnEnd {
	t.Helper()
	select {
	case ended := <-recorder.turnEnd:
		return ended
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn_end")
		return cancellationTurnEnd{}
	}
}
