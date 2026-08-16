package provider

import (
	"reflect"
	"testing"

	"xagent/internal/prompt"
	"xagent/internal/tool"
)

func TestForkWorkspaceFiltersParentScopesAndReplacesTools(t *testing.T) {
	parent, err := CapturePromptPrefix(ChatRequest{
		Model: "parent-model",
		StableSystem: []SystemBlock{
			{Name: "global", Content: safeText("PARENT-GLOBAL"), Cacheable: true, Scope: prompt.ScopeGlobal},
			{Name: "user", Content: safeText("PARENT-USER"), Cacheable: true, Scope: prompt.ScopeUser},
			{Name: "project", Content: safeText("MAIN-PROJECT-SECRET"), Cacheable: true, Scope: prompt.ScopeProject},
		},
		DynamicSystem: []SystemBlock{{Name: "runtime", Content: safeText("MAIN-RUNTIME-SECRET"), Scope: prompt.ScopeRuntime}},
		Messages:      []ModelMessage{{Role: ModelMessageRoleUser, Content: safeText("PARENT-MESSAGE")}},
		Tools:         []ToolDefinition{{Name: "ParentOnly", Description: "parent", Schema: tool.Schema{Type: "object"}}},
		Cache:         CachePolicy{EnablePromptCache: true, CacheTools: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := parent
	project := []SystemBlock{{Name: "project", Content: safeText("WORKTREE-PROJECT"), Cacheable: true, Scope: prompt.ScopeProject}}
	runtime := []SystemBlock{{Name: "runtime", Content: safeText("WORKTREE-RUNTIME"), Scope: prompt.ScopeRuntime}}
	tools := []ToolDefinition{{Name: "ChildOnly", Description: "child", Schema: tool.Schema{Type: "object"}}}

	child, err := parent.BuildWorkspaceChild(project, runtime, tools)
	if err != nil {
		t.Fatal(err)
	}
	if child.System != nil || len(child.StableSystem) != 3 || len(child.DynamicSystem) != 1 {
		t.Fatalf("workspace child layout = %#v", child)
	}
	if got := []string{child.StableSystem[0].Content.Text(), child.StableSystem[1].Content.Text(), child.StableSystem[2].Content.Text()}; !reflect.DeepEqual(got, []string{"PARENT-GLOBAL", "PARENT-USER", "WORKTREE-PROJECT"}) {
		t.Fatalf("workspace stable blocks = %v", got)
	}
	if child.DynamicSystem[0].Content.Text() != "WORKTREE-RUNTIME" || len(child.Messages) != 1 || child.Messages[0].Content.Text() != "PARENT-MESSAGE" {
		t.Fatalf("workspace runtime/messages = %#v/%#v", child.DynamicSystem, child.Messages)
	}
	if len(child.Tools) != 1 || child.Tools[0].Name != "ChildOnly" || child.Cache.CacheTools {
		t.Fatalf("workspace tools/cache = %#v/%#v", child.Tools, child.Cache)
	}
	if parent.Fingerprint != before.Fingerprint || parent.StableSystem[2].Content.Text() != "MAIN-PROJECT-SECRET" || parent.Tools[0].Name != "ParentOnly" {
		t.Fatal("BuildWorkspaceChild mutated its parent snapshot")
	}
}

func TestForkWorkspaceFailsClosedOnUnknownScopes(t *testing.T) {
	parent, err := CapturePromptPrefix(ChatRequest{StableSystem: []SystemBlock{{
		Name: "global", Content: safeText("global"), Cacheable: true, Scope: prompt.ScopeGlobal,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []prompt.Scope{"", "future"} {
		tampered := parent
		tampered.StableSystem = append([]SystemBlock(nil), parent.StableSystem...)
		tampered.StableSystem[0].Scope = scope
		if _, buildErr := tampered.BuildWorkspaceChild(nil, nil, nil); buildErr == nil {
			t.Fatalf("tampered parent scope %q was accepted", scope)
		}
		if _, buildErr := parent.BuildWorkspaceChild(
			[]SystemBlock{{Name: "bad", Content: safeText("bad"), Scope: scope}}, nil, nil,
		); buildErr == nil {
			t.Fatalf("workspace project scope %q was accepted", scope)
		}
	}
}

func TestForkWorkspacePreservesEmptyOrderedSystem(t *testing.T) {
	parent, err := CapturePromptPrefix(ChatRequest{System: []SystemBlock{}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := parent.BuildWorkspaceChild(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if child.System == nil || len(child.System) != 0 || child.StableSystem != nil || child.DynamicSystem != nil {
		t.Fatalf("empty ordered workspace child = %#v", child)
	}
}
