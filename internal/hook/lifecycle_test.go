package hook

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/diagnostics"
	"xagent/internal/netpolicy"
)

func TestHookLifecycleClosesAllWorkers(t *testing.T) {
	release := make(chan struct{})
	runner := &lifecycleCommandRunner{release: release, started: make(chan struct{}, 4)}
	engine := newLifecycleEngine(t, runner, nil, EngineOptions{
		AsyncWorkers:   2,
		AsyncQueue:     4,
		CleanupTimeout: time.Second,
		ShutdownGrace:  500 * time.Millisecond,
	})
	ownedClient := &lifecyclePolicyClient{onClose: func() {
		if runner.completed.Load() != 4 {
			runner.clientClosedEarly.Store(true)
		}
	}}
	seedOwnedLifecycleClient(t, engine, ownedClient)

	for index := 0; index < 4; index++ {
		engine.BeginTurn(context.Background(), "session", ExecutionMain, ModeDefault)
	}
	for index := 0; index < 2; index++ {
		select {
		case <-runner.started:
		case <-time.After(time.Second):
			t.Fatal("async Hook worker did not start")
		}
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- engine.Shutdown(context.Background()) }()
	close(release)
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if runner.completed.Load() != 4 {
		t.Fatalf("completed workers = %d, want 4", runner.completed.Load())
	}
	if runner.clientClosedEarly.Load() {
		t.Fatal("owned HTTP client closed before Hook workers finished")
	}
	if ownedClient.closes.Load() != 1 {
		t.Fatalf("owned HTTP client closed %d times", ownedClient.closes.Load())
	}
	select {
	case <-engine.async.workersDone:
	default:
		t.Fatal("Shutdown returned before all Hook workers exited")
	}

	borrowedClient := &lifecyclePolicyClient{}
	borrowedRunner := &DefaultHTTPRunner{clients: map[string]netpolicy.Client{"borrowed": borrowedClient}}
	borrowedEngine, err := NewEngine(Snapshot{}, EngineOptions{ProjectRoot: t.TempDir(), HTTPRunner: borrowedRunner, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := borrowedEngine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if borrowedClient.closes.Load() != 0 {
		t.Fatal("Engine closed a borrowed HTTP client")
	}
	borrowedRunner.CloseIdleConnections()
	if borrowedClient.closes.Load() != 1 {
		t.Fatal("borrowed HTTP owner could not close its client exactly once")
	}
}

func TestHookCloseContinuesAfterWaiterTimeout(t *testing.T) {
	release := make(chan struct{})
	runner := &lifecycleCommandRunner{release: release, started: make(chan struct{}, 1)}
	engine := newLifecycleEngine(t, runner, nil, EngineOptions{
		AsyncWorkers:   1,
		AsyncQueue:     1,
		CleanupTimeout: time.Second,
		ShutdownGrace:  500 * time.Millisecond,
	})
	ownedClient := &lifecyclePolicyClient{}
	seedOwnedLifecycleClient(t, engine, ownedClient)
	engine.BeginTurn(context.Background(), "session", ExecutionMain, ModeDefault)
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("async Hook worker did not start")
	}

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelWait()
	if err := engine.Shutdown(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller wait result = %v", err)
	}
	engine.lifecycleMu.Lock()
	state := engine.systemState
	engine.lifecycleMu.Unlock()
	if state != 4 {
		t.Fatalf("caller timeout changed closing state to %d", state)
	}
	close(release)
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.completed.Load() != 1 || ownedClient.closes.Load() != 1 {
		t.Fatalf("continued cleanup = completed %d, client closes %d", runner.completed.Load(), ownedClient.closes.Load())
	}
}

func TestHookCleanupTimeoutForcesCloseAndDiagnosesOnce(t *testing.T) {
	forceRelease := make(chan struct{})
	runner := &lifecycleCommandRunner{release: forceRelease, started: make(chan struct{}, 1), ignoreContext: true}
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{})
	engine := newLifecycleEngine(t, runner, collector, EngineOptions{
		AsyncWorkers:   1,
		AsyncQueue:     1,
		CleanupTimeout: 20 * time.Millisecond,
	})
	ownedClient := &lifecyclePolicyClient{onClose: func() { close(forceRelease) }}
	seedOwnedLifecycleClient(t, engine, ownedClient)
	engine.BeginTurn(context.Background(), "session", ExecutionMain, ModeDefault)
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("async Hook worker did not start")
	}

	first := engine.Shutdown(context.Background())
	second := engine.Shutdown(context.Background())
	if !errors.Is(first, errHookCleanupTimeout) || first != second {
		t.Fatalf("stable cleanup result = first %v, second %v", first, second)
	}
	if ownedClient.closes.Load() != 1 {
		t.Fatalf("forced HTTP client closes = %d", ownedClient.closes.Load())
	}
	select {
	case <-engine.async.workersDone:
	case <-time.After(time.Second):
		t.Fatal("forced close did not unblock Hook worker")
	}
	if countDiagnosticCode(collector.List(), "hook_cleanup_timeout") != 1 {
		t.Fatalf("cleanup diagnostics = %#v", collector.List())
	}
	if countDiagnosticCode(engine.ShutdownNotices(), "hook_cleanup_timeout") != 1 {
		t.Fatalf("cleanup shutdown notices = %#v", engine.ShutdownNotices())
	}
	if err := engine.Shutdown(context.Background()); err != first || countDiagnosticCode(collector.List(), "hook_cleanup_timeout") != 1 {
		t.Fatal("repeated Shutdown changed the final result or duplicated diagnostics")
	}
}

type lifecycleCommandRunner struct {
	release           <-chan struct{}
	started           chan struct{}
	ignoreContext     bool
	completed         atomic.Int32
	clientClosedEarly atomic.Bool
}

func (r *lifecycleCommandRunner) Run(ctx context.Context, _ CommandRequest) (CommandResult, error) {
	r.started <- struct{}{}
	if r.ignoreContext {
		<-r.release
	} else {
		select {
		case <-r.release:
		case <-ctx.Done():
			return CommandResult{}, ctx.Err()
		}
	}
	r.completed.Add(1)
	return CommandResult{}, nil
}

type lifecyclePolicyClient struct {
	closeOnce sync.Once
	closes    atomic.Int32
	onClose   func()
}

func (*lifecyclePolicyClient) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("unused")
}
func (*lifecyclePolicyClient) SDKHTTPClient() *http.Client { return nil }
func (c *lifecyclePolicyClient) CloseIdleConnections() {
	c.closes.Add(1)
	c.closeOnce.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
	})
}

func newLifecycleEngine(t *testing.T, runner CommandRunner, collector *diagnostics.Collector, options EngineOptions) *Engine {
	t.Helper()
	rule := commandRule(EventTurnStart, 1, "lifecycle", false, false, true)
	options.ProjectRoot = t.TempDir()
	options.CommandRunner = runner
	options.LegacyDiagnostics = collector
	engine, err := NewEngine(newSnapshot([]Rule{rule}), options)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func seedOwnedLifecycleClient(t *testing.T, engine *Engine, client netpolicy.Client) {
	t.Helper()
	if engine.ownedHTTP == nil {
		t.Fatal("test Engine does not own its HTTP runner")
	}
	engine.ownedHTTP.mu.Lock()
	engine.ownedHTTP.clients = map[string]netpolicy.Client{"owned": client}
	engine.ownedHTTP.mu.Unlock()
}

func countDiagnosticCode(items []diagnostics.Diagnostic, code string) int {
	count := 0
	for _, item := range items {
		if item.Code == code {
			count++
		}
	}
	return count
}
