package mcpclient

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/config"
	"xagent/internal/diagnostics"
	"xagent/internal/mcpclient/protocol"
)

func TestManagerPublishesOnlyRunningSessions(t *testing.T) {
	running := newManagerLifecycleSession(serverSessionStateRunning, "echo")
	nonRunning := newManagerLifecycleSession(serverSessionStateFailed, "must_not_publish")
	startFailed := newManagerLifecycleSession(serverSessionStateFailed, "also_must_not_publish")
	startFailed.startErr = errors.New("injected startup failure")

	manager := newManagerLifecycleTestManager(t, map[string]*managerLifecycleSession{
		"running":      running,
		"non-running":  nonRunning,
		"start-failed": startFailed,
	})
	manager.Start(context.Background())

	snapshot := manager.Snapshot()
	if snapshot.State != ManagerStateRunning {
		t.Fatalf("manager state = %v, want running", snapshot.State)
	}
	if len(snapshot.Tools) != 1 || snapshot.Tools[0].Name() != "mcp__running__echo" {
		t.Fatalf("published tools = %#v, want only the running session tool", snapshot.Tools)
	}
	if summary := manager.Summary(); summary.Ready != 1 || summary.Failed != 2 {
		t.Fatalf("manager summary = %#v, want one ready and two failed sessions", summary)
	}

	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("close manager: %v", err)
	}
	for name, session := range map[string]*managerLifecycleSession{
		"running": running, "non-running": nonRunning, "start-failed": startFailed,
	} {
		if got := session.closeCalls.Load(); got != 1 {
			t.Fatalf("%s session close calls = %d, want 1", name, got)
		}
	}

	t.Run("factory session ownership transfers before error handling", func(t *testing.T) {
		owned := newManagerLifecycleSession(serverSessionStateRunning, "must_not_publish")
		manager := newManagerWithFactoryForTest(t, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"owned": {Type: config.MCPTransportHTTP, URL: "https://example.invalid/mcp"},
		}}, ManagerOptions{}, managerSessionFactoryFunc(func(
			context.Context,
			string,
			config.MCPServerConfig,
			ManagerOptions,
		) (managerSession, error) {
			return owned, errors.New("injected factory failure after allocation")
		}), nil)

		manager.Start(context.Background())
		if snapshot := manager.Snapshot(); snapshot.State != ManagerStateRunning || len(snapshot.Tools) != 0 {
			t.Fatalf("snapshot after factory failure = %#v, want running with no tools", snapshot)
		}
		if got := owned.startCalls.Load(); got != 0 {
			t.Fatalf("owned session Start calls = %d, want 0", got)
		}
		if got := owned.closeCalls.Load(); got != 1 {
			t.Fatalf("owned session Close calls = %d, want 1", got)
		}
		if err := manager.Close(context.Background()); err != nil {
			t.Fatalf("close manager after factory failure: %v", err)
		}
		if got := owned.closeCalls.Load(); got != 1 {
			t.Fatalf("manager Close repeated owned session cleanup: %d calls", got)
		}
	})
}

func TestManagerCloseSnapshotRace(t *testing.T) {
	callGate := newManagerLifecycleGate(t)
	session := newManagerLifecycleSession(serverSessionStateRunning, "echo")
	session.callRelease = callGate.signal
	manager := newManagerLifecycleTestManager(t, map[string]*managerLifecycleSession{"server": session})
	manager.Start(context.Background())

	tools := manager.Tools()
	if len(tools) != 1 {
		t.Fatalf("published tools = %d, want 1", len(tools))
	}
	registeredName := tools[0].Name()
	tools[0] = nil
	if fresh := manager.Tools(); len(fresh) != 1 || fresh[0] == nil {
		t.Fatal("Tools returned mutable manager slice storage")
	}

	existingCall := make(chan error, 1)
	go func() {
		_, err := manager.CallTool(context.Background(), registeredName, nil)
		existingCall <- err
	}()
	waitManagerLifecycleSignal(t, session.callEntered, "existing tool call")

	const (
		observers    = 32
		callers      = 16
		closeWaiters = 4
	)
	startRace := make(chan struct{})
	snapshots := make(chan ManagerSnapshot, observers)
	toolCounts := make(chan int, observers)
	callResults := make(chan error, callers)
	closeResults := make(chan error, closeWaiters)

	for index := 0; index < observers; index++ {
		go func() {
			<-startRace
			snapshots <- manager.Snapshot()
		}()
		go func() {
			<-startRace
			toolCounts <- len(manager.Tools())
		}()
	}
	for index := 0; index < callers; index++ {
		go func() {
			<-startRace
			_, err := manager.CallTool(context.Background(), registeredName, nil)
			callResults <- err
		}()
	}
	for index := 0; index < closeWaiters; index++ {
		go func() {
			<-startRace
			closeResults <- manager.Close(context.Background())
		}()
	}
	close(startRace)
	waitManagerLifecycleSignal(t, session.closeEntered, "session close")

	closing := manager.Snapshot()
	if closing.State != ManagerStateClosing || len(closing.Tools) != 0 {
		t.Fatalf("closing snapshot = %#v, want closing with no tools", closing)
	}
	if _, err := manager.CallTool(context.Background(), registeredName, nil); err == nil {
		t.Fatal("CallTool admitted a new call after closing began")
	}
	select {
	case err := <-closeResults:
		t.Fatalf("Close completed while an existing lease was held: %v", err)
	default:
	}

	callGate.open()
	if err := waitManagerLifecycleResult(t, existingCall, "existing tool call result"); err != nil {
		t.Fatalf("existing tool call failed: %v", err)
	}
	for index := 0; index < callers; index++ {
		_ = waitManagerLifecycleResult(t, callResults, "racing tool call result")
	}
	for index := 0; index < closeWaiters; index++ {
		if err := waitManagerLifecycleResult(t, closeResults, "close result"); err != nil {
			t.Fatalf("close result = %v, want nil", err)
		}
	}

	for index := 0; index < observers; index++ {
		observed := waitManagerLifecycleSnapshot(t, snapshots)
		switch observed.State {
		case ManagerStateRunning:
			if len(observed.Tools) != 1 {
				t.Fatalf("running snapshot published %d tools, want 1", len(observed.Tools))
			}
		case ManagerStateClosing, ManagerStateClosed:
			if len(observed.Tools) != 0 {
				t.Fatalf("state %v published %d tools, want 0", observed.State, len(observed.Tools))
			}
		default:
			t.Fatalf("unexpected manager state observed during close: %v", observed.State)
		}
		count := waitManagerLifecycleInt(t, toolCounts, "Tools result")
		if count != 0 && count != 1 {
			t.Fatalf("Tools returned a partial publication of %d tools", count)
		}
	}

	final := manager.Snapshot()
	if final.State != ManagerStateClosed || len(final.Tools) != 0 {
		t.Fatalf("final snapshot = %#v, want closed with no tools", final)
	}
	if got := session.closeCalls.Load(); got != 1 {
		t.Fatalf("session close calls = %d, want 1", got)
	}
	if state, active := manager.leases.snapshot(); state != leaseGateStateClosed || active != 0 {
		t.Fatalf("lease gate = (%v, %d), want (closed, 0)", state, active)
	}

	t.Run("session close fanout precedes waiting", func(t *testing.T) {
		blockedGate := newManagerLifecycleGate(t)
		blocked := newManagerLifecycleSession(serverSessionStateRunning, "blocked")
		blocked.closeRelease = blockedGate.signal
		fast := newManagerLifecycleSession(serverSessionStateRunning, "fast")
		manager := newManagerLifecycleTestManager(t, map[string]*managerLifecycleSession{
			"a-blocked": blocked,
			"z-fast":    fast,
		})
		manager.Start(context.Background())

		closeResult := make(chan error, 1)
		go func() { closeResult <- manager.Close(context.Background()) }()
		waitManagerLifecycleSignal(t, blocked.closeEntered, "blocked session close")
		waitManagerLifecycleSignal(t, fast.closeEntered, "fast session close fanout")
		blockedGate.open()
		if err := waitManagerLifecycleResult(t, closeResult, "fanout manager close"); err != nil {
			t.Fatalf("fanout manager Close error = %v, want nil", err)
		}
	})
}

func TestManagerCloseContinuesAfterWaiterTimeout(t *testing.T) {
	t.Run("caller timeout does not poison final result", func(t *testing.T) {
		callGate := newManagerLifecycleGate(t)
		closeGate := newManagerLifecycleGate(t)
		session := newManagerLifecycleSession(serverSessionStateRunning, "echo")
		session.callRelease = callGate.signal
		session.closeRelease = closeGate.signal
		manager := newManagerLifecycleTestManager(t, map[string]*managerLifecycleSession{"server": session})
		manager.Start(context.Background())
		registeredName := manager.Tools()[0].Name()

		callResult := make(chan error, 1)
		go func() {
			_, err := manager.CallTool(context.Background(), registeredName, nil)
			callResult <- err
		}()
		waitManagerLifecycleSignal(t, session.callEntered, "existing tool call")

		waitContext := newManagerLifecycleDeadlineContext()
		firstClose := make(chan error, 1)
		go func() { firstClose <- manager.Close(waitContext) }()
		waitManagerLifecycleSignal(t, session.closeEntered, "session close")
		waitContext.expire()
		if err := waitManagerLifecycleResult(t, firstClose, "timed-out close waiter"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first Close error = %v, want deadline exceeded", err)
		}
		if snapshot := manager.Snapshot(); snapshot.State != ManagerStateClosing || len(snapshot.Tools) != 0 {
			t.Fatalf("snapshot after waiter timeout = %#v, want closing with no tools", snapshot)
		}
		if _, err := manager.CallTool(context.Background(), registeredName, nil); err == nil {
			t.Fatal("CallTool admitted a new call after the first waiter timed out")
		}

		finalClose := make(chan error, 1)
		go func() { finalClose <- manager.Close(context.Background()) }()
		closeGate.open()
		select {
		case err := <-finalClose:
			t.Fatalf("cleanup completed before the existing lease was released: %v", err)
		default:
		}
		callGate.open()
		if err := waitManagerLifecycleResult(t, callResult, "existing tool call result"); err != nil {
			t.Fatalf("existing tool call failed: %v", err)
		}
		if err := waitManagerLifecycleResult(t, finalClose, "final close result"); err != nil {
			t.Fatalf("final Close error = %v, want nil", err)
		}

		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := manager.Close(cancelled); err != nil {
			t.Fatalf("completed Close with cancelled waiter = %v, want cached nil", err)
		}
		if got := session.closeCalls.Load(); got != 1 {
			t.Fatalf("session close calls = %d, want 1", got)
		}
		for _, diagnostic := range manager.Diagnostics() {
			if diagnostic.Code == diagnosticMCPManagerCleanupTimeout {
				t.Fatal("caller timeout emitted an internal cleanup-timeout diagnostic")
			}
		}
	})

	t.Run("hard timeout is stable and diagnosed once", func(t *testing.T) {
		callGate := newManagerLifecycleGate(t)
		session := newManagerLifecycleSession(serverSessionStateRunning, "echo")
		session.callRelease = callGate.signal
		sink := &managerLifecycleDiagnosticSink{}
		manager := newManagerLifecycleTestManagerWithOptions(t,
			map[string]*managerLifecycleSession{"server": session},
			ManagerOptions{CleanupTimeout: 20 * time.Millisecond},
			sink,
		)
		manager.Start(context.Background())
		registeredName := manager.Tools()[0].Name()

		callResult := make(chan error, 1)
		go func() {
			_, err := manager.CallTool(context.Background(), registeredName, nil)
			callResult <- err
		}()
		waitManagerLifecycleSignal(t, session.callEntered, "existing tool call")

		const closeWaiters = 2
		closeResults := make(chan error, closeWaiters)
		for index := 0; index < closeWaiters; index++ {
			go func() { closeResults <- manager.Close(context.Background()) }()
		}
		waitManagerLifecycleSignal(t, session.closeEntered, "session close")
		for index := 0; index < closeWaiters; index++ {
			if err := waitManagerLifecycleResult(t, closeResults, "hard-timeout close result"); !errors.Is(err, errManagerCleanupTimeout) {
				t.Fatalf("Close error = %v, want manager cleanup timeout", err)
			}
		}

		if snapshot := manager.Snapshot(); snapshot.State != ManagerStateClosed || len(snapshot.Tools) != 0 {
			t.Fatalf("hard-timeout snapshot = %#v, want closed with no tools", snapshot)
		}
		diagnostics := manager.Diagnostics()
		if len(diagnostics) != 1 || diagnostics[0].Code != diagnosticMCPManagerCleanupTimeout || diagnostics[0].Source != diagnosticMCPManagerSource {
			t.Fatalf("hard-timeout diagnostics = %#v, want one stable manager timeout diagnostic", diagnostics)
		}
		if got := session.closeCalls.Load(); got != 1 {
			t.Fatalf("session close calls = %d, want 1", got)
		}

		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := manager.Close(cancelled); !errors.Is(err, errManagerCleanupTimeout) {
			t.Fatalf("cached Close error = %v, want manager cleanup timeout", err)
		}
		if got := len(manager.Diagnostics()); got != 1 {
			t.Fatalf("diagnostic count after repeated Close = %d, want 1", got)
		}
		sinkInputs := sink.snapshot()
		if len(sinkInputs) != 1 || sinkInputs[0].Code != diagnosticMCPManagerCleanupTimeout ||
			sinkInputs[0].Source != diagnosticMCPManagerSource ||
			!errors.Is(sinkInputs[0].Err, errManagerCleanupTimeout) {
			t.Fatalf("bounded sink inputs = %#v, want one stable manager timeout diagnostic", sinkInputs)
		}

		callGate.open()
		if err := waitManagerLifecycleResult(t, callResult, "timed-out lease result"); err != nil {
			t.Fatalf("existing tool call failed after forced manager close: %v", err)
		}
		if state, active := manager.leases.snapshot(); state != leaseGateStateClosed || active != 0 {
			t.Fatalf("lease gate after late release = (%v, %d), want (closed, 0)", state, active)
		}
		if got := session.closeCalls.Load(); got != 1 {
			t.Fatalf("repeated Close restarted session cleanup: %d calls", got)
		}
	})

	t.Run("session returned after hard timeout is detached and closed", func(t *testing.T) {
		factoryEntered := make(chan struct{})
		factoryRelease := make(chan struct{})
		late := newManagerLifecycleSession(serverSessionStateRunning, "late")
		manager := newManagerWithFactoryForTest(t, config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"late": {Type: config.MCPTransportHTTP, URL: "https://example.invalid/mcp"},
		}}, ManagerOptions{CleanupTimeout: 20 * time.Millisecond}, managerSessionFactoryFunc(func(
			context.Context,
			string,
			config.MCPServerConfig,
			ManagerOptions,
		) (managerSession, error) {
			close(factoryEntered)
			<-factoryRelease
			return late, nil
		}), nil)

		startDone := make(chan struct{})
		go func() {
			manager.Start(context.Background())
			close(startDone)
		}()
		waitManagerLifecycleSignal(t, factoryEntered, "blocking manager session factory")
		if err := manager.Close(context.Background()); !errors.Is(err, errManagerCleanupTimeout) {
			t.Fatalf("Close error = %v, want manager cleanup timeout", err)
		}

		close(factoryRelease)
		waitManagerLifecycleSignal(t, late.closeEntered, "detached late session close")
		waitManagerLifecycleSignal(t, startDone, "manager Start after late factory return")
		if got := late.startCalls.Load(); got != 0 {
			t.Fatalf("late session Start calls = %d, want 0", got)
		}
		if got := late.closeCalls.Load(); got != 1 {
			t.Fatalf("late session Close calls = %d, want 1", got)
		}
	})
}

type managerLifecycleSession struct {
	state        serverSessionState
	tools        []protocol.RemoteTool
	startErr     error
	callEntered  chan struct{}
	callRelease  <-chan struct{}
	closeEntered chan struct{}
	closeRelease <-chan struct{}
	callOnce     sync.Once
	closeOnce    sync.Once
	startCalls   atomic.Int32
	callCalls    atomic.Int32
	closeCalls   atomic.Int32
}

func newManagerLifecycleSession(state serverSessionState, toolNames ...string) *managerLifecycleSession {
	remoteTools := make([]protocol.RemoteTool, len(toolNames))
	for index, name := range toolNames {
		remoteTools[index] = protocol.RemoteTool{Name: name, InputSchema: []byte(`{"type":"object"}`)}
	}
	return &managerLifecycleSession{
		state: state, tools: remoteTools,
		callEntered: make(chan struct{}), closeEntered: make(chan struct{}),
	}
}

func (session *managerLifecycleSession) Start(context.Context) error {
	session.startCalls.Add(1)
	return session.startErr
}

func (session *managerLifecycleSession) Snapshot() serverSessionSnapshot {
	return serverSessionSnapshot{State: session.state, ToolsSupported: true, Tools: cloneSessionTools(session.tools)}
}

func (session *managerLifecycleSession) CallTool(context.Context, string, map[string]any) (protocol.CallToolResult, error) {
	session.callCalls.Add(1)
	session.callOnce.Do(func() { close(session.callEntered) })
	if session.callRelease != nil {
		<-session.callRelease
	}
	return protocol.CallToolResult{Content: []protocol.ContentBlock{{Type: "text", Text: "ok"}}}, nil
}

func (session *managerLifecycleSession) Close(context.Context) error {
	session.closeCalls.Add(1)
	session.closeOnce.Do(func() { close(session.closeEntered) })
	if session.closeRelease != nil {
		<-session.closeRelease
	}
	return nil
}

func newManagerLifecycleTestManager(t *testing.T, sessions map[string]*managerLifecycleSession) *Manager {
	return newManagerLifecycleTestManagerWithOptions(t, sessions, ManagerOptions{})
}

func newManagerLifecycleTestManagerWithOptions(
	t *testing.T,
	sessions map[string]*managerLifecycleSession,
	options ManagerOptions,
	sinks ...diagnostics.BoundedSink,
) *Manager {
	t.Helper()
	servers := make(map[string]config.MCPServerConfig, len(sessions))
	for name := range sessions {
		servers[name] = config.MCPServerConfig{Type: config.MCPTransportHTTP, URL: "https://example.invalid/mcp"}
	}
	var sink diagnostics.BoundedSink
	if len(sinks) > 0 {
		sink = sinks[0]
	}
	return newManagerWithFactoryForTest(t, config.MCPConfig{Servers: servers}, options, managerSessionFactoryFunc(func(
		_ context.Context,
		serverName string,
		_ config.MCPServerConfig,
		_ ManagerOptions,
	) (managerSession, error) {
		session := sessions[serverName]
		if session == nil {
			return nil, errors.New("unexpected test server")
		}
		return session, nil
	}), sink)
}

type managerLifecycleDiagnosticSink struct {
	mu     sync.Mutex
	inputs []diagnostics.SanitizeInput
}

func (sink *managerLifecycleDiagnosticSink) Add(input diagnostics.SanitizeInput) {
	sink.mu.Lock()
	sink.inputs = append(sink.inputs, input)
	sink.mu.Unlock()
}

func (sink *managerLifecycleDiagnosticSink) snapshot() []diagnostics.SanitizeInput {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]diagnostics.SanitizeInput(nil), sink.inputs...)
}

type managerLifecycleGate struct {
	signal chan struct{}
	once   sync.Once
}

func newManagerLifecycleGate(t *testing.T) *managerLifecycleGate {
	t.Helper()
	gate := &managerLifecycleGate{signal: make(chan struct{})}
	t.Cleanup(gate.open)
	return gate
}

func (gate *managerLifecycleGate) open() {
	gate.once.Do(func() { close(gate.signal) })
}

type managerLifecycleDeadlineContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newManagerLifecycleDeadlineContext() *managerLifecycleDeadlineContext {
	return &managerLifecycleDeadlineContext{Context: context.Background(), done: make(chan struct{})}
}

func (ctx *managerLifecycleDeadlineContext) Done() <-chan struct{} { return ctx.done }

func (ctx *managerLifecycleDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (ctx *managerLifecycleDeadlineContext) expire() {
	ctx.once.Do(func() { close(ctx.done) })
}

func waitManagerLifecycleSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitManagerLifecycleResult(t *testing.T, result <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return nil
	}
}

func waitManagerLifecycleSnapshot(t *testing.T, snapshots <-chan ManagerSnapshot) ManagerSnapshot {
	t.Helper()
	select {
	case snapshot := <-snapshots:
		return snapshot
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for manager snapshot")
		return ManagerSnapshot{}
	}
}

func waitManagerLifecycleInt(t *testing.T, values <-chan int, name string) int {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return 0
	}
}
