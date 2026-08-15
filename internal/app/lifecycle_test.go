package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
)

// The T4.26 tests use the narrow lifecycle seams rather than constructing a
// provider/orchestrator graph.  This keeps the assertions focused on App's
// ownership protocol: the waiter stands in for Orchestrator.WaitIdle and
// records the point at which request-owned streams/tools are known to be
// closed, while the Store and Hook fixtures expose only their lifecycle
// methods.

type t426Sequence struct {
	mu     sync.Mutex
	items  []string
	byName map[string]chan struct{}
}

func newT426Sequence(names ...string) *t426Sequence {
	sequence := &t426Sequence{byName: make(map[string]chan struct{}, len(names))}
	for _, name := range names {
		sequence.byName[name] = make(chan struct{})
	}
	return sequence
}

func (sequence *t426Sequence) add(name string) {
	if sequence == nil {
		return
	}
	sequence.mu.Lock()
	sequence.items = append(sequence.items, name)
	ready := sequence.byName[name]
	if ready != nil {
		select {
		case <-ready:
		default:
			close(ready)
		}
	}
	sequence.mu.Unlock()
}

func (sequence *t426Sequence) snapshot() []string {
	if sequence == nil {
		return nil
	}
	sequence.mu.Lock()
	defer sequence.mu.Unlock()
	return append([]string(nil), sequence.items...)
}

func (sequence *t426Sequence) count(name string) int {
	count := 0
	for _, item := range sequence.snapshot() {
		if item == name {
			count++
		}
	}
	return count
}

func (sequence *t426Sequence) wait(t *testing.T, name string) {
	t.Helper()
	sequence.mu.Lock()
	ready := sequence.byName[name]
	sequence.mu.Unlock()
	if ready == nil {
		t.Fatalf("unknown lifecycle sequence marker %q", name)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for lifecycle marker %q", name)
	}
}

type t426Waiter struct {
	sequence    *t426Sequence
	release     <-chan struct{}
	ignoreCtx   bool
	err         error
	waitCalls   int
	mu          sync.Mutex
	waitStarted chan struct{}
}

func (waiter *t426Waiter) WaitIdle(ctx context.Context) error {
	if waiter == nil {
		return nil
	}
	waiter.mu.Lock()
	waiter.waitCalls++
	started := waiter.waitStarted
	waiter.mu.Unlock()
	if started != nil {
		select {
		case <-started:
		default:
			close(started)
		}
	}
	if waiter.sequence != nil {
		waiter.sequence.add("wait_idle")
	}
	if waiter.release != nil {
		if waiter.ignoreCtx {
			<-waiter.release
		} else {
			select {
			case <-waiter.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if waiter.sequence != nil {
		waiter.sequence.add("resources_closed")
	}
	return waiter.err
}

func (waiter *t426Waiter) Calls() int {
	if waiter == nil {
		return 0
	}
	waiter.mu.Lock()
	defer waiter.mu.Unlock()
	return waiter.waitCalls
}

type t426Store struct {
	sequence  *t426Sequence
	mu        sync.Mutex
	saveCalls int
	last      *conversation.Conversation
	saveErr   error
}

func (store *t426Store) Save(_ context.Context, value *conversation.Conversation) (conversation.SaveResult, error) {
	if store == nil {
		return conversation.SaveResult{}, nil
	}
	store.mu.Lock()
	store.saveCalls++
	store.last = value
	store.mu.Unlock()
	if store.sequence != nil {
		id := "<nil>"
		if value != nil {
			id = value.ID
		}
		store.sequence.add("save:" + id)
	}
	return conversation.SaveResult{Kind: conversation.SaveNoop}, store.saveErr
}

func (store *t426Store) Calls() (int, *conversation.Conversation) {
	if store == nil {
		return 0, nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.saveCalls, store.last
}

type t426Hooks struct {
	sequence *t426Sequence
	mu       sync.Mutex
	ends     int
	ids      []string
	reasons  []hook.SessionEndReason
}

func (hooks *t426Hooks) SessionEnd(_ context.Context, id string, reason hook.SessionEndReason) {
	if hooks == nil {
		return
	}
	hooks.mu.Lock()
	hooks.ends++
	hooks.ids = append(hooks.ids, id)
	hooks.reasons = append(hooks.reasons, reason)
	hooks.mu.Unlock()
	if hooks.sequence != nil {
		hooks.sequence.add("session_end:" + id + ":" + string(reason))
	}
}

func (hooks *t426Hooks) Ends() int {
	if hooks == nil {
		return 0
	}
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	return hooks.ends
}

type t426DiagnosticSink struct {
	mu    sync.Mutex
	items []diagnostics.SanitizeInput
}

func (sink *t426DiagnosticSink) Add(item diagnostics.SanitizeInput) {
	if sink == nil {
		return
	}
	sink.mu.Lock()
	sink.items = append(sink.items, item)
	sink.mu.Unlock()
}

func (sink *t426DiagnosticSink) snapshot() []diagnostics.SanitizeInput {
	if sink == nil {
		return nil
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]diagnostics.SanitizeInput(nil), sink.items...)
}

func (sink *t426DiagnosticSink) countCode(code string) int {
	count := 0
	for _, item := range sink.snapshot() {
		if item.Code == code {
			count++
		}
	}
	return count
}

func t426Model(
	sequence *t426Sequence,
	active *conversation.Conversation,
	waiter navigationWaiter,
	store navigationSaver,
	hooks interface {
		SessionEnd(context.Context, string, hook.SessionEndReason)
	},
	options RuntimeOptions,
) Model {
	model := Model{
		runtimeOptions:  options,
		conversation:    active,
		lifecycle:       newLifecycleState(),
		lifecycleWaiter: waiter,
	}
	if active != nil {
		model.request = &RequestSession{}
		model.lifecycle.begin(model.request)
	}
	model.lifecycle.snapshot.store = store
	model.lifecycle.snapshot.waiter = waiter
	model.lifecycle.snapshot.hooks = hooks
	model.lifecycle.snapshot.conversation = active
	if sequence != nil {
		// Keep the argument explicit so a future fixture can attach more
		// markers without changing the lifecycle construction path.
		_ = sequence
	}
	return model
}

func assertT426Order(t *testing.T, got []string, want ...string) {
	t.Helper()
	position := 0
	for _, item := range got {
		if position < len(want) && item == want[position] {
			position++
		}
	}
	if position != len(want) {
		t.Fatalf("lifecycle order = %v, want subsequence %v", got, want)
	}
}

// TestAppCloseOrderAndSave locks the first-close sequence and proves that a
// shared lifecycle (including copied Bubble Tea Models) performs persistence
// and SessionEnd exactly once for an active conversation.
func TestAppCloseOrderAndSave(t *testing.T) {
	sequence := newT426Sequence("wait_idle", "resources_closed", "save:active", "session_end:active:exit")
	store := &t426Store{sequence: sequence}
	hooks := &t426Hooks{sequence: sequence}
	waiter := &t426Waiter{sequence: sequence}
	conversationValue := conversation.NewConversation("active", time.Unix(1, 0))
	cancelled := make(chan struct{})
	model := t426Model(sequence, conversationValue, waiter, store, hooks, RuntimeOptions{CleanupTimeout: time.Second})
	model.request.Cancel = func() {
		sequence.add("cancel")
		select {
		case <-cancelled:
		default:
			close(cancelled)
		}
	}
	model.lifecycle.replace(model.request)

	first, second := model, model
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	group.Add(2)
	for _, copyOfModel := range []*Model{&first, &second} {
		copyOfModel := copyOfModel
		go func() {
			defer group.Done()
			<-start
			results <- copyOfModel.Close(context.Background())
		}()
	}
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("copied Model.Close: %v", err)
		}
	}

	got := sequence.snapshot()
	assertT426Order(t, got, "cancel", "wait_idle", "resources_closed", "save:active", "session_end:active:exit")
	for _, marker := range []string{"cancel", "wait_idle", "resources_closed", "save:active", "session_end:active:exit"} {
		if count := sequence.count(marker); count != 1 {
			t.Fatalf("%s count = %d, want one (events=%v)", marker, count, got)
		}
	}
	if calls, saved := store.Calls(); calls != 1 || saved != conversationValue {
		t.Fatalf("Save calls/value = %d/%p, want one active conversation %p", calls, saved, conversationValue)
	}
	if hooks.Ends() != 1 {
		t.Fatalf("SessionEnd calls = %d, want one", hooks.Ends())
	}
	if model.lifecycle.isAccepting() {
		t.Fatal("Close left lifecycle accepting new intents")
	}
	if model.lifecycle.begin(&RequestSession{}) {
		t.Fatal("lifecycle admitted a request after Close")
	}
	if calls := waiter.Calls(); calls != 1 {
		t.Fatalf("WaitIdle calls = %d, want one across copied Models", calls)
	}
}

// TestAppCloseWithoutActiveConversationSkipsSaveAndSessionEnd protects the
// cold-start/list state from Save(nil) and fabricated exit notifications.
func TestAppCloseWithoutActiveConversationSkipsSaveAndSessionEnd(t *testing.T) {
	sequence := newT426Sequence("wait_idle")
	store := &t426Store{sequence: sequence}
	hooks := &t426Hooks{sequence: sequence}
	waiter := &t426Waiter{sequence: sequence}
	model := t426Model(sequence, nil, waiter, store, hooks, RuntimeOptions{CleanupTimeout: time.Second})
	if err := model.Close(context.Background()); err != nil {
		t.Fatalf("cold-start Model.Close: %v", err)
	}
	if calls, saved := store.Calls(); calls != 0 || saved != nil {
		t.Fatalf("cold-start Save calls/value = %d/%p, want 0/nil", calls, saved)
	}
	if hooks.Ends() != 0 {
		t.Fatalf("cold-start SessionEnd calls = %d, want 0", hooks.Ends())
	}
	if got := sequence.snapshot(); len(got) != 2 || got[0] != "wait_idle" || got[1] != "resources_closed" {
		t.Fatalf("cold-start lifecycle events = %v, want waiter/resource confirmation only", got)
	}
}

// TestAppCloseContinuesAfterWaiterTimeout distinguishes the caller's waiting
// context from the independent cleanup context.  The first Close returns the
// caller deadline, while the same worker continues to Save and SessionEnd and
// a later Close observes the stable final result.
func TestAppCloseContinuesAfterWaiterTimeout(t *testing.T) {
	sequence := newT426Sequence("wait_idle", "resources_closed", "save:active", "session_end:active:exit")
	release := make(chan struct{})
	waiter := &t426Waiter{sequence: sequence, release: release, ignoreCtx: true}
	store := &t426Store{sequence: sequence}
	hooks := &t426Hooks{sequence: sequence}
	conv := conversation.NewConversation("active", time.Unix(2, 0))
	model := t426Model(sequence, conv, waiter, store, hooks, RuntimeOptions{CleanupTimeout: time.Second})
	model.request.Cancel = func() { sequence.add("cancel") }
	model.lifecycle.replace(model.request)

	caller, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	first := model.Close(caller)
	if !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("first Close = %v, want caller deadline", first)
	}
	if calls, _ := store.Calls(); calls != 0 {
		t.Fatalf("Save started before WaitIdle release: %d calls", calls)
	}

	close(release)
	sequence.wait(t, "resources_closed")
	sequence.wait(t, "save:active")
	sequence.wait(t, "session_end:active:exit")
	second := model.Close(context.Background())
	if second != nil {
		t.Fatalf("final Close = %v, want nil", second)
	}
	if calls, saved := store.Calls(); calls != 1 || saved != conv {
		t.Fatalf("continued cleanup Save calls/value = %d/%p, want one/%p", calls, saved, conv)
	}
	if hooks.Ends() != 1 {
		t.Fatalf("continued cleanup SessionEnd calls = %d, want one", hooks.Ends())
	}
	for _, marker := range []string{"cancel", "wait_idle", "resources_closed", "save:active", "session_end:active:exit"} {
		if count := sequence.count(marker); count != 1 {
			t.Fatalf("continued cleanup %s count = %d, want one (events=%v)", marker, count, sequence.snapshot())
		}
	}
	if third := model.Close(context.Background()); third != second {
		t.Fatalf("repeated final Close changed result: first final=%v, repeated=%v", second, third)
	}
}

// TestAppCleanupTimeoutDiagnosesOnce forces the internal C7 budget to expire.
// Later stages are still signalled, one bounded timeout diagnostic is emitted,
// and all subsequent/concurrent Close calls return the same final error.
func TestAppCleanupTimeoutDiagnosesOnce(t *testing.T) {
	sequence := newT426Sequence("wait_idle", "resources_closed", "save:active", "session_end:active:exit")
	release := make(chan struct{})
	waiter := &t426Waiter{sequence: sequence, release: release, ignoreCtx: true}
	store := &t426Store{sequence: sequence}
	hooks := &t426Hooks{sequence: sequence}
	sink := &t426DiagnosticSink{}
	conv := conversation.NewConversation("active", time.Unix(3, 0))
	model := t426Model(sequence, conv, waiter, store, hooks, RuntimeOptions{CleanupTimeout: 20 * time.Millisecond, Diagnostics: sink})
	model.request.Cancel = func() { sequence.add("cancel") }
	model.lifecycle.replace(model.request)

	first := model.Close(context.Background())
	if !errors.Is(first, errAppCleanupTimeout) {
		t.Fatalf("first Close = %v, want app cleanup timeout", first)
	}
	// Save and SessionEnd are deliberately fire-and-forget after the hard
	// deadline. Wait for their markers before checking idempotence.
	sequence.wait(t, "save:active")
	sequence.wait(t, "session_end:active:exit")
	second := model.Close(context.Background())
	if first != second || !errors.Is(second, errAppCleanupTimeout) {
		t.Fatalf("cleanup timeout result was not stable: first=%v second=%v", first, second)
	}
	if got := sink.countCode(appCleanupTimeoutDiagnosticCode); got != 1 {
		t.Fatalf("cleanup timeout diagnostics = %d, want exactly one (%#v)", got, sink.snapshot())
	}
	if calls, saved := store.Calls(); calls != 1 || saved != conv {
		t.Fatalf("timeout cleanup Save calls/value = %d/%p, want one/%p", calls, saved, conv)
	}
	if hooks.Ends() != 1 {
		t.Fatalf("timeout cleanup SessionEnd calls = %d, want one", hooks.Ends())
	}
	// Unblock the deliberately uncooperative waiter so the test does not leave
	// a live goroutine after the final result has been published.
	close(release)
	sequence.wait(t, "resources_closed")
	for _, marker := range []string{"cancel", "wait_idle", "resources_closed", "save:active", "session_end:active:exit"} {
		if count := sequence.count(marker); count != 1 {
			t.Fatalf("timeout cleanup %s count = %d, want one (events=%v)", marker, count, sequence.snapshot())
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := model.Close(cancelled); got != first {
		t.Fatalf("Close with canceled caller after completion changed result: %v", got)
	}
}
