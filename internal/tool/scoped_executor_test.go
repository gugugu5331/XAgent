package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/permission"
)

func TestScopedExecutorRejectsInvalidTaskBindings(t *testing.T) {
	base, capabilities, cache, _, _ := newScopedExecutorTestRuntime(t, "TaskTool", []string{"TaskTool"})
	scope := newScopedExecutorTestTaskScope(t, "task-a")

	if _, err := NewScopedExecutor(nil, scope.Verifier, scope.ScopeID, capabilities, cache); err == nil {
		t.Fatal("nil base executor was accepted")
	}
	if _, err := NewScopedExecutor(base, nil, scope.ScopeID, capabilities, cache); err == nil {
		t.Fatal("nil task verifier was accepted")
	}
	if _, err := NewScopedExecutor(base, scope.Verifier, "task-b", capabilities, cache); err == nil {
		t.Fatal("verifier from another task scope was accepted")
	}
	legacyAuthority, err := permission.NewTicketAuthority()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewScopedExecutor(base, legacyAuthority, scope.ScopeID, capabilities, cache); err == nil {
		t.Fatal("legacy unscoped verifier was accepted")
	}
	if _, err := NewScopedExecutor(base, scope.Verifier, scope.ScopeID, nil, cache); err == nil {
		t.Fatal("nil capability source was accepted")
	}
	if _, err := NewScopedExecutor(base, scope.Verifier, scope.ScopeID, capabilities, nil); err == nil {
		t.Fatal("nil task read cache was accepted")
	}

	otherFactory := newReadCacheTestFactory(t)
	otherCache, err := NewReadCache(scopedExecutorTestCacheLimits(), otherFactory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewScopedExecutor(base, scope.Verifier, scope.ScopeID, capabilities, otherCache); err == nil {
		t.Fatal("read cache with a different result factory was accepted")
	}

	legacyRegistry := newEmptyRegistry()
	legacyTool := &scopedExecutorTestTool{name: "Legacy", factory: base.resultFactory}
	if err := legacyRegistry.Register(legacyTool); err != nil {
		t.Fatal(err)
	}
	if err := legacyRegistry.Seal(); err != nil {
		t.Fatal(err)
	}
	legacyBase := NewExecutor(legacyRegistry, t.TempDir(), time.Second, 1024)
	if _, err := NewScopedExecutor(legacyBase, scope.Verifier, scope.ScopeID, capabilities, cache); err == nil {
		t.Fatal("legacy executor without the safe result boundary was accepted")
	}

	_, unrelatedCapabilities, _, _, _ := newScopedExecutorTestRuntime(t, "TaskTool", []string{"TaskTool"})
	if _, err := NewScopedExecutor(base, scope.Verifier, scope.ScopeID, unrelatedCapabilities, cache); err == nil {
		t.Fatal("capability view from another registry lineage was accepted")
	}

	if _, err := NewScopedExecutor(base, scope.Verifier, scope.ScopeID, capabilities, cache); err != nil {
		t.Fatalf("valid scoped executor dependencies were rejected: %v", err)
	}
}

func TestScopedExecutorConsumesTaskTicketExactlyOnce(t *testing.T) {
	base, capabilities, cache, fixture, _ := newScopedExecutorTestRuntime(t, "TaskTool", []string{"TaskTool"})
	scope := newScopedExecutorTestTaskScope(t, "task-once")
	executor, err := NewScopedExecutor(base, scope.Verifier, scope.ScopeID, capabilities, cache)
	if err != nil {
		t.Fatal(err)
	}
	call := Call{ID: "call-once", Name: fixture.Name(), ArgumentsJSON: `{}`}
	validated, ticket := scopedExecutorTestAuthorization(t, base, scope, call)

	first := executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket)
	if first.Status != StatusSuccess || fixture.calls.Load() != 1 {
		t.Fatalf("first task execution failed: result=%#v calls=%d", first, fixture.calls.Load())
	}
	second := executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket)
	if second.Status != StatusDenied || second.Error == nil || second.Error.Code != ErrPermissionDenied {
		t.Fatalf("replayed task ticket was not denied: %#v", second)
	}
	if fixture.calls.Load() != 1 {
		t.Fatalf("single-use task ticket executed tool %d times", fixture.calls.Load())
	}
}

func TestScopedExecutorRejectsValidatedCallFromAnotherExecutor(t *testing.T) {
	base, capabilities, cache, fixture, root := newScopedExecutorTestRuntime(t, "Foreign", nil)
	scope := newScopedExecutorTestTaskScope(t, "task-provenance")
	executor, err := NewScopedExecutor(base, scope.Verifier, scope.ScopeID, capabilities, cache)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := NewExecutorWithResultFactory(base.Registry, root, time.Second, 1024, base.resultFactory)
	if err != nil {
		t.Fatal(err)
	}
	call := Call{ID: "foreign-call", Name: fixture.Name(), ArgumentsJSON: `{}`}
	validated, ticket := scopedExecutorTestAuthorization(t, foreign, scope, call)
	if result := executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket); result.Status != StatusDenied {
		t.Fatalf("foreign executor call was accepted: %#v", result)
	}
	if fixture.calls.Load() != 0 {
		t.Fatal("foreign executor call reached the task target")
	}

	// Provenance rejection happens before ticket consumption, so an equivalent
	// call prepared by the task executor can still use the issued identity.
	local, err := base.PrepareCall(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if result := executor.ExecuteValidatedAuthorized(context.Background(), local, ticket); result.Status != StatusSuccess {
		t.Fatalf("provenance rejection consumed the ticket: %#v", result)
	}
}

func TestScopedExecutorRejectsCrossTaskTicket(t *testing.T) {
	base, capabilities, cache, fixture, _ := newScopedExecutorTestRuntime(t, "TaskTool", []string{"TaskTool"})
	first := newScopedExecutorTestTaskScope(t, "task-first")
	second := newScopedExecutorTestTaskScope(t, "task-second")
	executor, err := NewScopedExecutor(base, second.Verifier, second.ScopeID, capabilities, cache)
	if err != nil {
		t.Fatal(err)
	}
	call := Call{ID: "same-call", Name: fixture.Name(), ArgumentsJSON: `{}`}
	validated, firstTicket := scopedExecutorTestAuthorization(t, base, first, call)

	result := executor.ExecuteValidatedAuthorized(context.Background(), validated, firstTicket)
	if result.Status != StatusDenied || result.Error == nil || result.Error.Code != ErrPermissionDenied {
		t.Fatalf("cross-task ticket was not denied: %#v", result)
	}
	if fixture.calls.Load() != 0 {
		t.Fatalf("cross-task ticket executed tool %d times", fixture.calls.Load())
	}
	identity, err := base.CallIdentity(validated)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Verifier.VerifyAndConsume(firstTicket, call.ID, identity); err != nil {
		t.Fatalf("cross-task attempt consumed the source task ticket: %v", err)
	}
}

func TestScopedExecutorRejectsCapabilitySourceExpansion(t *testing.T) {
	base, switcher, cache, fixture, _ := newScopedExecutorTestRuntime(t, "ForegroundOnly", []string{})
	source := &scopedExecutorSequenceCapabilitySource{views: []CapabilitySet{
		switcher.background.set.Clone(),
		switcher.foreground.set.Clone(),
	}}
	scope := newScopedExecutorTestTaskScope(t, "task-no-expansion")
	executor, err := NewScopedExecutor(base, scope.Verifier, scope.ScopeID, source, cache)
	if err != nil {
		t.Fatal(err)
	}
	call := Call{ID: "expanded-call", Name: fixture.Name(), ArgumentsJSON: `{}`}
	validated, ticket := scopedExecutorTestAuthorization(t, base, scope, call)

	result := executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket)
	if result.Status != StatusDenied || result.Error == nil || result.Error.Code != ErrToolFiltered || result.Data["filter_reason"] != string(FilterUnknown) {
		t.Fatalf("expanded capability source did not fail closed: %#v", result)
	}
	if fixture.calls.Load() != 0 {
		t.Fatalf("expanded capability source executed tool %d times", fixture.calls.Load())
	}
	identity, err := base.CallIdentity(validated)
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Verifier.VerifyAndConsume(ticket, call.ID, identity); err != nil {
		t.Fatalf("capability rejection consumed the task ticket: %v", err)
	}
}

func TestDetachRaceWaitingConfirmationRechecksCurrentCapabilities(t *testing.T) {
	base, switcher, cache, fixture, _ := newScopedExecutorTestRuntime(t, "ForegroundOnly", []string{})
	scope := newScopedExecutorTestTaskScope(t, "task-waiting-confirmation")
	blockingSource := &scopedExecutorBlockingCapabilitySource{
		delegate: switcher,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	executor, err := NewScopedExecutor(base, scope.Verifier, scope.ScopeID, blockingSource, cache)
	if err != nil {
		t.Fatal(err)
	}
	call := Call{ID: "confirmed-call", Name: fixture.Name(), ArgumentsJSON: `{}`}
	validated, ticket := scopedExecutorTestAuthorization(t, base, scope, call)

	resultCh := make(chan Result, 1)
	go func() {
		resultCh <- executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket)
	}()
	waitForExecutorSignal(t, blockingSource.entered, "final capability recheck")
	if changed, _ := switcher.MoveToBackground(); !changed {
		t.Fatal("task did not detach while confirmation was waiting")
	}
	close(blockingSource.release)

	result := waitForScopedExecutorResult(t, resultCh)
	if result.Status != StatusDenied || result.Error == nil || result.Error.Code != ErrToolFiltered {
		t.Fatalf("detached tool was not rejected: %#v", result)
	}
	if got := result.Data["filter_reason"]; got != string(FilterBackgroundDeny) {
		t.Fatalf("filtered rejection reason=%#v, want %q", got, FilterBackgroundDeny)
	}
	if fixture.calls.Load() != 0 {
		t.Fatalf("waiting-confirmation detach race executed tool %d times", fixture.calls.Load())
	}
}

func TestDetachRaceAllowsCallAlreadyPastStartBoundaryToFinish(t *testing.T) {
	base, switcher, cache, fixture, _ := newScopedExecutorTestRuntime(t, "ForegroundOnly", []string{})
	fixture.started = make(chan struct{})
	fixture.release = make(chan struct{})
	scope := newScopedExecutorTestTaskScope(t, "task-running")
	executor, err := NewScopedExecutor(base, scope.Verifier, scope.ScopeID, switcher, cache)
	if err != nil {
		t.Fatal(err)
	}
	call := Call{ID: "running-call", Name: fixture.Name(), ArgumentsJSON: `{}`}
	validated, ticket := scopedExecutorTestAuthorization(t, base, scope, call)

	resultCh := make(chan Result, 1)
	go func() {
		resultCh <- executor.ExecuteValidatedAuthorized(context.Background(), validated, ticket)
	}()
	waitForExecutorSignal(t, fixture.started, "tool start boundary")
	if changed, _ := switcher.MoveToBackground(); !changed {
		t.Fatal("task did not detach after the tool started")
	}
	close(fixture.release)

	result := waitForScopedExecutorResult(t, resultCh)
	if result.Status != StatusSuccess || fixture.calls.Load() != 1 {
		t.Fatalf("already-running tool did not finish exactly once: result=%#v calls=%d", result, fixture.calls.Load())
	}
}

func TestScopedExecutorAuthorizedReadCacheStartsAfterTicketConsumption(t *testing.T) {
	base, _, _, fixture, root := newScopedExecutorTestRuntime(t, "Read", []string{"Read"})
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("cache boundary"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope := newScopedExecutorTestTaskScope(t, "task-cache")
	call := Call{ID: "read-hit", Name: "Read", ArgumentsJSON: `{"path":"input.txt"}`}
	validated, ticket := scopedExecutorTestAuthorization(t, base, scope, call)
	verifier := &scopedExecutorObservedVerifier{delegate: scope.Verifier, scopeID: scope.ScopeID}
	cache := &scopedExecutorObservedCache{}
	cache.lookup = func(_ context.Context, current ValidatedCall) (Result, bool, error) {
		if !verifier.consumed.Load() {
			t.Fatal("read cache lookup ran before task ticket consumption")
		}
		cached, err := base.resultFactory.Build(ResultFactoryInput{
			CallID:  current.Call.ID,
			Name:    current.Call.Name,
			State:   Completed,
			Status:  StatusSuccess,
			Summary: "cached",
			Preview: "cached",
		})
		if err != nil {
			t.Fatal(err)
		}
		return cached, true, nil
	}

	result := base.ExecuteValidatedAuthorizedWithVerifier(context.Background(), validated, ticket, verifier, cache)
	if result.Status != StatusSuccess || result.CallID != call.ID || result.Summary != "cached" {
		t.Fatalf("authorized cache hit was not returned: %#v", result)
	}
	if cache.lookupCalls.Load() != 1 || cache.storeCalls.Load() != 0 || fixture.calls.Load() != 0 {
		t.Fatalf("cache hit crossed the wrong boundary: lookups=%d stores=%d executions=%d", cache.lookupCalls.Load(), cache.storeCalls.Load(), fixture.calls.Load())
	}

	other := newScopedExecutorTestTaskScope(t, "task-other")
	_, otherTicket := scopedExecutorTestAuthorization(t, base, other, call)
	failedVerifier := &scopedExecutorObservedVerifier{delegate: scope.Verifier, scopeID: scope.ScopeID}
	beforeLookups := cache.lookupCalls.Load()
	denied := base.ExecuteValidatedAuthorizedWithVerifier(context.Background(), validated, otherTicket, failedVerifier, cache)
	if denied.Status != StatusDenied || fixture.calls.Load() != 0 {
		t.Fatalf("invalid task ticket crossed the start boundary: %#v", denied)
	}
	if failedVerifier.consumed.Load() || cache.lookupCalls.Load() != beforeLookups {
		t.Fatal("read cache ran for a ticket that did not pass task verification")
	}

	errorCall := call
	errorCall.ID = "read-cache-error"
	errorValidated, errorTicket := scopedExecutorTestAuthorization(t, base, scope, errorCall)
	errorVerifier := &scopedExecutorObservedVerifier{delegate: scope.Verifier, scopeID: scope.ScopeID}
	errorCache := &scopedExecutorObservedCache{
		lookup: func(context.Context, ValidatedCall) (Result, bool, error) {
			if !errorVerifier.consumed.Load() {
				t.Fatal("cache error occurred before authorization start boundary")
			}
			return Result{}, false, errors.New("cache lookup unavailable")
		},
		store: func(context.Context, ValidatedCall, Result) error {
			return errors.New("cache store unavailable")
		},
	}
	errorResult := base.ExecuteValidatedAuthorizedWithVerifier(context.Background(), errorValidated, errorTicket, errorVerifier, errorCache)
	if errorResult.Status != StatusSuccess || fixture.calls.Load() != 1 {
		t.Fatalf("cache failure bypassed ordinary execution: result=%#v calls=%d", errorResult, fixture.calls.Load())
	}
	if errorCache.lookupCalls.Load() != 1 || errorCache.storeCalls.Load() != 1 {
		t.Fatalf("cache miss path was incomplete: lookups=%d stores=%d", errorCache.lookupCalls.Load(), errorCache.storeCalls.Load())
	}
	if errorResult.Data["read_cache_lookup"] != "error" || errorResult.Data["read_cache_store"] != "error" {
		t.Fatalf("cache errors were not observable safe misses: %#v", errorResult.Data)
	}
}

func TestScopedExecutorReadCacheHitRematerializesCurrentCall(t *testing.T) {
	base, capabilities, cache, fixture, root := newScopedExecutorTestRuntime(t, "Read", []string{"Read"})
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("cached dependency"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope := newScopedExecutorTestTaskScope(t, "task-read-cache")
	executor, err := NewScopedExecutor(base, scope.Verifier, scope.ScopeID, capabilities, cache)
	if err != nil {
		t.Fatal(err)
	}
	firstCall := Call{ID: "read-first", Name: "Read", ArgumentsJSON: `{"path":"input.txt"}`}
	secondCall := firstCall
	secondCall.ID = "read-second"
	firstValidated, firstTicket := scopedExecutorTestAuthorization(t, base, scope, firstCall)
	secondValidated, secondTicket := scopedExecutorTestAuthorization(t, base, scope, secondCall)

	first := executor.ExecuteValidatedAuthorized(context.Background(), firstValidated, firstTicket)
	second := executor.ExecuteValidatedAuthorized(context.Background(), secondValidated, secondTicket)
	if first.Status != StatusSuccess || second.Status != StatusSuccess {
		t.Fatalf("scoped read cache calls failed: first=%#v second=%#v", first, second)
	}
	if fixture.calls.Load() != 1 {
		t.Fatalf("cache-equivalent task reads executed tool %d times, want 1", fixture.calls.Load())
	}
	if second.CallID != secondCall.ID || second.CallID == first.CallID || second.Name != secondCall.Name {
		t.Fatalf("cache hit reused an old invocation identity: first=%#v second=%#v", first, second)
	}
}

func TestScopedExecutorAuthorizedCacheIgnoresNonReadTools(t *testing.T) {
	base, _, _, fixture, _ := newScopedExecutorTestRuntime(t, "TaskTool", []string{"TaskTool"})
	scope := newScopedExecutorTestTaskScope(t, "task-no-cache")
	call := Call{ID: "ordinary-call", Name: fixture.Name(), ArgumentsJSON: `{}`}
	validated, ticket := scopedExecutorTestAuthorization(t, base, scope, call)
	cache := &scopedExecutorObservedCache{}

	result := base.ExecuteValidatedAuthorizedWithVerifier(context.Background(), validated, ticket, scope.Verifier, cache)
	if result.Status != StatusSuccess || fixture.calls.Load() != 1 {
		t.Fatalf("ordinary execution failed: %#v", result)
	}
	if cache.lookupCalls.Load() != 0 || cache.storeCalls.Load() != 0 {
		t.Fatalf("non-read tool touched the read cache: lookups=%d stores=%d", cache.lookupCalls.Load(), cache.storeCalls.Load())
	}
}

func newScopedExecutorTestRuntime(t *testing.T, name string, backgroundAllowed []string) (*Executor, *CapabilitySwitch, *ReadCache, *scopedExecutorTestTool, string) {
	t.Helper()
	root := t.TempDir()
	factory := newReadCacheTestFactory(t)
	fixture := &scopedExecutorTestTool{name: name, factory: factory}
	registry := NewSafeCandidateRegistry()
	if err := registry.RegisterWithOptions(fixture, RegistrationOptions{Policy: ExecutionPolicy{ReadOnly: true, SideEffectFree: true, ConcurrentSafe: true}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	foreground, background, err := BuildCapabilityViews(registry, nil, BackgroundPolicy{AllowedNames: backgroundAllowed}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := NewCapabilitySwitch(foreground, background)
	if err != nil {
		t.Fatal(err)
	}
	base, err := NewExecutorWithResultFactory(registry, root, time.Second, 1024, factory)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewReadCache(scopedExecutorTestCacheLimits(), factory)
	if err != nil {
		t.Fatal(err)
	}
	return base, capabilities, cache, fixture, root
}

func scopedExecutorTestCacheLimits() ReadCacheLimits {
	return ReadCacheLimits{MaxEntries: 4, MaxBytes: 64 << 10, MaxValueBytes: 16 << 10, MaxDependenciesPerEntry: 32}
}

func newScopedExecutorTestTaskScope(t *testing.T, taskID string) permission.TaskScope {
	t.Helper()
	scope, err := (&permission.Authorizer{}).NewTaskScope(permission.TaskScopeOptions{ScopeID: taskID, Mode: permission.ModeDefault})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func scopedExecutorTestAuthorization(t *testing.T, base *Executor, scope permission.TaskScope, call Call) (ValidatedCall, permission.ExecutionTicket) {
	t.Helper()
	validated, err := base.PrepareCall(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := base.CallIdentity(validated)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := scope.Issuer.Issue(call.ID, identity)
	if err != nil {
		t.Fatal(err)
	}
	return validated, ticket
}

func waitForScopedExecutorResult(t *testing.T, resultCh <-chan Result) Result {
	t.Helper()
	select {
	case result := <-resultCh:
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped executor result")
		return Result{}
	}
}

type scopedExecutorTestTool struct {
	name        string
	factory     *ResultFactory
	calls       atomic.Int32
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
}

func (t *scopedExecutorTestTool) Name() string { return t.name }

func (*scopedExecutorTestTool) Description() string { return "scoped executor boundary fixture" }

func (*scopedExecutorTestTool) Schema() Schema { return ObjectSchema(nil, nil) }

func (*scopedExecutorTestTool) Risk() Risk { return RiskDangerous }

func (t *scopedExecutorTestTool) UsesSafeResultBoundary() bool { return t != nil && t.factory != nil }

func (t *scopedExecutorTestTool) Execute(_ context.Context, input Input) Result {
	t.calls.Add(1)
	if t.started != nil {
		t.startedOnce.Do(func() { close(t.started) })
	}
	if t.release != nil {
		<-t.release
	}
	result, _ := t.factory.Build(ResultFactoryInput{
		CallID:  input.CallID,
		Name:    input.Name,
		State:   Completed,
		Status:  StatusSuccess,
		Summary: "executed",
		Preview: "executed",
	})
	return result
}

type scopedExecutorBlockingCapabilitySource struct {
	delegate CapabilitySource
	entered  chan struct{}
	release  chan struct{}
	calls    atomic.Int32
	once     sync.Once
}

type scopedExecutorSequenceCapabilitySource struct {
	views []CapabilitySet
	calls atomic.Int32
}

func (s *scopedExecutorSequenceCapabilitySource) Current() CapabilitySet {
	index := int(s.calls.Add(1)) - 1
	if index >= len(s.views) {
		index = len(s.views) - 1
	}
	return s.views[index].Clone()
}

func (s *scopedExecutorBlockingCapabilitySource) Current() CapabilitySet {
	if s.calls.Add(1) > 1 {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.delegate.Current()
}

type scopedExecutorObservedVerifier struct {
	delegate permission.TicketVerifier
	scopeID  string
	consumed atomic.Bool
}

func (v *scopedExecutorObservedVerifier) ScopeID() string { return v.scopeID }

func (v *scopedExecutorObservedVerifier) VerifyAndConsume(ticket permission.ExecutionTicket, callID string, identity permission.CallIdentity) error {
	err := v.delegate.VerifyAndConsume(ticket, callID, identity)
	if err == nil {
		v.consumed.Store(true)
	}
	return err
}

type scopedExecutorObservedCache struct {
	lookupCalls atomic.Int32
	storeCalls  atomic.Int32
	lookup      func(context.Context, ValidatedCall) (Result, bool, error)
	store       func(context.Context, ValidatedCall, Result) error
}

func (c *scopedExecutorObservedCache) Lookup(ctx context.Context, call ValidatedCall) (Result, bool, error) {
	c.lookupCalls.Add(1)
	if c.lookup == nil {
		return Result{}, false, nil
	}
	return c.lookup(ctx, call)
}

func (c *scopedExecutorObservedCache) Store(ctx context.Context, call ValidatedCall, result Result) error {
	c.storeCalls.Add(1)
	if c.store == nil {
		return nil
	}
	return c.store(ctx, call, result)
}
