package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/budget"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/permission"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

func TestRemoteToolDefaultsFailClosed(t *testing.T) {
	digest := sha256.Sum256([]byte("final server configuration"))
	annotations := json.RawMessage(`{
		"readOnlyHint": true,
		"destructiveHint": false,
		"idempotentHint": true,
		"openWorldHint": false
	}`)
	adapter := newBoundToolAdapterForTest(t, "mcp__server__tool", digest, annotations, &fakeToolCaller{}, nil)

	registry, err := tool.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterWithOptions(adapter, adapter.RegistrationOptions()); err != nil {
		t.Fatal(err)
	}
	descriptor, ok := registry.Descriptor(adapter.Name())
	if !ok {
		t.Fatal("remote tool descriptor was not registered")
	}
	if descriptor.Risk != tool.RiskDangerous {
		t.Fatalf("remote tool risk = %q, want dangerous", descriptor.Risk)
	}
	if descriptor.Policy.ReadOnly || descriptor.Policy.ConcurrentSafe || descriptor.Policy.AllowsConcurrentExecution() {
		t.Fatalf("remote annotations loosened local fail-closed policy: %#v", descriptor.Policy)
	}
	if descriptor.TargetDigest == nil || *descriptor.TargetDigest != digest {
		t.Fatalf("server configuration digest was not frozen at registration: %#v", descriptor.TargetDigest)
	}

	// Both the constructor and accessor must detach remote-owned annotation bytes.
	annotations[0] = '['
	first := adapter.RegistrationOptions()
	first.RemoteAnnotations[0] = '['
	second := adapter.RegistrationOptions()
	if len(second.RemoteAnnotations) == 0 || second.RemoteAnnotations[0] != '{' {
		t.Fatal("registration options alias remote or caller-owned annotation memory")
	}
}

func TestMCPIdentityBindsServerAndArguments(t *testing.T) {
	firstDigest := sha256.Sum256([]byte("server configuration A"))
	secondDigest := sha256.Sum256([]byte("server configuration B"))

	first := mcpIdentityForTest(t, "mcp__server__tool", firstDigest, `{"label":"same","count":2}`)
	reordered := mcpIdentityForTest(t, "mcp__server__tool", firstDigest, `{"count":2,"label":"same"}`)
	if first != reordered {
		t.Fatal("equivalent canonical MCP arguments produced different identities")
	}
	changedArguments := mcpIdentityForTest(t, "mcp__server__tool", firstDigest, `{"count":3,"label":"same"}`)
	if first == changedArguments {
		t.Fatal("different MCP arguments reused one call identity")
	}
	changedServer := mcpIdentityForTest(t, "mcp__server__tool", secondDigest, `{"count":2,"label":"same"}`)
	if first == changedServer {
		t.Fatal("different server configurations reused one call identity")
	}
	changedRegistration := mcpIdentityForTest(t, "mcp__other__tool", firstDigest, `{"count":2,"label":"same"}`)
	if first == changedRegistration {
		t.Fatal("different registered names reused one call identity")
	}
}

func TestAdapterCanaryReturnsOnlySafeViews(t *testing.T) {
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(mcpSecretCanary)
	factory, err := tool.NewResultFactory(runtimeRedactor)
	if err != nil {
		t.Fatal("create MCP canary result factory")
	}
	digest := sha256.Sum256([]byte("server configuration"))
	caller := &fakeToolCaller{result: protocol.CallToolResult{
		Content: []protocol.ContentBlock{
			{Type: "text", Text: "visible text " + mcpSecretCanary},
			{Type: "resource", Raw: json.RawMessage(`{"type":"resource","text":"` + mcpSecretCanary + `"}`)},
		},
		StructuredContent: map[string]any{"answer": "structured " + mcpSecretCanary},
	}}
	adapter := newBoundToolAdapterForTest(t, "mcp__server__tool", digest, nil, caller, factory)
	result := adapter.Execute(context.Background(), tool.Input{
		Name:      adapter.Name(),
		CallID:    "call-success",
		Arguments: map[string]any{"query": "value"},
	})
	if result.ExecutionState() != tool.Completed || result.Status != tool.StatusSuccess {
		t.Fatal("successful MCP canary result has an unexpected state or status")
	}
	if result.Data != nil {
		t.Fatal("MCP adapter bypassed safe views through compatibility Data")
	}
	assertNoMCPResultCanary(t, mcpSecretCanary, result)
	if !strings.Contains(result.ModelContent().Text(), "[redacted]") || !strings.Contains(result.UserView().Preview.Text(), "[redacted]") {
		t.Fatal("shared runtime redactor was not applied to MCP output views")
	}
	if strings.Contains(result.PersistedContent().Text(), "visible text") {
		t.Fatal("persisted MCP result retained the model/user preview")
	}
	if caller.registeredName != adapter.Name() || caller.arguments["query"] != "value" {
		t.Fatal("adapter sent the wrong registered MCP call")
	}

	remoteFailure := newBoundToolAdapterForTest(t, "mcp__server__failure", digest, nil, &fakeToolCaller{
		err: errors.New("remote failure " + mcpSecretCanary),
	}, factory).Execute(context.Background(), tool.Input{Name: "mcp__server__failure", CallID: "call-error"})
	if remoteFailure.ExecutionState() != tool.Completed || remoteFailure.Status != tool.StatusError || remoteFailure.Error == nil {
		t.Fatal("failed MCP canary call has an unexpected safe result")
	}
	assertNoMCPResultCanary(t, mcpSecretCanary, remoteFailure)

	remoteToolError := newBoundToolAdapterForTest(t, "mcp__server__tool_error", digest, nil, &fakeToolCaller{
		result: protocol.CallToolResult{
			Content: []protocol.ContentBlock{
				{Type: "text", Text: "remote tool error " + mcpSecretCanary},
				{Type: "resource", Raw: json.RawMessage(`{"type":"resource","text":"` + mcpSecretCanary + `"}`)},
			},
			StructuredContent: map[string]any{"error": mcpSecretCanary},
			IsError:           true,
		},
	}, factory).Execute(context.Background(), tool.Input{Name: "mcp__server__tool_error", CallID: "call-tool-error"})
	if remoteToolError.ExecutionState() != tool.Completed || remoteToolError.Status != tool.StatusError ||
		remoteToolError.Error == nil || remoteToolError.Error.Code != tool.ErrCommandFailed {
		t.Fatal("IsError MCP canary call has an unexpected safe result")
	}
	assertNoMCPResultCanary(t, mcpSecretCanary, remoteToolError)

	timedOut := newBoundToolAdapterForTest(t, "mcp__server__timeout", digest, nil, &fakeToolCaller{
		err: context.DeadlineExceeded,
	}, factory).Execute(context.Background(), tool.Input{Name: "mcp__server__timeout", CallID: "call-timeout"})
	if timedOut.ExecutionState() != tool.CancelledAfterStart || timedOut.Status != tool.StatusTimeout || timedOut.Error == nil || timedOut.Error.Code != tool.ErrTimeout {
		t.Fatal("timed-out MCP canary call has an unexpected safe result")
	}
	assertNoMCPResultCanary(t, mcpSecretCanary, timedOut)
}

func TestMCPAdapterUsesInjectedResultFactoryOnEveryOutcome(t *testing.T) {
	const factoryCanary = "mcp-injected-factory-canary"
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(factoryCanary)
	factory, err := tool.NewResultFactory(runtimeRedactor)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("required MCP result boundary"))
	outcomes := []ToolAdapter{
		newBoundToolAdapterWithCaptureForTest(t, "mcp__server__success", digest, nil, &fakeToolCaller{result: protocol.CallToolResult{Content: []protocol.ContentBlock{{Type: "text", Text: factoryCanary}}}}, factory, newMCPTestCaptureFactory(t, 64, 128)),
		newBoundToolAdapterWithCaptureForTest(t, "mcp__server__tool_error", digest, nil, &fakeToolCaller{result: protocol.CallToolResult{IsError: true, Content: []protocol.ContentBlock{{Type: "text", Text: "failed"}}}}, factory, newMCPTestCaptureFactory(t, 8, 64)),
		newBoundToolAdapterWithCaptureForTest(t, "mcp__server__call_error", digest, nil, &fakeToolCaller{err: errors.New("remote failure")}, factory, newMCPTestCaptureFactory(t, 8, 64)),
		newBoundToolAdapterWithCaptureForTest(t, "mcp__server__timeout", digest, nil, &fakeToolCaller{err: context.DeadlineExceeded}, factory, newMCPTestCaptureFactory(t, 8, 64)),
		newBoundToolAdapterWithCaptureForTest(t, "mcp__server__capture_begin", digest, nil, &fakeToolCaller{}, factory, func(context.Context, artifact.Metadata) (*tool.Capture, error) {
			return nil, errors.New("begin failed")
		}),
	}
	for index, adapter := range outcomes {
		result := adapter.Execute(context.Background(), tool.Input{Name: adapter.Name(), CallID: "required", Arguments: map[string]any{}})
		if !result.ExecutionState().CanProduceResult() || result.ModelContent().Text() == "" || result.PersistedContent().Text() == "" || !result.UserView().State.CanProduceResult() {
			t.Fatalf("MCP outcome %d bypassed the injected result factory", index)
		}
		if result.Data != nil {
			t.Fatalf("MCP outcome %d published compatibility data", index)
		}
		if index == 0 {
			views := result.ModelContent().Text() + result.UserView().Preview.Text() + result.PersistedContent().Text()
			if strings.Contains(views, factoryCanary) || !strings.Contains(views, "[redacted]") {
				t.Fatal("MCP success did not pass through the injected RuntimeRedactor/ResultFactory identity")
			}
		}
	}

	freshTracker := newTrackedMCPCaptureFactory(t, 8, 64, func() *trackedMCPWriter {
		return &trackedMCPWriter{maxWrite: -1}
	})
	freshCaller := &fakeToolCaller{result: protocol.CallToolResult{Content: []protocol.ContentBlock{{Type: "text", Text: "ok"}}}}
	capturePrecededCall := true
	freshCaller.beforeCall = func() {
		if freshTracker.calls != freshCaller.calls {
			capturePrecededCall = false
		}
	}
	freshAdapter := newBoundToolAdapterWithCaptureForTest(t, "mcp__server__fresh", digest, nil, freshCaller, factory, freshTracker.Capture)
	for index := 0; index < 2; index++ {
		result := freshAdapter.Execute(context.Background(), tool.Input{Name: freshAdapter.Name(), CallID: fmt.Sprintf("fresh-%d", index)})
		if result.Status != tool.StatusSuccess || result.ModelContent().Text() == "" {
			t.Fatalf("fresh MCP call %d did not publish a safe result", index)
		}
	}
	if !capturePrecededCall || freshTracker.calls != 2 || freshCaller.calls != 2 || len(freshTracker.captures) != 2 ||
		freshTracker.captures[0] == freshTracker.captures[1] || freshTracker.stores[0] == freshTracker.stores[1] {
		t.Fatalf("MCP calls did not create fresh Capture/Writer before CallTool: captures=%d caller=%d", freshTracker.calls, freshCaller.calls)
	}
	for index, store := range freshTracker.stores {
		if store.begins != 1 || store.writer.commits != 0 || store.writer.aborts != 1 {
			t.Fatalf("fresh MCP call %d terminal counts = begin:%d commit:%d abort:%d", index, store.begins, store.writer.commits, store.writer.aborts)
		}
	}

	hardLimitTracker := newTrackedMCPCaptureFactory(t, 2, 8, func() *trackedMCPWriter {
		return &trackedMCPWriter{maxWrite: -1}
	})
	hardLimitAdapter := newBoundToolAdapterWithCaptureForTest(t, "mcp__server__hard_limit", digest, nil, &fakeToolCaller{result: protocol.CallToolResult{Content: []protocol.ContentBlock{{Type: "text", Text: "123456789"}}}}, factory, hardLimitTracker.Capture)
	hardLimitResult := hardLimitAdapter.Execute(context.Background(), tool.Input{Name: hardLimitAdapter.Name(), CallID: "hard-limit"})
	hardLimitMeta := hardLimitResult.OutputMeta()
	if hardLimitResult.Status != tool.StatusError || hardLimitMeta.Artifact == nil || hardLimitMeta.Artifact.Complete || hardLimitMeta.CapturedBytes != 8 ||
		hardLimitMeta.TruncationReason.Text() != string(tool.CaptureTruncatedHardLimit) || hardLimitTracker.stores[0].writer.commits != 1 || hardLimitTracker.stores[0].writer.aborts != 0 {
		t.Fatalf("MCP hard-limit capture = result:%#v meta:%#v writer:%#v", hardLimitResult, hardLimitMeta, hardLimitTracker.stores[0].writer)
	}

	partialErr := errors.New("partial MCP staging write")
	partialTracker := newTrackedMCPCaptureFactory(t, 8, 64, func() *trackedMCPWriter {
		return &trackedMCPWriter{maxWrite: 3, writeErr: partialErr}
	})
	partialAdapter := newBoundToolAdapterWithCaptureForTest(t, "mcp__server__partial_write", digest, nil, &fakeToolCaller{result: protocol.CallToolResult{Content: []protocol.ContentBlock{{Type: "text", Text: "123456789"}}}}, factory, partialTracker.Capture)
	partialResult := partialAdapter.Execute(context.Background(), tool.Input{Name: partialAdapter.Name(), CallID: "partial"})
	partialMeta := partialResult.OutputMeta()
	if partialResult.Status != tool.StatusError || partialMeta.Artifact == nil || partialMeta.Artifact.Complete || partialMeta.CapturedBytes != 3 ||
		partialMeta.TruncationReason.Text() != string(tool.CaptureTruncatedWriteFailure) || partialTracker.stores[0].writer.commits != 1 || partialTracker.stores[0].writer.aborts != 0 {
		t.Fatalf("MCP partial-write capture = result:%#v meta:%#v writer:%#v", partialResult, partialMeta, partialTracker.stores[0].writer)
	}

	zeroTracker := newTrackedMCPCaptureFactory(t, 8, 64, func() *trackedMCPWriter {
		return &trackedMCPWriter{maxWrite: 0, writeErr: errors.New("zero-byte MCP staging write")}
	})
	zeroAdapter := newBoundToolAdapterWithCaptureForTest(t, "mcp__server__zero_write", digest, nil, &fakeToolCaller{result: protocol.CallToolResult{Content: []protocol.ContentBlock{{Type: "text", Text: "payload"}}}}, factory, zeroTracker.Capture)
	zeroResult := zeroAdapter.Execute(context.Background(), tool.Input{Name: zeroAdapter.Name(), CallID: "zero"})
	zeroMeta := zeroResult.OutputMeta()
	if zeroResult.Status != tool.StatusError || zeroMeta.Artifact != nil || zeroMeta.CapturedBytes != 0 || zeroMeta.Truncated || zeroMeta.TruncationReason.Text() != "" ||
		zeroTracker.stores[0].writer.commits != 0 || zeroTracker.stores[0].writer.aborts != 1 {
		t.Fatalf("MCP zero-write normalization = result:%#v meta:%#v writer:%#v", zeroResult, zeroMeta, zeroTracker.stores[0].writer)
	}

	commitTracker := newTrackedMCPCaptureFactory(t, 8, 64, func() *trackedMCPWriter {
		return &trackedMCPWriter{maxWrite: -1, commitErr: errors.New("MCP commit failed")}
	})
	commitAdapter := newBoundToolAdapterWithCaptureForTest(t, "mcp__server__commit_failure", digest, nil, &fakeToolCaller{result: protocol.CallToolResult{Content: []protocol.ContentBlock{{Type: "text", Text: "123456789"}}}}, factory, commitTracker.Capture)
	commitResult := commitAdapter.Execute(context.Background(), tool.Input{Name: commitAdapter.Name(), CallID: "commit"})
	commitMeta := commitResult.OutputMeta()
	if commitResult.Status != tool.StatusError || commitMeta.Artifact != nil || commitMeta.CapturedBytes != 0 || commitMeta.Truncated || commitMeta.TruncationReason.Text() != "" ||
		commitTracker.stores[0].writer.commits != 1 || commitTracker.stores[0].writer.aborts != 0 {
		t.Fatalf("MCP commit-failure normalization = result:%#v meta:%#v writer:%#v", commitResult, commitMeta, commitTracker.stores[0].writer)
	}

	legacy := newBoundToolAdapterWithCaptureForTest(t, "mcp__server__legacy", digest, nil, &fakeToolCaller{}, factory, nil)
	legacyResult := legacy.Execute(context.Background(), tool.Input{Name: legacy.Name(), CallID: "legacy", Arguments: map[string]any{}})
	if legacyResult.ModelContent().Text() != "" || legacyResult.PersistedContent().Text() != "" || legacyResult.UserView().State != "" {
		t.Fatal("legacy MCP adapter entered the safe projection path")
	}
	if !freshAdapter.UsesSafeResultBoundary() || legacy.UsesSafeResultBoundary() {
		t.Fatal("MCP safe/legacy adapters reported the wrong result boundary")
	}
	registry := tool.NewSafeCandidateRegistry()
	if err := registry.Register(freshAdapter); err != nil {
		t.Fatalf("safe candidate registry rejected safe MCP adapter: %v", err)
	}
	if err := registry.Register(legacy); err == nil {
		t.Fatal("safe candidate registry accepted legacy MCP adapter")
	}
	baseOptions := RemoteToolAdapterOptions{
		RegisteredName: "mcp__server__missing", ServerName: "server", RemoteName: "missing",
		Schema: tool.Schema{Raw: json.RawMessage(`{"type":"object"}`)}, ServerConfigDigest: digest,
		Caller: &fakeToolCaller{}, ResultFactory: factory,
	}
	if _, err = NewRemoteToolAdapter(baseOptions); err == nil {
		t.Fatal("safe MCP adapter accepted a missing Capture")
	}
	baseOptions.Capture = newMCPTestCaptureFactory(t, 8, 64)
	if _, err = NewLegacyRemoteToolAdapter(baseOptions); err == nil {
		t.Fatal("legacy MCP adapter accepted a Capture")
	}
}

func mcpIdentityForTest(t *testing.T, registeredName string, digest [32]byte, arguments string) permission.CallIdentity {
	t.Helper()
	adapter := newBoundToolAdapterForTest(t, registeredName, digest, json.RawMessage(`{"readOnlyHint":true,"idempotentHint":true}`), &fakeToolCaller{}, nil)
	registry, err := tool.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterWithOptions(adapter, adapter.RegistrationOptions()); err != nil {
		t.Fatal(err)
	}
	validated, err := registry.ValidateCall(tool.Call{Name: registeredName, ArgumentsJSON: arguments})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := permission.NewCallIdentity(validated.IdentityInput())
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func newBoundToolAdapterForTest(
	t *testing.T,
	registeredName string,
	digest [32]byte,
	annotations json.RawMessage,
	caller ToolCaller,
	factory *tool.ResultFactory,
) ToolAdapter {
	return newBoundToolAdapterWithCaptureForTest(t, registeredName, digest, annotations, caller, factory, newMCPTestCaptureFactory(t, 4096, 1<<20))
}

func newBoundToolAdapterWithCaptureForTest(
	t *testing.T,
	registeredName string,
	digest [32]byte,
	annotations json.RawMessage,
	caller ToolCaller,
	factory *tool.ResultFactory,
	capture func(context.Context, artifact.Metadata) (*tool.Capture, error),
) ToolAdapter {
	t.Helper()
	if factory == nil {
		var err error
		factory, err = tool.NewResultFactory(redact.NewRuntimeRedactor())
		if err != nil {
			t.Fatal(err)
		}
	}
	options := RemoteToolAdapterOptions{
		RegisteredName:     registeredName,
		ServerName:         "server",
		RemoteName:         "tool",
		Description:        "remote description",
		Schema:             tool.Schema{Raw: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"},"label":{"type":"string"},"query":{"type":"string"}},"additionalProperties":false}`)},
		ServerConfigDigest: digest,
		RemoteAnnotations:  annotations,
		Caller:             caller,
		ResultFactory:      factory,
		Capture:            capture,
	}
	var adapter ToolAdapter
	var err error
	if capture == nil {
		adapter, err = NewLegacyRemoteToolAdapter(options)
	} else {
		adapter, err = NewRemoteToolAdapter(options)
	}
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func newMCPTestCaptureFactory(t *testing.T, inlineBytes, captureBytes int64) func(context.Context, artifact.Metadata) (*tool.Capture, error) {
	t.Helper()
	return func(ctx context.Context, metadata artifact.Metadata) (*tool.Capture, error) {
		limits, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: captureBytes})
		if err != nil {
			return nil, err
		}
		counter, err := budget.NewCounter(limits, limits)
		if err != nil {
			return nil, err
		}
		return tool.NewCapture(ctx, tool.CaptureOptions{Store: mcpCaptureStore{}, Counter: counter, InlineBytes: inlineBytes, Metadata: metadata})
	}
}

type mcpCaptureStore struct{}

func (mcpCaptureStore) Begin(context.Context, artifact.Metadata) (artifact.Writer, error) {
	return &mcpCaptureWriter{}, nil
}
func (mcpCaptureStore) OpenForUser(context.Context, string) (io.ReadCloser, artifact.Ref, error) {
	return nil, artifact.Ref{}, errors.New("not implemented")
}
func (mcpCaptureStore) Cleanup(context.Context) (artifact.CleanupResult, error) {
	return artifact.CleanupResult{}, nil
}
func (mcpCaptureStore) Close() error { return nil }

type mcpCaptureWriter struct{ bytes int64 }

func (w *mcpCaptureWriter) Write(input []byte) (int, error) {
	w.bytes += int64(len(input))
	return len(input), nil
}
func (w *mcpCaptureWriter) Commit(context.Context) (artifact.Ref, error) {
	return artifact.Ref{ID: strings.Repeat("d", 64), Bytes: w.bytes, CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Available: true, Complete: true}, nil
}
func (*mcpCaptureWriter) Abort() error { return nil }

type trackedMCPCaptureFactory struct {
	t            *testing.T
	inlineBytes  int64
	captureBytes int64
	newWriter    func() *trackedMCPWriter
	calls        int
	captures     []*tool.Capture
	stores       []*trackedMCPStore
}

func newTrackedMCPCaptureFactory(t *testing.T, inlineBytes, captureBytes int64, newWriter func() *trackedMCPWriter) *trackedMCPCaptureFactory {
	t.Helper()
	return &trackedMCPCaptureFactory{t: t, inlineBytes: inlineBytes, captureBytes: captureBytes, newWriter: newWriter}
}

func (f *trackedMCPCaptureFactory) Capture(ctx context.Context, metadata artifact.Metadata) (*tool.Capture, error) {
	f.t.Helper()
	limits, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: f.captureBytes})
	if err != nil {
		return nil, err
	}
	counter, err := budget.NewCounter(limits, limits)
	if err != nil {
		return nil, err
	}
	writer := f.newWriter()
	store := &trackedMCPStore{writer: writer}
	capture, err := tool.NewCapture(ctx, tool.CaptureOptions{Store: store, Counter: counter, InlineBytes: f.inlineBytes, Metadata: metadata})
	if err != nil {
		return nil, err
	}
	f.calls++
	f.captures = append(f.captures, capture)
	f.stores = append(f.stores, store)
	return capture, nil
}

type trackedMCPStore struct {
	writer *trackedMCPWriter
	begins int
}

func (s *trackedMCPStore) Begin(context.Context, artifact.Metadata) (artifact.Writer, error) {
	s.begins++
	return s.writer, nil
}
func (*trackedMCPStore) OpenForUser(context.Context, string) (io.ReadCloser, artifact.Ref, error) {
	return nil, artifact.Ref{}, errors.New("not implemented")
}
func (*trackedMCPStore) Cleanup(context.Context) (artifact.CleanupResult, error) {
	return artifact.CleanupResult{}, nil
}
func (*trackedMCPStore) Close() error { return nil }

type trackedMCPWriter struct {
	bytes     int64
	maxWrite  int
	writeErr  error
	commitErr error
	commits   int
	aborts    int
}

func (w *trackedMCPWriter) Write(input []byte) (int, error) {
	written := len(input)
	if w.maxWrite >= 0 && written > w.maxWrite {
		written = w.maxWrite
	}
	w.bytes += int64(written)
	return written, w.writeErr
}

func (w *trackedMCPWriter) Commit(context.Context) (artifact.Ref, error) {
	w.commits++
	if w.commitErr != nil {
		return artifact.Ref{}, w.commitErr
	}
	return artifact.Ref{ID: strings.Repeat("e", 64), Bytes: w.bytes, CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Available: true, Complete: true}, nil
}

func (w *trackedMCPWriter) Abort() error {
	w.aborts++
	return nil
}

func assertNoMCPResultCanary(t *testing.T, canary string, result tool.Result) {
	t.Helper()
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal("marshal MCP result compatibility view")
	}
	userJSON, err := json.Marshal(result.UserView())
	if err != nil {
		t.Fatal("marshal MCP result user view")
	}
	views := []string{
		result.Summary,
		result.Content,
		result.ModelContent().Text(),
		result.UserView().Summary.Text(),
		result.UserView().Preview.Text(),
		result.PersistedContent().Text(),
		result.OutputMeta().TruncationReason.Text(),
		string(resultJSON),
		string(userJSON),
	}
	if result.Error != nil {
		views = append(views, result.Error.Message)
	}
	if result.UserView().Error != nil {
		views = append(views, result.UserView().Error.Message.Text())
	}
	for _, view := range views {
		if containsMCPSecretMaterial(view, canary) {
			t.Fatal("MCP result safe view contains the raw canary")
		}
	}
}

type fakeToolCaller struct {
	registeredName string
	arguments      map[string]any
	result         protocol.CallToolResult
	err            error
	calls          int
	beforeCall     func()
}

func (f *fakeToolCaller) CallTool(_ context.Context, registeredName string, arguments map[string]any) (protocol.CallToolResult, error) {
	f.registeredName = registeredName
	f.arguments = arguments
	f.calls++
	if f.beforeCall != nil {
		f.beforeCall()
	}
	if f.err != nil {
		return protocol.CallToolResult{}, f.err
	}
	return f.result, nil
}

func (f *fakeToolCaller) CallToolCaptured(_ context.Context, registeredName string, arguments map[string]any, destination io.Writer) (protocol.CallToolResult, error) {
	f.registeredName = registeredName
	f.arguments = arguments
	f.calls++
	if f.beforeCall != nil {
		f.beforeCall()
	}
	if f.err != nil {
		return protocol.CallToolResult{}, f.err
	}
	if err := writeMCPResult(destination, f.result); err != nil {
		return protocol.CallToolResult{}, err
	}
	return f.result, nil
}
