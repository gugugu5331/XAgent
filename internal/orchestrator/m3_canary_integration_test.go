package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/artifact"
	"xagent/internal/budget"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/mcpclient"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/memory"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

const m3CrossChannelCanary = "M3_CROSS_CHANNEL_SECRET_20260803_7D91"

func TestM3CrossChannelCanaryNeverLeaks(t *testing.T) {
	orch, factory := newProjectionCandidate(t, nil)
	orch.runtimeRedactor.RegisterSecret(m3CrossChannelCanary)

	rawInputs := []string{
		"conversation " + m3CrossChannelCanary,
		"context " + m3CrossChannelCanary,
		"provider " + m3CrossChannelCanary,
		"mcp " + m3CrossChannelCanary,
		"memory " + m3CrossChannelCanary,
		"diagnostic " + m3CrossChannelCanary,
	}
	for _, input := range rawInputs {
		if !strings.Contains(input, m3CrossChannelCanary) {
			t.Fatal("M3 canary fixture did not receive its raw input")
		}
	}

	var safeOutputs []string
	safeOutputs = append(safeOutputs, m3ConversationAndContextOutputs(t, orch, factory, rawInputs[0], rawInputs[1])...)
	safeOutputs = append(safeOutputs, m3ProviderOutputs(t, orch, rawInputs[2])...)
	safeOutputs = append(safeOutputs, m3MCPOutputs(t, orch, factory, rawInputs[3])...)
	safeOutputs = append(safeOutputs, m3MemoryOutputs(t, orch.runtimeRedactor, rawInputs[4])...)
	safeOutputs = append(safeOutputs, m3DiagnosticOutputs(t, orch, rawInputs[5])...)
	if len(safeOutputs) == 0 {
		t.Fatal("M3 canary integration produced no observable safe output")
	}
	for _, output := range safeOutputs {
		if strings.Contains(output, m3CrossChannelCanary) {
			t.Fatal("M3 canary crossed a safe output boundary")
		}
	}
}

func TestM3CrossChannelCanaryTestOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("locate M3 canary test executable")
	}
	command := exec.Command(executable,
		"-test.run=^TestM3CrossChannelCanaryNeverLeaks$",
		"-test.count=1",
		"-test.v",
	)
	output, childErr := command.CombinedOutput()
	if strings.Contains(string(output), m3CrossChannelCanary) {
		t.Fatal("M3 canary appeared in child test output")
	}
	if childErr != nil {
		t.Fatal("M3 canary child test failed")
	}
}

func m3ConversationAndContextOutputs(t *testing.T, orch *Orchestrator, factory *tool.ResultFactory, conversationInput, contextInput string) []string {
	t.Helper()
	result, err := factory.Build(tool.ResultFactoryInput{
		CallID: "m3-context", Name: "Read", State: tool.Completed, Status: tool.StatusSuccess,
		Summary: contextInput, Preview: contextInput, CapturedBytes: int64(len(contextInput)),
	})
	if err != nil {
		t.Fatal("build M3 context result")
	}
	projection, err := orch.contextManager.ProjectToolResult(result)
	if err != nil {
		t.Fatal("project M3 context result")
	}
	outputs := flattenM3Projection(projection)
	request := provider.ChatRequest{Messages: []provider.ModelMessage{{Role: provider.ModelMessageRoleToolResult, Content: projection.ModelContent}}}
	outputs = append(outputs, flattenM3ProviderRequest(request)...)

	dataDir := t.TempDir()
	now := time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC)
	store, err := conversation.NewJSONLStore(conversation.JSONLStoreOptions{
		DataDir: dataDir, Redactor: orch.runtimeRedactor,
		MaxRecordBytes: 256 * 1024, MaxSessionBytes: 1024 * 1024,
		MaxScanFiles: 100, MaxScanBytes: 4 * 1024 * 1024,
		RetentionDays: 30, GapReminderDays: 7, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal("create M3 conversation store")
	}
	conv, err := store.Create(context.Background())
	if err != nil {
		t.Fatal("create M3 conversation")
	}
	orch.appendConversationMessage(conv, conversation.RoleUser, orch.safeText(conversationInput), nil)
	if _, err := conversation.AppendProjectedToolResultMessage(conv, conversation.ToolResultMessageInput{
		CallID: "m3-context", Name: "Read", PersistedContent: projection.PersistedContent,
		UserView: projection.UserView, OutputMeta: projection.OutputMeta,
	}); err != nil {
		t.Fatal("append M3 projected tool result")
	}
	if _, err := store.Save(context.Background(), conv); err != nil {
		t.Fatal("save M3 conversation")
	}
	outputs = append(outputs, scanM3Directory(t, dataDir)...)
	if len(outputs) < 3 {
		t.Fatal("M3 conversation/context boundary produced insufficient evidence")
	}
	return outputs
}

func m3ProviderOutputs(t *testing.T, orch *Orchestrator, raw string) []string {
	t.Helper()
	unsafeEvent := provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: testSafeText(raw)}
	if !strings.Contains(unsafeEvent.Delta.Text(), m3CrossChannelCanary) {
		t.Fatal("M3 Provider fixture did not retain the injected canary")
	}
	out := make(chan events.Event, 4)
	collector, reason, err := collectProviderStreamWithRedactor(
		context.Background(),
		newOrchestratorTestChatStream(unsafeEvent, provider.StreamEvent{Type: provider.StreamEventDone}),
		out,
		orch.redactText,
		orch.runtimeRedactor.MaxSecretBytes(),
	)
	if err != nil || reason != StopReasonCompleted {
		t.Fatal("collect M3 Provider stream")
	}
	close(out)
	outputs := []string{collector.AssistantText.String(), collector.ThinkingText.String()}
	for event := range out {
		outputs = append(outputs, flattenM3Event(event)...)
	}
	if len(outputs) < 3 {
		t.Fatal("M3 Provider boundary produced insufficient evidence")
	}
	return outputs
}

func m3MCPOutputs(t *testing.T, orch *Orchestrator, factory *tool.ResultFactory, raw string) []string {
	t.Helper()
	caller := &m3CanaryToolCaller{raw: raw}
	digest := sha256.Sum256([]byte("M3 fixed server configuration"))
	adapter, err := mcpclient.NewRemoteToolAdapter(mcpclient.RemoteToolAdapterOptions{
		RegisteredName: "mcp__m3__canary", ServerName: "m3", RemoteName: "canary",
		Description: "M3 canary adapter", Schema: tool.Schema{Type: "object"},
		ServerConfigDigest: digest, Caller: caller, ResultFactory: factory,
		Capture: m3CaptureFactory,
	})
	if err != nil {
		t.Fatal("create M3 MCP adapter")
	}
	result := adapter.Execute(context.Background(), tool.Input{
		Name: adapter.Name(), CallID: "m3-mcp", Arguments: map[string]any{"query": raw},
	})
	if caller.calls != 1 || !caller.sawRaw {
		t.Fatal("M3 MCP fixture did not receive the injected canary")
	}
	outputs := []string{result.ModelContent().Text(), result.PersistedContent().Text()}
	outputs = append(outputs, flattenM3UserView(result.UserView())...)
	meta := result.OutputMeta()
	outputs = append(outputs, meta.TruncationReason.Text())
	if len(outputs) < 4 {
		t.Fatal("M3 MCP boundary produced insufficient evidence")
	}
	return outputs
}

func m3MemoryOutputs(t *testing.T, runtimeRedactor *redact.RuntimeRedactor, raw string) []string {
	t.Helper()
	root := t.TempDir()
	providerImpl := &m3MemoryProvider{}
	manager := memory.NewManager(memory.ManagerOptions{
		ProjectDir: root, Provider: providerImpl, Redactor: runtimeRedactor,
		UpdateQueueSize: 1, UpdateConcurrency: 1, UpdateTimeoutMS: 1000,
		MaxCandidateBytes: 4096, MaxNoteBytes: 4096,
	})
	now := time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC)
	note := memory.NewNote(memory.NoteUserPreference, memory.ScopeProject, raw, raw, raw, now)
	if !strings.Contains(note.Title+note.Body+note.Source, m3CrossChannelCanary) {
		t.Fatal("M3 Memory note fixture did not receive the injected canary")
	}
	if err := manager.SaveNote(note); err != nil {
		t.Fatal("save M3 Memory note")
	}
	candidate := "请记住这个长期偏好：" + raw
	if !strings.Contains(candidate, m3CrossChannelCanary) {
		t.Fatal("M3 Memory update fixture did not receive the injected canary")
	}
	manager.UpdateAsync(memory.UpdateInput{Scope: memory.ScopeProject, Candidate: candidate, Source: raw, Now: now})
	if !manager.WaitUpdates(time.Second) {
		t.Fatal("wait for M3 Memory update")
	}
	request, calls := providerImpl.snapshot()
	if calls != 1 {
		t.Fatal("M3 Memory Provider was not called exactly once")
	}
	outputs := scanM3Directory(t, root)
	outputs = append(outputs, flattenM3ProviderRequest(request)...)
	for _, diagnostic := range manager.Diagnostics() {
		encoded, err := json.Marshal(diagnostic)
		if err != nil {
			t.Fatal("encode M3 Memory diagnostic")
		}
		outputs = append(outputs, string(encoded))
	}
	return outputs
}

func m3DiagnosticOutputs(t *testing.T, orch *Orchestrator, raw string) []string {
	t.Helper()
	collector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: orch.redactText})
	collector.Add(diagnostics.New("m3_canary", diagnostics.SeverityWarning, raw).
		WithSource(raw).WithPath(raw).WithAttributes(map[string]string{"raw": raw}))
	if collector.Count() != 1 {
		t.Fatal("M3 diagnostic input was not collected")
	}
	encoded, err := json.Marshal(collector.List())
	if err != nil {
		t.Fatal("encode M3 diagnostics")
	}
	return []string{string(encoded)}
}

func flattenM3Projection(projection contextmgr.ToolResultProjection) []string {
	outputs := []string{
		projection.ModelContent.Text(),
		projection.PersistedContent.Text(),
		projection.OutputMeta.TruncationReason.Text(),
	}
	return append(outputs, flattenM3UserView(projection.UserView)...)
}

func flattenM3UserView(view tool.UserView) []string {
	outputs := []string{view.Summary.Text(), view.Preview.Text(), view.TruncationReason.Text()}
	if view.Error != nil {
		outputs = append(outputs, view.Error.Code, view.Error.Message.Text())
	}
	if view.Artifact != nil {
		outputs = append(outputs, view.Artifact.ID)
	}
	return outputs
}

func flattenM3ProviderRequest(request provider.ChatRequest) []string {
	var outputs []string
	for _, block := range request.System {
		outputs = append(outputs, block.Name, block.Content.Text())
	}
	for _, block := range request.StableSystem {
		outputs = append(outputs, block.Name, block.Content.Text())
	}
	for _, block := range request.DynamicSystem {
		outputs = append(outputs, block.Name, block.Content.Text())
	}
	for _, message := range request.Messages {
		outputs = append(outputs, string(message.Role), message.Content.Text(), message.ToolCallID, message.ToolName,
			message.ArgumentsJSON.Text(), message.ToolResult.Text(), message.ToolResultStatus)
	}
	return outputs
}

func flattenM3Event(event events.Event) []string {
	outputs := []string{string(event.Type), event.Text.Text()}
	if event.Tool != nil {
		artifactID := ""
		if event.Tool.Artifact != nil {
			artifactID = event.Tool.Artifact.ID
		}
		outputs = append(outputs, event.Tool.CallID, event.Tool.Name, event.Tool.Arguments.Text(), event.Tool.Summary.Text(),
			event.Tool.Stdout.Text(), event.Tool.Stderr.Text(), event.Tool.TruncationReason.Text(), event.Tool.ErrorCode, artifactID)
	}
	if event.Progress != nil {
		outputs = append(outputs, event.Progress.Message.Text(), event.Progress.StopReason)
	}
	if event.Diagnostic != nil {
		outputs = append(outputs, event.Diagnostic.Code, event.Diagnostic.Source, event.Diagnostic.Message.Text(), event.Diagnostic.Hint.Text())
	}
	if event.Err != nil {
		outputs = append(outputs, event.Err.Code, event.Err.Message.Text())
	}
	return outputs
}

func scanM3Directory(t *testing.T, root string) []string {
	t.Helper()
	var outputs []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		outputs = append(outputs, string(data))
		return nil
	})
	if err != nil {
		t.Fatal("scan M3 persisted outputs")
	}
	if len(outputs) == 0 {
		t.Fatal("M3 persisted output scan found no files")
	}
	return outputs
}

type m3CanaryToolCaller struct {
	raw    string
	calls  int
	sawRaw bool
}

func (c *m3CanaryToolCaller) CallTool(_ context.Context, _ string, arguments map[string]any) (protocol.CallToolResult, error) {
	c.calls++
	query, _ := arguments["query"].(string)
	c.sawRaw = strings.Contains(query, m3CrossChannelCanary)
	return protocol.CallToolResult{
		Content:           []protocol.ContentBlock{{Type: "text", Text: c.raw}},
		StructuredContent: map[string]any{"raw": c.raw},
	}, nil
}

// CallToolCaptured mirrors the production MCP boundary used by the adapter:
// user text crosses the operation-local writer before the complete result DTO
// is published.  Keeping this canary caller on the same interface ensures the
// integration test exercises the wire-first path rather than the migration
// adapter.
func (c *m3CanaryToolCaller) CallToolCaptured(ctx context.Context, registeredName string, arguments map[string]any, destination io.Writer) (protocol.CallToolResult, error) {
	result, err := c.CallTool(ctx, registeredName, arguments)
	if err != nil {
		return protocol.CallToolResult{}, err
	}
	if destination == nil {
		return protocol.CallToolResult{}, errors.New("nil M3 capture destination")
	}
	wrote := false
	for _, block := range result.Content {
		if block.Type != "text" {
			continue
		}
		if wrote {
			if _, err := io.WriteString(destination, "\n"); err != nil {
				return protocol.CallToolResult{}, err
			}
		}
		if _, err := io.WriteString(destination, block.Text); err != nil {
			return protocol.CallToolResult{}, err
		}
		wrote = true
	}
	if result.StructuredContent != nil {
		if wrote {
			if _, err := io.WriteString(destination, "\n"); err != nil {
				return protocol.CallToolResult{}, err
			}
		}
		if _, err := io.WriteString(destination, "[structured MCP content omitted]"); err != nil {
			return protocol.CallToolResult{}, err
		}
	}
	return result, nil
}

func m3CaptureFactory(ctx context.Context, metadata artifact.Metadata) (*tool.Capture, error) {
	limits, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: 4096})
	if err != nil {
		return nil, err
	}
	counter, err := budget.NewCounter(limits, limits)
	if err != nil {
		return nil, err
	}
	return tool.NewCapture(ctx, tool.CaptureOptions{Store: m3CaptureStore{}, Counter: counter, InlineBytes: 1024, Metadata: metadata})
}

type m3CaptureStore struct{}

func (m3CaptureStore) Begin(context.Context, artifact.Metadata) (artifact.Writer, error) {
	return &m3CaptureWriter{}, nil
}
func (m3CaptureStore) OpenForUser(context.Context, string) (io.ReadCloser, artifact.Ref, error) {
	return nil, artifact.Ref{}, errors.New("not implemented")
}
func (m3CaptureStore) Cleanup(context.Context) (artifact.CleanupResult, error) {
	return artifact.CleanupResult{}, nil
}
func (m3CaptureStore) Close() error { return nil }

type m3CaptureWriter struct{ bytes int64 }

func (w *m3CaptureWriter) Write(input []byte) (int, error) {
	w.bytes += int64(len(input))
	return len(input), nil
}
func (w *m3CaptureWriter) Commit(context.Context) (artifact.Ref, error) {
	return artifact.Ref{ID: strings.Repeat("a", 64), Bytes: w.bytes, CreatedAt: time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC), Available: true, Complete: true}, nil
}
func (*m3CaptureWriter) Abort() error { return nil }

type m3MemoryProvider struct {
	mu      sync.Mutex
	request provider.ChatRequest
	calls   int
}

func (*m3MemoryProvider) Name() string { return "m3-memory" }

func (p *m3MemoryProvider) StreamChat(_ context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	p.mu.Lock()
	p.request = request
	p.calls++
	p.mu.Unlock()
	return newOrchestratorTestChatStream(
		provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: testSafeText(`{"action":"ignore"}`)},
		provider.StreamEvent{Type: provider.StreamEventDone},
	), nil
}

func (p *m3MemoryProvider) snapshot() (provider.ChatRequest, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.request, p.calls
}
