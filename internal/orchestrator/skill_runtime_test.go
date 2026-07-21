package orchestrator

import (
	"context"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/events"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/resources"
	"xagent/internal/skill"
	"xagent/internal/tool"
)

type scriptedSkillProvider struct {
	mu       sync.Mutex
	requests []provider.ChatRequest
	events   [][]provider.StreamEvent
}

func (p *scriptedSkillProvider) Name() string { return "skill-script" }

func (p *scriptedSkillProvider) StreamChat(_ context.Context, request provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	index := len(p.requests)
	p.requests = append(p.requests, request)
	var scripted []provider.StreamEvent
	if index < len(p.events) {
		scripted = append([]provider.StreamEvent(nil), p.events[index]...)
	}
	p.mu.Unlock()
	out := make(chan provider.StreamEvent, len(scripted)+1)
	for _, event := range scripted {
		out <- event
	}
	if len(scripted) == 0 {
		out <- provider.StreamEvent{Type: provider.StreamEventDone}
	}
	close(out)
	return out, nil
}

func (p *scriptedSkillProvider) Requests() []provider.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.ChatRequest(nil), p.requests...)
}

func newSkillRuntimeFixture(t *testing.T, files map[string]string, scripted [][]provider.StreamEvent) (*Orchestrator, *conversation.Conversation, *skill.Activity, *scriptedSkillProvider) {
	t.Helper()
	root := t.TempDir()
	store, err := conversation.NewFileStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := store.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tool.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	mapFiles := fstest.MapFS{}
	for name, content := range files {
		mapFiles[name] = &fstest.MapFile{Data: []byte(content), Mode: fs.FileMode(0o600)}
	}
	manager, err := skill.NewManager(skill.ManagerOptions{
		Sources:   []skill.SourceFS{{Source: skill.SourceProject, FS: mapFiles, Root: "."}},
		ToolNames: registry.Names(),
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedSkillProvider{events: scripted}
	executor := tool.NewExecutor(registry, root, time.Second, 4096)
	orch := NewWithOptions(OrchestratorOptions{
		Provider:     provider,
		Store:        store,
		Resources:    resources.New(),
		Thinking:     config.ThinkingConfig{},
		Registry:     registry,
		Executor:     executor,
		SkillManager: manager,
		DefaultModel: "default-model",
	})
	return orch, conv, skill.NewActivity(), provider
}

func TestEmptySkillCatalogPreservesOrdinaryRequestBehavior(t *testing.T) {
	script := [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: "ordinary reply"},
		{Type: provider.StreamEventUsage, Usage: &provider.Usage{InputTokens: 3, OutputTokens: 2}},
		{Type: provider.StreamEventDone},
	}}
	withSkills, withConversation, _, withProvider := newSkillRuntimeFixture(t, nil, script)
	withoutProvider := &scriptedSkillProvider{events: script}
	withoutSkills := NewWithOptions(OrchestratorOptions{
		Provider: withoutProvider, Store: withSkills.store, Resources: withSkills.resources,
		Thinking: withSkills.thinking, Registry: withSkills.registry, Executor: withSkills.executor,
		DefaultModel: withSkills.defaultModel,
	})
	withoutConversation := conversation.NewConversation("without-skills", withConversation.CreatedAt)

	type eventSnapshot struct {
		typeName events.Type
		text     string
		progress *events.AgentProgress
		usage    *events.UsageDisplay
	}
	run := func(orch *Orchestrator, conv *conversation.Conversation) []eventSnapshot {
		t.Helper()
		stream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "ordinary request", Mode: RunModeDefault})
		if err != nil {
			t.Fatal(err)
		}
		var snapshots []eventSnapshot
		for event := range stream {
			if event.Type == events.Error {
				t.Fatal(event.Err)
			}
			snapshots = append(snapshots, eventSnapshot{typeName: event.Type, text: event.Text, progress: event.Progress, usage: event.Usage})
		}
		return snapshots
	}

	withEvents := run(withSkills, withConversation)
	withoutEvents := run(withoutSkills, withoutConversation)
	if !reflect.DeepEqual(withEvents, withoutEvents) {
		t.Fatalf("empty Skill catalog changed event order: with=%#v without=%#v", withEvents, withoutEvents)
	}
	withRequests := withProvider.Requests()
	withoutRequests := withoutProvider.Requests()
	if len(withRequests) != 1 || len(withoutRequests) != 1 {
		t.Fatalf("unexpected Provider call counts: with=%d without=%d", len(withRequests), len(withoutRequests))
	}
	for index := range withRequests[0].Messages {
		withRequests[0].Messages[index].CreatedAt = time.Time{}
	}
	for index := range withoutRequests[0].Messages {
		withoutRequests[0].Messages[index].CreatedAt = time.Time{}
	}
	if !reflect.DeepEqual(withRequests[0], withoutRequests[0]) {
		t.Fatal("empty Skill catalog changed the ordinary Provider request")
	}
	withMessages := cloneMessages(withConversation.Messages)
	withoutMessages := cloneMessages(withoutConversation.Messages)
	for index := range withMessages {
		withMessages[index].CreatedAt = time.Time{}
	}
	for index := range withoutMessages {
		withoutMessages[index].CreatedAt = time.Time{}
	}
	if !reflect.DeepEqual(withMessages, withoutMessages) {
		t.Fatalf("empty Skill catalog changed conversation history: with=%#v without=%#v", withConversation.Messages, withoutConversation.Messages)
	}
}

func TestExecuteSharedSkillCommandBuildsProfile(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{
		"lint.md": `---
name: lint
description: Focused lint workflow
allowed_tools:
  - Read
mode: shared
model: skill-model
---
ACTIVE LINT {{args}}
`,
	}, [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: "lint done"},
		{Type: provider.StreamEventDone},
	}})

	eventStream, prepared, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "LINT", Args: "focus", Raw: "/lint focus", Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Mode != skill.ModeShared {
		t.Fatalf("unexpected prepared invocation: %#v", prepared)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	requests := scripted.Requests()
	if len(requests) != 1 {
		t.Fatalf("expected one request, got %d", len(requests))
	}
	request := requests[0]
	if request.Model != "skill-model" {
		t.Fatalf("unexpected request model: %q", request.Model)
	}
	if !systemBlocksContain(request.StableSystem, "Focused lint workflow") || systemBlocksContain(request.StableSystem, "ACTIVE LINT") {
		t.Fatalf("catalog leaked or omitted content: %#v", request.StableSystem)
	}
	if !systemBlocksContain(request.DynamicSystem, "ACTIVE LINT focus") {
		t.Fatalf("active SOP missing: %#v", request.DynamicSystem)
	}
	if got := toolDefinitionNames(request.Tools); strings.Join(got, ",") != "Read,load_skill" {
		t.Fatalf("unexpected tool view: %#v", got)
	}
	if len(conv.Messages) != 2 || conv.Messages[0].Content != "/lint focus" || conv.Messages[1].Content != "lint done" {
		t.Fatalf("unexpected main history: %#v", conv.Messages)
	}
	for _, message := range conv.Messages {
		if strings.Contains(message.Content, "ACTIVE LINT") {
			t.Fatalf("SOP leaked into history: %#v", conv.Messages)
		}
	}
}

func TestSharedSkillUsesRuntimeRedactorAcrossStreamingPath(t *testing.T) {
	opaqueSecret := "opaque-" + strings.Repeat("runtime-value-", 8) + "tail"
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{
		"secure.md": `---
name: secure
description: Exercises runtime redaction
mode: shared
---
SECURE {{args}}
`,
	}, [][]provider.StreamEvent{{
		{Type: provider.StreamEventTextDelta, Delta: "reply " + opaqueSecret[:55]},
		{Type: provider.StreamEventTextDelta, Delta: opaqueSecret[55:]},
		{Type: provider.StreamEventDone},
	}})
	runtimeRedactor := redact.NewRuntimeRedactor()
	runtimeRedactor.RegisterSecret(opaqueSecret)
	orch.redact = runtimeRedactor.Text
	orch.redactionLookbehind = max(64, runtimeRedactor.MaxSecretBytes())
	eventStream, _, err := orch.SendSkill(context.Background(), conv, skill.Invocation{
		Name: "secure", Args: opaqueSecret, Raw: "/secure " + opaqueSecret, Origin: skill.OriginSlash,
	}, activity, RunModeDefault)
	if err != nil {
		t.Fatal(err)
	}
	var visible strings.Builder
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
		visible.WriteString(event.Text)
	}
	if strings.Contains(visible.String(), opaqueSecret) {
		t.Fatalf("shared streaming events leaked runtime secret: %q", visible.String())
	}
	for _, message := range conv.Messages {
		if strings.Contains(message.Content, opaqueSecret) {
			t.Fatalf("shared history leaked runtime secret: %#v", conv.Messages)
		}
	}
	requests := scripted.Requests()
	if len(requests) != 1 {
		t.Fatalf("expected one request, got %d", len(requests))
	}
	for _, block := range append(append([]provider.SystemBlock(nil), requests[0].StableSystem...), requests[0].DynamicSystem...) {
		if strings.Contains(block.Content, opaqueSecret) {
			t.Fatalf("shared provider prompt leaked runtime secret: %q", block.Content)
		}
	}
}

func TestLoadSharedSkillAtIterationBoundary(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{
		"lint.md": `---
name: lint
description: Focused lint workflow
allowed_tools: [Read]
mode: shared
---
SECOND ITERATION SOP {{args}}
`,
	}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "load-1", Name: tool.LoadSkillToolName, ArgumentsJSON: `{"name":"lint","args":"now"}`}}},
		{{Type: provider.StreamEventTextDelta, Delta: "done"}, {Type: provider.StreamEventDone}},
		{{Type: provider.StreamEventTextDelta, Delta: "follow-up done"}, {Type: provider.StreamEventDone}},
	})

	eventStream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "please lint", Mode: RunModeDefault, Activity: activity})
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	requests := scripted.Requests()
	if len(requests) != 2 {
		t.Fatalf("expected two requests, got %d", len(requests))
	}
	if systemBlocksContain(requests[0].DynamicSystem, "SECOND ITERATION SOP") {
		t.Fatal("SOP appeared before load_skill completed")
	}
	if !systemBlocksContain(requests[1].DynamicSystem, "SECOND ITERATION SOP now") {
		t.Fatalf("SOP missing after load: %#v", requests[1].DynamicSystem)
	}
	if got := strings.Join(toolDefinitionNames(requests[1].Tools), ","); got != "Read,load_skill" {
		t.Fatalf("second request tools were not narrowed: %s", got)
	}
	var hasCall, hasResult bool
	for _, message := range conv.Messages {
		hasCall = hasCall || message.Role == conversation.RoleToolCall && message.ToolName == tool.LoadSkillToolName
		hasResult = hasResult || message.Role == conversation.RoleToolResult && message.ToolName == tool.LoadSkillToolName
	}
	if !hasCall || !hasResult {
		t.Fatalf("shared load was not recorded normally: %#v", conv.Messages)
	}
	if active := activity.Snapshot().Active; len(active) != 1 || active[0].Name != "lint" {
		t.Fatalf("Agent-loaded shared Skill was not retained after its loading request: %#v", active)
	}

	followUp, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "continue linting", Mode: RunModeDefault, Activity: activity})
	if err != nil {
		t.Fatal(err)
	}
	for event := range followUp {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	requests = scripted.Requests()
	if len(requests) != 3 {
		t.Fatalf("expected loading iteration plus a later user request, got %d requests", len(requests))
	}
	if !systemBlocksContain(requests[2].DynamicSystem, "SECOND ITERATION SOP now") {
		t.Fatalf("Agent-loaded shared SOP did not persist into the next user request: %#v", requests[2].DynamicSystem)
	}
	if got := strings.Join(toolDefinitionNames(requests[2].Tools), ","); got != "Read,load_skill" {
		t.Fatalf("Agent-loaded tool restriction did not persist into the next user request: %s", got)
	}
}

func TestMixedSharedLoadPreservesCallOrderAndIterationProfile(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{
		"read-only.md": `---
name: read-only
description: Restrict later iterations to Read
allowed_tools: [Read]
mode: shared
---
READ ONLY
`,
	}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCalls: []tool.Call{
			{ID: "glob-before", Name: "Glob", ArgumentsJSON: `{"pattern":"*.md"}`},
			{ID: "load-middle", Name: tool.LoadSkillToolName, ArgumentsJSON: `{"name":"read-only"}`},
			{ID: "grep-after", Name: "Grep", ArgumentsJSON: `{"pattern":"needle","path":"."}`},
		}}},
		{{Type: provider.StreamEventTextDelta, Delta: "mixed calls done"}, {Type: provider.StreamEventDone}},
	})

	eventStream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "load while using tools", Mode: RunModeDefault, Activity: activity})
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}

	requests := scripted.Requests()
	if len(requests) != 2 {
		t.Fatalf("expected two requests, got %d", len(requests))
	}
	if got := strings.Join(toolDefinitionNames(requests[1].Tools), ","); got != "Read,load_skill" {
		t.Fatalf("next iteration was not narrowed: %s", got)
	}
	var calls []string
	var grepError string
	for _, message := range conv.Messages {
		if message.Role == conversation.RoleToolCall {
			calls = append(calls, message.ToolName)
		}
		if message.Role == conversation.RoleToolResult && message.ToolName == "Grep" {
			grepError = message.ToolErrorCode
		}
	}
	if got := strings.Join(calls, ","); got != "Glob,load_skill,Grep" {
		t.Fatalf("provider tool-call order changed: %s", got)
	}
	if grepError == tool.ErrToolNotFound {
		t.Fatal("tool after load_skill incorrectly used the next-iteration whitelist")
	}
}

func TestFilteredToolCannotExecute(t *testing.T) {
	orch, conv, activity, scripted := newSkillRuntimeFixture(t, map[string]string{
		"read-only.md": `---
name: read-only
description: Restrict tools to Read
allowed_tools: [Read]
mode: shared
---
READ ONLY
`,
	}, [][]provider.StreamEvent{
		{{Type: provider.StreamEventToolCall, ToolCall: &tool.Call{ID: "forged", Name: "Bash", ArgumentsJSON: `{"command":"touch should-not-exist"}`}}},
		{{Type: provider.StreamEventTextDelta, Delta: "handled rejection"}, {Type: provider.StreamEventDone}},
	})
	if _, err := orch.PrepareSkill(skill.Invocation{Name: "read-only", Origin: skill.OriginSlash}, activity); err != nil {
		t.Fatal(err)
	}
	eventStream, err := orch.SendRequest(context.Background(), conv, RunRequest{UserText: "try forged tool", Mode: RunModeDefault, Activity: activity})
	if err != nil {
		t.Fatal(err)
	}
	for event := range eventStream {
		if event.Type == events.Error {
			t.Fatal(event.Err)
		}
	}
	if len(scripted.Requests()) != 2 {
		t.Fatalf("expected rejection followed by final response, got %d calls", len(scripted.Requests()))
	}
	if got := strings.Join(toolDefinitionNames(scripted.Requests()[0].Tools), ","); got != "Read,load_skill" {
		t.Fatalf("filtered request exposed unexpected tools: %s", got)
	}
	var rejected bool
	for _, message := range conv.Messages {
		if message.Role == conversation.RoleToolResult && message.ToolErrorCode == tool.ErrToolNotFound {
			rejected = true
		}
	}
	if !rejected {
		t.Fatalf("forged filtered tool was not rejected: %#v", conv.Messages)
	}
}

func systemBlocksContain(blocks []provider.SystemBlock, value string) bool {
	for _, block := range blocks {
		if strings.Contains(block.Content, value) {
			return true
		}
	}
	return false
}

func toolDefinitionNames(definitions []provider.ToolDefinition) []string {
	result := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, definition.Name)
	}
	return result
}
