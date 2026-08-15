package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/budget"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/mcpclient/transport"
)

func TestSessionStartupRollback(t *testing.T) {
	const canary = "session-startup-secret-canary"
	tests := []struct {
		name        string
		configure   func(*sessionTransportFake)
		wantMethods []string
	}{
		{
			name: "transport start",
			configure: func(fake *sessionTransportFake) {
				fake.startErr = errors.New("start failed: " + canary)
			},
		},
		{
			name: "initialize rpc",
			configure: func(fake *sessionTransportFake) {
				fake.initializeRPCError = true
			},
			wantMethods: []string{"initialize"},
		},
		{
			name: "protocol version",
			configure: func(fake *sessionTransportFake) {
				fake.initialize.ProtocolVersion = "unsupported-version"
			},
			wantMethods: []string{"initialize"},
		},
		{
			name: "initialized notification",
			configure: func(fake *sessionTransportFake) {
				fake.notificationErr = errors.New("notification failed: " + canary)
			},
			wantMethods: []string{"initialize", "notifications/initialized"},
		},
		{
			name: "tool discovery",
			configure: func(fake *sessionTransportFake) {
				fake.listRPCErrorAt = 1
			},
			wantMethods: []string{"initialize", "notifications/initialized", "tools/list"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transportFake := newSessionTransportFake()
			test.configure(transportFake)
			session := newServerSessionForTest(t, transportFake, serverSessionOptions{})

			err := session.Start(context.Background())
			if err == nil {
				t.Fatal("startup failure was reported as success")
			}
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("startup error exposed canary: %v", err)
			}
			snapshot := session.Snapshot()
			if snapshot.State != serverSessionStateFailed || snapshot.ProtocolVersion != "" || snapshot.ToolsSupported || len(snapshot.Tools) != 0 {
				t.Fatalf("failed session published partial snapshot: %#v", snapshot)
			}
			if got := transportFake.closeCalls.Load(); got != 1 {
				t.Fatalf("Transport.Close calls = %d, want 1", got)
			}
			if methods := transportFake.methodSnapshot(); !reflect.DeepEqual(methods, test.wantMethods) {
				t.Fatalf("methods = %#v, want %#v", methods, test.wantMethods)
			}
		})
	}
}

func TestSessionDiscoveryUsesCumulativeBudget(t *testing.T) {
	t.Run("page budget is cumulative", func(t *testing.T) {
		transportFake := newSessionTransportFake()
		transportFake.pages = []protocol.ListToolsResult{
			{Tools: []protocol.RemoteTool{sessionTestTool("one")}, NextCursor: "cursor-1"},
			{Tools: []protocol.RemoteTool{sessionTestTool("two")}, NextCursor: "cursor-2"},
			{Tools: []protocol.RemoteTool{sessionTestTool("three")}},
		}
		session := newServerSessionForTest(t, transportFake, serverSessionOptions{MaxPages: 2, MaxTools: 10})

		err := session.Start(context.Background())
		assertSessionLimitScope(t, err, budget.MCPMaxPages)
		assertFailedSessionHasNoTools(t, session)
		if got := transportFake.listCallCount(); got != 2 {
			t.Fatalf("tools/list calls = %d, want 2 before page 3 was rejected", got)
		}
	})

	t.Run("tool budget is cumulative", func(t *testing.T) {
		transportFake := newSessionTransportFake()
		transportFake.pages = []protocol.ListToolsResult{
			{Tools: []protocol.RemoteTool{sessionTestTool("one"), sessionTestTool("two")}, NextCursor: "cursor-1"},
			{Tools: []protocol.RemoteTool{sessionTestTool("three"), sessionTestTool("four")}},
		}
		session := newServerSessionForTest(t, transportFake, serverSessionOptions{MaxPages: 4, MaxTools: 3})

		err := session.Start(context.Background())
		assertSessionLimitScope(t, err, budget.MCPMaxTools)
		assertFailedSessionHasNoTools(t, session)
		if got := transportFake.listCallCount(); got != 2 {
			t.Fatalf("tools/list calls = %d, want 2", got)
		}
	})

	t.Run("response bytes accumulate across pages", func(t *testing.T) {
		transportFake := newSessionTransportFake()
		transportFake.pages = []protocol.ListToolsResult{
			{Tools: []protocol.RemoteTool{sessionTestTool("one")}, NextCursor: "cursor-1"},
			{Tools: []protocol.RemoteTool{sessionTestTool("two")}},
		}
		initializeBytes := sessionResponseSize(t, 1, transportFake.initialize)
		firstPageBytes := sessionResponseSize(t, 2, transportFake.pages[0])
		secondPageBytes := sessionResponseSize(t, 3, transportFake.pages[1])
		limit := initializeBytes + firstPageBytes + secondPageBytes - 1
		session := newServerSessionForTest(t, transportFake, serverSessionOptions{
			MaxResponseBytes: limit,
			MaxPages:         4,
			MaxTools:         10,
		})

		if err := session.Start(context.Background()); err == nil {
			t.Fatal("cumulative response byte overflow was accepted")
		}
		assertFailedSessionHasNoTools(t, session)
		used := session.limits.frames.counter.Snapshot().Used(budget.Bytes)
		if want := initializeBytes + firstPageBytes; used != want {
			t.Fatalf("consumed response bytes = %d, want %d before over-limit frame", used, want)
		}
		if secondPageBytes >= limit {
			t.Fatalf("second page size %d was not individually below cumulative limit %d", secondPageBytes, limit)
		}
		if got := transportFake.listCallCount(); got != 2 {
			t.Fatalf("tools/list calls = %d, want 2", got)
		}
	})

	t.Run("snapshot publishes only after final page", func(t *testing.T) {
		transportFake := newSessionTransportFake()
		transportFake.pages = []protocol.ListToolsResult{
			{Tools: []protocol.RemoteTool{sessionTestTool("one")}, NextCursor: "cursor-1"},
			{Tools: []protocol.RemoteTool{sessionTestTool("two"), sessionTestTool("three")}},
		}
		transportFake.holdListAt = 2
		session := newServerSessionForTest(t, transportFake, serverSessionOptions{MaxPages: 4, MaxTools: 10})
		startResult := make(chan error, 1)
		go func() {
			startResult <- session.Start(context.Background())
		}()

		waitManagedSignal(t, transportFake.holdStarted, "second tools/list response barrier")
		if snapshot := session.Snapshot(); snapshot.State != serverSessionStateDiscovering || len(snapshot.Tools) != 0 {
			t.Fatalf("discovering session published partial tools: %#v", snapshot)
		}
		close(transportFake.holdRelease)
		select {
		case err := <-startResult:
			if err != nil {
				t.Fatalf("successful session startup: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for successful session startup")
		}

		snapshot := session.Snapshot()
		if snapshot.State != serverSessionStateRunning || !snapshot.ToolsSupported || len(snapshot.Tools) != 3 {
			t.Fatalf("running snapshot = %#v", snapshot)
		}
		snapshot.Tools[0].InputSchema[0] = 'X'
		if fresh := session.Snapshot(); fresh.Tools[0].InputSchema[0] == 'X' {
			t.Fatal("snapshot exposed mutable session tool storage")
		}
		if cursors := transportFake.cursorSnapshot(); !reflect.DeepEqual(cursors, []string{"", "cursor-1"}) {
			t.Fatalf("tools/list cursors = %#v", cursors)
		}
		if used := session.limits.pages.counter.Snapshot().Used(budget.Items); used != 2 {
			t.Fatalf("used pages = %d, want 2", used)
		}
		if used := session.limits.tools.counter.Snapshot().Used(budget.Items); used != 3 {
			t.Fatalf("used tools = %d, want 3", used)
		}
		if err := session.connection.Close(context.Background()); err != nil {
			t.Fatalf("close successful session connection: %v", err)
		}
	})
}

func TestServerSessionCallToolUsesProtocolDTO(t *testing.T) {
	transportFake := newSessionTransportFake()
	session := newServerSessionForTest(t, transportFake, serverSessionOptions{})
	if _, err := session.CallTool(context.Background(), "echo", nil); !errors.Is(err, errManagerToolUnavailable) {
		t.Fatalf("pre-start CallTool error = %v", err)
	}
	if err := session.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := session.CallTool(context.Background(), "remote tool/name", map[string]any{"message": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hello" {
		t.Fatalf("CallTool result = %#v", result)
	}
	request := transportFake.callSnapshot()
	if request.Name != "remote tool/name" || request.Arguments["message"] != "hello" {
		t.Fatalf("tools/call request = %#v", request)
	}
	if _, err := session.CallTool(context.Background(), "", nil); !errors.Is(err, errManagerToolUnavailable) {
		t.Fatalf("empty remote tool name error = %v", err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteSessionCleanupPrecedesLocalClose(t *testing.T) {
	transportFake := newSessionTransportFake()
	session := newServerSessionForTest(t, transportFake, serverSessionOptions{})
	if err := session.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := transportFake.lifecycleSnapshot(); !reflect.DeepEqual(got, []string{"remote-close", "local-close"}) {
		t.Fatalf("session cleanup order = %#v", got)
	}
	if transportFake.remoteCloseCalls.Load() != 1 || transportFake.closeCalls.Load() != 1 {
		t.Fatalf("remote/local close calls = %d/%d, want 1/1", transportFake.remoteCloseCalls.Load(), transportFake.closeCalls.Load())
	}
	if snapshot := session.Snapshot(); snapshot.State != serverSessionStateClosed || len(snapshot.Tools) != 0 {
		t.Fatalf("closed session snapshot = %#v", snapshot)
	}
}

func TestRemoteCleanupFailureStillClosesLocally(t *testing.T) {
	const canary = "remote-cleanup-secret-canary"
	transportFake := newSessionTransportFake()
	transportFake.remoteCloseErr = errors.New("remote delete failed: " + canary)
	diagnosticSink := newManagedDiagnosticSink()
	session, err := newServerSession(serverSessionOptions{
		Transport:   transportFake,
		Diagnostics: diagnosticSink,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("best-effort remote failure changed final local result: %v", err)
	}
	if got := transportFake.lifecycleSnapshot(); !reflect.DeepEqual(got, []string{"remote-close", "local-close"}) {
		t.Fatalf("failed remote cleanup order = %#v", got)
	}
	inputs := diagnosticSink.snapshot()
	if len(inputs) != 1 || inputs[0].Code != diagnosticMCPRemoteSessionCleanupFailed ||
		!errors.Is(inputs[0].Err, errRemoteSessionCleanupFailed) {
		t.Fatalf("remote cleanup diagnostics = %#v", inputs)
	}
	if containsManagedDiagnosticText(inputs[0], canary) {
		t.Fatalf("remote cleanup diagnostic exposed canary: %#v", inputs[0])
	}
	if transportFake.closeCalls.Load() != 1 {
		t.Fatalf("local close calls = %d, want 1", transportFake.closeCalls.Load())
	}
}

func TestSessionCloseContinuesAfterWaiterTimeout(t *testing.T) {
	t.Run("caller timeout does not cancel cleanup", func(t *testing.T) {
		transportFake := newSessionTransportFake()
		transportFake.blockRemoteClose = true
		session := newServerSessionForTest(t, transportFake, serverSessionOptions{CleanupTimeout: time.Second})
		if err := session.Start(context.Background()); err != nil {
			t.Fatal(err)
		}

		waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		firstResult := make(chan error, 1)
		go func() { firstResult <- session.Close(waitCtx) }()
		waitManagedSignal(t, transportFake.remoteCloseStarted, "remote session cleanup start")
		select {
		case err := <-firstResult:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("first close result = %v, want caller deadline", err)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for caller-limited Close")
		}
		if transportFake.closeCalls.Load() != 0 {
			t.Fatal("local cleanup ran before the blocked remote cleanup finished")
		}

		close(transportFake.remoteCloseRelease)
		if err := session.Close(context.Background()); err != nil {
			t.Fatalf("stable final close result: %v", err)
		}
		cancelled, cancelNow := context.WithCancel(context.Background())
		cancelNow()
		if err := session.Close(cancelled); err != nil {
			t.Fatalf("completed cleanup did not win over cancelled waiter: %v", err)
		}
		if transportFake.remoteCloseCalls.Load() != 1 || transportFake.closeCalls.Load() != 1 {
			t.Fatalf("remote/local close calls = %d/%d", transportFake.remoteCloseCalls.Load(), transportFake.closeCalls.Load())
		}
	})

	t.Run("internal hard timeout is stable and diagnosed once", func(t *testing.T) {
		transportFake := newSessionTransportFake()
		transportFake.blockRemoteClose = true
		diagnosticSink := newManagedDiagnosticSink()
		session, err := newServerSession(serverSessionOptions{
			Transport:      transportFake,
			Diagnostics:    diagnosticSink,
			CleanupTimeout: 20 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Start(context.Background()); err != nil {
			t.Fatal(err)
		}

		first := session.Close(context.Background())
		second := session.Close(context.Background())
		if !errors.Is(first, errServerSessionCleanupTimeout) || !errors.Is(second, errServerSessionCleanupTimeout) {
			t.Fatalf("hard-timeout results = %v / %v", first, second)
		}
		waitManagedSignal(t, session.connection.transportCloseDone, "local Transport.Close after remote timeout")
		inputs := diagnosticSink.snapshot()
		if len(inputs) != 1 || inputs[0].Code != diagnosticMCPServerSessionTimeout {
			t.Fatalf("session timeout diagnostics = %#v, want exactly one", inputs)
		}
		if transportFake.remoteCloseCalls.Load() != 1 || transportFake.closeCalls.Load() != 1 {
			t.Fatalf("hard-timeout remote/local calls = %d/%d", transportFake.remoteCloseCalls.Load(), transportFake.closeCalls.Load())
		}
	})
}

func assertSessionLimitScope(t *testing.T, err error, scope budget.Scope) {
	t.Helper()
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) || limitErr.Scope != string(scope) {
		t.Fatalf("limit error = %T %v, want scope %s", err, err, scope)
	}
}

func assertFailedSessionHasNoTools(t *testing.T, session *serverSession) {
	t.Helper()
	snapshot := session.Snapshot()
	if snapshot.State != serverSessionStateFailed || len(snapshot.Tools) != 0 || snapshot.ToolsSupported {
		t.Fatalf("failed session snapshot = %#v", snapshot)
	}
}

func newServerSessionForTest(t *testing.T, transportFake transport.Transport, override serverSessionOptions) *serverSession {
	t.Helper()
	override.Transport = transportFake
	override.Diagnostics = newManagedDiagnosticSink()
	session, err := newServerSession(override)
	if err != nil {
		t.Fatalf("create server session: %v", err)
	}
	return session
}

func sessionTestTool(name string) protocol.RemoteTool {
	return protocol.RemoteTool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func sessionResponseSize(t *testing.T, id int64, result any) int64 {
	t.Helper()
	resultFrame, err := protocol.Encode(result)
	if err != nil {
		t.Fatalf("encode response result: %v", err)
	}
	frame, err := protocol.Encode(protocol.RPCResponse{JSONRPC: "2.0", ID: protocol.NumberID(id), Result: resultFrame})
	if err != nil {
		t.Fatalf("encode response frame: %v", err)
	}
	return int64(len(frame))
}

type sessionTransportFake struct {
	mu sync.Mutex

	startErr           error
	initialize         protocol.InitializeResult
	initializeRPCError bool
	notificationErr    error
	listRPCErrorAt     int
	pages              []protocol.ListToolsResult
	methods            []string
	cursors            []string
	listCalls          int
	callRequest        protocol.CallToolRequest
	lifecycle          []string
	remoteCloseErr     error
	blockRemoteClose   bool

	holdListAt  int
	holdStarted chan struct{}
	holdRelease chan struct{}
	holdOnce    sync.Once

	responses          chan transport.TransportEvent
	closed             chan struct{}
	closeOnce          sync.Once
	startCalls         atomic.Int64
	closeCalls         atomic.Int64
	remoteCloseCalls   atomic.Int64
	remoteCloseStarted chan struct{}
	remoteCloseRelease chan struct{}
	remoteCloseOnce    sync.Once
}

var _ transport.Transport = (*sessionTransportFake)(nil)

func newSessionTransportFake() *sessionTransportFake {
	return &sessionTransportFake{
		initialize: protocol.InitializeResult{
			ProtocolVersion: protocol.SupportedProtocolVersion,
			Capabilities:    map[string]any{"tools": map[string]any{}},
		},
		pages:              []protocol.ListToolsResult{{}},
		holdStarted:        make(chan struct{}),
		holdRelease:        make(chan struct{}),
		responses:          make(chan transport.TransportEvent, 16),
		closed:             make(chan struct{}),
		remoteCloseStarted: make(chan struct{}),
		remoteCloseRelease: make(chan struct{}),
	}
}

func (fake *sessionTransportFake) Start(context.Context) error {
	fake.startCalls.Add(1)
	return fake.startErr
}

func (fake *sessionTransportFake) Send(ctx context.Context, frame json.RawMessage) error {
	var envelope struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return errors.New("fake received malformed request")
	}

	fake.mu.Lock()
	fake.methods = append(fake.methods, envelope.Method)
	fake.mu.Unlock()
	if envelope.Method == "notifications/initialized" {
		return fake.notificationErr
	}

	var request protocol.RPCRequest
	if err := json.Unmarshal(frame, &request); err != nil {
		return errors.New("fake received invalid rpc request")
	}
	response := protocol.RPCResponse{JSONRPC: "2.0", ID: request.ID}
	var result any
	switch request.Method {
	case "initialize":
		if fake.initializeRPCError {
			response.Error = &protocol.RPCError{Code: -32000, Message: "initialize failed: session-startup-secret-canary"}
		} else {
			result = fake.initialize
		}
	case "tools/list":
		var params protocol.ListToolsRequest
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return errors.New("fake received invalid tools/list params")
		}
		fake.mu.Lock()
		fake.listCalls++
		listCall := fake.listCalls
		fake.cursors = append(fake.cursors, params.Cursor)
		fake.mu.Unlock()
		if fake.listRPCErrorAt == listCall {
			response.Error = &protocol.RPCError{Code: -32001, Message: "list failed: session-startup-secret-canary"}
		} else if listCall <= len(fake.pages) {
			result = fake.pages[listCall-1]
		} else {
			result = protocol.ListToolsResult{}
		}
		if fake.holdListAt == listCall {
			fake.holdOnce.Do(func() { close(fake.holdStarted) })
			select {
			case <-fake.holdRelease:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	case "tools/call":
		var params protocol.CallToolRequest
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return errors.New("fake received invalid tools/call params")
		}
		fake.mu.Lock()
		fake.callRequest = protocol.CallToolRequest{Name: params.Name, Arguments: cloneMCPArguments(params.Arguments)}
		fake.mu.Unlock()
		message, _ := params.Arguments["message"].(string)
		result = protocol.CallToolResult{Content: []protocol.ContentBlock{{Type: "text", Text: message}}}
	default:
		return errors.New("fake received unexpected method")
	}
	if response.Error == nil {
		resultFrame, err := protocol.Encode(result)
		if err != nil {
			return err
		}
		response.Result = resultFrame
	}
	responseFrame, err := protocol.Encode(response)
	if err != nil {
		return err
	}
	select {
	case fake.responses <- transport.TransportEvent{Frame: responseFrame}:
		return nil
	case <-fake.closed:
		return transport.ErrClosing
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (fake *sessionTransportFake) Receive(ctx context.Context) (transport.TransportEvent, error) {
	select {
	case response := <-fake.responses:
		return response, nil
	case <-fake.closed:
		return transport.TransportEvent{}, transport.ErrClosing
	case <-ctx.Done():
		return transport.TransportEvent{}, ctx.Err()
	}
}

func (fake *sessionTransportFake) Close(context.Context) error {
	fake.closeCalls.Add(1)
	fake.mu.Lock()
	fake.lifecycle = append(fake.lifecycle, "local-close")
	fake.mu.Unlock()
	fake.closeOnce.Do(func() { close(fake.closed) })
	return nil
}

func (fake *sessionTransportFake) CloseRemote(ctx context.Context) error {
	fake.remoteCloseCalls.Add(1)
	fake.mu.Lock()
	fake.lifecycle = append(fake.lifecycle, "remote-close")
	block := fake.blockRemoteClose
	err := fake.remoteCloseErr
	fake.mu.Unlock()
	fake.remoteCloseOnce.Do(func() { close(fake.remoteCloseStarted) })
	if block {
		select {
		case <-fake.remoteCloseRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (fake *sessionTransportFake) methodSnapshot() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.methods...)
}

func (fake *sessionTransportFake) cursorSnapshot() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.cursors...)
}

func (fake *sessionTransportFake) listCallCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.listCalls
}

func (fake *sessionTransportFake) callSnapshot() protocol.CallToolRequest {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return protocol.CallToolRequest{Name: fake.callRequest.Name, Arguments: cloneMCPArguments(fake.callRequest.Arguments)}
}

func (fake *sessionTransportFake) lifecycleSnapshot() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.lifecycle...)
}
