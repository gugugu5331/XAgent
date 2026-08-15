package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/hook"
)

// maxAppCleanupTimeout is the C7 hard upper bound.  A caller may choose a
// shorter timeout through RuntimeOptions, but an App must never extend the
// cleanup budget on behalf of a leaf operation.
const maxAppCleanupTimeout = 2 * time.Second

var (
	// errAppCleanupTimeout is deliberately package scoped so the App tests and
	// the process lifecycle can distinguish an internal cleanup deadline from a
	// caller that merely stopped waiting.
	errAppCleanupTimeout = errors.New("app cleanup exceeded its hard deadline")
	// Keep the more generic spelling available to adjacent lifecycle code.  It
	// is the same value, so errors.Is and equality remain stable.
	errRuntimeCleanupTimeout = errAppCleanupTimeout
)

const (
	appCleanupTimeoutDiagnosticCode = "app_cleanup_timeout"
	appCleanupDiagnosticSource      = "app"
	appWaitIdleDiagnosticCode       = "app_wait_idle_failed"
	appSaveDiagnosticCode           = "app_save_failed"
	appTaskShutdownDiagnosticCode   = "app_task_shutdown_failed"
)

// RuntimeOptions are the lifecycle-only inputs owned by an App runtime.
// Diagnostics is intentionally the narrow bounded sink rather than a
// Collector or a process-wide container.  A nil value is tolerated by the
// compatibility constructor; in that case App falls back to Deps.Diagnostics
// when one is available.
type RuntimeOptions struct {
	CleanupTimeout time.Duration
	Diagnostics    diagnostics.BoundedSink
}

func normalizeRuntimeOptions(options RuntimeOptions) RuntimeOptions {
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = maxAppCleanupTimeout
	}
	if options.CleanupTimeout > maxAppCleanupTimeout {
		options.CleanupTimeout = maxAppCleanupTimeout
	}
	return options
}

// lifecycleCloseSnapshot contains the ownership inputs needed by the one and
// only close worker.  Keeping these values on the shared pointer is important
// because Bubble Tea copies Model values; a Close call on a stale copy must not
// lose the active conversation or replace a service with a zero value.
type lifecycleCloseSnapshot struct {
	request      *RequestSession
	conversation *conversation.Conversation
	store        navigationSaver
	waiter       navigationWaiter
	hooks        interface {
		SessionEnd(context.Context, string, hook.SessionEndReason)
	}
}

// lifecycleState is shared by every Bubble Tea Model value copy. Mutable
// process/request ownership must not live solely on Model itself because
// Bubble Tea passes Models by value through Update.
type lifecycleState struct {
	mu        sync.Mutex
	request   *RequestSession
	canceling bool
	terminal  bool

	// accepting is closed before the asynchronous cleanup goroutine starts.
	// It is checked by beginRequest and can also be used by future intent
	// producers without needing to know about closeOnce.
	accepting bool

	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error

	snapshot lifecycleCloseSnapshot
}

func newLifecycleState() *lifecycleState {
	return &lifecycleState{accepting: true, closeDone: make(chan struct{})}
}

// bindModel publishes immutable service dependencies and the currently active
// conversation into the shared close snapshot. Nil model fields never erase a
// previously published active value: a copied Model may legitimately lag the
// value that performed the latest navigation commit.
func (s *lifecycleState) bindModel(model *Model) {
	if s == nil || model == nil {
		return
	}
	s.mu.Lock()
	s.bindModelLocked(model)
	s.mu.Unlock()
}

// bindModelLocked is used at the close linearization point.  Once closing has
// begun no later Model copy is allowed to publish fields into the snapshot;
// otherwise the background worker could race a stale Bubble Tea copy while it
// is clearing that copy's presentation state.
func (s *lifecycleState) bindModelLocked(model *Model) {
	if s == nil || model == nil {
		return
	}
	if s.snapshot.store == nil && model.deps.Store != nil {
		s.snapshot.store = model.deps.Store
	}
	if s.snapshot.waiter == nil {
		if model.lifecycleWaiter != nil {
			s.snapshot.waiter = model.lifecycleWaiter
		} else if model.orchestrator != nil {
			s.snapshot.waiter = model.orchestrator
		}
	}
	if s.snapshot.hooks == nil && model.hooks != nil {
		s.snapshot.hooks = model.hooks
	}
	if model.conversation != nil {
		s.snapshot.conversation = model.conversation
	}
	if model.request != nil {
		s.snapshot.request = model.request
	}
}

func (s *lifecycleState) bindConversation(conversation *conversation.Conversation) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.snapshot.conversation = conversation
	s.mu.Unlock()
}

func (s *lifecycleState) bindRequest(request *RequestSession) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting {
		return false
	}
	s.snapshot.request = request
	return true
}

func (s *lifecycleState) closeSnapshot() lifecycleCloseSnapshot {
	if s == nil {
		return lifecycleCloseSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot
}

func (s *lifecycleState) begin(request *RequestSession) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting {
		return false
	}
	s.request = request
	s.snapshot.request = request
	s.canceling = false
	s.terminal = false
	return true
}

func (s *lifecycleState) replace(request *RequestSession) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accepting {
		return false
	}
	s.request = request
	s.snapshot.request = request
	return true
}

func (s *lifecycleState) active() (*RequestSession, bool, bool) {
	if s == nil {
		return nil, false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.request, s.canceling, s.terminal
}

func (s *lifecycleState) isAccepting() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	accepting := s.accepting
	s.mu.Unlock()
	return accepting
}

// acceptsIntent is the App-facing admission check used before a command can
// create a session, start a provider request, or publish an asynchronous
// navigation.  A nil lifecycle is the legacy pre-initialization state and is
// therefore treated as accepting until the first request/Close establishes it.
func (m *Model) acceptsIntent() bool {
	if m == nil {
		return false
	}
	return m.lifecycle == nil || m.lifecycle.isAccepting()
}

// stopIntents is intentionally separate from startClose.  It gives callers a
// synchronous linearization point: once Close returns from this method, a
// concurrently arriving request cannot publish itself into the close plan.
func (s *lifecycleState) stopIntents() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.accepting = false
	s.mu.Unlock()
}

func (s *lifecycleState) cancel() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	request := s.request
	if request == nil || s.canceling {
		s.mu.Unlock()
		return request != nil
	}
	s.canceling = true
	cancel := request.Cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}

// cancelSafely preserves the historical bool-only cancel API while allowing
// the Close protocol to treat a malicious cancel callback as a failed stage
// and continue with WaitIdle/Save/SessionEnd.
func (s *lifecycleState) cancelSafely() (cancelled bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("request cancellation panic: %v", recovered)
		}
	}()
	return s.cancel(), nil
}

func (s *lifecycleState) markTerminal() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.request != nil {
		s.terminal = true
	}
	s.mu.Unlock()
}

func (s *lifecycleState) finish() *RequestSession {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	request := s.request
	s.request = nil
	s.snapshot.request = nil
	s.canceling = false
	s.terminal = false
	s.mu.Unlock()
	return request
}

// startClose starts exactly one background cleanup worker.  The worker owns
// the internal context; no caller context is captured here.
func (s *lifecycleState) startClose(fn func() error) {
	s.startCloseModel(nil, fn)
}

// startCloseModel atomically seals intent admission, snapshots the first
// caller's immutable lifecycle inputs, and starts exactly one cleanup worker.
// Subsequent calls do not inspect their Model receiver, which is essential for
// race-free Close on concurrent Bubble Tea value copies.
func (s *lifecycleState) startCloseModel(model *Model, fn func() error) {
	if s == nil {
		if fn != nil {
			go fn()
		}
		return
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.accepting = false
		s.bindModelLocked(model)
		s.mu.Unlock()
		go func() {
			var err error
			if fn != nil {
				err = fn()
			}
			s.mu.Lock()
			s.closeErr = err
			s.mu.Unlock()
			close(s.closeDone)
		}()
	})
}

func (s *lifecycleState) waitClose(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-s.closeDone:
		return s.finalCloseErr()
	default:
	}
	select {
	case <-s.closeDone:
		return s.finalCloseErr()
	case <-ctx.Done():
		// A caller timeout/cancellation is a waiting result only.  Do one last
		// non-blocking check so a cleanup that completed at the same instant is
		// reported instead of being mistaken for a caller error.
		select {
		case <-s.closeDone:
			return s.finalCloseErr()
		default:
			return ctx.Err()
		}
	}
}

func (s *lifecycleState) finalCloseErr() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	err := s.closeErr
	s.mu.Unlock()
	return err
}

// close is retained as a small compatibility helper for code written before
// the C7 two-context lifecycle. New App code uses startClose/waitClose.
func (s *lifecycleState) close(fn func() error) error {
	if s == nil {
		if fn == nil {
			return nil
		}
		return fn()
	}
	s.startClose(fn)
	return s.waitClose(context.Background())
}

// appCleanupError is a stable, redacted aggregate returned after every
// cleanup stage has had an opportunity to run. The underlying raw errors are
// never retained in lifecycle state or diagnostics.
func (m *Model) appCleanupError(parts []string) error {
	if len(parts) == 0 {
		return nil
	}
	return m.redactError(fmt.Errorf("%s", strings.Join(parts, "; ")))
}

// appCleanupCall runs one potentially blocking owner operation under the
// independent cleanup context. It returns timedOut=true when the hard budget
// elapsed; the caller can then invoke later best-effort stages without
// allowing a hostile implementation to extend the App's close result.
func appCleanupCall(ctx context.Context, call func(context.Context) error) (err error, timedOut bool) {
	if call == nil {
		return nil, false
	}
	result := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				result <- fmt.Errorf("cleanup panic: %v", recovered)
			}
		}()
		result <- call(ctx)
	}()
	select {
	case err = <-result:
		return err, false
	case <-ctx.Done():
		return errAppCleanupTimeout, true
	}
}

// appCleanupFireAndForget is used only after the hard deadline. It still
// invokes the owner with a canceled context so the required later cleanup
// stages are signalled, but it cannot delay the final Close result.
func appCleanupFireAndForget(call func(context.Context) error) {
	if call == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	go func() {
		defer func() { _ = recover() }()
		_ = call(ctx)
	}()
}

func (m *Model) addCleanupDiagnostic(code, hint string, err error) {
	if m == nil || err == nil {
		return
	}
	input := diagnostics.SanitizeInput{
		Code: code, Source: appCleanupDiagnosticSource, Hint: hint,
		Severity: diagnostics.SeverityError, Err: err,
	}
	if sink := m.runtimeOptions.Diagnostics; sink != nil {
		sink.Add(input)
		return
	}
	if m.deps.Diagnostics != nil {
		// Collector is the legacy App diagnostic owner. Convert through the
		// runtime redactor before publication so this fallback remains safe.
		message := m.redactText(err.Error())
		m.deps.Diagnostics.Add(diagnostics.Diagnostic{
			Code: code, Source: appCleanupDiagnosticSource,
			Severity: diagnostics.SeverityError, Message: message,
		})
	}
}
