package sessionctx

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/memory"
	"xagent/internal/prompt"
	"xagent/internal/redact"
)

func TestSessionContextAndPromptAcceptOnlySafeText(t *testing.T) {
	redactor := redact.NewRuntimeRedactor()
	const canary = "session-prompt-runtime-secret"
	redactor.RegisterSecret(canary)
	manager := &Manager{
		Redactor:        redactor,
		MaxSectionBytes: 96,
		Instructions: fakeInstructionLoader{sections: []prompt.Section{{
			Name: "runtime instruction", Content: strings.Repeat("a", 160) + canary, Stable: true,
		}}},
		Memory: fakeMemoryProvider{index: memory.Index{Scope: memory.ScopeProject, Entries: []memory.IndexEntry{{
			ID: "safe", Title: "memory " + canary, Body: strings.Repeat("b", 160) + canary,
		}}}},
	}
	prepared := manager.PrepareStable(context.Background())
	for _, section := range prepared.StableSections {
		if strings.Contains(section.Content, canary) {
			t.Fatalf("session context retained runtime secret: %#v", section)
		}
		if len([]byte(section.Content)) > manager.MaxSectionBytes {
			t.Fatalf("session context section exceeded byte budget: %d", len([]byte(section.Content)))
		}
	}

	safeRequestType := reflect.TypeOf(prompt.SafeDynamicRequest{})
	safeTextType := reflect.TypeOf(redact.SafeText{})
	for _, fieldName := range []string{"ProjectRoot", "ActiveSkills"} {
		field, ok := safeRequestType.FieldByName(fieldName)
		if !ok || field.Type != safeTextType {
			t.Fatalf("SafeDynamicRequest.%s = %v, want redact.SafeText", fieldName, field.Type)
		}
	}
	blocks, err := prompt.DynamicBlocksFromSafe(prompt.SafeDynamicRequest{
		Mode:         prompt.RunModeDefault,
		Iteration:    1,
		ProjectRoot:  redactor.Redact("/repo/" + canary),
		ActiveSkills: redactor.Redact("active " + canary),
		MaxBytes:     1024,
	})
	if err != nil {
		t.Fatalf("build safe dynamic prompt: %v", err)
	}
	if joined := prompt.JoinBlocks(blocks); strings.Contains(joined, canary) || !strings.Contains(joined, "[redacted]") {
		t.Fatalf("safe dynamic prompt crossed boundary unsafely: %q", joined)
	}
	if _, err := prompt.DynamicBlocksFromSafe(prompt.SafeDynamicRequest{
		ProjectRoot: redactor.Redact(strings.Repeat("x", 64)), MaxBytes: 8,
	}); err == nil {
		t.Fatal("safe dynamic prompt accepted content beyond its byte budget")
	}
}

func TestSessionContextPreparesInstructionsMemoryAndBoundary(t *testing.T) {
	manager := &Manager{
		Instructions: fakeInstructionLoader{sections: []prompt.Section{{Name: "项目指令", Priority: 1000, Content: "项目规则", Stable: true}}},
		Memory:       fakeMemoryProvider{index: memory.Index{Scope: memory.ScopeProject, Entries: []memory.IndexEntry{{ID: "n1", Title: "偏好", Body: "用户喜欢简洁中文"}}}},
	}
	prepared, err := manager.Prepare(context.Background(), conversation.NewConversation("c", zeroTime()), PrepareAuto)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	joined := joinSections(prepared.StableSections)
	for _, want := range []string{"项目规则", "长期记忆", "用户喜欢简洁中文", "会话来自恢复"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("prepared sections missing %q: %s", want, joined)
		}
	}
}

func TestSessionContextRestoredOversizedConversationCompressesBeforeProvider(t *testing.T) {
	conv := conversation.NewConversation("c", zeroTime())
	conversation.AppendUserMessage(conv, "hello")
	manager := &Manager{Context: &fakeContextPreparer{changed: true}}
	prepared, err := manager.Prepare(context.Background(), conv, PrepareAuto)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !prepared.MessagesChanged || !prepared.ContextResult.Changed {
		t.Fatalf("expected context changed: %#v", prepared)
	}
}

func TestSessionContextMergesDiagnostics(t *testing.T) {
	manager := &Manager{
		Instructions: fakeInstructionLoader{diagnostics: []diagnostics.Diagnostic{diagnostics.New("instructions_warn", diagnostics.SeverityWarning, "warn")}},
		Memory:       fakeMemoryProvider{err: assertErr("memory failed"), diagnostics: []diagnostics.Diagnostic{diagnostics.New("memory_warn", diagnostics.SeverityWarning, "warn")}},
	}
	prepared, err := manager.Prepare(context.Background(), conversation.NewConversation("c", zeroTime()), PrepareAuto)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(prepared.Diagnostics) < 3 {
		t.Fatalf("expected merged diagnostics: %#v", prepared.Diagnostics)
	}
}

func TestPrepareStableDoesNotMutateConversationContext(t *testing.T) {
	contextPreparer := &fakeContextPreparer{changed: true}
	manager := &Manager{
		Instructions: fakeInstructionLoader{sections: []prompt.Section{{Name: "project", Content: "stable project rules", Stable: true}}},
		Context:      contextPreparer,
	}
	prepared := manager.PrepareStable(context.Background())
	if contextPreparer.calls != 0 || prepared.MessagesChanged || prepared.ContextResult.Changed {
		t.Fatalf("stable preparation invoked mutating context path: calls=%d prepared=%#v", contextPreparer.calls, prepared)
	}
	if !strings.Contains(joinSections(prepared.StableSections), "stable project rules") {
		t.Fatalf("stable preparation omitted instructions: %#v", prepared.StableSections)
	}
}

func TestSessionContextPriority(t *testing.T) {
	manager := &Manager{
		Instructions: fakeInstructionLoader{sections: []prompt.Section{
			{Name: "项目根指令", Priority: 1000, Content: "项目根约束优先", Stable: true},
			{Name: "用户指令", Priority: 1020, Content: "用户通用偏好", Stable: true},
		}},
		Memory: fakeMemoryProvider{indices: map[memory.Scope]memory.Index{
			memory.ScopeProject: {Scope: memory.ScopeProject, Entries: []memory.IndexEntry{{ID: "project-note", Title: "项目记忆", Body: "项目长期记忆"}}},
			memory.ScopeUser:    {Scope: memory.ScopeUser, Entries: []memory.IndexEntry{{ID: "user-note", Title: "用户记忆", Body: "用户长期记忆"}}},
		}},
	}
	prepared, err := manager.Prepare(context.Background(), conversation.NewConversation("c", zeroTime()), PrepareAuto)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	assertSectionOrder(t, sectionContents(prepared.StableSections),
		"项目根约束优先",
		"用户通用偏好",
		"长期记忆是不可信背景",
		"项目长期记忆",
		"用户长期记忆",
		"如果当前会话来自恢复",
	)
}

func TestPrepareWithOptions(t *testing.T) {
	preparer := &fakeContextPreparer{changed: true}
	manager := &Manager{
		Instructions: fakeInstructionLoader{sections: []prompt.Section{{Name: "project", Content: "stable project rules", Stable: true}}},
		Context:      preparer,
	}
	conv := conversation.NewConversation("c", zeroTime())

	if _, err := manager.Prepare(context.Background(), conv, PrepareAuto); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Prepare(context.Background(), conv, PrepareManual); err != nil {
		t.Fatal(err)
	}
	observer := sessionCompactionObserver{}
	prepared, err := manager.PrepareWithOptions(context.Background(), conv, contextmgr.PrepareOptions{
		Mode:             contextmgr.ModeAuto,
		PersistArtifacts: false,
		Observer:         observer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.MessagesChanged || !strings.Contains(joinSections(prepared.StableSections), "stable project rules") {
		t.Fatalf("options prepare dropped prepared context: %#v", prepared)
	}
	if len(preparer.options) != 3 {
		t.Fatalf("PrepareWithOptions calls = %#v", preparer.options)
	}
	assertPrepareOption := func(index int, mode contextmgr.Mode, persist bool) {
		t.Helper()
		got := preparer.options[index]
		if got.Mode != mode || got.PersistArtifacts != persist {
			t.Fatalf("options[%d] = %#v, want mode=%s persist=%v", index, got, mode, persist)
		}
	}
	assertPrepareOption(0, contextmgr.ModeAuto, true)
	assertPrepareOption(1, contextmgr.ModeManual, true)
	assertPrepareOption(2, contextmgr.ModeAuto, false)
	if preparer.options[2].Observer == nil {
		t.Fatal("transient observer was not passed through")
	}

	legacy := &legacyContextPreparer{}
	legacyManager := &Manager{Context: legacy}
	if _, err := legacyManager.PrepareWithOptions(context.Background(), conv, contextmgr.PrepareOptions{Mode: contextmgr.ModeAuto, PersistArtifacts: false}); err != nil {
		t.Fatal(err)
	}
	if legacy.calls != 0 {
		t.Fatalf("transient prepare fell back to artifact-capable legacy path: calls=%d", legacy.calls)
	}
}

func TestNoHookContextCompatibility(t *testing.T) {
	newManager := func(preparer *fakeContextPreparer) *Manager {
		return &Manager{
			Instructions: fakeInstructionLoader{sections: []prompt.Section{{Name: "project", Priority: 1000, Content: "stable project rules", Stable: true}}},
			Memory: fakeMemoryProvider{indices: map[memory.Scope]memory.Index{
				memory.ScopeProject: {Scope: memory.ScopeProject, Entries: []memory.IndexEntry{{ID: "project", Title: "knowledge", Body: "stable memory"}}},
				memory.ScopeUser:    {Scope: memory.ScopeUser},
			}},
			Context: preparer,
		}
	}
	type snapshot struct {
		prepared PreparedContext
		messages []conversation.Message
		options  []contextmgr.PrepareOptions
	}
	observers := []struct {
		name     string
		observer contextmgr.CompactionObserver
	}{{name: "nil", observer: nil}, {name: "noop", observer: sessionCompactionObserver{}}}
	var baseline *snapshot
	for _, item := range observers {
		preparer := &fakeContextPreparer{changed: true}
		manager := newManager(preparer)
		conv := conversation.NewConversation("compat", zeroTime())
		conversation.AppendUserMessage(conv, "unchanged message")
		for index := range conv.Messages {
			conv.Messages[index].CreatedAt = time.Time{}
		}
		prepared, err := manager.PrepareWithOptions(context.Background(), conv, contextmgr.PrepareOptions{
			Mode: contextmgr.ModeAuto, PersistArtifacts: false, Observer: item.observer,
		})
		if err != nil {
			t.Fatalf("%s prepare: %v", item.name, err)
		}
		options := append([]contextmgr.PrepareOptions(nil), preparer.options...)
		for index := range options {
			options[index].Observer = nil
		}
		got := snapshot{prepared: prepared, messages: append([]conversation.Message(nil), conv.Messages...), options: options}
		if baseline == nil {
			copy := got
			baseline = &copy
		} else if !reflect.DeepEqual(*baseline, got) {
			t.Fatalf("%s observer changed session context:\nlegacy=%#v\nactual=%#v", item.name, *baseline, got)
		}
	}
	if baseline == nil || !baseline.prepared.MessagesChanged || len(baseline.prepared.Diagnostics) != 0 ||
		!strings.Contains(joinSections(baseline.prepared.StableSections), "stable project rules") ||
		!strings.Contains(joinSections(baseline.prepared.StableSections), "stable memory") || len(baseline.messages) != 1 || len(baseline.options) != 1 {
		t.Fatalf("legacy session-context golden changed: %#v", baseline)
	}
}

func TestNoHookSessionCompatibility(t *testing.T) {
	type snapshot struct {
		newPrepared    PreparedContext
		resumePrepared PreparedContext
		newMessages    []conversation.Message
		resumeMessages []conversation.Message
		contextCalls   int
		contextOptions []contextmgr.PrepareOptions
	}
	observers := []struct {
		name     string
		observer contextmgr.CompactionObserver
	}{{name: "nil", observer: nil}, {name: "noop", observer: sessionCompactionObserver{}}}
	var baseline *snapshot
	for _, item := range observers {
		preparer := &fakeContextPreparer{}
		manager := &Manager{
			Instructions: fakeInstructionLoader{sections: []prompt.Section{{Name: "session-policy", Content: "same session policy", Stable: true}}},
			Context:      preparer,
		}
		newConversation := conversation.NewConversation("new", zeroTime())
		resumedConversation := conversation.NewConversation("resumed", zeroTime())
		conversation.AppendUserMessage(newConversation, "new message")
		conversation.AppendAssistantMessage(resumedConversation, "restored message")
		for _, conv := range []*conversation.Conversation{newConversation, resumedConversation} {
			for index := range conv.Messages {
				conv.Messages[index].CreatedAt = time.Time{}
			}
			conv.UpdatedAt = time.Time{}
		}
		opts := contextmgr.PrepareOptions{Mode: contextmgr.ModeAuto, PersistArtifacts: true, Observer: item.observer}
		newPrepared, err := manager.PrepareWithOptions(context.Background(), newConversation, opts)
		if err != nil {
			t.Fatalf("%s new session prepare: %v", item.name, err)
		}
		resumePrepared, err := manager.PrepareWithOptions(context.Background(), resumedConversation, opts)
		if err != nil {
			t.Fatalf("%s resumed session prepare: %v", item.name, err)
		}
		options := append([]contextmgr.PrepareOptions(nil), preparer.options...)
		for index := range options {
			options[index].Observer = nil
		}
		got := snapshot{
			newPrepared: newPrepared, resumePrepared: resumePrepared,
			newMessages:    append([]conversation.Message(nil), newConversation.Messages...),
			resumeMessages: append([]conversation.Message(nil), resumedConversation.Messages...),
			contextCalls:   preparer.calls, contextOptions: options,
		}
		if baseline == nil {
			copy := got
			baseline = &copy
		} else if !reflect.DeepEqual(*baseline, got) {
			t.Fatalf("%s observer changed per-session state:\nlegacy=%#v\nactual=%#v", item.name, *baseline, got)
		}
	}
	if baseline == nil || baseline.contextCalls != 2 || len(baseline.contextOptions) != 2 ||
		len(baseline.newPrepared.Diagnostics) != 0 || len(baseline.resumePrepared.Diagnostics) != 0 ||
		!strings.Contains(joinSections(baseline.newPrepared.StableSections), "same session policy") ||
		!reflect.DeepEqual(baseline.newPrepared.StableSections, baseline.resumePrepared.StableSections) ||
		len(baseline.newMessages) != 1 || baseline.newMessages[0].Content.Text() != "new message" ||
		len(baseline.resumeMessages) != 1 || baseline.resumeMessages[0].Content.Text() != "restored message" {
		t.Fatalf("legacy per-session golden changed: %#v", baseline)
	}
}

type fakeInstructionLoader struct {
	sections    []prompt.Section
	diagnostics []diagnostics.Diagnostic
}

func (l fakeInstructionLoader) Load(ctx context.Context) ([]prompt.Section, []diagnostics.Diagnostic) {
	return l.sections, l.diagnostics
}

type fakeMemoryProvider struct {
	index       memory.Index
	indices     map[memory.Scope]memory.Index
	err         error
	diagnostics []diagnostics.Diagnostic
}

func (p fakeMemoryProvider) LoadIndex(scope memory.Scope) (memory.Index, error) {
	if p.indices != nil {
		if index, ok := p.indices[scope]; ok {
			return index, nil
		}
		return memory.Index{Scope: scope}, p.err
	}
	if p.index.Scope != "" && p.index.Scope != scope {
		return memory.Index{Scope: scope}, p.err
	}
	return p.index, p.err
}

func (p fakeMemoryProvider) Diagnostics() []diagnostics.Diagnostic { return p.diagnostics }

type fakeContextPreparer struct {
	changed bool
	calls   int
	options []contextmgr.PrepareOptions
}

func (p *fakeContextPreparer) Prepare(ctx context.Context, conv *conversation.Conversation, mode contextmgr.Mode) (contextmgr.Result, error) {
	p.calls++
	return contextmgr.Result{Changed: p.changed}, nil
}

func (p *fakeContextPreparer) PrepareWithOptions(ctx context.Context, conv *conversation.Conversation, opts contextmgr.PrepareOptions) (contextmgr.Result, error) {
	p.calls++
	p.options = append(p.options, opts)
	return contextmgr.Result{Changed: p.changed}, nil
}

type sessionCompactionObserver struct{}

func (sessionCompactionObserver) Before(context.Context, contextmgr.Attempt) any { return nil }

func (sessionCompactionObserver) After(context.Context, any, contextmgr.Result, error) {}

type legacyContextPreparer struct{ calls int }

func (p *legacyContextPreparer) Prepare(context.Context, *conversation.Conversation, contextmgr.Mode) (contextmgr.Result, error) {
	p.calls++
	return contextmgr.Result{Changed: true}, nil
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

func joinSections(sections []prompt.Section) string {
	parts := make([]string, 0, len(sections))
	for _, section := range sections {
		parts = append(parts, section.Content)
	}
	return strings.Join(parts, "\n")
}

func sectionContents(sections []prompt.Section) []string {
	contents := make([]string, 0, len(sections))
	for _, section := range sections {
		contents = append(contents, section.Content)
	}
	return contents
}

func assertSectionOrder(t *testing.T, contents []string, wants ...string) {
	t.Helper()
	start := 0
	for _, want := range wants {
		found := -1
		for i := start; i < len(contents); i++ {
			if strings.Contains(contents[i], want) {
				found = i
				break
			}
		}
		if found == -1 {
			t.Fatalf("section containing %q not found after index %d in %#v", want, start, contents)
		}
		start = found + 1
	}
}

func zeroTime() time.Time { return time.Time{} }
