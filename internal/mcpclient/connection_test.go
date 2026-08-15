package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/budget"
	"xagent/internal/diagnostics"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/mcpclient/transport"
)

func TestTakePendingHasSingleWinnerWithoutChannelClose(t *testing.T) {
	const iterations = 128
	for iteration := range iterations {
		table := newPendingTable()
		id := protocol.NumberID(int64(iteration + 1))
		call, err := table.registerPending(id, "tools/call")
		if err != nil {
			t.Fatalf("iteration %d register pending: %v", iteration, err)
		}
		if capacity := cap(call.outcome); capacity != 1 {
			t.Fatalf("iteration %d pending capacity = %d, want 1", iteration, capacity)
		}

		start := make(chan struct{})
		var winners atomic.Int64
		var contenders sync.WaitGroup
		for _, path := range []string{"response", "cancellation", "send failure"} {
			contenders.Add(1)
			go func(path string) {
				defer contenders.Done()
				<-start
				won, ok := table.takePending(id)
				if !ok {
					return
				}
				winners.Add(1)
				won.deliver(pendingOutcome{err: fmt.Errorf("%s won", path)})
			}(path)
		}
		contenders.Add(1)
		go func() {
			defer contenders.Done()
			<-start
			calls := table.takeAllPending()
			if len(calls) == 0 {
				return
			}
			if len(calls) != 1 || calls[0] != call {
				t.Errorf("iteration %d termination took %#v", iteration, calls)
				return
			}
			winners.Add(1)
			calls[0].deliver(pendingOutcome{err: errors.New("connection termination won")})
		}()

		close(start)
		contenders.Wait()
		if got := winners.Load(); got != 1 {
			t.Fatalf("iteration %d completion winners = %d, want 1", iteration, got)
		}
		if got := table.count(); got != 0 {
			t.Fatalf("iteration %d pending count = %d, want 0", iteration, got)
		}
		if _, ok := table.takePending(id); ok {
			t.Fatalf("iteration %d pending was taken twice", iteration)
		}

		outcome, open := <-call.outcome
		if !open || outcome.err == nil {
			t.Fatalf("iteration %d winner outcome = %#v/open=%t", iteration, outcome, open)
		}
		select {
		case extra, open := <-call.outcome:
			if !open {
				t.Fatalf("iteration %d pending outcome channel was closed", iteration)
			}
			t.Fatalf("iteration %d pending received extra outcome %#v", iteration, extra)
		default:
			// An empty open channel is the required terminal state.
		}
	}
}

func TestConnectionHasSingleReceiveLoop(t *testing.T) {
	transportFake := newManagedTransportFake()
	diagnosticSink := newManagedDiagnosticSink()
	connection := newManagedConnectionForTest(t, transportFake, diagnosticSink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := connection.Start(ctx); err != nil {
		t.Fatalf("start connection: %v", err)
	}
	if err := connection.Start(ctx); !errors.Is(err, errManagedConnectionAlreadyStarted) {
		t.Fatalf("second Start error = %v, want %v", err, errManagedConnectionAlreadyStarted)
	}

	firstID, first, err := connection.registerRequest("initialize")
	if err != nil {
		t.Fatalf("register first request: %v", err)
	}
	secondID, second, err := connection.registerRequest("tools/list")
	if err != nil {
		t.Fatalf("register second request: %v", err)
	}
	transportFake.enqueueFrame(t, protocol.RPCResponse{JSONRPC: "2.0", ID: firstID, Result: json.RawMessage(`{"ok":true}`)})
	transportFake.enqueueFrame(t, protocol.RPCResponse{JSONRPC: "2.0", ID: secondID, Result: json.RawMessage(`[]`)})

	if outcome := waitManagedOutcome(t, first); outcome.err != nil || outcome.response.ID != firstID {
		t.Fatalf("first outcome = %#v, want matching response", outcome)
	}
	if outcome := waitManagedOutcome(t, second); outcome.err != nil || outcome.response.ID != secondID {
		t.Fatalf("second outcome = %#v, want matching response", outcome)
	}

	lateID := protocol.StringID("sensitive-late-response-id")
	transportFake.enqueueFrame(t, protocol.RPCResponse{JSONRPC: "2.0", ID: lateID, Result: json.RawMessage(`null`)})
	unknown := diagnosticSink.wait(t)
	if unknown.Code != diagnosticMCPUnknownResponseID || unknown.Source != diagnosticMCPConnectionSource {
		t.Fatalf("unknown-id diagnostic = %#v", unknown)
	}
	if containsManagedDiagnosticText(unknown, "sensitive-late-response-id") {
		t.Fatalf("unknown-id diagnostic retained the response id: %#v", unknown)
	}

	cancel()
	waitManagedReceiveDone(t, connection)
	if got := transportFake.startCalls.Load(); got != 1 {
		t.Fatalf("Transport.Start calls = %d, want 1", got)
	}
	if got := transportFake.maxActiveReceive.Load(); got != 1 {
		t.Fatalf("maximum concurrent Receive calls = %d, want 1", got)
	}
}

func TestConnectionFailureCompletesAllPending(t *testing.T) {
	transportFake := newManagedTransportFake()
	connection := newManagedConnectionForTest(t, transportFake, newManagedDiagnosticSink())
	ctx, cancel := context.WithCancel(context.Background())
	if err := connection.Start(ctx); err != nil {
		t.Fatalf("start connection: %v", err)
	}

	calls := make([]*pendingCall, 0, 4)
	for index := range 4 {
		_, call, err := connection.registerRequest(fmt.Sprintf("method-%d", index))
		if err != nil {
			t.Fatalf("register request %d: %v", index, err)
		}
		calls = append(calls, call)
	}
	cancel()

	for index, call := range calls {
		outcome := waitManagedOutcome(t, call)
		if !errors.Is(outcome.err, errManagedConnectionFailed) {
			t.Fatalf("pending %d error = %v, want %v", index, outcome.err, errManagedConnectionFailed)
		}
		select {
		case extra, open := <-call.outcome:
			if !open {
				t.Fatalf("pending %d outcome channel was closed", index)
			}
			t.Fatalf("pending %d received a second outcome: %#v", index, extra)
		default:
		}
	}
	waitManagedReceiveDone(t, connection)
	waitManagedSignal(t, connection.transportCloseDone, "failed connection Transport.Close completion")
	if got := connection.pendingCount(); got != 0 {
		t.Fatalf("pending count = %d, want 0", got)
	}
	if got := transportFake.closeCalls.Load(); got != 1 {
		t.Fatalf("Transport.Close calls = %d, want 1", got)
	}
	if state := connection.currentState(); state != managedConnectionStateFailed {
		t.Fatalf("connection state = %d, want Failed", state)
	}
}

func TestFatalReceiveErrorFailsConnectionAndPending(t *testing.T) {
	transportFake := newManagedTransportFake()
	diagnosticSink := newManagedDiagnosticSink()
	connection := newManagedConnectionForTest(t, transportFake, diagnosticSink)
	if err := connection.Start(context.Background()); err != nil {
		t.Fatalf("start connection: %v", err)
	}

	calls := make([]*pendingCall, 0, 3)
	for index := range 3 {
		_, call, err := connection.registerRequest(fmt.Sprintf("fatal-%d", index))
		if err != nil {
			t.Fatalf("register request %d: %v", index, err)
		}
		calls = append(calls, call)
	}
	transportFake.receiveResults <- managedReceiveResult{err: errors.New("fatal framing error: raw-secret-value")}
	waitManagedReceiveDone(t, connection)
	waitManagedSignal(t, connection.transportCloseDone, "fatal Transport.Close completion")

	for index, call := range calls {
		if outcome := waitManagedOutcome(t, call); !errors.Is(outcome.err, errManagedConnectionFailed) {
			t.Fatalf("pending %d outcome = %#v, want fixed connection failure", index, outcome)
		}
	}
	if got := connection.pendingCount(); got != 0 {
		t.Fatalf("pending count = %d, want 0", got)
	}
	if state := connection.currentState(); state != managedConnectionStateFailed {
		t.Fatalf("connection state = %d, want Failed", state)
	}
	if got := transportFake.closeCalls.Load(); got != 1 {
		t.Fatalf("Transport.Close calls = %d, want 1", got)
	}

	inputs := diagnosticSink.snapshot()
	if len(inputs) != 1 {
		t.Fatalf("fatal diagnostics = %d, want 1: %#v", len(inputs), inputs)
	}
	if inputs[0].Code != diagnosticMCPConnectionFailed || !errors.Is(inputs[0].Err, errManagedConnectionFailed) {
		t.Fatalf("fatal diagnostic = %#v", inputs[0])
	}
	if containsManagedDiagnosticText(inputs[0], "raw-secret-value") {
		t.Fatalf("fatal diagnostic retained transport error text: %#v", inputs[0])
	}
}

func TestResponseCancellationRace(t *testing.T) {
	transportFake := newManagedTransportFake()
	diagnosticSink := newManagedDiagnosticSink()
	connection := newManagedConnectionForTest(t, transportFake, diagnosticSink)
	if err := connection.Start(context.Background()); err != nil {
		t.Fatalf("start connection: %v", err)
	}

	const iterations = 128
	for iteration := range iterations {
		id, call, err := connection.registerRequest(fmt.Sprintf("race-%d", iteration))
		if err != nil {
			t.Fatalf("iteration %d register request: %v", iteration, err)
		}
		frame, err := protocol.Encode(protocol.RPCResponse{
			JSONRPC: "2.0",
			ID:      id,
			Result:  json.RawMessage(`{"winner":"response"}`),
		})
		if err != nil {
			t.Fatalf("iteration %d encode response: %v", iteration, err)
		}

		callCtx, cancelCall := context.WithCancel(context.Background())
		result := make(chan pendingOutcome, 1)
		go func() {
			result <- connection.waitForOutcome(callCtx, id, call)
		}()

		start := make(chan struct{})
		var contenders sync.WaitGroup
		contenders.Add(2)
		go func() {
			defer contenders.Done()
			<-start
			cancelCall()
		}()
		go func() {
			defer contenders.Done()
			<-start
			transportFake.receiveResults <- managedReceiveResult{event: transport.TransportEvent{Frame: frame}}
		}()
		close(start)
		contenders.Wait()

		var outcome pendingOutcome
		select {
		case outcome = <-result:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d timed out waiting for call result", iteration)
		}
		switch {
		case errors.Is(outcome.err, context.Canceled):
			late := diagnosticSink.wait(t)
			if late.Code != diagnosticMCPUnknownResponseID || late.Hint != diagnosticMCPResponseIgnoredHint {
				t.Fatalf("iteration %d late-response diagnostic = %#v", iteration, late)
			}
		case outcome.err == nil && outcome.response.ID == id:
			// The response won; cancellation observed and returned its outcome.
		default:
			t.Fatalf("iteration %d outcome = %#v", iteration, outcome)
		}
		if got := connection.pendingCount(); got != 0 {
			t.Fatalf("iteration %d pending count = %d, want 0", iteration, got)
		}
		select {
		case extra, open := <-call.outcome:
			if !open {
				t.Fatalf("iteration %d outcome channel was closed", iteration)
			}
			t.Fatalf("iteration %d call completed twice: %#v", iteration, extra)
		default:
		}
	}

	if err := connection.Close(context.Background()); err != nil {
		t.Fatalf("close connection: %v", err)
	}
}

func TestConnectionCloseContinuesAfterWaiterTimeout(t *testing.T) {
	t.Run("caller cancellation only stops waiting", func(t *testing.T) {
		transportFake := newManagedBlockingCloseTransport()
		connection := newManagedConnectionForTestWithTimeout(t, transportFake, newManagedDiagnosticSink(), time.Second)
		if err := connection.Start(context.Background()); err != nil {
			t.Fatalf("start connection: %v", err)
		}
		_, pending, err := connection.registerRequest("tools/call")
		if err != nil {
			t.Fatalf("register pending request: %v", err)
		}

		waitCtx, cancelWait := context.WithCancel(context.Background())
		firstResult := make(chan error, 1)
		go func() {
			firstResult <- connection.Close(waitCtx)
		}()
		waitManagedSignal(t, transportFake.closeStarted, "Transport.Close start")
		cancelWait()
		select {
		case err := <-firstResult:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("first Close = %v, want caller cancellation", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for first Close")
		}

		if outcome := waitManagedOutcome(t, pending); !errors.Is(outcome.err, errManagedConnectionClosed) {
			t.Fatalf("pending close outcome = %#v", outcome)
		}
		if _, _, err := connection.registerRequest("after-close"); !errors.Is(err, errManagedConnectionNotRunning) {
			t.Fatalf("register while closing = %v, want not running", err)
		}
		if state := connection.currentState(); state != managedConnectionStateClosing {
			t.Fatalf("state after caller cancellation = %d, want Closing", state)
		}

		close(transportFake.releaseClose)
		if err := connection.Close(context.Background()); err != nil {
			t.Fatalf("second Close = %v, want final cleanup result", err)
		}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := connection.Close(cancelled); err != nil {
			t.Fatalf("completed Close with cancelled waiter = %v", err)
		}
		if got := transportFake.closeCalls.Load(); got != 1 {
			t.Fatalf("Transport.Close calls = %d, want 1", got)
		}
		if got := connection.pendingCount(); got != 0 {
			t.Fatalf("pending count = %d, want 0", got)
		}
		if state := connection.currentState(); state != managedConnectionStateClosed {
			t.Fatalf("final state = %d, want Closed", state)
		}
	})

	t.Run("hard timeout is stable and diagnosed once", func(t *testing.T) {
		transportFake := newManagedBlockingCloseTransport()
		diagnosticSink := newManagedDiagnosticSink()
		connection := newManagedConnectionForTestWithTimeout(t, transportFake, diagnosticSink, 20*time.Millisecond)
		if err := connection.Start(context.Background()); err != nil {
			t.Fatalf("start connection: %v", err)
		}

		first := connection.Close(context.Background())
		second := connection.Close(context.Background())
		if !errors.Is(first, errManagedConnectionCleanupTimeout) || !errors.Is(second, errManagedConnectionCleanupTimeout) {
			t.Fatalf("hard-timeout results = %v / %v", first, second)
		}
		inputs := diagnosticSink.snapshot()
		if len(inputs) != 1 || inputs[0].Code != diagnosticMCPConnectionTimeout {
			t.Fatalf("cleanup-timeout diagnostics = %#v, want exactly one", inputs)
		}
		if got := transportFake.closeCalls.Load(); got != 1 {
			t.Fatalf("Transport.Close calls = %d, want 1", got)
		}
		if state := connection.currentState(); state != managedConnectionStateClosed {
			t.Fatalf("hard-timeout state = %d, want Closed", state)
		}

		close(transportFake.releaseClose)
		waitManagedSignal(t, connection.transportCloseDone, "blocked Transport.Close completion")
	})
}

type managedReceiveResult struct {
	event transport.TransportEvent
	err   error
}

type managedTransportFake struct {
	receiveResults   chan managedReceiveResult
	closed           chan struct{}
	closeOnce        sync.Once
	startCalls       atomic.Int64
	closeCalls       atomic.Int64
	activeReceive    atomic.Int64
	maxActiveReceive atomic.Int64
}

var _ transport.Transport = (*managedTransportFake)(nil)

func newManagedTransportFake() *managedTransportFake {
	return &managedTransportFake{
		receiveResults: make(chan managedReceiveResult, 16),
		closed:         make(chan struct{}),
	}
}

func (fake *managedTransportFake) Start(context.Context) error {
	fake.startCalls.Add(1)
	return nil
}

func (fake *managedTransportFake) Send(context.Context, json.RawMessage) error {
	return nil
}

func (fake *managedTransportFake) Receive(ctx context.Context) (transport.TransportEvent, error) {
	active := fake.activeReceive.Add(1)
	for {
		maximum := fake.maxActiveReceive.Load()
		if active <= maximum || fake.maxActiveReceive.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer fake.activeReceive.Add(-1)

	select {
	case result := <-fake.receiveResults:
		return result.event, result.err
	case <-fake.closed:
		return transport.TransportEvent{}, transport.ErrClosing
	case <-ctx.Done():
		return transport.TransportEvent{}, ctx.Err()
	}
}

func (fake *managedTransportFake) Close(context.Context) error {
	fake.closeCalls.Add(1)
	fake.closeOnce.Do(func() {
		close(fake.closed)
	})
	return nil
}

type managedBlockingCloseTransport struct {
	closeStarted chan struct{}
	releaseClose chan struct{}
	closeOnce    sync.Once
	closeCalls   atomic.Int64
}

var _ transport.Transport = (*managedBlockingCloseTransport)(nil)

func newManagedBlockingCloseTransport() *managedBlockingCloseTransport {
	return &managedBlockingCloseTransport{
		closeStarted: make(chan struct{}),
		releaseClose: make(chan struct{}),
	}
}

func (*managedBlockingCloseTransport) Start(context.Context) error {
	return nil
}

func (*managedBlockingCloseTransport) Send(context.Context, json.RawMessage) error {
	return nil
}

func (fake *managedBlockingCloseTransport) Receive(ctx context.Context) (transport.TransportEvent, error) {
	select {
	case <-fake.closeStarted:
		return transport.TransportEvent{}, transport.ErrClosing
	case <-ctx.Done():
		return transport.TransportEvent{}, ctx.Err()
	}
}

func (fake *managedBlockingCloseTransport) Close(ctx context.Context) error {
	fake.closeCalls.Add(1)
	fake.closeOnce.Do(func() {
		close(fake.closeStarted)
	})
	select {
	case <-fake.releaseClose:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (fake *managedTransportFake) enqueueFrame(t *testing.T, response protocol.RPCResponse) {
	t.Helper()
	frame, err := protocol.Encode(response)
	if err != nil {
		t.Fatalf("encode response: %v", err)
	}
	fake.receiveResults <- managedReceiveResult{event: transport.TransportEvent{Frame: frame}}
}

type managedDiagnosticSink struct {
	mu     sync.Mutex
	inputs []diagnostics.SanitizeInput
	events chan diagnostics.SanitizeInput
}

var _ diagnostics.BoundedSink = (*managedDiagnosticSink)(nil)

func newManagedDiagnosticSink() *managedDiagnosticSink {
	return &managedDiagnosticSink{events: make(chan diagnostics.SanitizeInput, 16)}
}

func (sink *managedDiagnosticSink) Add(input diagnostics.SanitizeInput) {
	sink.mu.Lock()
	sink.inputs = append(sink.inputs, input)
	sink.mu.Unlock()
	sink.events <- input
}

func (sink *managedDiagnosticSink) wait(t *testing.T) diagnostics.SanitizeInput {
	t.Helper()
	select {
	case input := <-sink.events:
		return input
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for diagnostic")
		return diagnostics.SanitizeInput{}
	}
}

func (sink *managedDiagnosticSink) snapshot() []diagnostics.SanitizeInput {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]diagnostics.SanitizeInput(nil), sink.inputs...)
}

func newManagedConnectionForTest(t *testing.T, transportFake transport.Transport, sink diagnostics.BoundedSink) *managedConnection {
	return newManagedConnectionForTestWithTimeout(t, transportFake, sink, 0)
}

func newManagedConnectionForTestWithTimeout(t *testing.T, transportFake transport.Transport, sink diagnostics.BoundedSink, timeout time.Duration) *managedConnection {
	t.Helper()
	effective, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: 1 << 20})
	if err != nil {
		t.Fatalf("create frame limits: %v", err)
	}
	counter, err := budget.NewCounter(effective, effective)
	if err != nil {
		t.Fatalf("create frame counter: %v", err)
	}
	connection, err := newManagedConnection(managedConnectionOptions{
		Transport:      transportFake,
		FrameCounter:   counter,
		Diagnostics:    sink,
		CleanupTimeout: timeout,
	})
	if err != nil {
		t.Fatalf("create managed connection: %v", err)
	}
	return connection
}

func waitManagedSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitManagedOutcome(t *testing.T, call *pendingCall) pendingOutcome {
	t.Helper()
	select {
	case outcome, open := <-call.outcome:
		if !open {
			t.Fatal("pending outcome channel was closed")
		}
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pending outcome")
		return pendingOutcome{}
	}
}

func waitManagedReceiveDone(t *testing.T, connection *managedConnection) {
	t.Helper()
	select {
	case <-connection.receiveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for receive loop")
	}
}

func containsManagedDiagnosticText(input diagnostics.SanitizeInput, value string) bool {
	return strings.Contains(input.Code, value) ||
		strings.Contains(input.Source, value) ||
		strings.Contains(input.Hint, value) ||
		input.Err != nil && strings.Contains(input.Err.Error(), value)
}
