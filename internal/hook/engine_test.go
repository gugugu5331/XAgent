package hook

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/diagnostics"
)

type fakeCommandRunner struct {
	mu      sync.Mutex
	calls   []string
	results map[string]CommandResult
	errors  map[string]error
	block   map[string]chan struct{}
	started map[string]chan struct{}
}

func (r *fakeCommandRunner) Run(ctx context.Context, request CommandRequest) (CommandResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, request.Command)
	gate := r.block[request.Command]
	started := r.started[request.Command]
	result := r.results[request.Command]
	err := r.errors[request.Command]
	r.mu.Unlock()
	if started != nil {
		close(started)
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return CommandResult{}, ctx.Err()
		}
	}
	return result, err
}
func (r *fakeCommandRunner) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func commandRule(event Event, ordinal int, command string, decision, once, async bool) Rule {
	return Rule{Event: event, Source: Source{Path: "rules.yaml", Ordinal: ordinal, EffectiveOrdinal: ordinal}, Once: once, Async: async, Timeout: time.Second, action: compiledAction{typeName: ActionCommand, command: command, decision: decision, timeout: time.Second}}
}

func newTestEngine(t *testing.T, rules []Rule, runner CommandRunner, collector *diagnostics.Collector) *Engine {
	t.Helper()
	engine, err := NewEngine(newSnapshot(rules), EngineOptions{ProjectRoot: t.TempDir(), CommandRunner: runner, LegacyDiagnostics: collector, ShutdownGrace: time.Second, ShutdownJoinGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestDispatchStableOrderAndToolDecision(t *testing.T) {
	runner := &fakeCommandRunner{results: map[string]CommandResult{"allow": {Stdout: []byte(`{"decision":"allow"}`)}, "deny": {Stdout: []byte(`{"decision":"deny","reason":"blocked"}`)}, "after": {Stdout: []byte(`{"decision":"allow"}`)}}, errors: map[string]error{}, block: map[string]chan struct{}{}}
	engine := newTestEngine(t, []Rule{commandRule(EventToolBefore, 1, "allow", true, false, false), commandRule(EventToolBefore, 2, "deny", true, false, false), commandRule(EventToolBefore, 3, "after", true, false, false)}, runner, nil)
	decision := engine.BeforeTool(context.Background(), ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault}, NewToolInput("c", "Read", map[string]any{}))
	if !decision.IsDeny() || decision.Reason() != "blocked" {
		t.Fatalf("decision = %#v", decision)
	}
	calls := runner.Calls()
	if len(calls) != 2 || calls[0] != "allow" || calls[1] != "deny" {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestDispatchFailOpenAndOnce(t *testing.T) {
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{})
	runner := &fakeCommandRunner{results: map[string]CommandResult{"deny": {Stdout: []byte(`{"decision":"deny","reason":"stop"}`)}}, errors: map[string]error{"fail": errors.New("canary payload")}, block: map[string]chan struct{}{}}
	engine := newTestEngine(t, []Rule{commandRule(EventToolBefore, 1, "fail", true, true, false), commandRule(EventToolBefore, 2, "deny", true, false, false)}, runner, collector)
	ref := ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t"}
	if !engine.BeforeTool(context.Background(), ref, ToolInput{}).IsDeny() {
		t.Fatal("failure did not continue")
	}
	if !engine.BeforeTool(context.Background(), ref, ToolInput{}).IsDeny() {
		t.Fatal("once failure was not releasable")
	}
	calls := runner.Calls()
	if len(calls) != 4 {
		t.Fatalf("calls = %#v", calls)
	}
	for _, item := range collector.List() {
		if item.Code == DiagnosticCommandFailed {
			return
		}
	}
	t.Fatal("missing command diagnostic")
}

func TestEngineLifecycleState(t *testing.T) {
	runner := &fakeCommandRunner{results: map[string]CommandResult{}, errors: map[string]error{}, block: map[string]chan struct{}{}}
	rules := []Rule{commandRule(EventSystemStart, 1, "system-start", false, false, false), commandRule(EventSessionStart, 2, "session-start", false, false, false), commandRule(EventTurnStart, 3, "turn-start", false, false, false), commandRule(EventTurnEnd, 4, "turn-end", false, false, false), commandRule(EventSessionEnd, 5, "session-end", false, false, false), commandRule(EventSystemStop, 6, "system-stop", false, false, false)}
	engine := newTestEngine(t, rules, runner, nil)
	engine.SystemStart(context.Background())
	engine.SystemStart(context.Background())
	engine.SessionStart(context.Background(), "s", SessionNew)
	ref := engine.BeginTurn(context.Background(), "s", ExecutionMain, ModeDefault)
	engine.EndTurn(context.Background(), ref, TurnCompleted, "")
	engine.EndTurn(context.Background(), ref, TurnCompleted, "")
	engine.SessionEnd(context.Background(), "s", SessionEndExit)
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"system-start", "session-start", "turn-start", "turn-end", "session-end", "system-stop"}
	got := runner.Calls()
	if len(got) != len(want) {
		t.Fatalf("calls = %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls = %#v", got)
		}
	}
}

func TestSystemStopAsyncAdmission(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	runner := &fakeCommandRunner{results: map[string]CommandResult{}, errors: map[string]error{}, block: map[string]chan struct{}{"stop": gate}, started: map[string]chan struct{}{"stop": started}}
	engine := newTestEngine(t, []Rule{commandRule(EventSystemStop, 1, "stop", false, true, true)}, runner, nil)
	engine.SystemStart(context.Background())
	done := make(chan struct{})
	go func() { _ = engine.Shutdown(context.Background()); close(done) }()
	<-started
	close(gate)
	<-done
	if got := runner.Calls(); len(got) != 1 || got[0] != "stop" {
		t.Fatalf("calls = %#v", got)
	}
}

func TestShutdownCancellationNotice(t *testing.T) {
	started := make(chan struct{})
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{},
		errors:  map[string]error{},
		block:   map[string]chan struct{}{"background-1": make(chan struct{})},
		started: map[string]chan struct{}{"background-1": started},
	}
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{})
	engine, err := NewEngine(
		newSnapshot([]Rule{
			commandRule(EventTurnStart, 1, "background-1", false, false, true),
			commandRule(EventTurnStart, 2, "background-2", false, false, true),
			commandRule(EventTurnStart, 3, "background-3", false, false, true),
		}),
		EngineOptions{
			ProjectRoot:       t.TempDir(),
			CommandRunner:     runner,
			LegacyDiagnostics: collector,
			AsyncWorkers:      1,
			AsyncQueue:        3,
			ShutdownGrace:     10 * time.Millisecond,
			ShutdownJoinGrace: time.Second,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	engine.SystemStart(context.Background())
	engine.BeginTurn(context.Background(), "session", ExecutionMain, ModeDefault)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background Hook did not start")
	}
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeated shutdown: %v", err)
	}
	notices := engine.ShutdownNotices()
	if len(notices) != 1 || notices[0].Code != DiagnosticShutdownCancelled {
		t.Fatalf("shutdown notices = %#v", notices)
	}
	if notices[0].Attributes["count"] != "3" || !strings.Contains(notices[0].Message, "3") {
		t.Fatalf("shutdown count = %#v", notices[0])
	}
	shutdownDuration, parseErr := strconv.Atoi(notices[0].Attributes["duration_ms"])
	if parseErr != nil || shutdownDuration <= 0 {
		t.Fatalf("shutdown duration = %q", notices[0].Attributes["duration_ms"])
	}
	if notices[0].Path != "" || notices[0].Source != "" {
		t.Fatalf("aggregate diagnostic has fake rule location: %#v", notices[0])
	}
	for _, forbidden := range []string{"action", "rule_ordinal", "effective_rule_ordinal"} {
		if _, exists := notices[0].Attributes[forbidden]; exists {
			t.Fatalf("aggregate diagnostic has fake %s: %#v", forbidden, notices[0])
		}
	}
	items := collector.List()
	if len(items) < 1 || items[len(items)-1].Text() != notices[0].Text() {
		t.Fatalf("collector/notices diverged: items=%#v notices=%#v", items, notices)
	}
	second := engine.ShutdownNotices()
	notices[0].Attributes["event"] = "mutated"
	notices[0].Attributes["count"] = "999"
	notices[0].Attributes["new"] = "value"
	notices[0].Message = "mutated"
	if second[0].Attributes["event"] != string(EventSystemStop) || second[0].Attributes["count"] != "3" || second[0].Message == "mutated" {
		t.Fatal("ShutdownNotices copies share state")
	}
	third := engine.ShutdownNotices()
	if third[0].Attributes["event"] != string(EventSystemStop) || third[0].Attributes["count"] != "3" || third[0].Attributes["new"] != "" || third[0].Message == "mutated" {
		t.Fatal("ShutdownNotices exposed internal attributes")
	}
}

func TestAsyncFailureRecordsRealDuration(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{},
		errors:  map[string]error{"slow-fail": errors.New("failure canary")},
		block:   map[string]chan struct{}{"slow-fail": release},
		started: map[string]chan struct{}{"slow-fail": started},
	}
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{})
	engine := newTestEngine(t, []Rule{commandRule(EventTurnStart, 1, "slow-fail", false, false, true)}, runner, collector)
	engine.SystemStart(context.Background())
	engine.BeginTurn(context.Background(), "session", ExecutionMain, ModeDefault)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("async Hook did not start")
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, item := range collector.List() {
		if item.Code != DiagnosticCommandFailed {
			continue
		}
		duration, err := strconv.Atoi(item.Attributes["duration_ms"])
		if err != nil || duration < 10 {
			t.Fatalf("async duration = %q", item.Attributes["duration_ms"])
		}
		return
	}
	t.Fatal("missing async failure diagnostic")
}

func TestEndLifecycleDispatchesBeforeCleanup(t *testing.T) {
	turnStarted, sessionStarted := make(chan struct{}), make(chan struct{})
	turnRelease, sessionRelease := make(chan struct{}), make(chan struct{})
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{},
		errors:  map[string]error{},
		block: map[string]chan struct{}{
			"turn-end":    turnRelease,
			"session-end": sessionRelease,
		},
		started: map[string]chan struct{}{
			"turn-end":    turnStarted,
			"session-end": sessionStarted,
		},
	}
	engine := newTestEngine(t, []Rule{
		commandRule(EventTurnEnd, 1, "turn-end", false, false, false),
		commandRule(EventSessionEnd, 2, "session-end", false, false, false),
	}, runner, nil)
	engine.SessionStart(context.Background(), "session", SessionNew)
	ref := engine.BeginTurn(context.Background(), "session", ExecutionMain, ModeDefault)

	turnDone := make(chan struct{})
	go func() {
		engine.EndTurn(context.Background(), ref, TurnCompleted, "")
		close(turnDone)
	}()
	<-turnStarted
	engine.lifecycleMu.Lock()
	turnActive, turnEnding := engine.executions[ref.ExecutionID], engine.endingExecutions[ref.ExecutionID]
	engine.lifecycleMu.Unlock()
	if !turnActive || !turnEnding {
		t.Fatalf("turn state cleaned before turn_end dispatch: active=%v ending=%v", turnActive, turnEnding)
	}
	engine.EndTurn(context.Background(), ref, TurnCompleted, "")
	close(turnRelease)
	<-turnDone
	engine.lifecycleMu.Lock()
	_, turnActive = engine.executions[ref.ExecutionID]
	engine.lifecycleMu.Unlock()
	if turnActive {
		t.Fatal("turn state remained after turn_end dispatch")
	}

	sessionDone := make(chan struct{})
	go func() {
		engine.SessionEnd(context.Background(), "session", SessionEndExit)
		close(sessionDone)
	}()
	<-sessionStarted
	engine.lifecycleMu.Lock()
	sessionActive, sessionEnding := engine.sessions["session"], engine.endingSessions["session"]
	engine.lifecycleMu.Unlock()
	if !sessionActive || !sessionEnding {
		t.Fatalf("session state cleaned before session_end dispatch: active=%v ending=%v", sessionActive, sessionEnding)
	}
	engine.SessionStart(context.Background(), "session", SessionResumed)
	engine.SessionEnd(context.Background(), "session", SessionEndExit)
	close(sessionRelease)
	<-sessionDone
	engine.lifecycleMu.Lock()
	_, sessionActive = engine.sessions["session"]
	engine.lifecycleMu.Unlock()
	if sessionActive {
		t.Fatal("session state remained after session_end dispatch")
	}
	if calls := runner.Calls(); len(calls) != 2 {
		t.Fatalf("duplicate end dispatch: %#v", calls)
	}
}

func TestShutdownWaitsForAdmittedEndBeforeSystemStop(t *testing.T) {
	turnStarted := make(chan struct{})
	turnRelease := make(chan struct{})
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{},
		errors:  map[string]error{},
		block:   map[string]chan struct{}{"turn-end": turnRelease},
		started: map[string]chan struct{}{"turn-end": turnStarted},
	}
	engine := newTestEngine(t, []Rule{
		commandRule(EventTurnEnd, 1, "turn-end", false, false, false),
		commandRule(EventSystemStop, 2, "system-stop", false, false, false),
	}, runner, nil)
	engine.SystemStart(context.Background())
	ref := engine.BeginTurn(context.Background(), "session", ExecutionMain, ModeDefault)

	turnDone := make(chan struct{})
	go func() {
		engine.EndTurn(context.Background(), ref, TurnCompleted, "")
		close(turnDone)
	}()
	<-turnStarted
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- engine.Shutdown(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	if calls := runner.Calls(); len(calls) != 1 || calls[0] != "turn-end" {
		t.Fatalf("system_stop overtook admitted turn_end: %#v", calls)
	}
	close(turnRelease)
	<-turnDone
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if calls := runner.Calls(); len(calls) != 2 || calls[0] != "turn-end" || calls[1] != "system-stop" {
		t.Fatalf("lifecycle order = %#v", calls)
	}
}

func TestShutdownAdmissionBarrierAndContext(t *testing.T) {
	stopStarted := make(chan struct{})
	stopRelease := make(chan struct{})
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{},
		errors:  map[string]error{},
		block:   map[string]chan struct{}{"system-stop": stopRelease},
		started: map[string]chan struct{}{"system-stop": stopStarted},
	}
	engine := newTestEngine(t, []Rule{
		commandRule(EventSystemStop, 1, "system-stop", false, false, false),
		commandRule(EventSessionStart, 2, "session-start", false, false, false),
		commandRule(EventTurnStart, 3, "turn-start", false, false, false),
		commandRule(EventMessageBefore, 4, "message-before", false, false, false),
		commandRule(EventToolBefore, 5, "tool-before", false, false, false),
		commandRule(EventCompactBefore, 6, "compact-before", false, false, false),
	}, runner, nil)
	engine.SystemStart(context.Background())
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- engine.Shutdown(context.Background()) }()
	<-stopStarted

	engine.SessionStart(context.Background(), "late", SessionNew)
	if ref := engine.BeginTurn(context.Background(), "late", ExecutionMain, ModeDefault); ref.ExecutionID != "" {
		t.Fatalf("turn admitted while stopping: %#v", ref)
	}
	if token := engine.BeginMessage(context.Background(), ExecutionRef{}, MessageUser, "late"); token.event != nil {
		t.Fatal("message admitted while stopping")
	}
	if decision := engine.BeforeTool(context.Background(), ExecutionRef{}, ToolInput{Name: "Read"}); decision.IsDeny() {
		t.Fatalf("stopped tool gate changed decision: %#v", decision)
	}
	if token := engine.BeforeCompact(context.Background(), CompactBinding{}, CompactInput{Reason: CompactManual}); token.event != nil {
		t.Fatal("compact admitted while stopping")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := engine.Shutdown(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent shutdown wait = %v", err)
	}
	if calls := runner.Calls(); len(calls) != 1 || calls[0] != "system-stop" {
		t.Fatalf("events admitted during stop: %#v", calls)
	}

	close(stopRelease)
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	engine.SessionStart(context.Background(), "after", SessionNew)
	if ref := engine.BeginTurn(context.Background(), "after", ExecutionMain, ModeDefault); ref.ExecutionID != "" {
		t.Fatalf("turn admitted after stop: %#v", ref)
	}
	if calls := runner.Calls(); len(calls) != 1 || calls[0] != "system-stop" {
		t.Fatalf("events admitted after stop: %#v", calls)
	}
}

func TestShutdownWaiterCancellationDoesNotRestartCleanup(t *testing.T) {
	turnStarted := make(chan struct{})
	turnRelease := make(chan struct{})
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{},
		errors:  map[string]error{},
		block:   map[string]chan struct{}{"turn-end": turnRelease},
		started: map[string]chan struct{}{"turn-end": turnStarted},
	}
	engine := newTestEngine(t, []Rule{
		commandRule(EventTurnEnd, 1, "turn-end", false, false, false),
		commandRule(EventSystemStop, 2, "system-stop", false, false, false),
	}, runner, nil)
	engine.SystemStart(context.Background())
	ref := engine.BeginTurn(context.Background(), "session", ExecutionMain, ModeDefault)
	turnDone := make(chan struct{})
	go func() {
		engine.EndTurn(context.Background(), ref, TurnCompleted, "")
		close(turnDone)
	}()
	<-turnStarted

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := engine.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown wait = %v", err)
	}
	engine.lifecycleMu.Lock()
	state := engine.systemState
	engine.lifecycleMu.Unlock()
	if state != 4 {
		t.Fatalf("caller cancellation changed closing state to %d", state)
	}
	close(turnRelease)
	<-turnDone
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := runner.Calls(); len(calls) != 2 || calls[0] != "turn-end" || calls[1] != "system-stop" {
		t.Fatalf("continued lifecycle = %#v", calls)
	}
}

func TestCommandIOFailureSettlement(t *testing.T) {
	const canary = "raw-command-io-settlement-secret"
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{})
	runner := &fakeCommandRunner{
		results: map[string]CommandResult{"after": {}},
		errors:  map[string]error{"io-failure": errors.New(canary)},
		block:   map[string]chan struct{}{},
	}
	engine := newTestEngine(t, []Rule{
		commandRule(EventToolBefore, 1, "io-failure", true, true, false),
		commandRule(EventToolBefore, 2, "after", false, false, false),
	}, runner, collector)
	ref := ExecutionRef{SessionID: "s", ExecutionID: "e", TurnID: "t", Kind: ExecutionMain, Mode: ModeDefault}
	for attempt := 0; attempt < 2; attempt++ {
		decision := engine.BeforeTool(context.Background(), ref, NewToolInput("c", "Read", map[string]any{}))
		if decision.IsDeny() {
			t.Fatalf("I/O failure produced deny on attempt %d: %#v", attempt, decision)
		}
	}
	if calls := runner.Calls(); len(calls) != 4 || calls[0] != "io-failure" || calls[1] != "after" || calls[2] != "io-failure" || calls[3] != "after" {
		t.Fatalf("calls = %#v", calls)
	}
	items := collector.List()
	if len(items) != 2 {
		t.Fatalf("diagnostics = %#v", items)
	}
	for _, item := range items {
		if item.Code != DiagnosticCommandFailed || item.Message != "hook action failed" {
			t.Fatalf("unsafe diagnostic = %#v", item)
		}
		if strings.Contains(item.Text(), canary) {
			t.Fatalf("I/O error leaked: %s", item.Text())
		}
	}
}
