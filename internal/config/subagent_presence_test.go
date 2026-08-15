package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/subagent"
)

func TestSubagentConfigPresenceIsPreservedUntilResolve(t *testing.T) {
	absent, err := decodePartial("absent.yaml", []byte("llm: {}\nsubagent: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if absent.LLM.ModelAliases.Haiku.Set || absent.Subagent.MaxConcurrent.Set || absent.Subagent.BackgroundTools.Set {
		t.Fatal("absent subagent configuration became present")
	}

	explicit, err := decodePartial("explicit.yaml", []byte(`llm:
  model_aliases:
    haiku: ""
    sonnet: claude-sonnet-test
subagent:
  max_concurrent: 0
  background_tools: []
`))
	if err != nil {
		t.Fatal(err)
	}
	if !explicit.LLM.ModelAliases.Haiku.Set || explicit.LLM.ModelAliases.Haiku.Value != "" {
		t.Fatal("explicit empty model alias was not preserved")
	}
	if !explicit.LLM.ModelAliases.Sonnet.Set || explicit.LLM.ModelAliases.Sonnet.Value != "claude-sonnet-test" {
		t.Fatal("explicit model alias was not preserved")
	}
	if !explicit.Subagent.MaxConcurrent.Set || explicit.Subagent.MaxConcurrent.Value != 0 {
		t.Fatal("explicit zero subagent limit was not preserved")
	}
	if !explicit.Subagent.BackgroundTools.Set || explicit.Subagent.BackgroundTools.Value == nil || len(explicit.Subagent.BackgroundTools.Value) != 0 {
		t.Fatal("explicit empty background tool list was not preserved")
	}
}

func TestResolveSubagentConfigAppliesDefaultsOnce(t *testing.T) {
	resolved, err := ResolveSubagentConfig(PartialSubagentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved.RoleLimits, agentrole.DefaultLimits()) {
		t.Fatalf("role limits = %#v", resolved.RoleLimits)
	}
	if !reflect.DeepEqual(resolved.Limits, subagent.DefaultLimits()) {
		t.Fatalf("runtime limits = %#v", resolved.Limits)
	}
	if resolved.Limits.AutoBackgroundAfter != 10*time.Second || resolved.Limits.MaxTaskDuration != 0 {
		t.Fatalf("duration defaults = %#v", resolved.Limits)
	}
	if resolved.BackgroundTools != nil {
		t.Fatalf("absent background tools should retain default-policy sentinel: %#v", resolved.BackgroundTools)
	}

	partial := PartialSubagentConfig{
		MaxConcurrent:         Optional[int64]{Set: true, Value: 2},
		MaxQueued:             Optional[int64]{Set: true, Value: 3},
		MaxRetainedTasks:      Optional[int64]{Set: true, Value: 5},
		AutoBackgroundAfterMS: Optional[int64]{Set: true, Value: 25},
		MaxTaskDurationMS:     Optional[int64]{Set: true, Value: 100},
		BackgroundTools:       Optional[[]string]{Set: true, Value: []string{}},
	}
	resolved, err = ResolveSubagentConfig(partial)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Limits.MaxConcurrent != 2 || resolved.Limits.MaxQueued != 3 || resolved.Limits.MaxRetainedTasks != 5 {
		t.Fatalf("resolved limits = %#v", resolved.Limits)
	}
	if resolved.Limits.AutoBackgroundAfter != 25*time.Millisecond || resolved.Limits.MaxTaskDuration != 100*time.Millisecond {
		t.Fatalf("resolved durations = %#v", resolved.Limits)
	}
	if resolved.BackgroundTools == nil || len(resolved.BackgroundTools) != 0 {
		t.Fatalf("explicit empty background tools lost presence: %#v", resolved.BackgroundTools)
	}
}

func TestResolveModelAliasesRejectsExplicitEmptyAndKeepsAbsentUnavailable(t *testing.T) {
	resolved, err := ResolveModelAliases(PartialModelAliases{}, 256)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Haiku != "" || resolved.Sonnet != "" || resolved.Opus != "" {
		t.Fatalf("absent aliases were defaulted: %#v", resolved)
	}

	_, err = ResolveModelAliases(PartialModelAliases{Haiku: Optional[string]{Set: true, Value: ""}}, 256)
	if err == nil || !strings.Contains(err.Error(), "llm.model_aliases.haiku") {
		t.Fatalf("explicit empty alias was accepted: %v", err)
	}

	resolved, err = ResolveModelAliases(PartialModelAliases{
		Haiku: Optional[string]{Set: true, Value: "claude-haiku-test"},
	}, 256)
	if err != nil || resolved.Haiku != "claude-haiku-test" {
		t.Fatalf("non-empty alias did not resolve: %#v %v", resolved, err)
	}
}

func TestResolveSubagentConfigFailsClosedOnInvalidLimits(t *testing.T) {
	cases := []struct {
		name    string
		partial PartialSubagentConfig
	}{
		{name: "zero concurrent", partial: PartialSubagentConfig{MaxConcurrent: Optional[int64]{Set: true, Value: 0}}},
		{name: "retained below active capacity", partial: PartialSubagentConfig{
			MaxConcurrent:    Optional[int64]{Set: true, Value: 4},
			MaxQueued:        Optional[int64]{Set: true, Value: 32},
			MaxRetainedTasks: Optional[int64]{Set: true, Value: 35},
		}},
		{name: "cache value above total", partial: PartialSubagentConfig{
			ReadCacheMaxBytes:      Optional[int64]{Set: true, Value: 8},
			ReadCacheMaxValueBytes: Optional[int64]{Set: true, Value: 9},
		}},
		{name: "negative wall clock", partial: PartialSubagentConfig{MaxTaskDurationMS: Optional[int64]{Set: true, Value: -1}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ResolveSubagentConfig(testCase.partial); err == nil {
				t.Fatal("invalid subagent configuration was accepted")
			}
		})
	}
}

func TestResolveConfigReservesOneMaximumSubagentResultInsidePlanningWindow(t *testing.T) {
	t.Parallel()

	base := PartialAppConfig{Context: PartialContextConfig{
		ModelWindowTokens:  Optional[int64]{Set: true, Value: 1_000},
		AutoMarginTokens:   Optional[int64]{Set: true, Value: 100},
		ManualMarginTokens: Optional[int64]{Set: true, Value: 100},
		RecentKeepTokens:   Optional[int64]{Set: true, Value: 100},
	}}
	valid := base
	valid.Subagent.MaxResultBytes = Optional[int64]{Set: true, Value: 1_024}
	if _, err := ResolveConfig(MergeResult{Value: valid}, LoadOptions{}); err != nil {
		t.Fatalf("valid result reserve was rejected: %v", err)
	}

	tooLarge := base
	tooLarge.Subagent.MaxResultBytes = Optional[int64]{Set: true, Value: 4_096}
	if _, err := ResolveConfig(MergeResult{Value: tooLarge}, LoadOptions{}); err == nil ||
		!strings.Contains(err.Error(), "subagent.max_result_bytes") {
		t.Fatalf("result reserve that consumes the planning window was accepted: %v", err)
	}
}
