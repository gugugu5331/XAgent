package orchestrator

import (
	"bytes"
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
	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/mcpclient"
	"xagent/internal/mcpclient/protocol"
	"xagent/internal/memory"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/resources"
	"xagent/internal/tool"
)

const (
	t327SharedCanary   = "T327-CROSS-BOUNDARY-6F4E2A9C"
	t327CanaryChildEnv = "XAGENT_T327_CANARY_CHILD"
)

// TestM3SharedCanaryAcrossConversationContextProviderMCPAndMemory is one end-to-end leak gate:
// the same raw value enters the MCP call, Conversation request, Context result
// projection, Provider request/response, and Memory update input. Only typed,
// redacted views may reach the observable boundaries checked below.
func TestM3SharedCanaryAcrossConversationContextProviderMCPAndMemory(t *testing.T) {
	if os.Getenv(t327CanaryChildEnv) == "1" {
		runT327SharedCanaryScenario(t)
		return
	}

	command := exec.Command(
		os.Args[0],
		"-test.run=^TestM3SharedCanaryAcrossConversationContextProviderMCPAndMemory$",
		"-test.count=1",
		"-test.v",
	)
	command.Env = append(os.Environ(), t327CanaryChildEnv+"=1")
	output, err := command.CombinedOutput()
	for _, fragment := range t327CanaryFragments() {
		if bytes.Contains(output, []byte(fragment)) {
			t.Fatal("integrated canary material appeared in child test output")
		}
	}
	if err != nil {
		t.Fatal("integrated canary child scenario failed")
	}
}

func runT327SharedCanaryScenario(t *testing.T) {
	t.Helper()
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(t327SharedCanary)
	root := t.TempDir()

	store, err := conversation.NewJSONLStore(conversation.JSONLStoreOptions{
		DataDir:         filepath.Join(root, "conversations"),
		Redactor:        runtimeRedactor,
		MaxRecordBytes:  1 << 20,
		MaxSessionBytes: 8 << 20,
		MaxScanFiles:    32,
		MaxScanBytes:    8 << 20,
		RetentionDays:   7,
		GapReminderDays: 7,
		Now:             func() time.Time { return time.Date(2026, time.August, 3, 8, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal("create integrated conversation store")
	}
	conversationState, err := store.Create(context.Background())
	if err != nil {
		t.Fatal("create integrated conversation")
	}

	providerRecorder := &t327CanaryProvider{redactor: runtimeRedactor}
	contextManager, err := contextmgr.New(providerRecorder, contextmgr.ManagerOptions{
		Context:           t327ContextConfig(),
		InlineOutputBytes: 4096,
		RuntimeRedactor:   runtimeRedactor,
	})
	if err != nil {
		t.Fatal("create integrated context manager")
	}
	resultFactory, err := tool.NewResultFactory(runtimeRedactor)
	if err != nil {
		t.Fatal("create integrated result factory")
	}

	caller := &t327CanaryMCPCaller{}
	adapter, err := mcpclient.NewRemoteToolAdapter(mcpclient.RemoteToolAdapterOptions{
		RegisteredName:     "mcp__t327__canary",
		ServerName:         "t327",
		RemoteName:         "canary",
		Description:        "integrated canary tool",
		Schema:             tool.Schema{Raw: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"additionalProperties":false}`)},
		ServerConfigDigest: sha256.Sum256([]byte("t327 fixed server configuration")),
		Caller:             caller,
		ResultFactory:      resultFactory,
		Capture:            t327CaptureFactory,
	})
	if err != nil {
		t.Fatal("create integrated MCP adapter")
	}
	mcpResult := adapter.Execute(context.Background(), tool.Input{
		CallID: "t327-call",
		Name:   adapter.Name(),
		Arguments: map[string]any{
			"query": "MCP input " + t327SharedCanary,
		},
	})
	if !caller.receivedRawCanary() {
		t.Fatal("integrated MCP input did not receive the raw canary")
	}
	assertT327NoCanary(t, "MCP result views", mcpResultViewTexts(mcpResult))

	projection, err := contextManager.ProjectToolResult(mcpResult)
	if err != nil {
		t.Fatal("project integrated MCP result through Context")
	}
	assertT327NoCanary(t, "Context projection", t327ProjectionTexts(projection))
	if !strings.Contains(projection.ModelContent.Text(), "[redacted]") {
		t.Fatal("Context projection did not retain a redaction marker")
	}
	if _, err := conversation.AppendProjectedToolResultMessage(conversationState, conversation.ToolResultMessageInput{
		CallID:           "t327-call",
		Name:             adapter.Name(),
		PersistedContent: projection.PersistedContent,
		UserView:         projection.UserView,
		OutputMeta:       projection.OutputMeta,
	}); err != nil {
		t.Fatal("append integrated Context projection to Conversation")
	}
	collectedEvents := []events.Event{events.ToolResultEventFromUserView(
		"t327-call", adapter.Name(), runtimeRedactor.Redact("query="+t327SharedCanary), projection.UserView,
	)}

	diagnosticCollector := diagnostics.NewCollector(diagnostics.CollectorOptions{Redactor: runtimeRedactor.Text})
	diagnosticCollector.Add(diagnostics.New(
		"t327_integrated_canary", diagnostics.SeverityWarning, "diagnostic input "+t327SharedCanary,
	).WithSource("integration").WithPath("/private/" + t327SharedCanary))

	memoryManager := memory.NewManager(memory.ManagerOptions{
		ProjectDir:        filepath.Join(root, "memory"),
		UpdateQueueSize:   1,
		UpdateConcurrency: 1,
		UpdateTimeoutMS:   1000,
		MaxCandidateBytes: 4096,
		Provider:          providerRecorder,
		Redactor:          runtimeRedactor,
	})
	memoryInput := &t327RecordingMemoryInput{manager: memoryManager}
	orchestrator := NewWithOptions(OrchestratorOptions{
		Provider:        providerRecorder,
		Store:           store,
		Resources:       resources.New(),
		ContextManager:  contextManager,
		ResultFactory:   resultFactory,
		MaxRecordBytes:  1 << 20,
		MaxSessionBytes: 8 << 20,
		Memory:          memoryInput,
		Diagnostics:     diagnosticCollector,
		RuntimeRedactor: runtimeRedactor,
	})
	eventStream, err := orchestrator.Send(
		context.Background(), conversationState, "请记住这个集成检查偏好 "+t327SharedCanary,
	)
	if err != nil {
		t.Fatal("start integrated orchestrator request")
	}
	for event := range eventStream {
		collectedEvents = append(collectedEvents, event)
		if event.Type == events.Error {
			if event.Err != nil {
				t.Fatal("integrated orchestrator request returned an error event: " + event.Err.Message.Text())
			}
			t.Fatal("integrated orchestrator request returned an error event")
		}
	}
	if !memoryInput.receivedRawCanary() {
		t.Fatal("Memory input did not receive the raw shared canary")
	}
	if !memoryManager.WaitUpdates(2 * time.Second) {
		t.Fatal("integrated Memory update did not complete")
	}
	restarted, err := store.Load(context.Background(), conversationState.ID)
	if err != nil || !restarted.Available || restarted.Conversation == nil {
		t.Fatal("reload integrated Conversation from JSONL")
	}

	mainRequest, memoryRequest, mainCalls, memoryCalls := providerRecorder.requests()
	if mainCalls == 0 || memoryCalls == 0 {
		t.Fatal("Provider did not receive both main and Memory requests")
	}
	assertT327NoCanary(t, "Conversation state", t327ConversationTexts(conversationState))
	assertT327NoCanary(t, "reloaded Conversation state", t327ConversationTexts(restarted.Conversation))
	assertT327NoCanary(t, "model input", t327RequestTexts(mainRequest))
	assertT327NoCanary(t, "Memory Provider input", t327RequestTexts(memoryRequest))
	assertT327NoCanary(t, "events", t327EventTexts(collectedEvents))
	if !t327ContainsRedactionMarker(t327ConversationTexts(conversationState)) ||
		!t327ContainsRedactionMarker(t327ConversationTexts(restarted.Conversation)) ||
		!t327ContainsRedactionMarker(t327RequestTexts(mainRequest)) ||
		!t327ContainsRedactionMarker(t327RequestTexts(memoryRequest)) ||
		!t327ContainsRedactionMarker(t327EventTexts(collectedEvents)) {
		t.Fatal("an integrated observable boundary did not retain a redaction marker")
	}
	assertT327NoCanary(t, "diagnostics", diagnosticCollector.List(), memoryManager.Diagnostics())
	assertT327TreeHasNoCanary(t, "JSONL", filepath.Join(root, "conversations"), true)
	assertT327TreeHasNoCanary(t, "Memory", filepath.Join(root, "memory"), true)
}

func t327ContextConfig() config.ContextConfig {
	enabled := true
	return config.ContextConfig{
		Enabled:                   &enabled,
		ToolResultThresholdChars:  1 << 20,
		ToolResultsThresholdChars: 4 << 20,
		ModelWindowTokens:         10_000_000,
		AutoMarginTokens:          1,
		ManualMarginTokens:        1,
		RecentKeepTokens:          1,
		RecentKeepMessages:        1,
		SummaryFailureLimit:       1,
		PreviewChars:              1 << 20,
	}
}

func assertT327NoCanary(t *testing.T, boundary string, values ...any) {
	t.Helper()
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(boundary + " could not be encoded for the integrated leak assertion")
	}
	for _, fragment := range t327CanaryFragments() {
		if bytes.Contains(encoded, []byte(fragment)) {
			t.Fatal(boundary + " retained shared canary material")
		}
	}
}

func mcpResultViewTexts(result tool.Result) []string {
	view := result.UserView()
	metadata := result.OutputMeta()
	values := []string{
		result.ModelContent().Text(), view.Summary.Text(), view.Preview.Text(),
		view.TruncationReason.Text(), result.PersistedContent().Text(), metadata.TruncationReason.Text(),
	}
	if view.Error != nil {
		values = append(values, view.Error.Message.Text())
	}
	return values
}

func t327ProjectionTexts(projection contextmgr.ToolResultProjection) []string {
	view := projection.UserView
	values := []string{
		projection.ModelContent.Text(), view.Summary.Text(), view.Preview.Text(),
		view.TruncationReason.Text(), projection.PersistedContent.Text(), projection.OutputMeta.TruncationReason.Text(),
	}
	if view.Error != nil {
		values = append(values, view.Error.Message.Text())
	}
	return values
}

func t327ConversationTexts(state *conversation.Conversation) []string {
	if state == nil {
		return nil
	}
	values := []string{state.Title.Text()}
	if state.Context != nil {
		values = append(values, state.Context.Summary.Text(), state.Context.LastBoundary.Text())
	}
	for _, message := range state.Messages {
		values = append(values, message.Content.Text())
		if message.Tool == nil {
			continue
		}
		values = append(values,
			message.Tool.ArgumentsJSON.Text(), message.Tool.Summary.Text(), message.Tool.Result.Text(),
			message.Tool.TruncationReason.Text(),
		)
		if message.Tool.Error != nil {
			values = append(values, message.Tool.Error.Message.Text())
		}
	}
	return values
}

func t327RequestTexts(request provider.ChatRequest) []string {
	values := make([]string, 0, len(request.System)+len(request.StableSystem)+len(request.DynamicSystem)+len(request.Messages)*3)
	for _, blocks := range [][]provider.SystemBlock{request.System, request.StableSystem, request.DynamicSystem} {
		for _, block := range blocks {
			values = append(values, block.Content.Text())
		}
	}
	for _, message := range request.Messages {
		values = append(values, message.Content.Text(), message.ArgumentsJSON.Text(), message.ToolResult.Text())
	}
	return values
}

func t327EventTexts(items []events.Event) []string {
	values := make([]string, 0, len(items)*4)
	for _, event := range items {
		values = append(values, event.Text.Text())
		if event.Err != nil {
			values = append(values, event.Err.Message.Text())
		}
		if event.Tool != nil {
			values = append(values,
				event.Tool.Arguments.Text(), event.Tool.Summary.Text(), event.Tool.Stdout.Text(),
				event.Tool.Stderr.Text(), event.Tool.TruncationReason.Text(),
			)
		}
		if event.Diagnostic != nil {
			values = append(values,
				event.Diagnostic.Message.Text(), event.Diagnostic.Hint.Text(),
			)
		}
		if event.Progress != nil {
			values = append(values, event.Progress.Message.Text())
		}
	}
	return values
}

func t327ContainsRedactionMarker(values []string) bool {
	for _, value := range values {
		if strings.Contains(value, "[redacted]") {
			return true
		}
	}
	return false
}

func assertT327TreeHasNoCanary(t *testing.T, boundary string, root string, requireFile bool) {
	t.Helper()
	files := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		files++
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, fragment := range t327CanaryFragments() {
			if bytes.Contains(data, []byte(fragment)) {
				return errors.New(boundary + " retained shared canary material")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal("inspect integrated " + boundary + " output")
	}
	if requireFile && files == 0 {
		t.Fatal("integrated scenario produced no " + boundary + " output")
	}
}

func t327CanaryFragments() []string {
	middle := len(t327SharedCanary) / 2
	return []string{t327SharedCanary, t327SharedCanary[:middle], t327SharedCanary[middle:]}
}

type t327CanaryProvider struct {
	redactor *redact.RuntimeRedactor
	mu       sync.Mutex
	main     provider.ChatRequest
	memory   provider.ChatRequest
	mainRuns int
	memRuns  int
}

func (providerRecorder *t327CanaryProvider) Name() string { return "t327-canary-provider" }

func (providerRecorder *t327CanaryProvider) StreamChat(_ context.Context, request provider.ChatRequest) (provider.ChatStream, error) {
	isMemory := len(request.StableSystem) == 1 && request.StableSystem[0].Name == "memory-update"
	providerRecorder.mu.Lock()
	if isMemory {
		providerRecorder.memory = request
		providerRecorder.memRuns++
	} else {
		providerRecorder.main = request
		providerRecorder.mainRuns++
	}
	providerRecorder.mu.Unlock()

	if isMemory {
		return newOrchestratorTestChatStream(
			provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: providerRecorder.redactor.Redact(`{"action":"add","title":"integrated preference","type":"user_preference","body":"stored ` + t327SharedCanary + `"}`)},
			provider.StreamEvent{Type: provider.StreamEventDone},
		), nil
	}
	return newOrchestratorTestChatStream(
		provider.StreamEvent{Type: provider.StreamEventTextDelta, Delta: providerRecorder.redactor.Redact("完成集成检查 " + t327SharedCanary)},
		provider.StreamEvent{Type: provider.StreamEventDone},
	), nil
}

func (providerRecorder *t327CanaryProvider) requests() (provider.ChatRequest, provider.ChatRequest, int, int) {
	providerRecorder.mu.Lock()
	defer providerRecorder.mu.Unlock()
	return providerRecorder.main, providerRecorder.memory, providerRecorder.mainRuns, providerRecorder.memRuns
}

type t327RecordingMemoryInput struct {
	manager *memory.Manager
	mu      sync.Mutex
	sawRaw  bool
}

func (input *t327RecordingMemoryInput) UpdateAsync(update memory.UpdateInput) {
	input.mu.Lock()
	input.sawRaw = strings.Contains(update.Candidate, t327SharedCanary)
	input.mu.Unlock()
	input.manager.UpdateAsync(update)
}

func (input *t327RecordingMemoryInput) receivedRawCanary() bool {
	input.mu.Lock()
	defer input.mu.Unlock()
	return input.sawRaw
}

type t327CanaryMCPCaller struct {
	mu        sync.Mutex
	arguments map[string]any
}

func (caller *t327CanaryMCPCaller) CallTool(_ context.Context, _ string, arguments map[string]any) (protocol.CallToolResult, error) {
	caller.mu.Lock()
	caller.arguments = arguments
	caller.mu.Unlock()
	return protocol.CallToolResult{Content: []protocol.ContentBlock{{
		Type: "text",
		Text: "MCP output " + t327SharedCanary,
	}}}, nil
}

func (caller *t327CanaryMCPCaller) CallToolCaptured(ctx context.Context, registeredName string, arguments map[string]any, destination io.Writer) (protocol.CallToolResult, error) {
	result, err := caller.CallTool(ctx, registeredName, arguments)
	if err != nil {
		return protocol.CallToolResult{}, err
	}
	if destination == nil {
		return protocol.CallToolResult{}, errors.New("nil integrated MCP capture destination")
	}
	for _, block := range result.Content {
		if block.Type == "text" {
			if _, err := io.WriteString(destination, block.Text); err != nil {
				return protocol.CallToolResult{}, err
			}
		}
	}
	return result, nil
}

func (caller *t327CanaryMCPCaller) receivedRawCanary() bool {
	caller.mu.Lock()
	defer caller.mu.Unlock()
	query, _ := caller.arguments["query"].(string)
	return strings.Contains(query, t327SharedCanary)
}

func t327CaptureFactory(ctx context.Context, metadata artifact.Metadata) (*tool.Capture, error) {
	limits, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: 4096})
	if err != nil {
		return nil, err
	}
	counter, err := budget.NewCounter(limits, limits)
	if err != nil {
		return nil, err
	}
	return tool.NewCapture(ctx, tool.CaptureOptions{
		Store:       t327CaptureStore{},
		Counter:     counter,
		InlineBytes: 4096,
		Metadata:    metadata,
	})
}

type t327CaptureStore struct{}

func (t327CaptureStore) Begin(context.Context, artifact.Metadata) (artifact.Writer, error) {
	return &t327CaptureWriter{}, nil
}

func (t327CaptureStore) OpenForUser(context.Context, string) (io.ReadCloser, artifact.Ref, error) {
	return nil, artifact.Ref{}, errors.New("not implemented")
}

func (t327CaptureStore) Cleanup(context.Context) (artifact.CleanupResult, error) {
	return artifact.CleanupResult{}, nil
}

func (t327CaptureStore) Close() error { return nil }

type t327CaptureWriter struct{}

func (*t327CaptureWriter) Write(input []byte) (int, error) { return len(input), nil }
func (*t327CaptureWriter) Commit(context.Context) (artifact.Ref, error) {
	return artifact.Ref{}, errors.New("unexpected commit")
}
func (*t327CaptureWriter) Abort() error { return nil }
