package hook

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

var errActionPanic = errors.New("hook action panic")

const maxHookCleanupTimeout = 2 * time.Second

var errHookCleanupTimeout = errors.New("hook cleanup exceeded its hard deadline")

type EngineOptions struct {
	ProjectRoot string
	// Diagnostics is the process-wide bounded C7 sink injected by Assembly.
	// LegacyDiagnostics only feeds the staged user-facing Hook notice view and
	// is removed with the old production composition path at T4.29a.
	Diagnostics       diagnostics.BoundedSink
	LegacyDiagnostics *diagnostics.Collector
	Redactor          *redact.RuntimeRedactor
	CommandRunner     CommandRunner
	HTTPRunner        HTTPRunner
	Clock             func() time.Time
	IDSource          func() string
	Limits            Limits
	AsyncWorkers      int
	AsyncQueue        int
	CleanupTimeout    time.Duration
	ShutdownGrace     time.Duration
	ShutdownJoinGrace time.Duration
}

type Engine struct {
	rules             []Rule
	factory           *eventFactory
	prompts           *promptState
	once              *onceState
	diagnostics       diagnostics.BoundedSink
	legacyDiagnostics *diagnostics.Collector
	redactor          *redact.RuntimeRedactor
	limits            Limits
	command           CommandRunner
	http              HTTPRunner
	ownedHTTP         *DefaultHTTPRunner
	ownedHTTPClose    sync.Once
	async             *asyncPool
	cleanupTimeout    time.Duration
	shutdownOnce      sync.Once
	shutdownDone      chan struct{}
	shutdownErr       error
	lifecycleMu       sync.Mutex
	lifecycleCond     *sync.Cond
	systemState       uint8
	lifecycleInFlight int
	sessions          map[string]bool
	executions        map[string]bool
	endingSessions    map[string]bool
	endingExecutions  map[string]bool
	shutdownNotices   []diagnostics.Diagnostic
}

type actionOutcome struct {
	success  bool
	decision *ToolDecision
	code     string
	stage    string
	err      error
	duration time.Duration
}

func NewEngine(snapshot Snapshot, options EngineOptions) (*Engine, error) {
	limits := normalizeLimits(options.Limits)
	cleanupTimeout := options.CleanupTimeout
	if cleanupTimeout < 0 || cleanupTimeout > maxHookCleanupTimeout {
		return nil, fmt.Errorf("hook cleanup timeout must be between 1ns and 2s")
	}
	if cleanupTimeout == 0 {
		cleanupTimeout = maxHookCleanupTimeout
	}
	factory, err := newEventFactory(options.ProjectRoot, limits, options.Clock, options.IDSource)
	if err != nil {
		return nil, err
	}
	redactor := options.Redactor
	if redactor == nil {
		redactor = redact.NewRuntimeRedactor()
	}
	command := options.CommandRunner
	if command == nil {
		command = &ShellCommandRunner{Limits: limits, Redactor: redactor}
	}
	httpRunner := options.HTTPRunner
	var ownedHTTP *DefaultHTTPRunner
	if httpRunner == nil {
		ownedHTTP = &DefaultHTTPRunner{Limits: limits, Redactor: redactor}
		httpRunner = ownedHTTP
	}
	rules := snapshot.Rules()
	engine := &Engine{
		rules: rules, factory: factory, prompts: newPromptState(limits), once: newOnceState(),
		diagnostics: options.Diagnostics, legacyDiagnostics: options.LegacyDiagnostics,
		redactor: redactor, limits: limits, command: command, http: httpRunner,
		ownedHTTP: ownedHTTP, cleanupTimeout: cleanupTimeout, shutdownDone: make(chan struct{}),
		sessions: map[string]bool{}, executions: map[string]bool{},
		endingSessions: map[string]bool{}, endingExecutions: map[string]bool{},
	}
	engine.lifecycleCond = sync.NewCond(&engine.lifecycleMu)
	drainGrace, joinGrace := cleanupWindows(cleanupTimeout, options.ShutdownGrace, options.ShutdownJoinGrace)
	for _, rule := range rules {
		if rule.Async {
			engine.async = newAsyncPool(options.AsyncWorkers, options.AsyncQueue, drainGrace, joinGrace)
			break
		}
	}
	return engine, nil
}

func cleanupWindows(cleanupTimeout, drainGrace, joinGrace time.Duration) (time.Duration, time.Duration) {
	if drainGrace <= 0 || drainGrace >= cleanupTimeout {
		drainGrace = cleanupTimeout / 2
	}
	if drainGrace <= 0 {
		drainGrace = cleanupTimeout
	}
	remaining := cleanupTimeout - drainGrace
	if remaining <= 0 {
		remaining = drainGrace
	}
	if joinGrace <= 0 || joinGrace > remaining {
		joinGrace = remaining
	}
	return drainGrace, joinGrace
}

func (e *Engine) SystemStart(ctx context.Context) {
	if e == nil {
		return
	}
	e.lifecycleMu.Lock()
	if e.systemState != 0 {
		e.lifecycleMu.Unlock()
		return
	}
	e.systemState = 3
	e.lifecycleMu.Unlock()
	e.dispatch(ctx, e.factory.freeze(e.factory.base(EventSystemStart)), false)
	e.lifecycleMu.Lock()
	if e.systemState == 3 {
		e.systemState = 1
	}
	e.lifecycleCond.Broadcast()
	e.lifecycleMu.Unlock()
}

func (e *Engine) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	e.shutdownOnce.Do(func() {
		go e.cleanup()
	})
	select {
	case <-e.shutdownDone:
		return e.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) cleanup() {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout)
	err := e.shutdownInternal(ctx)
	timedOut := errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded)
	cancel()
	if timedOut {
		e.forceCleanup()
		e.shutdownErr = errHookCleanupTimeout
		e.addCleanupTimeoutNotice(time.Since(started))
	} else {
		e.shutdownErr = err
	}
	close(e.shutdownDone)
}

func (e *Engine) shutdownInternal(ctx context.Context) error {
	e.lifecycleMu.Lock()
	for {
		if err := ctx.Err(); err != nil {
			e.lifecycleMu.Unlock()
			return err
		}
		switch e.systemState {
		case 0:
			e.systemState = 4
			e.lifecycleCond.Broadcast()
			if err := e.waitLifecycleLocked(ctx, func() bool { return e.lifecycleInFlight == 0 }); err != nil {
				e.lifecycleMu.Unlock()
				return err
			}
			e.lifecycleMu.Unlock()
			if err := e.closeResources(ctx); err != nil {
				return err
			}
			e.lifecycleMu.Lock()
			e.systemState = 2
			e.lifecycleCond.Broadcast()
			e.lifecycleMu.Unlock()
			return nil
		case 1:
			e.systemState = 4
			e.lifecycleCond.Broadcast()
			if err := e.waitLifecycleLocked(ctx, func() bool { return e.lifecycleInFlight == 0 }); err != nil {
				e.lifecycleMu.Unlock()
				return err
			}
			e.lifecycleMu.Unlock()
			e.dispatch(ctx, e.factory.freeze(e.factory.base(EventSystemStop)), false)
			if err := e.closeResources(ctx); err != nil {
				return err
			}
			e.lifecycleMu.Lock()
			e.systemState = 2
			e.lifecycleCond.Broadcast()
			e.lifecycleMu.Unlock()
			return nil
		case 2:
			e.lifecycleMu.Unlock()
			return nil
		case 3, 4:
			if err := e.waitLifecycleLocked(ctx, func() bool {
				return e.systemState != 3 && e.systemState != 4
			}); err != nil {
				e.lifecycleMu.Unlock()
				return err
			}
		}
	}
}

// waitLifecycleLocked waits while the caller holds lifecycleMu. The context
// callback only wakes the condition; state ownership remains with the waiter.
func (e *Engine) waitLifecycleLocked(ctx context.Context, ready func() bool) error {
	stopWake := context.AfterFunc(ctx, func() {
		e.lifecycleMu.Lock()
		e.lifecycleCond.Broadcast()
		e.lifecycleMu.Unlock()
	})
	defer stopWake()
	for !ready() {
		if err := ctx.Err(); err != nil {
			return err
		}
		e.lifecycleCond.Wait()
	}
	return nil
}

// beginLifecycleEvent linearizes event admission against Shutdown. Calls that
// were admitted before stopping are allowed to finish; later calls are inert.
func (e *Engine) beginLifecycleEvent() bool {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if e.systemState == 2 || e.systemState == 4 {
		return false
	}
	e.lifecycleInFlight++
	return true
}

func (e *Engine) finishLifecycleEvent() {
	e.lifecycleMu.Lock()
	if e.lifecycleInFlight > 0 {
		e.lifecycleInFlight--
	}
	e.lifecycleCond.Broadcast()
	e.lifecycleMu.Unlock()
}

// ShutdownNotices returns diagnostics created after the interactive UI has
// already exited. Process owners can mirror these safe summaries to stderr so
// shutdown-only failures remain observable without exposing Hook payloads.
func (e *Engine) ShutdownNotices() []diagnostics.Diagnostic {
	if e == nil {
		return nil
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	result := make([]diagnostics.Diagnostic, len(e.shutdownNotices))
	for index, item := range e.shutdownNotices {
		result[index] = item.WithAttributes(item.Attributes)
	}
	return result
}

func (e *Engine) closeResources(ctx context.Context) error {
	if e == nil {
		return nil
	}
	started := time.Now()
	if e.async != nil {
		count, err := e.async.closeWithContext(ctx)
		if err != nil {
			return err
		}
		if count > 0 {
			e.addShutdownNotice(count, time.Since(started))
		}
	}
	e.closeOwnedHTTP()
	return nil
}

func (e *Engine) closeOwnedHTTP() {
	if e == nil || e.ownedHTTP == nil {
		return
	}
	e.ownedHTTPClose.Do(e.ownedHTTP.CloseIdleConnections)
}

func (e *Engine) forceCleanup() {
	if e == nil {
		return
	}
	if e.async != nil {
		e.async.beginClose()
		e.async.cancelWorkers()
	}
	e.closeOwnedHTTP()
	e.lifecycleMu.Lock()
	e.systemState = 2
	e.lifecycleCond.Broadcast()
	e.lifecycleMu.Unlock()
}

func (e *Engine) addShutdownNotice(count int, duration time.Duration) {
	summary := e.redactor.Redact(fmt.Sprintf("%d background hook actions were cancelled or unfinished", count))
	item := diagnostics.New(
		DiagnosticShutdownCancelled,
		diagnostics.SeverityWarning,
		safeSummary(summary, e.limits.DiagnosticBytes).Text(),
	).WithAttributes(map[string]string{
		"count":       fmt.Sprintf("%d", count),
		"duration_ms": fmt.Sprintf("%d", duration.Milliseconds()),
		"event":       string(EventSystemStop),
		"stage":       "shutdown",
	})
	safeCollector := diagnostics.NewCollector(diagnostics.CollectorOptions{
		Limit:    1,
		MaxBytes: e.limits.DiagnosticBytes,
		Redactor: e.redactor.Text,
	})
	safeCollector.Add(item)
	if items := safeCollector.List(); len(items) == 1 {
		item = items[0]
	}
	e.publishDiagnostic(item, "shutdown")
	e.lifecycleMu.Lock()
	e.shutdownNotices = append(e.shutdownNotices, item.WithAttributes(item.Attributes))
	e.lifecycleMu.Unlock()
}

func (e *Engine) addCleanupTimeoutNotice(duration time.Duration) {
	item := diagnostics.New(
		"hook_cleanup_timeout",
		diagnostics.SeverityError,
		safeSummary(e.redactor.Redact(errHookCleanupTimeout.Error()), e.limits.DiagnosticBytes).Text(),
	).WithAttributes(map[string]string{
		"duration_ms": fmt.Sprintf("%d", duration.Milliseconds()),
		"event":       string(EventSystemStop),
		"stage":       "cleanup",
	})
	safeCollector := diagnostics.NewCollector(diagnostics.CollectorOptions{
		Limit:    1,
		MaxBytes: e.limits.DiagnosticBytes,
		Redactor: e.redactor.Text,
	})
	safeCollector.Add(item)
	if items := safeCollector.List(); len(items) == 1 {
		item = items[0]
	}
	e.publishDiagnostic(item, "cleanup_timeout")
	e.lifecycleMu.Lock()
	e.shutdownNotices = append(e.shutdownNotices, item.WithAttributes(item.Attributes))
	e.lifecycleMu.Unlock()
}

func (e *Engine) SessionStart(ctx context.Context, id string, state SessionState) {
	if e == nil || id == "" {
		return
	}
	if !e.beginLifecycleEvent() {
		return
	}
	defer e.finishLifecycleEvent()
	e.lifecycleMu.Lock()
	if e.sessions[id] || e.endingSessions[id] {
		e.lifecycleMu.Unlock()
		return
	}
	e.sessions[id] = true
	e.lifecycleMu.Unlock()
	e.prompts.startSession(id)
	c := e.factory.base(EventSessionStart)
	c.Session = &SessionContext{ID: id, State: state}
	e.dispatch(ctx, e.factory.freeze(c), false)
}

func (e *Engine) SessionEnd(ctx context.Context, id string, reason SessionEndReason) {
	if e == nil || id == "" {
		return
	}
	if !e.beginLifecycleEvent() {
		return
	}
	defer e.finishLifecycleEvent()
	e.lifecycleMu.Lock()
	if e.endingSessions[id] {
		e.lifecycleMu.Unlock()
		return
	}
	if !e.sessions[id] {
		e.lifecycleMu.Unlock()
		e.prompts.endSession(id)
		return
	}
	e.endingSessions[id] = true
	e.lifecycleMu.Unlock()
	c := e.factory.base(EventSessionEnd)
	c.Session = &SessionContext{ID: id, EndReason: reason}
	e.dispatch(ctx, e.factory.freeze(c), false)
	e.prompts.endSession(id)
	e.lifecycleMu.Lock()
	delete(e.sessions, id)
	delete(e.endingSessions, id)
	e.lifecycleCond.Broadcast()
	e.lifecycleMu.Unlock()
}

func (e *Engine) BeginTurn(ctx context.Context, sessionID string, kind ExecutionKind, mode HookMode) ExecutionRef {
	if e == nil {
		return ExecutionRef{}
	}
	if !e.beginLifecycleEvent() {
		return ExecutionRef{}
	}
	defer e.finishLifecycleEvent()
	if kind == "" {
		kind = ExecutionMain
	}
	if mode == "" {
		mode = ModeDefault
	}
	ref := ExecutionRef{SessionID: sessionID, ExecutionID: e.factory.idSource(), TurnID: e.factory.idSource(), Kind: kind, Mode: mode}
	e.lifecycleMu.Lock()
	e.executions[ref.ExecutionID] = true
	e.lifecycleMu.Unlock()
	e.prompts.startTurn(ref)
	e.dispatch(ctx, e.factory.freeze(e.factory.executionBase(EventTurnStart, ref)), false)
	return ref
}

func (e *Engine) EndTurn(ctx context.Context, ref ExecutionRef, status TurnStatus, safeError string) {
	if e == nil {
		return
	}
	if !e.beginLifecycleEvent() {
		return
	}
	defer e.finishLifecycleEvent()
	e.lifecycleMu.Lock()
	if e.endingExecutions[ref.ExecutionID] || !e.executions[ref.ExecutionID] {
		e.lifecycleMu.Unlock()
		return
	}
	e.endingExecutions[ref.ExecutionID] = true
	e.lifecycleMu.Unlock()
	c := e.factory.executionBase(EventTurnEnd, ref)
	c.Turn.Status, c.Turn.Error = status, safeError
	e.dispatch(ctx, e.factory.freeze(c), false)
	e.prompts.endTurn(ref)
	e.lifecycleMu.Lock()
	delete(e.executions, ref.ExecutionID)
	delete(e.endingExecutions, ref.ExecutionID)
	e.lifecycleCond.Broadcast()
	e.lifecycleMu.Unlock()
}

func (e *Engine) BeginMessage(ctx context.Context, ref ExecutionRef, role MessageRole, content string) MessageToken {
	if e == nil {
		return MessageToken{}
	}
	if !e.beginLifecycleEvent() {
		return MessageToken{}
	}
	defer e.finishLifecycleEvent()
	c := e.factory.executionBase(EventMessageBefore, ref)
	c.Message = &MessageContext{ID: e.factory.idSource(), Role: role, Content: content}
	event := e.factory.freeze(c)
	e.dispatch(ctx, event, false)
	return MessageToken{ref: ref, event: event}
}

func (e *Engine) EndMessage(ctx context.Context, token MessageToken) {
	if e == nil || token.event == nil || token.event.value.Message == nil {
		return
	}
	if !e.beginLifecycleEvent() {
		return
	}
	defer e.finishLifecycleEvent()
	c := e.factory.executionBase(EventMessageAfter, token.ref)
	message := *token.event.value.Message
	c.Message = &message
	e.dispatch(ctx, e.factory.freeze(c), false)
}

func (e *Engine) BeforeTool(ctx context.Context, ref ExecutionRef, input ToolInput) ToolDecision {
	if e == nil {
		return Continue()
	}
	if !e.beginLifecycleEvent() {
		return Continue()
	}
	defer e.finishLifecycleEvent()
	c := e.factory.executionBase(EventToolBefore, ref)
	// freeze performs the only deep copy after its bounded JSON preflight. Avoid
	// cloning attacker-controlled arguments before that hard boundary.
	c.Tool = &ToolContext{CallID: input.CallID, Name: input.Name, Arguments: input.arguments}
	return e.dispatch(ctx, e.factory.freeze(c), true)
}

func (e *Engine) AfterTool(ctx context.Context, ref ExecutionRef, input ToolInput, output ToolOutput, duration time.Duration) {
	if e == nil {
		return
	}
	if !e.beginLifecycleEvent() {
		return
	}
	defer e.finishLifecycleEvent()
	c := e.factory.executionBase(EventToolAfter, ref)
	result := &ToolResultContext{Content: output.Content.Text()}
	if output.Error != nil {
		result.Error = &ToolResultError{Code: output.Error.Code, Message: output.Error.Message.Text(), Recoverable: output.Error.Recoverable}
	}
	durationMS := duration.Milliseconds()
	c.Tool = &ToolContext{CallID: input.CallID, Name: input.Name, Arguments: input.arguments, Status: output.Status, DurationMS: &durationMS, Result: result}
	e.dispatch(ctx, e.factory.freeze(c), false)
}

func (e *Engine) BeforeCompact(ctx context.Context, binding CompactBinding, input CompactInput) CompactToken {
	if e == nil {
		return CompactToken{}
	}
	if !e.beginLifecycleEvent() {
		return CompactToken{}
	}
	defer e.finishLifecycleEvent()
	c := e.factory.base(EventCompactBefore)
	if binding.SessionID != "" {
		c.Session = &SessionContext{ID: binding.SessionID}
	}
	if binding.Execution != nil {
		execution := *binding.Execution
		c.Execution = &ExecutionContext{ID: execution.ExecutionID, Kind: execution.Kind, Mode: execution.Mode}
		c.Turn = &TurnContext{ID: execution.TurnID}
		if c.Session == nil {
			c.Session = &SessionContext{ID: execution.SessionID}
		}
	}
	c.Compact = &CompactContext{Reason: input.Reason, Before: input.Before}
	event := e.factory.freeze(c)
	e.dispatch(ctx, event, false)
	return CompactToken{binding: binding, event: event}
}

func (e *Engine) AfterCompact(ctx context.Context, token CompactToken, output CompactOutput) {
	if e == nil || token.event == nil || token.event.value.Compact == nil {
		return
	}
	if !e.beginLifecycleEvent() {
		return
	}
	defer e.finishLifecycleEvent()
	before := token.event.value
	c := e.factory.base(EventCompactAfter)
	c.Execution, c.Session, c.Turn = before.Execution, before.Session, before.Turn
	compact := *before.Compact
	compact.Status, compact.After, compact.Error = output.Status, output.After, output.Error
	c.Compact = &compact
	e.dispatch(ctx, e.factory.freeze(c), false)
}

func (e *Engine) AcquirePrompts(ctx context.Context, ref ExecutionRef) (PromptLease, error) {
	if e == nil {
		return noopPromptLease{}, nil
	}
	e.lifecycleMu.Lock()
	stopping := e.systemState == 2 || e.systemState == 4
	e.lifecycleMu.Unlock()
	if stopping {
		return noopPromptLease{}, nil
	}
	return e.prompts.acquire(ctx, ref)
}

func (e *Engine) dispatch(ctx context.Context, event *frozenEvent, decisionEvent bool) ToolDecision {
	decision := Continue()
	for index := range e.rules {
		rule := e.rules[index]
		if rule.Event != event.value.Event {
			continue
		}
		matched, err := rule.condition.matches(event)
		if err != nil {
			e.addDiagnostic(rule, event.value.Event, DiagnosticConditionFailed, "condition", 0, e.redactor.Redact("condition evaluation failed"))
			continue
		}
		if !matched {
			continue
		}
		reserved := false
		if rule.Once {
			reserved = e.once.reserve(rule.Source.Key())
			if !reserved {
				continue
			}
		}
		if rule.Async {
			run, preparation := e.prepareAsync(rule, event)
			if preparation.err != nil {
				if reserved {
					e.once.release(rule.Source.Key())
				}
				e.recordOutcome(rule, event.value.Event, preparation, 0)
				continue
			}
			accepted := e.async != nil && e.async.enqueue(rule.Timeout, run, func(outcome actionOutcome) {
				if reserved {
					if outcome.success {
						e.once.commit(rule.Source.Key())
					} else {
						e.once.release(rule.Source.Key())
					}
				}
				e.recordOutcome(rule, event.value.Event, outcome, outcome.duration)
			})
			if !accepted {
				if reserved {
					e.once.release(rule.Source.Key())
				}
				e.addDiagnostic(rule, event.value.Event, DiagnosticAsyncQueueFull, "enqueue", 0, e.redactor.Redact("background hook queue is full"))
			}
			continue
		}
		started := time.Now()
		outcome := e.runSync(ctx, rule, event)
		duration := time.Since(started)
		if reserved {
			if outcome.success {
				e.once.commit(rule.Source.Key())
			} else {
				e.once.release(rule.Source.Key())
			}
		}
		e.recordOutcome(rule, event.value.Event, outcome, duration)
		if decisionEvent && outcome.success && outcome.decision != nil && outcome.decision.IsDeny() {
			return *outcome.decision
		}
	}
	return decision
}

func (e *Engine) runSync(ctx context.Context, rule Rule, event *frozenEvent) (outcome actionOutcome) {
	defer func() {
		if recover() != nil {
			outcome = actionOutcome{code: DiagnosticActionPanic, stage: "run", err: errActionPanic}
		}
	}()
	if rule.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, rule.Timeout)
		defer cancel()
	}
	return e.execute(ctx, rule, event)
}

func (e *Engine) execute(ctx context.Context, rule Rule, event *frozenEvent) actionOutcome {
	switch rule.action.typeName {
	case ActionCommand:
		payload, ok := event.JSON()
		if !ok {
			return actionOutcome{code: DiagnosticLimitExceeded, stage: "payload", err: fmt.Errorf("event payload exceeds limit")}
		}
		result, err := e.command.Run(ctx, CommandRequest{Command: rule.action.command, ProjectRoot: e.factory.projectRoot, Environment: rule.action.env, EventJSON: payload})
		if err != nil {
			return e.actionError(ctx, DiagnosticCommandFailed, "command", err)
		}
		if rule.action.decision {
			parsed, err := parseDecision(result.Stdout, e.limits, e.redactor)
			if err != nil {
				return actionOutcome{code: DiagnosticDecisionInvalid, stage: "decision", err: err}
			}
			return actionOutcome{success: true, decision: &parsed}
		}
		return actionOutcome{success: true}
	case ActionHTTP:
		payload := []byte(nil)
		if rule.action.http.sendEvent {
			var ok bool
			payload, ok = event.JSON()
			if !ok {
				return actionOutcome{code: DiagnosticLimitExceeded, stage: "payload", err: fmt.Errorf("event payload exceeds limit")}
			}
		}
		result, err := e.http.Run(ctx, HTTPRequest{URL: cloneURL(rule.action.http.url), Method: rule.action.http.method, Headers: rule.action.http.headers.Clone(), SendEvent: rule.action.http.sendEvent, EventJSON: payload})
		if err != nil {
			return e.actionError(ctx, DiagnosticHTTPFailed, "http", err)
		}
		if rule.action.decision {
			parsed, err := parseDecision(result.Body, e.limits, e.redactor)
			if err != nil {
				return actionOutcome{code: DiagnosticDecisionInvalid, stage: "decision", err: err}
			}
			return actionOutcome{success: true, decision: &parsed}
		}
		return actionOutcome{success: true}
	case ActionPrompt:
		content, err := rule.action.template.render(event, e.limits.PromptFragmentBytes, e.redactor)
		if err != nil {
			return actionOutcome{code: DiagnosticTemplateFailed, stage: "template", err: err}
		}
		if err := e.prompts.append(event, rule, rule.action.scope, content); err != nil {
			return actionOutcome{code: DiagnosticPromptScopeUnavailable, stage: "prompt", err: err}
		}
		return actionOutcome{success: true}
	case ActionSubAgent:
		if _, err := rule.action.template.render(event, e.limits.SubAgentInputBytes, e.redactor); err != nil {
			return actionOutcome{code: DiagnosticTemplateFailed, stage: "template", err: err}
		}
		e.addDiagnostic(rule, event.value.Event, DiagnosticSubAgentNotImplemented, "subagent", 0, e.redactor.Redact("subagent hook action is not implemented"))
		return actionOutcome{success: true}
	default:
		return actionOutcome{stage: "action", err: fmt.Errorf("unknown action")}
	}
}

func (e *Engine) prepareAsync(rule Rule, event *frozenEvent) (func(context.Context) actionOutcome, actionOutcome) {
	switch rule.action.typeName {
	case ActionCommand:
		payload, ok := event.JSON()
		if !ok {
			return nil, actionOutcome{code: DiagnosticLimitExceeded, stage: "payload", err: fmt.Errorf("event payload exceeds limit")}
		}
		request := CommandRequest{Command: rule.action.command, ProjectRoot: e.factory.projectRoot, Environment: cloneStringMap(rule.action.env), EventJSON: payload}
		return func(ctx context.Context) actionOutcome {
			result, err := e.command.Run(ctx, request)
			_ = result
			if err != nil {
				return e.actionError(ctx, DiagnosticCommandFailed, "command", err)
			}
			return actionOutcome{success: true}
		}, actionOutcome{}
	case ActionHTTP:
		payload := []byte(nil)
		if rule.action.http.sendEvent {
			var ok bool
			payload, ok = event.JSON()
			if !ok {
				return nil, actionOutcome{code: DiagnosticLimitExceeded, stage: "payload", err: fmt.Errorf("event payload exceeds limit")}
			}
		}
		request := HTTPRequest{URL: cloneURL(rule.action.http.url), Method: rule.action.http.method, Headers: rule.action.http.headers.Clone(), SendEvent: rule.action.http.sendEvent, EventJSON: payload}
		return func(ctx context.Context) actionOutcome {
			_, err := e.http.Run(ctx, request)
			if err != nil {
				return e.actionError(ctx, DiagnosticHTTPFailed, "http", err)
			}
			return actionOutcome{success: true}
		}, actionOutcome{}
	case ActionSubAgent:
		_, err := rule.action.template.render(event, e.limits.SubAgentInputBytes, e.redactor)
		if err != nil {
			return nil, actionOutcome{code: DiagnosticTemplateFailed, stage: "template", err: err}
		}
		eventName := event.value.Event
		return func(context.Context) actionOutcome {
			e.addDiagnostic(rule, eventName, DiagnosticSubAgentNotImplemented, "subagent", 0, e.redactor.Redact("subagent hook action is not implemented"))
			return actionOutcome{success: true}
		}, actionOutcome{}
	default:
		return nil, actionOutcome{stage: "enqueue", err: fmt.Errorf("action cannot be asynchronous")}
	}
}

func (e *Engine) actionError(ctx context.Context, code, stage string, err error) actionOutcome {
	if ctx.Err() != nil {
		return actionOutcome{code: DiagnosticActionTimeout, stage: stage, err: ctx.Err()}
	}
	return actionOutcome{code: code, stage: stage, err: err}
}
func (e *Engine) recordOutcome(rule Rule, event Event, outcome actionOutcome, duration time.Duration) {
	if outcome.err == nil {
		return
	}
	code := outcome.code
	if code == "" {
		code = DiagnosticActionPanic
	}
	e.addDiagnostic(rule, event, code, outcome.stage, duration, e.redactor.Redact("hook action failed"))
}
func (e *Engine) addDiagnostic(rule Rule, event Event, code, stage string, duration time.Duration, summary redact.SafeText) {
	safeRule := diagnosticRule{
		source:           e.redactor.Redact(rule.Source.Path),
		ordinal:          rule.Source.Ordinal,
		effectiveOrdinal: rule.Source.EffectiveOrdinal,
		action:           rule.action.typeName,
	}
	e.publishDiagnostic(hookDiagnostic(code, safeRule, event, stage, duration, summary, e.limits), stage)
}

func (e *Engine) publishDiagnostic(item diagnostics.Diagnostic, hint string) {
	if e == nil {
		return
	}
	if e.diagnostics != nil {
		e.diagnostics.Add(diagnostics.SanitizeInput{
			Code: item.Code, Source: item.Source, Hint: hint,
			Severity: item.Severity, Err: errors.New(item.Message),
		})
	}
	if e.legacyDiagnostics != nil {
		e.legacyDiagnostics.Add(item)
	}
}
func cloneStringMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
