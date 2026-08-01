package config

import "testing"

func TestMergePreservesExplicitFalseZeroAndNegative(t *testing.T) {
	defaults := PartialAppConfig{
		UI:    PartialUIConfig{ShowResponseTimer: Optional[bool]{Set: true, Value: true}},
		Tool:  PartialToolConfig{TimeoutMS: Optional[int64]{Set: true, Value: 30_000}},
		Agent: PartialAgentConfig{MaxIterations: Optional[int64]{Set: true, Value: 10}},
	}
	user := PartialAppConfig{
		UI: PartialUIConfig{ShowResponseTimer: Optional[bool]{Set: true, Value: false}},
	}
	project := PartialAppConfig{
		Tool: PartialToolConfig{TimeoutMS: Optional[int64]{Set: true, Value: 0}},
	}
	runtime := PartialAppConfig{
		Agent: PartialAgentConfig{MaxIterations: Optional[int64]{Set: true, Value: -1}},
	}

	merged, err := MergeLayers(
		ConfigLayer{Source: SourceRuntime, Value: runtime},
		ConfigLayer{Source: SourceDefault, Value: defaults},
		ConfigLayer{Source: SourceProject, Value: project},
		ConfigLayer{Source: SourceUser, Value: user},
	)
	if err != nil {
		t.Fatal("merge config layers failed")
	}
	if !merged.Value.UI.ShowResponseTimer.Set || merged.Value.UI.ShowResponseTimer.Value {
		t.Fatal("merge replaced explicit false")
	}
	if !merged.Value.Tool.TimeoutMS.Set || merged.Value.Tool.TimeoutMS.Value != 0 {
		t.Fatal("merge replaced explicit zero")
	}
	if !merged.Value.Agent.MaxIterations.Set || merged.Value.Agent.MaxIterations.Value != -1 {
		t.Fatal("merge replaced explicit negative value before validation")
	}
	if merged.Provenance["ui.show_response_timer"] != SourceUser ||
		merged.Provenance["tool.timeout_ms"] != SourceProject ||
		merged.Provenance["agent.max_iterations"] != SourceRuntime {
		t.Fatal("merge did not record winning field provenance")
	}
}

func TestMergeMCPServerReplacementDoesNotAliasOrInherit(t *testing.T) {
	userHeaders := map[string]string{"Authorization": "user-value"}
	user := PartialAppConfig{MCP: PartialMCPConfig{Servers: map[string]PartialMCPServerConfig{
		"shared": {
			URL:     Optional[string]{Set: true, Value: "https://user.invalid"},
			Headers: Optional[map[string]string]{Set: true, Value: userHeaders},
		},
		"user-only": {Headers: Optional[map[string]string]{Set: true, Value: userHeaders}},
	}}}
	project := PartialAppConfig{MCP: PartialMCPConfig{Servers: map[string]PartialMCPServerConfig{
		"shared": {Disabled: Optional[bool]{Set: true, Value: true}},
	}}}

	merged, err := MergeLayers(
		ConfigLayer{Source: SourceUser, Value: user},
		ConfigLayer{Source: SourceProject, Value: project},
	)
	if err != nil {
		t.Fatal("merge MCP layers failed")
	}
	shared := merged.Value.MCP.Servers["shared"]
	if !shared.Disabled.Set || !shared.Disabled.Value || shared.URL.Set || shared.Headers.Set {
		t.Fatal("higher-priority MCP server inherited lower-priority fields")
	}
	if _, ok := merged.Value.MCP.Servers["user-only"]; !ok {
		t.Fatal("different MCP server name was discarded")
	}
	userOnly := merged.Value.MCP.Servers["user-only"]
	userHeaders["Authorization"] = "changed"
	if shared.Headers.Set || !userOnly.Headers.Set || userOnly.Headers.Value["Authorization"] != "user-value" {
		t.Fatal("MCP server replacement inherited or aliased lower-layer values")
	}
	if merged.Provenance["mcp.servers.shared"] != SourceProject {
		t.Fatal("MCP server provenance did not follow whole-entry replacement")
	}
}
