package provider

import (
	"strings"
	"testing"

	"xagent/internal/config"
	"xagent/internal/prompt"
	"xagent/internal/tool"
)

func TestPromptPrefixSnapshotRequiresAndFingerprintsScope(t *testing.T) {
	request := ChatRequest{
		StableSystem: []SystemBlock{{Name: "project", Content: safeText("project rules"), Cacheable: true, Scope: prompt.ScopeProject}},
	}
	snapshot, err := CapturePromptPrefix(request)
	if err != nil {
		t.Fatalf("capture scoped prefix: %v", err)
	}
	tampered := snapshot
	tampered.StableSystem = cloneSystemBlocks(snapshot.StableSystem)
	tampered.StableSystem[0].Scope = prompt.ScopeUser
	if err := tampered.Validate(); err == nil {
		t.Fatal("snapshot validated after scope changed without updating fingerprint")
	}
	for _, scope := range []prompt.Scope{"", "future"} {
		request.StableSystem[0].Scope = scope
		if _, err := CapturePromptPrefix(request); err == nil {
			t.Fatalf("captured invalid scope %q", scope)
		}
	}
}

func TestPromptPrefixSnapshotCaptureDeepCopiesAndValidatesSplitLayout(t *testing.T) {
	request := ChatRequest{
		Model: "parent-model",
		StableSystem: []SystemBlock{
			{Name: "stable", Content: safeText("stable rules"), Cacheable: true, Scope: prompt.ScopeGlobal},
		},
		DynamicSystem: []SystemBlock{
			{Name: "dynamic", Content: safeText("dynamic reminder"), Scope: prompt.ScopeRuntime},
		},
		Messages: []ModelMessage{{Role: ModelMessageRoleUser, Content: safeText("parent message")}},
		Thinking: config.ThinkingConfig{Enabled: true, Show: true, BudgetTokens: 4096},
		Tools: []ToolDefinition{{
			Name: "Read", Description: "read files",
			Schema: tool.ObjectSchema([]string{"path"}, map[string]tool.SchemaProperty{"path": tool.StringProperty("path")}),
		}},
		Cache: CachePolicy{EnablePromptCache: true, CacheTools: true},
	}

	snapshot, err := CapturePromptPrefix(request)
	if err != nil {
		t.Fatalf("capture prompt prefix: %v", err)
	}
	if snapshot.OrderedSystem || snapshot.System != nil || len(snapshot.StableSystem) != 1 || len(snapshot.DynamicSystem) != 1 {
		t.Fatalf("captured layout = %#v", snapshot)
	}
	if snapshot.MessagePrefix != 1 || len(snapshot.ToolFingerprint) != 64 || len(snapshot.Fingerprint) != 64 {
		t.Fatalf("captured prefix identity = %#v", snapshot)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("captured snapshot invalid: %v", err)
	}

	request.StableSystem[0].Content = safeText("mutated stable")
	request.Messages[0].Content = safeText("mutated parent")
	request.Tools[0].Schema.Properties["path"] = tool.EnumProperty("mutated", "secret")
	child := snapshot.BuildChild(
		[]SystemBlock{{Name: "role", Content: safeText("role instructions"), Cacheable: true, Scope: prompt.ScopeRuntime}},
		[]ModelMessage{{Role: ModelMessageRoleUser, Content: safeText("child task")}},
		snapshot.Tools,
	)
	if err := child.Validate(); err != nil {
		t.Fatalf("built child invalid: %v", err)
	}
	if child.System != nil || len(child.StableSystem) != 1 || len(child.DynamicSystem) != 2 ||
		child.StableSystem[0].Content.Text() != "stable rules" || child.DynamicSystem[1].Content.Text() != "role instructions" ||
		child.DynamicSystem[1].Cacheable || len(child.Messages) != 2 || child.Messages[0].Content.Text() != "parent message" ||
		child.Cache.CacheTools != true {
		t.Fatalf("child did not preserve immutable prefix/cache: %#v", child)
	}
	if property := child.Tools[0].Schema.Properties["path"]; property.Description != "path" || len(property.Enum) != 0 {
		t.Fatalf("captured tool schema aliased caller: %#v", property)
	}

	// Returned requests must not alias the immutable snapshot either.
	child.Messages[0].Content = safeText("mutated child")
	child.Tools[0].Schema.Properties["path"] = tool.EnumProperty("changed child", "x")
	second := snapshot.BuildChild(nil, nil, snapshot.Tools)
	if second.Messages[0].Content.Text() != "parent message" || second.Tools[0].Schema.Properties["path"].Description != "path" {
		t.Fatalf("child request mutated snapshot: %#v", second)
	}

	tampered := snapshot
	tampered.Fingerprint = strings.Repeat("0", 64)
	if err := tampered.Validate(); err == nil {
		t.Fatal("tampered prompt fingerprint validated")
	}
}

func TestPromptPrefixSnapshotChangedToolsDropsOnlyToolCache(t *testing.T) {
	snapshot, err := CapturePromptPrefix(ChatRequest{
		Model: "parent-model",
		StableSystem: []SystemBlock{
			{Name: "stable", Content: safeText("stable rules"), Cacheable: true, Scope: prompt.ScopeGlobal},
		},
		Messages: []ModelMessage{{Role: ModelMessageRoleUser, Content: safeText("parent")}},
		Tools: []ToolDefinition{
			{Name: "Read", Description: "read", Schema: tool.Schema{Type: "object"}},
			{Name: "Write", Description: "write", Schema: tool.Schema{Type: "object"}},
		},
		Cache: CachePolicy{EnablePromptCache: true, CacheTools: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	child := snapshot.BuildChild(
		[]SystemBlock{{Name: "role", Content: safeText("restricted role"), Scope: prompt.ScopeRuntime}},
		[]ModelMessage{{Role: ModelMessageRoleUser, Content: safeText("task")}},
		[]ToolDefinition{{Name: "Read", Description: "read", Schema: tool.Schema{Type: "object"}}},
	)
	if !child.Cache.EnablePromptCache || child.Cache.CacheTools {
		t.Fatalf("changed tools cache policy = %#v, want system-only cache", child.Cache)
	}
	system := toAnthropicSystemBlocks(child)
	if len(system) != 2 || system[0].CacheControl.Type == "" || system[1].CacheControl.Type != "" {
		t.Fatalf("system cache boundary was not retained: %#v", system)
	}
	tools := toAnthropicTools(child)
	if len(tools) != 1 || tools[0].OfTool == nil || tools[0].OfTool.CacheControl.Type != "" {
		t.Fatalf("changed child tools retained cache breakpoint: %#v", tools)
	}
}

func TestPromptPrefixSnapshotPreservesOrderedLayoutAndMaterializesRegistry(t *testing.T) {
	registry, err := tool.NewReadOnlyRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := ChatRequest{
		Model:    "ordered-model",
		System:   []SystemBlock{{Name: "fixed", Content: safeText("fixed"), Scope: prompt.ScopeGlobal}},
		ToolDefs: registry,
		Cache:    CachePolicy{EnablePromptCache: true, SystemBreakpointName: "fixed", CacheTools: true},
	}
	snapshot, err := CapturePromptPrefix(request)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.OrderedSystem || snapshot.System == nil || snapshot.StableSystem != nil || snapshot.DynamicSystem != nil || len(snapshot.Tools) != 3 {
		t.Fatalf("ordered/registry capture = %#v", snapshot)
	}
	child := snapshot.BuildChild([]SystemBlock{{Name: "role", Content: safeText("role"), Scope: prompt.ScopeRuntime}}, nil, snapshot.Tools[:1])
	if child.System == nil || child.StableSystem != nil || child.DynamicSystem != nil || len(child.System) != 2 || child.ToolDefs != nil {
		t.Fatalf("ordered child changed representation or retained registry: %#v", child)
	}
}

func TestPromptPrefixSnapshotPreservesEmptyOrderedLayout(t *testing.T) {
	request := ChatRequest{System: []SystemBlock{}}
	snapshot, err := CapturePromptPrefix(request)
	if err != nil {
		t.Fatalf("capture empty ordered system: %v", err)
	}
	if !snapshot.OrderedSystem || snapshot.System == nil || len(snapshot.System) != 0 {
		t.Fatalf("empty ordered layout was collapsed: %#v", snapshot)
	}
	child := snapshot.BuildChild(nil, nil, nil)
	if child.System == nil || child.StableSystem != nil || child.DynamicSystem != nil {
		t.Fatalf("empty ordered child changed representation: %#v", child)
	}
}

func TestChatRequestValidateRejectsMixedLayoutsAndUnknownRoles(t *testing.T) {
	invalid := []ChatRequest{
		{System: []SystemBlock{}, StableSystem: []SystemBlock{{Content: safeText("stable")}}},
		{Messages: []ModelMessage{{Role: ModelMessageRole("future"), Content: safeText("payload")}}},
		{Messages: []ModelMessage{{Role: ModelMessageRoleSubagentResult, Content: safeText("not-json")}}},
		{Messages: []ModelMessage{{Role: ModelMessageRoleSubagentResult, Content: safeText(`{"schema_version":1}`), ToolCallID: "must-not-pair"}}},
	}
	for index, request := range invalid {
		if err := request.Validate(); err == nil {
			t.Fatalf("invalid request %d validated: %#v", index, request)
		}
		if _, err := CapturePromptPrefix(request); err == nil {
			t.Fatalf("invalid request %d captured", index)
		}
	}
}
