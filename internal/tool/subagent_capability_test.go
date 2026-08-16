package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"xagent/internal/agentrole"
)

func TestRegistrySealFreezesRegistrationAndRejectsDefinitionDrift(t *testing.T) {
	registry := newEmptyRegistry()
	mutable := &mutableDefinitionTool{
		name:        "Mutable",
		description: "before",
		schema:      ObjectSchema(nil, map[string]SchemaProperty{"value": StringProperty("value")}),
		risk:        RiskSafe,
	}
	if err := registry.Register(mutable); err != nil {
		t.Fatal(err)
	}
	mutable.description = "after"
	if err := registry.Seal(); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("Seal accepted a changed registration-time definition: %v", err)
	}

	stable := newEmptyRegistry()
	if err := stable.Register(fakeTool{schema: ObjectSchema(nil, nil)}); err != nil {
		t.Fatal(err)
	}
	if err := stable.Seal(); err != nil {
		t.Fatalf("Seal failed: %v", err)
	}
	if !stable.IsSealed() {
		t.Fatal("registry did not report its sealed lifecycle state")
	}
	if err := stable.Seal(); err == nil {
		t.Fatal("a second Seal must fail")
	}
	if err := stable.Register(&mutableDefinitionTool{name: "Late", schema: ObjectSchema(nil, nil)}); err == nil {
		t.Fatal("registration after Seal must fail")
	}
}

func TestToolRouteAndBackgroundMetadata(t *testing.T) {
	registry, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewAgentTool()); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"Read", "Glob", "Grep"} {
		descriptor, ok := registry.Descriptor(name)
		if !ok {
			t.Fatalf("missing descriptor for %s", name)
		}
		if descriptor.Route != RouteExecutor || !descriptor.Policy.ReadOnly || !descriptor.Policy.SideEffectFree || !descriptor.Policy.ConcurrentSafe || !descriptor.Policy.AllowsBackgroundByDefault() {
			t.Fatalf("%s has unsafe/incomplete metadata: %#v", name, descriptor)
		}
	}
	for _, name := range []string{"Write", "Edit", "Bash"} {
		descriptor, ok := registry.Descriptor(name)
		if !ok || descriptor.Route != RouteExecutor || descriptor.Policy.AllowsBackgroundByDefault() {
			t.Fatalf("%s unexpectedly qualifies for background execution: %#v", name, descriptor)
		}
	}
	for _, name := range []string{LoadSkillToolName, AgentToolName} {
		descriptor, ok := registry.Descriptor(name)
		if !ok || descriptor.Route != RouteSystem {
			t.Fatalf("%s must use the system route: %#v", name, descriptor)
		}
	}
}

func TestRegistryViewRequiresSealAndCannotResurrectFilteredTools(t *testing.T) {
	registry, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.View(ViewOptions{}); err == nil {
		t.Fatal("View accepted a mutable registry")
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}

	view, err := registry.View(ViewOptions{
		AllowedNames:  map[string]struct{}{},
		AlwaysInclude: []string{LoadSkillToolName},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !view.IsSealed() || len(view.Names()) != 0 {
		t.Fatalf("AlwaysInclude reopened a filtered system tool: %#v", view.Names())
	}
	if err := view.Register(NewAgentTool()); err == nil {
		t.Fatal("a sealed view accepted registration")
	}
}

func TestAgentToolSchemaIsStableAndBounded(t *testing.T) {
	agent := NewAgentTool()
	if agent.Name() != AgentToolName || agent.Risk() != RiskSafe {
		t.Fatalf("unexpected Agent identity: %q/%q", agent.Name(), agent.Risk())
	}
	schemaJSON, err := json.Marshal(agent.Schema())
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Type                 string                    `json:"type"`
		Properties           map[string]map[string]any `json:"properties"`
		Required             []string                  `json:"required"`
		AdditionalProperties *bool                     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatalf("Agent schema must be a closed object: %s", schemaJSON)
	}
	if !reflect.DeepEqual(schema.Required, []string{"task", "type"}) {
		t.Fatalf("unexpected required fields: %#v", schema.Required)
	}
	if got := sortedMapKeys(schema.Properties); !reflect.DeepEqual(got, []string{"placement", "role", "task", "type"}) {
		t.Fatalf("Agent schema fields drifted: %#v", got)
	}
	if got := stringSlice(schema.Properties["type"]["enum"]); !reflect.DeepEqual(got, []string{"defined", "fork"}) {
		t.Fatalf("unexpected type enum: %#v", got)
	}
	if got := stringSlice(schema.Properties["placement"]["enum"]); !reflect.DeepEqual(got, []string{"default", "foreground", "background"}) {
		t.Fatalf("unexpected placement enum: %#v", got)
	}
	if got := schema.Properties["task"]["maxLength"]; got != float64(AgentToolMaxTaskBytes) {
		t.Fatalf("task maxLength = %#v, want %d", got, AgentToolMaxTaskBytes)
	}

	registry := newEmptyRegistry()
	if err := registry.Register(agent); err != nil {
		t.Fatal(err)
	}
	valid := `{"task":"inspect this project","type":"defined","role":"reviewer","placement":"foreground"}`
	if _, err := registry.ValidateCall(Call{Name: AgentToolName, ArgumentsJSON: valid}); err != nil {
		t.Fatalf("valid Agent call failed schema validation: %v", err)
	}
	for name, arguments := range map[string]string{
		"unknown field":     `{"task":"x","type":"defined","extra":true}`,
		"unknown type":      `{"task":"x","type":"other"}`,
		"unknown placement": `{"task":"x","type":"fork","placement":"later"}`,
		"oversized task":    `{"task":"` + strings.Repeat("x", AgentToolMaxTaskBytes+1) + `","type":"defined"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := registry.ValidateCall(Call{Name: AgentToolName, ArgumentsJSON: arguments}); err == nil {
				t.Fatalf("invalid Agent call passed: %s", arguments)
			}
		})
	}

	result := agent.Execute(context.Background(), Input{Name: AgentToolName, CallID: "agent-1"})
	if result.Status != StatusError || result.Error == nil || result.Error.Code != ErrInternalRoutingRequired || !result.Error.Recoverable {
		t.Fatalf("direct Agent execution must require the system router: %#v", result)
	}
}

func TestDefaultBackgroundPolicyUsesOnlyFullySafeMetadata(t *testing.T) {
	registry, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewAgentTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}

	policy, err := DefaultBackgroundPolicy(registry)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Enabled || !reflect.DeepEqual(policy.AllowedNames, []string{"Glob", "Grep", "Read"}) || policy.Fingerprint == "" {
		t.Fatalf("unexpected default background policy: %#v", policy)
	}
	for _, name := range []string{"Glob", "Grep", "Read"} {
		if !policy.Allows(name) {
			t.Fatalf("default background policy rejected %s", name)
		}
	}
	for _, name := range []string{"Write", "Edit", "Bash", LoadSkillToolName, AgentToolName, "missing"} {
		if policy.Allows(name) {
			t.Fatalf("default background policy allowed %s", name)
		}
	}

	if err := (BackgroundPolicy{Enabled: true, AllowedNames: []string{"missing"}}).Validate(registry); err == nil {
		t.Fatal("background policy accepted an unknown tool")
	}
	if err := (BackgroundPolicy{Enabled: true, AllowedNames: []string{"Read", "Read"}}).Validate(registry); err == nil {
		t.Fatal("background policy accepted duplicate tool names")
	}
}

func TestBuildCapabilitySetAppliesFiltersMonotonically(t *testing.T) {
	registry, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewLoadSkillTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(NewAgentTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	role := &agentrole.Definition{Metadata: agentrole.Metadata{
		ToolAllow: []string{"Read", "Write", "Bash", LoadSkillToolName, AgentToolName},
		ToolDeny:  []string{"Bash"},
	}}
	foreground, err := BuildCapabilitySet(
		registry,
		role,
		BackgroundPolicy{Enabled: false, AllowedNames: []string{"Read"}},
		map[string]struct{}{AgentToolName: {}},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(foreground.Names, []string{"Read", LoadSkillToolName}) {
		t.Fatalf("foreground capability order mismatch: %#v", foreground.Names)
	}
	for name, reason := range map[string]FilterReason{
		"Write":       FilterPlanMode,
		"Edit":        FilterRoleAllow,
		"Bash":        FilterRoleDeny,
		"Glob":        FilterRoleAllow,
		"Grep":        FilterRoleAllow,
		AgentToolName: FilterRecursive,
	} {
		if got := foreground.FilterReason(name); got != reason {
			t.Fatalf("FilterReason(%q) = %q, want %q", name, got, reason)
		}
	}
	if got := foreground.FilterReason("not-registered"); got != FilterUnknown {
		t.Fatalf("unknown tool reason = %q", got)
	}
	if foreground.Fingerprint == "" || !foreground.Registry.IsSealed() {
		t.Fatalf("capability set was not sealed/fingerprinted: %#v", foreground)
	}

	background, err := BuildCapabilitySet(
		registry,
		nil,
		BackgroundPolicy{Enabled: true, AllowedNames: []string{"Read", "Write"}},
		map[string]struct{}{AgentToolName: {}},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(background.Names, []string{"Read", "Write"}) {
		t.Fatalf("explicit background extension was not an intersection: %#v", background.Names)
	}
	if got := background.FilterReason("Glob"); got != FilterBackgroundDeny {
		t.Fatalf("Glob rejection = %q, want %q", got, FilterBackgroundDeny)
	}
	if got := background.FilterReason(AgentToolName); got != FilterRecursive {
		t.Fatalf("Agent rejection = %q, want %q", got, FilterRecursive)
	}

	cloned := background.Clone()
	cloned.Names[0] = "mutated"
	cloned.Rejections["Glob"] = FilterUnknown
	if background.Names[0] != "Read" || background.Rejections["Glob"] != FilterBackgroundDeny {
		t.Fatal("CapabilitySet.Clone aliases mutable state")
	}

	unrestrictedChild, err := BuildCapabilitySet(registry, nil, BackgroundPolicy{Enabled: false}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if unrestrictedChild.Allows(AgentToolName) {
		t.Fatal("child capability set exposed recursive Agent delegation")
	}
	if got := unrestrictedChild.FilterReason(AgentToolName); got != FilterRecursive {
		t.Fatalf("recursive Agent base rejection = %q, want %q", got, FilterRecursive)
	}

	parentSubset, err := registry.View(ViewOptions{AllowedNames: nameSet("Read")})
	if err != nil {
		t.Fatal(err)
	}
	subsetBackground, err := BuildCapabilitySet(parentSubset, nil, BackgroundPolicy{Enabled: true, AllowedNames: []string{"Read", "Write"}}, nil, false)
	if err != nil {
		t.Fatalf("background policy valid for the sealed registry failed on a parent subset: %v", err)
	}
	if !reflect.DeepEqual(subsetBackground.Names, []string{"Read"}) {
		t.Fatalf("background policy expanded a parent subset: %#v", subsetBackground.Names)
	}
}

func TestCapabilitySwitchMovesOnceToBackgroundWithoutExpansion(t *testing.T) {
	registry, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Seal(); err != nil {
		t.Fatal(err)
	}
	foreground, background, err := BuildCapabilityViews(
		registry,
		nil,
		BackgroundPolicy{Enabled: true, AllowedNames: []string{"Read", "Glob", "Grep"}},
		nil,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	switcher, err := NewCapabilitySwitch(foreground, background)
	if err != nil {
		t.Fatal(err)
	}
	if got := switcher.Current().Names; !reflect.DeepEqual(got, registry.Names()) {
		t.Fatalf("switch did not start in foreground: %#v", got)
	}

	const callers = 16
	var wg sync.WaitGroup
	changed := make(chan bool, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			moved, current := switcher.MoveToBackground()
			changed <- moved
			if !reflect.DeepEqual(current.Names, []string{"Read", "Glob", "Grep"}) {
				t.Errorf("MoveToBackground returned the wrong view: %#v", current.Names)
			}
		}()
	}
	wg.Wait()
	close(changed)
	changedCount := 0
	for moved := range changed {
		if moved {
			changedCount++
		}
	}
	if changedCount != 1 {
		t.Fatalf("background transition linearized %d times, want 1", changedCount)
	}
	if got := switcher.Current().Names; !reflect.DeepEqual(got, []string{"Read", "Glob", "Grep"}) {
		t.Fatalf("active capability expanded or changed after detach: %#v", got)
	}

	expanded, err := BuildCapabilitySet(registry, nil, BackgroundPolicy{Enabled: false}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCapabilitySwitch(background, expanded); err == nil {
		t.Fatal("CapabilitySwitch accepted a background view that expands foreground")
	}

	otherRegistry := newEmptyRegistry()
	if err := otherRegistry.Register(&mutableDefinitionTool{name: "Read", description: "replacement target", schema: ObjectSchema(nil, nil), risk: RiskSafe}); err != nil {
		t.Fatal(err)
	}
	if err := otherRegistry.Seal(); err != nil {
		t.Fatal(err)
	}
	replacement, err := BuildCapabilitySet(otherRegistry, nil, BackgroundPolicy{Enabled: false}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	readOnlyForeground, err := BuildCapabilitySet(registry, &agentrole.Definition{Metadata: agentrole.Metadata{ToolAllow: []string{"Read"}}}, BackgroundPolicy{Enabled: false}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCapabilitySwitch(readOnlyForeground, replacement); err == nil {
		t.Fatal("CapabilitySwitch accepted a replacement registry/execution target")
	}
}

func sortedMapKeys(values map[string]map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func stringSlice(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		text, _ := item.(string)
		result = append(result, text)
	}
	return result
}

type mutableDefinitionTool struct {
	name        string
	description string
	schema      Schema
	risk        Risk
}

func (t *mutableDefinitionTool) Name() string                          { return t.name }
func (t *mutableDefinitionTool) Description() string                   { return t.description }
func (t *mutableDefinitionTool) Schema() Schema                        { return t.schema }
func (t *mutableDefinitionTool) Risk() Risk                            { return t.risk }
func (t *mutableDefinitionTool) Execute(context.Context, Input) Result { return Result{} }
