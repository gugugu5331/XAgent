package hook

import (
	"context"
	"strings"
	"testing"
)

func promptEvent(t *testing.T, eventName Event, ref ExecutionRef, sequence uint64) *frozenEvent {
	t.Helper()
	factory := testFactory(t, Limits{})
	c := factory.executionBase(eventName, ref)
	c.Sequence = sequence
	return factory.freeze(c)
}

func TestPromptScopeBinding(t *testing.T) {
	state := newPromptState(Limits{})
	ref := ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault}
	state.startSession("s")
	state.startTurn(ref)
	rule := Rule{Source: Source{EffectiveOrdinal: 1}}
	for _, scope := range []PromptScope{ScopeNext, ScopeTurn, ScopeSession} {
		if err := state.append(promptEvent(t, EventTurnStart, ref, uint64(len(state.entries)+1)), rule, scope, string(scope)); err != nil {
			t.Fatalf("%s: %v", scope, err)
		}
	}
	manual := testFactory(t, Limits{}).base(EventCompactBefore)
	manual.Session = &SessionContext{ID: "s"}
	manual.Compact = &CompactContext{Reason: CompactManual}
	manualEvent := testFactory(t, Limits{}).freeze(manual)
	if err := state.append(manualEvent, rule, ScopeNext, "manual"); err != nil {
		t.Fatal(err)
	}
	if err := state.append(manualEvent, rule, ScopeTurn, "bad"); err == nil {
		t.Fatal("turn scope without turn accepted")
	}
}

func TestPromptOrderingAndCleanup(t *testing.T) {
	state := newPromptState(Limits{})
	ref := ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault}
	state.startSession("s")
	state.startTurn(ref)
	entries := []struct {
		sequence uint64
		ordinal  int
		scope    PromptScope
		content  string
	}{{2, 2, ScopeTurn, "third"}, {1, 2, ScopeSession, "second"}, {1, 1, ScopeNext, "first"}}
	for _, item := range entries {
		rule := Rule{Source: Source{EffectiveOrdinal: item.ordinal, Path: "source"}}
		if err := state.append(promptEvent(t, EventTurnStart, ref, item.sequence), rule, item.scope, item.content); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := state.acquire(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	blocks := lease.Blocks()
	if len(blocks) != 3 || blocks[0].Content != "first" || blocks[1].Content != "second" || blocks[2].Content != "third" {
		t.Fatalf("blocks = %#v", blocks)
	}
	blocks[0].Content = "mutated"
	if lease.Blocks()[0].Content != "first" {
		t.Fatal("Blocks exposed storage")
	}
	lease.Commit()
	lease.Commit()
	lease.Release()
	again, _ := state.acquire(context.Background(), ref)
	if got := again.Blocks(); len(got) != 2 {
		t.Fatalf("next not consumed: %#v", got)
	}
	again.Release()
	state.endTurn(ref)
	afterTurn, _ := state.acquire(context.Background(), ref)
	if got := afterTurn.Blocks(); len(got) != 0 {
		t.Fatalf("stale turn acquired prompts after cleanup: %#v", got)
	}
	nextRef := ExecutionRef{SessionID: "s", ExecutionID: "e2", TurnID: "t2", Kind: ExecutionMain, Mode: ModeDefault}
	state.startTurn(nextRef)
	nextTurn, _ := state.acquire(context.Background(), nextRef)
	if got := nextTurn.Blocks(); len(got) != 1 || got[0].Content != "second" {
		t.Fatalf("session prompt was not visible to the next active turn: %#v", got)
	}
	nextTurn.Release()
	state.endSession("s")
	afterSession, _ := state.acquire(context.Background(), nextRef)
	if len(afterSession.Blocks()) != 0 {
		t.Fatal("session cleanup failed")
	}
}

func TestPromptLeaseTerminalOperations(t *testing.T) {
	state := newPromptState(Limits{})
	ref := ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t"}
	state.startSession(ref.SessionID)
	state.startTurn(ref)
	rule := Rule{Source: Source{EffectiveOrdinal: 1}}
	if err := state.append(promptEvent(t, EventTurnStart, ref, 1), rule, ScopeNext, "next"); err != nil {
		t.Fatal(err)
	}
	lease, _ := state.acquire(context.Background(), ref)
	state.endTurn(ref)
	lease.Release()
	lease.Commit()
	if len(state.entries) != 0 {
		t.Fatal("late lease revived entry")
	}

	factory := testFactory(t, Limits{})
	sessionContext := factory.base(EventSessionStart)
	sessionContext.Session = &SessionContext{ID: ref.SessionID, State: SessionNew}
	if err := state.append(factory.freeze(sessionContext), rule, ScopeNext, "session-next"); err != nil {
		t.Fatal(err)
	}
	state.startTurn(ref)
	sessionLease, err := state.acquire(context.Background(), ref)
	if err != nil || len(sessionLease.Blocks()) != 1 {
		t.Fatalf("session-next lease: blocks=%#v err=%v", sessionLease.Blocks(), err)
	}
	state.endTurn(ref)
	sessionLease.Commit()
	nextRef := ExecutionRef{SessionID: ref.SessionID, ExecutionID: "e2", TurnID: "t2"}
	state.startTurn(nextRef)
	retry, err := state.acquire(context.Background(), nextRef)
	if err != nil || len(retry.Blocks()) != 1 || retry.Blocks()[0].Content != "session-next" {
		t.Fatalf("late lease consumed session-next: blocks=%#v err=%v", retry.Blocks(), err)
	}
	retry.Release()
}

func TestPromptConcurrentSendGateGeneration(t *testing.T) {
	state := newPromptState(Limits{})
	ref := ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t"}
	state.startTurn(ref)
	rule := Rule{Source: Source{EffectiveOrdinal: 1}}
	if err := state.append(promptEvent(t, EventTurnStart, ref, 1), rule, ScopeNext, "old"); err != nil {
		t.Fatal(err)
	}
	first, _ := state.acquire(context.Background(), ref)
	started := make(chan struct{})
	acquired := make(chan PromptLease, 1)
	go func() { close(started); lease, _ := state.acquire(context.Background(), ref); acquired <- lease }()
	<-started
	if err := state.append(promptEvent(t, EventTurnStart, ref, 2), rule, ScopeNext, "new"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-acquired:
		t.Fatal("next gate was bypassed")
	default:
	}
	first.Release()
	second := <-acquired
	blocks := second.Blocks()
	if len(blocks) != 2 || blocks[0].Content != "old" || blocks[1].Content != "new" {
		t.Fatalf("generation = %#v", blocks)
	}
	second.Commit()
}

func TestPromptBudgets(t *testing.T) {
	limits := DefaultLimits()
	limits.PromptFragmentBytes = 4
	limits.PromptOwnerBytes = 6
	state := newPromptState(limits)
	ref := ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t"}
	state.startTurn(ref)
	rule := Rule{Source: Source{EffectiveOrdinal: 1}}
	event := promptEvent(t, EventTurnStart, ref, 1)
	if err := state.append(event, rule, ScopeTurn, "1234"); err != nil {
		t.Fatal(err)
	}
	if err := state.append(event, rule, ScopeTurn, "789"); err == nil {
		t.Fatal("fragment/owner limit accepted")
	}
	if len(state.entries) != 1 {
		t.Fatal("failed append was partial")
	}
	if err := state.append(event, rule, ScopeTurn, strings.Repeat("x", 5)); err == nil {
		t.Fatal("fragment limit accepted")
	}
}

func TestPromptLifecycleBoundaries(t *testing.T) {
	ref := ExecutionRef{SessionID: "session", ExecutionID: "execution", TurnID: "turn", Kind: ExecutionMain, Mode: ModeDefault}
	rule := Rule{Source: Source{Path: "lifecycle.yaml", Ordinal: 1, EffectiveOrdinal: 1}}
	state := newPromptState(Limits{})
	state.startTurn(ref)
	if state.activeSessions[ref.SessionID] {
		t.Fatal("turn start implicitly activated session scope")
	}
	if err := state.append(promptEvent(t, EventTurnStart, ref, 1), rule, ScopeSession, "orphan"); err == nil {
		t.Fatal("session prompt accepted without SessionStart")
	}
	state.startSession(ref.SessionID)
	if err := state.append(promptEvent(t, EventTurnStart, ref, 2), rule, ScopeSession, "active"); err != nil {
		t.Fatalf("active session prompt rejected: %v", err)
	}

	engine, err := NewEngine(newSnapshot(nil), EngineOptions{ProjectRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	staleRef := ExecutionRef{SessionID: "stale", ExecutionID: "stale-execution", TurnID: "stale-turn", Kind: ExecutionMain, Mode: ModeDefault}
	engine.prompts.startSession(staleRef.SessionID)
	engine.prompts.startTurn(staleRef)
	contextValue := engine.factory.executionBase(EventTurnStart, staleRef)
	if err := engine.prompts.append(engine.factory.freeze(contextValue), rule, ScopeSession, "stale"); err != nil {
		t.Fatal(err)
	}
	if engine.sessions[staleRef.SessionID] {
		t.Fatal("test precondition: stale session unexpectedly registered")
	}
	engine.SessionEnd(context.Background(), staleRef.SessionID, SessionEndSwitch)
	if engine.prompts.activeSessions[staleRef.SessionID] {
		t.Fatal("unregistered SessionEnd left a stale session owner")
	}
	if _, exists := engine.prompts.activeExecutions[staleRef.ExecutionID]; exists {
		t.Fatal("unregistered SessionEnd left a stale execution owner")
	}
	for _, entry := range engine.prompts.entries {
		if entry.sessionID == staleRef.SessionID {
			t.Fatal("unregistered SessionEnd left a stale prompt")
		}
	}
}
