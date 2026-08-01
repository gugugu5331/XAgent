package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/events"
	"xagent/internal/hook"
	"xagent/internal/permission"
	"xagent/internal/provider"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

type toolHookRecorder struct {
	hook.Runtime

	mu        sync.Mutex
	decision  hook.ToolDecision
	before    []hook.ToolInput
	after     []hook.ToolOutput
	durations []time.Duration
}

func newToolHookRecorder(decision hook.ToolDecision) *toolHookRecorder {
	return &toolHookRecorder{Runtime: hook.Noop(), decision: decision}
}

func (r *toolHookRecorder) BeforeTool(_ context.Context, _ hook.ExecutionRef, input hook.ToolInput) hook.ToolDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.before = append(r.before, input)
	return r.decision
}

func (r *toolHookRecorder) AfterTool(_ context.Context, _ hook.ExecutionRef, _ hook.ToolInput, output hook.ToolOutput, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.after = append(r.after, output)
	r.durations = append(r.durations, duration)
}

func (r *toolHookRecorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.before), len(r.after)
}

type validatedRecordingTool struct {
	mu    sync.Mutex
	count int
	input tool.Input
}

type noHookCompatibilityMCPTool struct {
	mu    sync.Mutex
	calls int
	input tool.Input
}

func (*noHookCompatibilityMCPTool) Name() string { return "mcp__fixture__echo" }
func (*noHookCompatibilityMCPTool) Description() string {
	return "deterministic MCP compatibility fixture"
}
func (*noHookCompatibilityMCPTool) Schema() tool.Schema {
	return tool.ObjectSchema([]string{"query"}, map[string]tool.SchemaProperty{
		"query": tool.StringProperty("query"),
	})
}
func (*noHookCompatibilityMCPTool) Risk() tool.Risk { return tool.RiskDangerous }
func (m *noHookCompatibilityMCPTool) Execute(_ context.Context, input tool.Input) tool.Result {
	m.mu.Lock()
	m.calls++
	m.input = input
	m.mu.Unlock()
	return tool.Success(input, "MCP echo complete", "echo:"+input.Arguments["query"].(string), map[string]any{"source": "fixture"})
}

func (t *validatedRecordingTool) Name() string        { return "Recorder" }
func (t *validatedRecordingTool) Description() string { return "records a validated input" }
func (t *validatedRecordingTool) Schema() tool.Schema { return tool.Schema{Type: "object"} }
func (t *validatedRecordingTool) Risk() tool.Risk     { return tool.RiskDangerous }
func (t *validatedRecordingTool) Execute(_ context.Context, input tool.Input) tool.Result {
	t.mu.Lock()
	t.count++
	t.input = input
	t.mu.Unlock()
	return tool.Success(input, "recorded", "ok", nil)
}

func newHookToolFixture(t *testing.T, runtime hook.Runtime) (*Orchestrator, *tool.Registry, *validatedRecordingTool) {
	t.Helper()
	root := t.TempDir()
	registry, err := tool.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &validatedRecordingTool{}
	if err := registry.Register(recorder); err != nil {
		t.Fatal(err)
	}
	executor := tool.NewExecutor(registry, root, time.Second, 4096)
	orch := NewWithOptions(OrchestratorOptions{Registry: registry, Executor: executor, Hooks: runtime})
	return orch, registry, recorder
}

func TestToolSafetyChainOrder(t *testing.T) {
	t.Run("structure validation precedes hook", func(t *testing.T) {
		hooks := newToolHookRecorder(hook.Continue())
		orch, registry, _ := newHookToolFixture(t, hooks)
		out := make(chan events.Event, 8)
		execution := orch.prepareToolExecutionWithRegistryAndRef(context.Background(), RunModeDefault, registry, indexedToolCall{Call: tool.Call{ID: "bad", Name: "Read", ArgumentsJSON: `[]`}}, hook.ExecutionRef{ExecutionID: "e"}, out)
		if execution.Result.Error == nil || execution.Result.Error.Code != tool.ErrInvalidArguments {
			t.Fatalf("unexpected malformed result: %#v", execution.Result)
		}
		before, after := hooks.counts()
		if before != 0 || after != 0 {
			t.Fatalf("malformed call reached hooks: before=%d after=%d", before, after)
		}
	})

	t.Run("plan policy precedes normalization and hook", func(t *testing.T) {
		hooks := newToolHookRecorder(hook.Continue())
		orch, registry, _ := newHookToolFixture(t, hooks)
		out := make(chan events.Event, 8)
		execution := orch.prepareToolExecutionWithRegistryAndRef(context.Background(), RunModePlan, registry, indexedToolCall{Call: tool.Call{ID: "plan", Name: "Write", ArgumentsJSON: `{"path":"note.txt","content":"x"}`}}, hook.ExecutionRef{ExecutionID: "e"}, out)
		if execution.Result.Status != tool.StatusDenied || execution.Result.Error == nil || execution.Result.Error.Code != tool.ErrPermissionDenied {
			t.Fatalf("unexpected Plan result: %#v", execution.Result)
		}
		before, after := hooks.counts()
		if before != 0 || after != 0 {
			t.Fatalf("Plan-denied call reached hooks: before=%d after=%d", before, after)
		}
	})

	t.Run("hard safety precedes hook", func(t *testing.T) {
		hooks := newToolHookRecorder(hook.Continue())
		orch, registry, _ := newHookToolFixture(t, hooks)
		out := make(chan events.Event, 8)
		execution := orch.prepareToolExecutionWithRegistryAndRef(context.Background(), RunModeDefault, registry, indexedToolCall{Call: tool.Call{ID: "hard", Name: "Bash", ArgumentsJSON: `{"command":"rm -rf /"}`}}, hook.ExecutionRef{ExecutionID: "e"}, out)
		if execution.Result.Status != tool.StatusDenied {
			t.Fatalf("hard safety did not deny: %#v", execution.Result)
		}
		before, after := hooks.counts()
		if before != 0 || after != 0 {
			t.Fatalf("hard-denied call reached hooks: before=%d after=%d", before, after)
		}
	})
}

func TestValidatedCallFlowsEndToEnd(t *testing.T) {
	hooks := newToolHookRecorder(hook.Continue())
	orch, registry, recorder := newHookToolFixture(t, hooks)
	orch.SetPermissionMode(permission.ModePermissive)
	out := make(chan events.Event, 8)
	call := tool.Call{ID: "validated", Name: "Recorder", ArgumentsJSON: `{"large":9007199254740993123456789,"label":"same"}`}
	execution := orch.prepareToolExecutionWithRegistryAndRef(context.Background(), RunModeDefault, registry, indexedToolCall{Call: call}, hook.ExecutionRef{ExecutionID: "e", TurnID: "t"}, out)
	if execution.Result.CallID != "" || !execution.Ticket.Issued() {
		t.Fatalf("call was not prepared: %#v", execution)
	}
	execution = orch.executePreparedTool(context.Background(), execution, out)
	if execution.Result.Status != tool.StatusSuccess {
		t.Fatalf("execution failed: %#v", execution.Result)
	}
	before, after := hooks.counts()
	if before != 1 || after != 1 {
		t.Fatalf("hook counts = before %d after %d", before, after)
	}
	want := json.Number("9007199254740993123456789")
	if hooks.before[0].Arguments["large"] != want {
		t.Fatalf("hook lost json.Number: %#v", hooks.before[0].Arguments["large"])
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.count != 1 || recorder.input.Arguments["large"] != want {
		t.Fatalf("handler did not receive validated map: count=%d input=%#v", recorder.count, recorder.input.Arguments)
	}
}

func TestHookDenyShortCircuit(t *testing.T) {
	hooks := newToolHookRecorder(hook.Deny("blocked by project policy"))
	orch, registry, recorder := newHookToolFixture(t, hooks)
	out := make(chan events.Event, 8)
	call := tool.Call{ID: "denied", Name: "Recorder", ArgumentsJSON: `{}`}
	execution := orch.prepareToolExecutionWithRegistryAndRef(context.Background(), RunModeDefault, registry, indexedToolCall{Call: call}, hook.ExecutionRef{ExecutionID: "e"}, out)
	if execution.Result.Status != tool.StatusDenied || execution.Result.Error == nil || execution.Result.Error.Code != tool.ErrHookDenied || !execution.Result.Error.Recoverable {
		t.Fatalf("unexpected hook denial: %#v", execution.Result)
	}
	if !strings.Contains(execution.Result.Content, "blocked by project policy") {
		t.Fatalf("safe denial reason missing: %#v", execution.Result)
	}
	recorder.mu.Lock()
	count := recorder.count
	recorder.mu.Unlock()
	before, after := hooks.counts()
	if count != 0 || before != 1 || after != 0 {
		t.Fatalf("deny crossed boundary: handler=%d before=%d after=%d", count, before, after)
	}
	for len(out) > 0 {
		event := <-out
		if event.Type == events.ToolWaitingConfirmation {
			t.Fatal("hook denial incorrectly opened ordinary confirmation")
		}
	}
}

func TestToolAfterPayloadIsRedacted(t *testing.T) {
	secret := "hook-after-super-secret"
	hooks := newToolHookRecorder(hook.Continue())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	executor := tool.NewExecutor(registry, root, time.Second, 4096)
	orch := NewWithOptions(OrchestratorOptions{Registry: registry, Executor: executor, Hooks: hooks, Redact: func(value string) string { return strings.ReplaceAll(value, secret, "[REDACTED]") }})
	out := make(chan events.Event, 8)
	execution := orch.prepareToolExecutionWithRegistryAndRef(context.Background(), RunModeDefault, registry, indexedToolCall{Call: tool.Call{ID: "read", Name: "Read", ArgumentsJSON: `{"path":"secret.txt"}`}}, hook.ExecutionRef{ExecutionID: "e"}, out)
	execution = orch.executePreparedTool(context.Background(), execution, out)
	if execution.Result.Status != tool.StatusSuccess {
		t.Fatalf("read failed: %#v", execution.Result)
	}
	_, after := hooks.counts()
	if after != 1 || strings.Contains(hooks.after[0].Content, secret) || !strings.Contains(hooks.after[0].Content, "[REDACTED]") {
		t.Fatalf("unsafe after payload: %#v", hooks.after)
	}
}

func TestToolAfterPayloadBoundsEveryResultField(t *testing.T) {
	const secret = "opaque-hook-output-canary-73f1"
	const maxOutput = 8 << 10
	hooks := newToolHookRecorder(hook.Continue())
	executor := &tool.Executor{MaxOutputBytes: maxOutput}
	orch := NewWithOptions(OrchestratorOptions{
		Executor: executor,
		Hooks:    hooks,
		Redact: func(value string) string {
			return strings.ReplaceAll(value, secret, "[REDACTED]")
		},
	})
	huge := secret + strings.Repeat("x", maxOutput*4) + secret
	orch.dispatchToolAfter(context.Background(), ToolExecution{
		Ref:       hook.ExecutionRef{ExecutionID: "e", TurnID: "t"},
		HookInput: hook.ToolInput{CallID: "large", Name: "MCP"},
	}, tool.Result{
		Status:  tool.StatusError,
		Summary: huge,
		Content: huge,
		Error:   &tool.Error{Code: tool.ErrCommandFailed, Message: huge, Recoverable: true},
	}, time.Millisecond)

	_, after := hooks.counts()
	if after != 1 {
		t.Fatalf("ToolAfter count=%d", after)
	}
	output := hooks.after[0]
	if output.Error == nil || len(output.Content) > maxOutput || len(output.Error.Message) > maxOutput/4 {
		t.Fatalf("ToolAfter output was not bounded: content=%d error=%#v", len(output.Content), output.Error)
	}
	if strings.Contains(output.Content, secret) || strings.Contains(output.Error.Message, secret) {
		t.Fatalf("ToolAfter output leaked runtime secret: %#v", output)
	}
	if !strings.Contains(output.Content, "truncated") || !utf8.ValidString(output.Content) || !utf8.ValidString(output.Error.Message) {
		t.Fatalf("ToolAfter output is not usable UTF-8 truncation: %#v", output)
	}
}

func TestLoadSkillUsesHookGate(t *testing.T) {
	skillFile := map[string]string{
		"lint.md": `---
name: lint
description: Hook-gated lint workflow
mode: shared
---
LINT {{args}}
`,
	}

	t.Run("deny in Plan mode does not activate", func(t *testing.T) {
		script := [][]provider.StreamEvent{
			{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "load-denied", Name: tool.LoadSkillToolName, ArgumentsJSON: `{"name":"lint","args":"now"}`}}},
			{{Type: provider.StreamEventTextDelta, Delta: "used safer path"}, {Type: provider.StreamEventDone}},
		}
		orch, conv, activity, _ := newSkillRuntimeFixture(t, skillFile, script)
		hooks := newToolHookRecorder(hook.Deny("skill loading disabled here"))
		orch.hooks = hooks
		stream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "load lint", Mode: RunModePlan, Activity: activity})
		if err != nil {
			t.Fatal(err)
		}
		for event := range stream {
			if event.Type == events.Error {
				t.Fatal(event.Err)
			}
		}
		before, after := hooks.counts()
		if before != 1 || after != 0 || len(activity.Snapshot().Active) != 0 {
			t.Fatalf("load_skill deny boundary failed: before=%d after=%d active=%#v", before, after, activity.Snapshot().Active)
		}
		var found bool
		for _, message := range conv.Messages {
			if message.Role == conversation.RoleToolResult && message.ToolName == tool.LoadSkillToolName {
				found = message.ToolErrorCode == tool.ErrHookDenied && strings.Contains(message.Content, "skill loading disabled here")
			}
		}
		if !found {
			t.Fatalf("hook_denied did not flow into tool history: %#v", conv.Messages)
		}
	})

	t.Run("schema validation stops before hooks", func(t *testing.T) {
		script := [][]provider.StreamEvent{
			{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "load-invalid", Name: tool.LoadSkillToolName, ArgumentsJSON: `{"name":42}`}}},
			{{Type: provider.StreamEventTextDelta, Delta: "recovered"}, {Type: provider.StreamEventDone}},
		}
		orch, conv, activity, _ := newSkillRuntimeFixture(t, skillFile, script)
		hooks := newToolHookRecorder(hook.Continue())
		orch.hooks = hooks
		stream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "load lint", Mode: RunModeDefault, Activity: activity})
		if err != nil {
			t.Fatal(err)
		}
		for event := range stream {
			if event.Type == events.Error {
				t.Fatal(event.Err)
			}
		}
		before, after := hooks.counts()
		if before != 0 || after != 0 || len(activity.Snapshot().Active) != 0 {
			t.Fatalf("load_skill schema boundary failed: before=%d after=%d active=%#v", before, after, activity.Snapshot().Active)
		}
		var found bool
		for _, message := range conv.Messages {
			if message.Role == conversation.RoleToolResult && message.ToolName == tool.LoadSkillToolName {
				found = message.ToolErrorCode == tool.ErrInvalidArguments
			}
		}
		if !found {
			t.Fatalf("handler error did not flow into tool history: %#v", conv.Messages)
		}
	})
}

type countingPromptLease struct {
	mu       sync.Mutex
	blocks   []hook.PromptBlock
	commits  int
	releases int
}

func (l *countingPromptLease) Blocks() []hook.PromptBlock {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]hook.PromptBlock(nil), l.blocks...)
}

func (l *countingPromptLease) Commit() {
	l.mu.Lock()
	l.commits++
	l.mu.Unlock()
}

func (l *countingPromptLease) Release() {
	l.mu.Lock()
	l.releases++
	l.mu.Unlock()
}

func (l *countingPromptLease) terminalCounts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.commits, l.releases
}

func TestPromptLeaseObserver(t *testing.T) {
	t.Run("sent commits exactly once", func(t *testing.T) {
		lease := &countingPromptLease{}
		observer := newPromptLeaseObserver(lease)
		observer.MarkSent()
		var group sync.WaitGroup
		for range 32 {
			group.Add(2)
			go func() { defer group.Done(); observer.MarkSent() }()
			go func() { defer group.Done(); observer.Finish(false) }()
		}
		group.Wait()
		commits, releases := lease.terminalCounts()
		if commits != 1 || releases != 0 {
			t.Fatalf("sent lease terminal calls = commit %d release %d", commits, releases)
		}
	})

	t.Run("pre-send failure releases exactly once", func(t *testing.T) {
		lease := &countingPromptLease{}
		observer := newPromptLeaseObserver(lease)
		var group sync.WaitGroup
		for range 32 {
			group.Add(1)
			go func() { defer group.Done(); observer.Finish(false) }()
		}
		group.Wait()
		commits, releases := lease.terminalCounts()
		if commits != 0 || releases != 1 {
			t.Fatalf("unsent lease terminal calls = commit %d release %d", commits, releases)
		}
	})

	t.Run("post-send finish commits", func(t *testing.T) {
		lease := &countingPromptLease{}
		observer := newPromptLeaseObserver(lease)
		observer.Finish(true)
		observer.Finish(false)
		commits, releases := lease.terminalCounts()
		if commits != 1 || releases != 0 {
			t.Fatalf("sent finish terminal calls = commit %d release %d", commits, releases)
		}
	})
}

func TestNextPromptRequestAttempt(t *testing.T) {
	projectRoot := t.TempDir()
	hookDir := filepath.Join(projectRoot, ".xagent")
	if err := os.MkdirAll(hookDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "hooks.yaml"), []byte(`version: 1
hooks:
  - event: turn_start
    action:
      type: prompt
      scope: next
      content: NEXT_REQUEST_ONLY
`), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := hook.Load(hook.LoadOptions{HomeDir: t.TempDir(), ProjectRoot: projectRoot})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := hook.NewEngine(snapshot, hook.EngineOptions{ProjectRoot: projectRoot})
	if err != nil {
		t.Fatal(err)
	}
	engine.SessionStart(context.Background(), "session", hook.SessionNew)

	ref := engine.BeginTurn(context.Background(), "session", hook.ExecutionMain, hook.ModeDefault)
	first, err := engine.AcquirePrompts(context.Background(), ref)
	if err != nil || len(first.Blocks()) != 1 {
		t.Fatalf("first next lease: blocks=%#v err=%v", first.Blocks(), err)
	}
	blockedCtx, cancelBlocked := context.WithCancel(context.Background())
	cancelBlocked()
	if lease, acquireErr := engine.AcquirePrompts(blockedCtx, ref); !errors.Is(acquireErr, context.Canceled) || lease != nil {
		t.Fatalf("leased next prompt did not gate a competing attempt: lease=%#v err=%v", lease, acquireErr)
	}
	newPromptLeaseObserver(first).Finish(false)
	retry, err := engine.AcquirePrompts(context.Background(), ref)
	if err != nil || len(retry.Blocks()) != 1 {
		t.Fatalf("pre-send failure did not release next prompt: blocks=%#v err=%v", retry.Blocks(), err)
	}
	observer := newPromptLeaseObserver(retry)
	observer.MarkSent()
	observer.Finish(false)
	consumed, err := engine.AcquirePrompts(context.Background(), ref)
	if err != nil || len(consumed.Blocks()) != 0 {
		t.Fatalf("post-write attempt did not consume next prompt: blocks=%#v err=%v", consumed.Blocks(), err)
	}
	consumed.Release()
	engine.EndTurn(context.Background(), ref, hook.TurnCompleted, "")
	engine.SessionEnd(context.Background(), "session", hook.SessionEndExit)
}

type acquiringHookRuntime struct {
	hook.Runtime
	mu           sync.Mutex
	lease        *countingPromptLease
	compactAfter bool
	acquireAfter bool
	compactInput hook.CompactInput
	compactOut   hook.CompactOutput
	binding      hook.CompactBinding
}

func (r *acquiringHookRuntime) BeforeCompact(_ context.Context, binding hook.CompactBinding, input hook.CompactInput) hook.CompactToken {
	r.mu.Lock()
	r.binding = binding
	r.compactInput = input
	r.mu.Unlock()
	return hook.CompactToken{}
}

func (r *acquiringHookRuntime) AfterCompact(_ context.Context, _ hook.CompactToken, output hook.CompactOutput) {
	r.mu.Lock()
	r.compactAfter = true
	r.compactOut = output
	r.mu.Unlock()
}

func (r *acquiringHookRuntime) AcquirePrompts(context.Context, hook.ExecutionRef) (hook.PromptLease, error) {
	r.mu.Lock()
	r.acquireAfter = r.compactAfter
	lease := r.lease
	r.mu.Unlock()
	return lease, nil
}

type observingProvider struct {
	mu      sync.Mutex
	request provider.ChatRequest
}

func (p *observingProvider) Name() string { return "hook-observer" }

func (p *observingProvider) StreamChat(_ context.Context, request provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	p.request = request
	p.mu.Unlock()
	if request.Observer != nil {
		request.Observer.MarkSent()
		request.Observer.Finish(true)
	}
	out := make(chan provider.StreamEvent, 1)
	out <- provider.StreamEvent{Type: provider.StreamEventDone}
	close(out)
	return out, nil
}

func (p *observingProvider) lastRequest() provider.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.request
}

func TestAcquirePromptsBeforeProvider(t *testing.T) {
	enabled := true
	cfg := config.ContextConfig{
		Enabled:                   &enabled,
		ToolResultThresholdChars:  1,
		ToolResultsThresholdChars: 1 << 20,
		ModelWindowTokens:         1 << 30,
		SummaryFailureLimit:       3,
		PreviewChars:              16,
	}
	providerRecorder := &observingProvider{}
	lease := &countingPromptLease{blocks: []hook.PromptBlock{{Name: "compact-policy", Content: "PROMPT FROM COMPACT AFTER", Source: "test"}}}
	runtime := &acquiringHookRuntime{Runtime: hook.Noop(), lease: lease}
	manager := contextmgr.New(providerRecorder, t.TempDir(), cfg)
	orch := NewWithOptions(OrchestratorOptions{Provider: providerRecorder, ContextManager: manager, Hooks: runtime})
	conv := conversation.NewConversation("session-compact", time.Now())
	conversation.AppendToolResultMessage(conv, "call", "Read", "success", "large", "large result", "", false, nil, nil)
	ref := hook.ExecutionRef{SessionID: conv.ID, ExecutionID: "execution", TurnID: "turn", Kind: hook.ExecutionMain, Mode: hook.ModeDefault}
	stream, err := orch.streamWithExecution(context.Background(), conv, RunModeDefault, skill.ExecutionProfile{Persist: true}, false, 1, ref)
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	runtime.mu.Lock()
	acquireAfter := runtime.acquireAfter
	runtime.mu.Unlock()
	if !acquireAfter {
		t.Fatal("AcquirePrompts ran before compact_after")
	}
	request := providerRecorder.lastRequest()
	if len(request.System) == 0 || request.Observer == nil || len(request.StableSystem) != 0 || len(request.DynamicSystem) != 0 {
		t.Fatalf("Hook prompt did not select ordered request path: %#v", request)
	}
	var found bool
	for _, block := range request.System {
		found = found || block.Content == "PROMPT FROM COMPACT AFTER"
	}
	if !found || request.Cache.SystemBreakpointName == "" || request.Cache.CacheTools {
		t.Fatalf("ordered prompt/cache contract failed: %#v", request)
	}
	commits, releases := lease.terminalCounts()
	if commits != 1 || releases != 0 {
		t.Fatalf("provider handshake did not commit lease: commit=%d release=%d", commits, releases)
	}
}

func TestNoHookPromptKeepsLegacyRequestPath(t *testing.T) {
	providerRecorder := &observingProvider{}
	lease := &countingPromptLease{}
	runtime := &acquiringHookRuntime{Runtime: hook.Noop(), lease: lease}
	orch := NewWithOptions(OrchestratorOptions{Provider: providerRecorder, Hooks: runtime})
	conv := conversation.NewConversation("legacy", time.Now())
	ref := hook.ExecutionRef{SessionID: conv.ID, ExecutionID: "execution", TurnID: "turn", Kind: hook.ExecutionMain, Mode: hook.ModeDefault}
	stream, err := orch.streamWithExecution(context.Background(), conv, RunModeDefault, skill.ExecutionProfile{}, false, 1, ref)
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	request := providerRecorder.lastRequest()
	if len(request.System) != 0 || request.Observer != nil || len(request.StableSystem) == 0 || len(request.DynamicSystem) == 0 {
		t.Fatalf("empty Hook prompt changed legacy request path: %#v", request)
	}
	commits, releases := lease.terminalCounts()
	if commits != 0 || releases != 1 {
		t.Fatalf("empty lease was not locally released: commit=%d release=%d", commits, releases)
	}
}

func TestNoHookWireCompatibility(t *testing.T) {
	projectRoot := t.TempDir()
	emptyEngine, err := hook.NewEngine(hook.Snapshot{}, hook.EngineOptions{ProjectRoot: projectRoot})
	if err != nil {
		t.Fatal(err)
	}
	runtimes := []struct {
		name    string
		runtime hook.Runtime
	}{
		{name: "nil-compatible-noop", runtime: hook.Noop()},
		{name: "empty-engine", runtime: emptyEngine},
	}
	requests := make([]provider.ChatRequest, 0, len(runtimes))
	for _, item := range runtimes {
		providerRecorder := &observingProvider{}
		orch := NewWithOptions(OrchestratorOptions{Provider: providerRecorder, Hooks: item.runtime})
		conv := conversation.NewConversation("same-session", time.Unix(1, 0))
		conversation.AppendUserMessage(conv, "same input")
		conv.Messages[0].CreatedAt = time.Unix(2, 0)
		item.runtime.SystemStart(context.Background())
		item.runtime.SessionStart(context.Background(), conv.ID, hook.SessionNew)
		ref := item.runtime.BeginTurn(context.Background(), conv.ID, hook.ExecutionMain, hook.ModeDefault)
		stream, streamErr := orch.streamWithExecution(context.Background(), conv, RunModeDefault, skill.ExecutionProfile{}, false, 1, ref)
		if streamErr != nil {
			t.Fatalf("%s: %v", item.name, streamErr)
		}
		for range stream {
		}
		item.runtime.EndTurn(context.Background(), ref, hook.TurnCompleted, "")
		item.runtime.SessionEnd(context.Background(), conv.ID, hook.SessionEndExit)
		if shutdownErr := item.runtime.Shutdown(context.Background()); shutdownErr != nil {
			t.Fatalf("%s shutdown: %v", item.name, shutdownErr)
		}
		request := providerRecorder.lastRequest()
		if request.Observer != nil || len(request.System) != 0 {
			t.Fatalf("%s exposed empty Hook metadata: %#v", item.name, request)
		}
		requests = append(requests, request)
	}
	if !reflect.DeepEqual(requests[0], requests[1]) {
		t.Fatalf("empty Hook changed Provider wire:\nnoop=%#v\nengine=%#v", requests[0], requests[1])
	}
}

func TestNoHookToolChainCompatibility(t *testing.T) {
	type toolLine struct {
		name         string
		validated    string
		ticketScope  permission.GrantScope
		ticketSource permission.SourceKind
		ticketIssued bool
		status       tool.ResultStatus
		content      string
		events       []events.Type
	}
	type snapshot struct {
		lines             []toolLine
		mcpCalls          int
		mcpQuery          string
		activeSkills      []string
		loadResultStatus  string
		loadSystemRoute   bool
		loadTools         []string
		loadOrderedSystem int
		loadObserver      bool
		diagnostics       int
	}

	var baseline *snapshot
	for _, runtimeName := range []string{"nil", "noop", "empty-engine"} {
		collector := diagnostics.NewCollector(diagnostics.CollectorOptions{})
		var runtime hook.Runtime
		if runtimeName == "noop" {
			runtime = hook.Noop()
		}
		if runtimeName == "empty-engine" {
			engine, err := hook.NewEngine(hook.Snapshot{}, hook.EngineOptions{
				ProjectRoot: t.TempDir(), Diagnostics: collector,
			})
			if err != nil {
				t.Fatalf("create empty Hook engine: %v", err)
			}
			runtime = engine
			defer func() {
				if err := engine.Shutdown(context.Background()); err != nil {
					t.Errorf("shutdown empty Hook engine: %v", err)
				}
			}()
		}

		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("fixture payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		registry, err := tool.NewRegistry(root)
		if err != nil {
			t.Fatal(err)
		}
		mcpTool := &noHookCompatibilityMCPTool{}
		if err := registry.Register(mcpTool); err != nil {
			t.Fatal(err)
		}
		executor := tool.NewExecutor(registry, root, time.Second, 4096)
		orch := NewWithOptions(OrchestratorOptions{
			Registry: registry, Executor: executor, Hooks: runtime, Diagnostics: collector,
		})
		orch.SetPermissionMode(permission.ModePermissive)
		activeRuntime := orch.hookRuntime()
		activeRuntime.SystemStart(context.Background())
		activeRuntime.SessionStart(context.Background(), "tool-session", hook.SessionNew)
		ref := activeRuntime.BeginTurn(context.Background(), "tool-session", hook.ExecutionMain, hook.ModeDefault)

		calls := []tool.Call{
			{ID: "read", Name: "Read", ArgumentsJSON: `{"path":"fixture.txt"}`},
			{ID: "mcp", Name: "mcp__fixture__echo", ArgumentsJSON: `{"query":"same"}`},
		}
		got := snapshot{}
		for index, call := range calls {
			out := make(chan events.Event, 16)
			prepared := make(chan ToolExecution, 1)
			go func() {
				prepared <- orch.prepareToolExecutionWithRegistryAndRef(context.Background(), RunModeDefault, registry, indexedToolCall{Call: call, Index: index}, ref, out)
			}()
			var execution ToolExecution
			var permissionEvents []events.Type
		prepareLoop:
			for {
				select {
				case execution = <-prepared:
					break prepareLoop
				case event := <-out:
					permissionEvents = append(permissionEvents, event.Type)
					if event.Confirmation != nil {
						event.Confirmation.Decision <- events.ToolConfirmationDecision{
							CallID: call.ID, Allowed: true, Action: events.PermissionAllowOnce,
						}
					}
				}
			}
			if execution.Result.CallID != "" || !execution.Ticket.Issued() {
				t.Fatalf("%s %s permission decision did not allow with a ticket: %#v", runtimeName, call.Name, execution)
			}
			execution = orch.executePreparedTool(context.Background(), execution, out)
			if execution.Result.Status != tool.StatusSuccess {
				t.Fatalf("%s %s result: %#v", runtimeName, call.Name, execution.Result)
			}
			validated, err := json.Marshal(execution.Validated.Arguments)
			if err != nil {
				t.Fatal(err)
			}
			line := toolLine{
				name: call.Name, validated: string(validated), ticketScope: execution.Scope,
				ticketSource: execution.Source.Kind, ticketIssued: execution.Ticket.Issued(), status: execution.Result.Status,
				content: execution.Result.Content, events: permissionEvents,
			}
			for len(out) > 0 {
				line.events = append(line.events, (<-out).Type)
			}
			got.lines = append(got.lines, line)
		}
		activeRuntime.EndTurn(context.Background(), ref, hook.TurnCompleted, "")
		activeRuntime.SessionEnd(context.Background(), "tool-session", hook.SessionEndExit)
		mcpTool.mu.Lock()
		got.mcpCalls = mcpTool.calls
		if mcpTool.input.Arguments != nil {
			got.mcpQuery, _ = mcpTool.input.Arguments["query"].(string)
		}
		mcpTool.mu.Unlock()

		skillFiles := map[string]string{"compat.md": `---
name: compat
description: no-Hook route compatibility
allowed_tools: [Read]
mode: shared
---
COMPAT ROUTE {{args}}
`}
		script := [][]provider.StreamEvent{
			{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "load", Name: tool.LoadSkillToolName, ArgumentsJSON: `{"name":"compat","args":"same"}`}}},
			{{Type: provider.StreamEventTextDelta, Delta: "loaded"}, {Type: provider.StreamEventDone}},
		}
		skillOrch, conv, activity, scripted := newSkillRuntimeFixture(t, skillFiles, script)
		skillOrch.hooks = runtime
		skillOrch.diagnostics = collector
		skillOrch.SetPermissionMode(permission.ModePermissive)
		skillRuntime := skillOrch.hookRuntime()
		skillRuntime.SystemStart(context.Background())
		skillRuntime.SessionStart(context.Background(), conv.ID, hook.SessionNew)
		stream, err := skillOrch.SendRequest(context.Background(), conv, RunRequest{UserText: "load compatibility Skill", Mode: RunModeDefault, Activity: activity})
		if err != nil {
			t.Fatalf("%s load_skill start: %v", runtimeName, err)
		}
		for event := range stream {
			if event.Type == events.Error {
				t.Fatalf("%s load_skill: %v", runtimeName, event.Err)
			}
		}
		skillRuntime.SessionEnd(context.Background(), conv.ID, hook.SessionEndExit)
		for _, active := range activity.Snapshot().Active {
			got.activeSkills = append(got.activeSkills, active.Name)
		}
		for _, message := range conv.Messages {
			if message.Role == conversation.RoleToolResult && message.ToolName == tool.LoadSkillToolName {
				got.loadResultStatus = message.ToolResultStatus
			}
		}
		requests := scripted.Requests()
		if len(requests) != 2 {
			t.Fatalf("%s load_skill requests = %d, want 2", runtimeName, len(requests))
		}
		got.loadSystemRoute = systemBlocksContain(requests[1].DynamicSystem, "COMPAT ROUTE same")
		got.loadTools = toolDefinitionNames(requests[1].Tools)
		got.loadOrderedSystem = len(requests[1].System)
		got.loadObserver = requests[1].Observer != nil
		got.diagnostics = collector.Count()

		if baseline == nil {
			copy := got
			baseline = &copy
		} else if !reflect.DeepEqual(*baseline, got) {
			t.Fatalf("%s changed no-Hook tool chain:\nlegacy=%#v\nactual=%#v", runtimeName, *baseline, got)
		}
	}
	if baseline == nil || len(baseline.lines) != 2 || baseline.lines[0].name != "Read" || baseline.lines[0].content != "fixture payload" ||
		baseline.lines[0].validated != `{"path":"fixture.txt"}` || baseline.lines[1].name != "mcp__fixture__echo" ||
		baseline.lines[1].validated != `{"query":"same"}` || baseline.lines[1].content != "echo:same" ||
		!baseline.lines[0].ticketIssued || !baseline.lines[1].ticketIssued || baseline.mcpCalls != 1 || baseline.mcpQuery != "same" ||
		strings.Join(baseline.activeSkills, ",") != "compat" || baseline.loadResultStatus != string(tool.StatusSuccess) ||
		!baseline.loadSystemRoute || strings.Join(baseline.loadTools, ",") != "Read,load_skill" || baseline.loadOrderedSystem != 0 ||
		baseline.loadObserver || baseline.diagnostics != 0 {
		t.Fatalf("legacy tool-chain golden changed: %#v", baseline)
	}
	readLine, mcpLine := baseline.lines[0], baseline.lines[1]
	if readLine.ticketScope != permission.GrantMode || readLine.ticketSource != permission.SourceMode ||
		readLine.status != tool.StatusSuccess || !reflect.DeepEqual(readLine.events, []events.Type{events.ToolPending, events.ToolRunning, events.ToolSuccess}) {
		t.Fatalf("legacy built-in permission/event golden changed: %#v", readLine)
	}
	if mcpLine.ticketScope != permission.GrantOnce || mcpLine.ticketSource != permission.SourceUserDecision ||
		mcpLine.status != tool.StatusSuccess || !reflect.DeepEqual(mcpLine.events, []events.Type{events.ToolPending, events.ToolWaitingConfirmation, events.ToolRunning, events.ToolSuccess}) {
		t.Fatalf("legacy MCP permission/event golden changed: %#v", mcpLine)
	}
}

func TestHookCompactAdapter(t *testing.T) {
	runtime := &acquiringHookRuntime{Runtime: hook.Noop()}
	manager := contextmgr.New(&observingProvider{}, t.TempDir(), config.ContextConfig{})
	orch := NewWithOptions(OrchestratorOptions{ContextManager: manager, Hooks: runtime})
	conv := conversation.NewConversation("manual-session", time.Now())
	if _, err := orch.CompactContext(context.Background(), conv); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.binding.SessionID != conv.ID || runtime.binding.Execution != nil {
		t.Fatalf("manual compact forged execution identity: %#v", runtime.binding)
	}
	if runtime.compactInput.Reason != hook.CompactManual || !runtime.compactAfter || runtime.compactOut.Status != hook.CompactSuccess || runtime.compactOut.After == nil || runtime.compactOut.After.Messages != 0 || runtime.compactOut.After.EstimatedTokens != 0 {
		t.Fatalf("manual compact mapping failed: input=%#v output=%#v after=%v", runtime.compactInput, runtime.compactOut, runtime.compactAfter)
	}
}

func TestHookCompactErrorIsRedacted(t *testing.T) {
	const secret = "compact-private-secret"
	runtime := &acquiringHookRuntime{Runtime: hook.Noop()}
	observer := hookCompactionObserver{runtime: runtime, binding: hook.CompactBinding{SessionID: "session"}, redact: func(value string) string { return strings.ReplaceAll(value, secret, "[REDACTED]") }}
	token := observer.Before(context.Background(), contextmgr.Attempt{Reason: string(contextmgr.ModeAuto), Messages: 3, EstimatedTokens: 9})
	observer.After(context.Background(), token, contextmgr.Result{AfterMessages: 2, AfterEstimatedTokens: 4}, errors.New("failed with "+secret))
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.compactOut.Status != hook.CompactErrorStatus || strings.Contains(runtime.compactOut.Error, secret) || !strings.Contains(runtime.compactOut.Error, "[REDACTED]") {
		t.Fatalf("unsafe compact error output: %#v", runtime.compactOut)
	}
}

type lifecycleContextRuntime struct {
	hook.Runtime
	mu              sync.Mutex
	turnError       string
	toolAfterErr    error
	compactAfterErr error
}

func (r *lifecycleContextRuntime) EndTurn(_ context.Context, _ hook.ExecutionRef, _ hook.TurnStatus, safeError string) {
	r.mu.Lock()
	r.turnError = safeError
	r.mu.Unlock()
}

func (r *lifecycleContextRuntime) AfterTool(ctx context.Context, _ hook.ExecutionRef, _ hook.ToolInput, _ hook.ToolOutput, _ time.Duration) {
	r.mu.Lock()
	r.toolAfterErr = ctx.Err()
	r.mu.Unlock()
}

func (r *lifecycleContextRuntime) AfterCompact(ctx context.Context, _ hook.CompactToken, _ hook.CompactOutput) {
	r.mu.Lock()
	r.compactAfterErr = ctx.Err()
	r.mu.Unlock()
}

func TestHookLifecycleFinalizersUseSafeContextAndError(t *testing.T) {
	const secret = "provider-response-private-body"
	runtime := &lifecycleContextRuntime{Runtime: hook.Noop()}
	orch := NewWithOptions(OrchestratorOptions{Hooks: runtime})
	orch.endTurn(hook.ExecutionRef{ExecutionID: "execution"}, RunResult{Reason: StopReasonProviderError, Err: errors.New("provider failed: " + secret)})

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	execution := ToolExecution{Ref: hook.ExecutionRef{ExecutionID: "execution"}, HookInput: hook.ToolInput{Name: "Read"}}
	orch.dispatchToolAfter(canceled, execution, tool.Result{Status: tool.StatusSuccess, Content: "ok"}, time.Millisecond)
	observer := hookCompactionObserver{runtime: runtime, binding: hook.CompactBinding{SessionID: "session"}}
	observer.After(canceled, hook.CompactToken{}, contextmgr.Result{}, nil)

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.turnError != "agent run failed" || strings.Contains(runtime.turnError, secret) {
		t.Fatalf("unsafe turn error: %q", runtime.turnError)
	}
	if runtime.toolAfterErr != nil || runtime.compactAfterErr != nil {
		t.Fatalf("lifecycle after inherited cancellation: tool=%v compact=%v", runtime.toolAfterErr, runtime.compactAfterErr)
	}
}
