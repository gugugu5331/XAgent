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

func TestWorktreeConfigPresenceIsPreservedUntilResolve(t *testing.T) {
	absent, err := decodePartial("absent.yaml", []byte("subagent:\n  worktree: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if absent.Subagent.Worktree.Init.Copy.Set || absent.Subagent.Worktree.Init.Link.Set ||
		absent.Subagent.Worktree.Init.IgnoredCopy.Set || absent.Subagent.Worktree.Lifecycle.RetentionTTLMS.Set {
		t.Fatalf("absent worktree values became present: %#v", absent.Subagent.Worktree)
	}

	explicit, err := decodePartial("explicit.yaml", []byte(`subagent:
  worktree:
    lifecycle:
      retention_ttl_ms: 1000
    init:
      copy: []
      link:
        - source: node_modules
          target: node_modules
      ignored_copy:
        - source: local.yaml
          target: config/local.yaml
      git_hooks:
        enabled: true
        path: .githooks
`))
	if err != nil {
		t.Fatal(err)
	}
	worktree := explicit.Subagent.Worktree
	if !worktree.Lifecycle.RetentionTTLMS.Set || worktree.Lifecycle.RetentionTTLMS.Value != 1000 {
		t.Fatalf("retention TTL presence lost: %#v", worktree.Lifecycle)
	}
	if !worktree.Init.Copy.Set || worktree.Init.Copy.Value == nil || len(worktree.Init.Copy.Value) != 0 {
		t.Fatalf("explicit empty copy list lost: %#v", worktree.Init.Copy)
	}
	if !worktree.Init.Link.Set || len(worktree.Init.Link.Value) != 1 || worktree.Init.Link.Value[0].Source != "node_modules" {
		t.Fatalf("link rules = %#v", worktree.Init.Link)
	}
	if !worktree.Init.IgnoredCopy.Set || len(worktree.Init.IgnoredCopy.Value) != 1 {
		t.Fatalf("ignored copy rules = %#v", worktree.Init.IgnoredCopy)
	}
	if !worktree.Init.GitHooks.Enabled.Set || !worktree.Init.GitHooks.Enabled.Value ||
		!worktree.Init.GitHooks.Path.Set || worktree.Init.GitHooks.Path.Value != ".githooks" {
		t.Fatalf("git hooks presence = %#v", worktree.Init.GitHooks)
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

func TestResolveWorktreeConfigAppliesBoundedDefaults(t *testing.T) {
	resolved, err := ResolveSubagentConfig(PartialSubagentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := resolved.Worktree.Lifecycle
	if lifecycle.RetentionTTL != 24*time.Hour || lifecycle.JanitorInterval != 30*time.Minute ||
		lifecycle.GitTimeout != 30*time.Second || lifecycle.LockTimeout != 10*time.Second ||
		lifecycle.InitTimeout != 2*time.Minute || lifecycle.RecoveryTimeout != 30*time.Second ||
		lifecycle.SettleTimeout != 2*time.Minute || lifecycle.JanitorTimeout != time.Minute {
		t.Fatalf("worktree lifecycle defaults = %#v", lifecycle)
	}
	limits := resolved.Worktree.Limits
	if limits.MaxActive != 8 || limits.MaxRetained != 32 || limits.MaxNameBytes != 192 ||
		limits.MaxSegmentBytes != 64 || limits.MaxDepth != 8 || limits.MaxInitFiles != 10_000 ||
		limits.MaxInitBytes != 1<<30 || limits.MaxInitDepth != 32 ||
		limits.MaxJanitorCandidates != 256 || limits.MaxJanitorConcurrency != 4 {
		t.Fatalf("worktree limit defaults = %#v", limits)
	}
	if resolved.Worktree.Init.Copy != nil || resolved.Worktree.Init.Link != nil || resolved.Worktree.Init.IgnoredCopy != nil ||
		resolved.Worktree.Init.GitHooks.Enabled || resolved.Worktree.Init.GitHooks.Path != "" {
		t.Fatalf("worktree init must default to no implicit actions: %#v", resolved.Worktree.Init)
	}
}

func TestResolveWorktreeConfigPreservesExplicitInitRules(t *testing.T) {
	partial := PartialSubagentConfig{Worktree: PartialWorktreeConfig{Init: PartialWorktreeInitConfig{
		Copy:        Optional[[]PartialWorktreeCopyRule]{Set: true, Value: []PartialWorktreeCopyRule{}},
		Link:        Optional[[]PartialWorktreeLinkRule]{Set: true, Value: []PartialWorktreeLinkRule{{Source: "node_modules", Target: "node_modules"}}},
		IgnoredCopy: Optional[[]PartialWorktreeCopyRule]{Set: true, Value: []PartialWorktreeCopyRule{{Source: "local.yaml", Target: "config/local.yaml"}}},
		GitHooks: PartialWorktreeGitHooksRule{
			Enabled: Optional[bool]{Set: true, Value: true},
			Path:    Optional[string]{Set: true, Value: ".githooks"},
		},
	}}}
	resolved, err := ResolveSubagentConfig(partial)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Worktree.Init.Copy == nil || len(resolved.Worktree.Init.Copy) != 0 {
		t.Fatalf("explicit empty copy list lost: %#v", resolved.Worktree.Init.Copy)
	}
	if len(resolved.Worktree.Init.Link) != 1 || resolved.Worktree.Init.Link[0].Source != "node_modules" {
		t.Fatalf("link rules = %#v", resolved.Worktree.Init.Link)
	}
	if len(resolved.Worktree.Init.IgnoredCopy) != 1 || resolved.Worktree.Init.IgnoredCopy[0].Target != "config/local.yaml" {
		t.Fatalf("ignored-copy rules = %#v", resolved.Worktree.Init.IgnoredCopy)
	}
	if !resolved.Worktree.Init.GitHooks.Enabled || resolved.Worktree.Init.GitHooks.Path != ".githooks" {
		t.Fatalf("git hooks rule = %#v", resolved.Worktree.Init.GitHooks)
	}
}

func TestResolveWorktreeConfigRejectsUnsafeBounds(t *testing.T) {
	tests := []struct {
		name    string
		partial PartialWorktreeConfig
	}{
		{name: "zero retention TTL", partial: PartialWorktreeConfig{Lifecycle: PartialWorktreeLifecycleConfig{RetentionTTLMS: Optional[int64]{Set: true, Value: 0}}}},
		{name: "git timeout above hard cap", partial: PartialWorktreeConfig{Lifecycle: PartialWorktreeLifecycleConfig{GitTimeoutMS: Optional[int64]{Set: true, Value: int64((5*time.Minute)/time.Millisecond) + 1}}}},
		{name: "init timeout above initializer hard cap", partial: PartialWorktreeConfig{Lifecycle: PartialWorktreeLifecycleConfig{InitTimeoutMS: Optional[int64]{Set: true, Value: int64((5*time.Minute)/time.Millisecond) + 1}}}},
		{name: "active above hard cap", partial: PartialWorktreeConfig{Limits: PartialWorktreeLimits{MaxActive: Optional[int64]{Set: true, Value: 65}}}},
		{name: "init files above manifest hard cap", partial: PartialWorktreeConfig{Limits: PartialWorktreeLimits{MaxInitFiles: Optional[int64]{Set: true, Value: 10_001}}}},
		{name: "init bytes above initializer hard cap", partial: PartialWorktreeConfig{Limits: PartialWorktreeLimits{MaxInitBytes: Optional[int64]{Set: true, Value: 1<<30 + 1}}}},
		{name: "init depth above initializer hard cap", partial: PartialWorktreeConfig{Limits: PartialWorktreeLimits{MaxInitDepth: Optional[int64]{Set: true, Value: 65}}}},
		{name: "retained below active", partial: PartialWorktreeConfig{Limits: PartialWorktreeLimits{
			MaxActive: Optional[int64]{Set: true, Value: 8}, MaxRetained: Optional[int64]{Set: true, Value: 7},
		}}},
		{name: "segment above complete name", partial: PartialWorktreeConfig{Limits: PartialWorktreeLimits{
			MaxNameBytes: Optional[int64]{Set: true, Value: 8}, MaxSegmentBytes: Optional[int64]{Set: true, Value: 9},
		}}},
		{name: "janitor concurrency above candidates", partial: PartialWorktreeConfig{Limits: PartialWorktreeLimits{
			MaxJanitorCandidates: Optional[int64]{Set: true, Value: 3}, MaxJanitorConcurrency: Optional[int64]{Set: true, Value: 4},
		}}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ResolveSubagentConfig(PartialSubagentConfig{Worktree: testCase.partial})
			if err == nil || !strings.Contains(err.Error(), "subagent.worktree") {
				t.Fatalf("unsafe worktree configuration accepted: %v", err)
			}
		})
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
