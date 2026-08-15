package tool

import (
	"context"
	"reflect"
	"testing"
)

func TestLoadSkillTool(t *testing.T) {
	loader := NewLoadSkillTool()
	if loader.Name() != LoadSkillToolName {
		t.Fatalf("unexpected name: %q", loader.Name())
	}
	if loader.Risk() != RiskSafe {
		t.Fatalf("load_skill must be safe, got %q", loader.Risk())
	}
	schema := loader.Schema()
	if schema.Type != "object" || !reflect.DeepEqual(schema.Required, []string{"name"}) {
		t.Fatalf("unexpected schema: %#v", schema)
	}
	if schema.Properties["name"].Type != "string" || schema.Properties["args"].Type != "string" {
		t.Fatalf("schema must contain name and args strings: %#v", schema.Properties)
	}
	result := loader.Execute(context.Background(), Input{Name: LoadSkillToolName, CallID: "call-1"})
	if result.Status != StatusError || result.Error == nil || result.Error.Code != ErrInternalRoutingRequired || !result.Error.Recoverable {
		t.Fatalf("direct execution must require internal routing: %#v", result)
	}
}

func TestRegistryView(t *testing.T) {
	base, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Register(NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	if err := base.Seal(); err != nil {
		t.Fatal(err)
	}
	baseNames := []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep", LoadSkillToolName}
	if !reflect.DeepEqual(base.Names(), baseNames) {
		t.Fatalf("unexpected base order: %#v", base.Names())
	}

	tests := []struct {
		name    string
		options ViewOptions
		want    []string
	}{
		{name: "unrestricted", options: ViewOptions{}, want: baseNames},
		{name: "empty allowlist cannot revive system tool", options: ViewOptions{AllowedNames: map[string]struct{}{}, AlwaysInclude: []string{LoadSkillToolName}}, want: []string{}},
		{name: "partial cannot revive system tool", options: ViewOptions{AllowedNames: nameSet("Bash", "Read"), AlwaysInclude: []string{LoadSkillToolName}}, want: []string{"Read", "Bash"}},
		{name: "read only", options: ViewOptions{ReadOnly: true, AlwaysInclude: []string{LoadSkillToolName}}, want: []string{"Read", "Glob", "Grep", LoadSkillToolName}},
		{name: "allowlist and read only intersect", options: ViewOptions{AllowedNames: nameSet("Read", "Write", "Grep", LoadSkillToolName), AlwaysInclude: []string{LoadSkillToolName}, ReadOnly: true}, want: []string{"Read", "Grep", LoadSkillToolName}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view, err := base.View(tt.options)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(view.Names(), tt.want) {
				t.Fatalf("view order mismatch: got %#v want %#v", view.Names(), tt.want)
			}
			assertRegistryDefinitionsMatch(t, view, tt.want)
			for _, name := range tt.want {
				baseTool, _ := base.Get(name)
				viewTool, ok := view.Get(name)
				if !ok || baseTool != viewTool {
					t.Fatalf("view did not reuse %s tool instance", name)
				}
			}
			view.order = append(view.order, "not-in-base")
			if !reflect.DeepEqual(base.Names(), baseNames) {
				t.Fatalf("mutating view changed base: %#v", base.Names())
			}
		})
	}

	if _, err := base.View(ViewOptions{AllowedNames: nameSet("missing")}); err == nil {
		t.Fatal("unknown allowed tool must fail")
	}
	if _, err := base.View(ViewOptions{AlwaysInclude: []string{"missing"}}); err == nil {
		t.Fatal("unknown always-included tool must fail")
	}
	view, err := base.View(ViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := view.Register(NewReadTool(t.TempDir())); err == nil {
		t.Fatal("filtered view must be immutable")
	}
}

func nameSet(names ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result
}

func assertRegistryDefinitionsMatch(t *testing.T, registry *Registry, want []string) {
	t.Helper()
	listed := registry.List()
	if len(listed) != len(want) {
		t.Fatalf("List length mismatch: %d", len(listed))
	}
	for index, item := range listed {
		if item.Name() != want[index] {
			t.Fatalf("List[%d] = %q, want %q", index, item.Name(), want[index])
		}
	}
	anthropic := registry.AnthropicDefinitions()
	openAI := registry.OpenAIDefinitions()
	if len(anthropic) != len(want) || len(openAI) != len(want) {
		t.Fatalf("definition lengths mismatch: anthropic=%d openai=%d", len(anthropic), len(openAI))
	}
	for index, name := range want {
		if anthropic[index].Name != name || openAI[index].Function.Name != name {
			t.Fatalf("definition[%d] mismatch: %#v %#v", index, anthropic[index], openAI[index])
		}
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("Get(%q) failed", name)
		}
	}
}
