package sessionctx

import (
	"context"
	"strings"
	"testing"
	"time"

	"xagent/internal/contextmgr"
	"xagent/internal/conversation"
	"xagent/internal/diagnostics"
	"xagent/internal/memory"
	"xagent/internal/prompt"
)

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
}

func (p *fakeContextPreparer) Prepare(ctx context.Context, conv *conversation.Conversation, mode contextmgr.Mode) (contextmgr.Result, error) {
	p.calls++
	return contextmgr.Result{Changed: p.changed}, nil
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
