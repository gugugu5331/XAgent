package config

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"xagent/internal/redact"
)

var approvedAppConfigNumericKeys = []string{
	"agent.max_iterations",
	"agent.max_unknown_tool_calls",
	"artifact.max_file_bytes",
	"artifact.max_total_bytes",
	"artifact.retention_days",
	"context.auto_margin_tokens",
	"context.manual_margin_tokens",
	"context.model_window_tokens",
	"context.preview_chars",
	"context.recent_keep_messages",
	"context.recent_keep_tokens",
	"context.summary_failure_limit",
	"context.tool_result_threshold_chars",
	"context.tool_results_threshold_chars",
	"diagnostics.max_item_bytes",
	"diagnostics.max_items",
	"diagnostics.max_total_bytes",
	"files.read_max_bytes",
	"files.scan_max_bytes",
	"files.scan_max_directories",
	"files.scan_max_files",
	"files.scan_max_lines",
	"instructions.max_expanded_bytes",
	"instructions.max_file_bytes",
	"instructions.max_files",
	"instructions.max_include_depth",
	"instructions.max_total_bytes",
	"lifecycle.cleanup_timeout_ms",
	"llm.request_timeout_ms",
	"llm.stream.max_event_bytes",
	"llm.stream.max_events",
	"llm.stream.max_response_bytes",
	"llm.stream.max_text_bytes",
	"llm.stream.max_thinking_bytes",
	"llm.stream.max_tool_arguments_bytes",
	"llm.thinking.budget_tokens",
	"mcp.default_timeout_ms",
	"mcp.max_pages",
	"mcp.max_protocol_errors",
	"mcp.max_response_bytes",
	"mcp.max_tools",
	"mcp.servers.<name>.timeout_ms",
	"memory.max_candidate_bytes",
	"memory.max_index_bytes",
	"memory.max_index_lines",
	"memory.update_concurrency",
	"memory.update_queue_size",
	"memory.update_timeout_ms",
	"session.gap_reminder_days",
	"session.max_record_bytes",
	"session.max_scan_bytes",
	"session.max_scan_files",
	"session.max_session_bytes",
	"session.retention_days",
	"tool.capture_bytes",
	"tool.inline_output_bytes",
	"tool.timeout_ms",
}

func TestAppConfigNumericManifestIsExact(t *testing.T) {
	manifest := appConfigNumericKeys()
	sort.Strings(manifest)
	if !reflect.DeepEqual(manifest, approvedAppConfigNumericKeys) {
		t.Fatal("AppConfig numeric manifest differs from the approved snapshot")
	}

	partialKeys := partialNumericKeys(t)
	if _, ok := partialKeys["tool.max_output_bytes"]; !ok {
		t.Fatal("PartialAppConfig lost the approved legacy tool output input")
	}
	delete(partialKeys, "tool.max_output_bytes")
	actual := make([]string, 0, len(partialKeys))
	for path := range partialKeys {
		actual = append(actual, path)
	}
	sort.Strings(actual)
	if !reflect.DeepEqual(actual, approvedAppConfigNumericKeys) {
		t.Fatal("PartialAppConfig numeric leaves differ from the approved snapshot")
	}

	cases := numericBoundaryCases(10, 1, 100)
	if len(cases) != 7 || cases[0].name != "absent_default" || cases[6].encoded != "9223372036854775808" {
		t.Fatal("numeric boundary matrix skeleton is incomplete")
	}
}

func TestNonAppConfigNumericInputsAreAbsentFromPartialConfig(t *testing.T) {
	typeOf := reflect.TypeOf(PartialAppConfig{})
	for _, forbidden := range []string{"hook", "hooks", "skill"} {
		if _, ok := yamlField(typeOf, forbidden); ok {
			t.Fatal("PartialAppConfig contains a numeric input owned by another loader")
		}
	}
	permissionField, ok := yamlField(typeOf, "permission")
	if !ok || permissionField.Type != reflect.TypeOf(PartialPermissionConfig{}) || permissionField.Type.NumField() != 1 ||
		permissionField.Type.Field(0).Tag.Get("yaml") != "mode" {
		t.Fatal("PartialAppConfig absorbed permission RuleFile inputs")
	}
	if partialContainsYAMLName(typeOf, "history") || partialContainsYAMLName(typeOf, "version") {
		t.Fatal("PartialAppConfig absorbed Skill history or owner-specific version inputs")
	}
	for _, path := range appConfigNumericKeys() {
		if strings.HasPrefix(path, "hook.") || strings.HasPrefix(path, "hooks.") ||
			strings.HasPrefix(path, "skill.") || path == "permission.version" || strings.HasSuffix(path, ".history") {
			t.Fatal("AppConfig numeric manifest contains an input owned by another loader")
		}
	}
}

func TestResolveToolArtifactAndFilesNumericMatrix(t *testing.T) {
	for _, spec := range toolArtifactFilesNumericSpecs {
		for _, testCase := range numericBoundaryCases(spec.defaultVal, spec.minimum, spec.hardCap) {
			if testCase.encoded != "" {
				continue
			}
			t.Run(spec.path+"/"+testCase.name, func(t *testing.T) {
				var partial PartialAppConfig
				prepareToolArtifactFilesBoundary(&partial, spec.path)
				setPartialNumericValue(t, &partial, spec.path, testCase.candidate)
				loaded, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{})
				if testCase.wantValid {
					if err != nil || resolvedNumericValue(t, loaded.Config, spec.path) != testCase.want {
						t.Fatal("valid numeric boundary did not resolve to the expected value")
					}
				} else if err == nil {
					t.Fatal("invalid numeric boundary was accepted")
				}
			})
		}
	}

	_, err := decodePartial("overflow.yaml", []byte("tool:\n  timeout_ms: 9223372036854775808\n"))
	if err == nil {
		t.Fatal("overflowing numeric configuration was accepted")
	}
}

func prepareToolArtifactFilesBoundary(partial *PartialAppConfig, path string) {
	switch path {
	case "tool.capture_bytes":
		partial.Tool.InlineOutputBytes = Optional[int64]{Set: true, Value: 1}
		partial.Artifact.MaxFileBytes = Optional[int64]{Set: true, Value: 512 * mebibyte}
		partial.Artifact.MaxTotalBytes = Optional[int64]{Set: true, Value: 8 * gibibyte}
	case "artifact.max_file_bytes":
		partial.Tool.InlineOutputBytes = Optional[int64]{Set: true, Value: 1}
		partial.Tool.CaptureBytes = Optional[int64]{Set: true, Value: 1}
	case "artifact.max_total_bytes":
		partial.Tool.InlineOutputBytes = Optional[int64]{Set: true, Value: 1}
		partial.Tool.CaptureBytes = Optional[int64]{Set: true, Value: 1}
		partial.Artifact.MaxFileBytes = Optional[int64]{Set: true, Value: 1}
	}
}

func TestLegacyToolOutputMapsOnlyToInlineAndConflictsWithNewKey(t *testing.T) {
	partial := PartialAppConfig{Tool: PartialToolConfig{MaxOutputBytes: Optional[int64]{Set: true, Value: 123}}}
	loaded, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{})
	if err != nil {
		t.Fatal("legacy tool output value did not resolve")
	}
	if loaded.Config.Tool.InlineOutputBytes != 123 || loaded.Config.Tool.MaxOutputBytes != 123 ||
		loaded.Config.Tool.CaptureBytes != 64*mebibyte {
		t.Fatal("legacy tool output value mapped beyond inline output")
	}

	partial.Tool.InlineOutputBytes = Optional[int64]{Set: true, Value: 456}
	if _, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{}); err == nil {
		t.Fatal("legacy and current inline output keys did not conflict")
	}
}

func TestResolveInstructionAndMCPNumericMatrix(t *testing.T) {
	for _, spec := range instructionMCPNumericSpecs {
		for _, testCase := range numericBoundaryCases(spec.defaultVal, spec.minimum, spec.hardCap) {
			if testCase.encoded != "" {
				continue
			}
			t.Run(spec.path+"/"+testCase.name, func(t *testing.T) {
				var partial PartialAppConfig
				prepareInstructionMCPBoundary(&partial, spec.path)
				setPartialNumericValue(t, &partial, spec.path, testCase.candidate)
				loaded, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{})
				if testCase.wantValid {
					if err != nil || resolvedNumericValue(t, loaded.Config, spec.path) != testCase.want {
						t.Fatal("valid Instruction or MCP boundary did not resolve")
					}
				} else if err == nil {
					t.Fatal("invalid Instruction or MCP boundary was accepted")
				}
			})
		}
	}

	for _, testCase := range numericBoundaryCases(30_000, mcpServerTimeoutSpec.minimum, mcpServerTimeoutSpec.hardCap) {
		if testCase.encoded != "" || testCase.name == "absent_default" {
			continue
		}
		t.Run(mcpServerTimeoutSpec.path+"/"+testCase.name, func(t *testing.T) {
			partial := PartialAppConfig{MCP: PartialMCPConfig{Servers: map[string]PartialMCPServerConfig{
				"local": {TimeoutMS: testCase.candidate},
			}}}
			loaded, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{})
			if testCase.wantValid {
				if err != nil || loaded.Config.MCP.Servers["local"].TimeoutMS != testCase.want {
					t.Fatal("valid MCP server timeout boundary did not resolve")
				}
			} else if err == nil {
				t.Fatal("invalid MCP server timeout boundary was accepted")
			}
		})
	}
	_, err := decodePartial("overflow.yaml", []byte("mcp:\n  max_tools: 9223372036854775808\n"))
	if err == nil {
		t.Fatal("overflowing MCP numeric configuration was accepted")
	}
}

func prepareInstructionMCPBoundary(partial *PartialAppConfig, path string) {
	if path == "instructions.max_total_bytes" || path == "instructions.max_expanded_bytes" {
		partial.Instructions.MaxFileBytes = Optional[int64]{Set: true, Value: 1}
	}
}

func TestMCPServerTimeoutInheritsResolvedDefault(t *testing.T) {
	user := PartialAppConfig{MCP: PartialMCPConfig{
		DefaultTimeoutMS: Optional[int64]{Set: true, Value: 1_111},
		Servers: map[string]PartialMCPServerConfig{
			"inherited": {},
			"explicit":  {TimeoutMS: Optional[int64]{Set: true, Value: 333}},
		},
	}}
	project := PartialAppConfig{MCP: PartialMCPConfig{DefaultTimeoutMS: Optional[int64]{Set: true, Value: 2_222}}}
	merged, err := MergeLayers(
		ConfigLayer{Source: SourceUser, Value: user},
		ConfigLayer{Source: SourceProject, Value: project},
	)
	if err != nil {
		t.Fatal("merge MCP timeout layers failed")
	}
	loaded, err := ResolveConfig(merged, LoadOptions{})
	if err != nil {
		t.Fatal("resolve inherited MCP timeout failed")
	}
	if loaded.Config.MCP.DefaultTimeoutMS != 2_222 || loaded.Config.MCP.Servers["inherited"].TimeoutMS != 2_222 ||
		loaded.Config.MCP.Servers["explicit"].TimeoutMS != 333 {
		t.Fatal("MCP server timeout did not inherit the resolved default")
	}

	user.MCP.Servers["inherited"] = PartialMCPServerConfig{TimeoutMS: Optional[int64]{Set: true, Value: 0}}
	merged, err = MergeLayers(ConfigLayer{Source: SourceUser, Value: user}, ConfigLayer{Source: SourceProject, Value: project})
	if err != nil {
		t.Fatal("merge explicit zero MCP timeout failed")
	}
	if _, err := ResolveConfig(merged, LoadOptions{}); err == nil {
		t.Fatal("explicit zero MCP timeout inherited the default")
	}
}

func TestResolveLLMAgentAndStreamNumericMatrix(t *testing.T) {
	for _, spec := range llmAgentStreamNumericSpecs {
		for _, testCase := range numericBoundaryCases(spec.defaultVal, spec.minimum, spec.hardCap) {
			if testCase.encoded != "" {
				continue
			}
			t.Run(spec.path+"/"+testCase.name, func(t *testing.T) {
				var partial PartialAppConfig
				prepareLLMAgentStreamBoundary(&partial, spec.path)
				setPartialNumericValue(t, &partial, spec.path, testCase.candidate)
				loaded, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{})
				if testCase.wantValid {
					if err != nil || resolvedNumericValue(t, loaded.Config, spec.path) != testCase.want {
						t.Fatal("valid LLM, Agent, or stream boundary did not resolve")
					}
				} else if err == nil {
					t.Fatal("invalid LLM, Agent, or stream boundary was accepted")
				}
			})
		}
	}

	partial := PartialAppConfig{LLM: PartialLLMConfig{Thinking: PartialThinkingConfig{
		Enabled:      Optional[bool]{Set: true, Value: false},
		BudgetTokens: Optional[int64]{Set: true, Value: 0},
	}}}
	if _, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{}); err == nil {
		t.Fatal("disabled thinking bypassed budget validation")
	}

	_, err := decodePartial("overflow.yaml", []byte("llm:\n  stream:\n    max_events: 9223372036854775808\n"))
	if err == nil {
		t.Fatal("overflowing LLM stream numeric configuration was accepted")
	}
}

func prepareLLMAgentStreamBoundary(partial *PartialAppConfig, path string) {
	if path == "llm.stream.max_response_bytes" {
		partial.LLM.Stream.MaxEventBytes = Optional[int64]{Set: true, Value: 1}
		partial.LLM.Stream.MaxTextBytes = Optional[int64]{Set: true, Value: 1}
		partial.LLM.Stream.MaxThinkingBytes = Optional[int64]{Set: true, Value: 1}
		partial.LLM.Stream.MaxToolArgumentsBytes = Optional[int64]{Set: true, Value: 1}
		return
	}
	if strings.HasPrefix(path, "llm.stream.max_") && path != "llm.stream.max_events" {
		partial.LLM.Stream.MaxResponseBytes = Optional[int64]{Set: true, Value: 64 * mebibyte}
	}
}

func TestStreamSubLimitsCannotExceedResponseLimit(t *testing.T) {
	valid := PartialStreamConfig{
		MaxResponseBytes:      Optional[int64]{Set: true, Value: 1},
		MaxEventBytes:         Optional[int64]{Set: true, Value: 1},
		MaxTextBytes:          Optional[int64]{Set: true, Value: 1},
		MaxThinkingBytes:      Optional[int64]{Set: true, Value: 1},
		MaxToolArgumentsBytes: Optional[int64]{Set: true, Value: 1},
	}
	if _, err := ResolveConfig(MergeResult{Value: PartialAppConfig{LLM: PartialLLMConfig{Stream: valid}}}, LoadOptions{}); err != nil {
		t.Fatal("stream sub-limits equal to the response limit were rejected")
	}

	for _, path := range []string{
		"llm.stream.max_event_bytes",
		"llm.stream.max_text_bytes",
		"llm.stream.max_thinking_bytes",
		"llm.stream.max_tool_arguments_bytes",
	} {
		t.Run(path, func(t *testing.T) {
			partial := PartialAppConfig{LLM: PartialLLMConfig{Stream: valid}}
			setPartialNumericValue(t, &partial, path, Optional[int64]{Set: true, Value: 2})
			if _, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{}); err == nil {
				t.Fatal("stream sub-limit above the response limit was accepted")
			}
		})
	}
}

func TestResolveContextAndSessionNumericMatrix(t *testing.T) {
	for _, spec := range contextSessionNumericSpecs {
		for _, testCase := range numericBoundaryCases(spec.defaultVal, spec.minimum, spec.hardCap) {
			if testCase.encoded != "" {
				continue
			}
			t.Run(spec.path+"/"+testCase.name, func(t *testing.T) {
				var partial PartialAppConfig
				prepareContextSessionBoundary(&partial, spec.path)
				setPartialNumericValue(t, &partial, spec.path, testCase.candidate)
				loaded, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{})
				if testCase.wantValid {
					if err != nil || resolvedNumericValue(t, loaded.Config, spec.path) != testCase.want {
						t.Fatal("valid Context or Session boundary did not resolve")
					}
				} else if err == nil {
					t.Fatal("invalid Context or Session boundary was accepted")
				}
			})
		}
	}

	_, err := decodePartial("overflow.yaml", []byte("context:\n  preview_chars: 9223372036854775808\n"))
	if err == nil {
		t.Fatal("overflowing Context numeric configuration was accepted")
	}
}

func prepareContextSessionBoundary(partial *PartialAppConfig, path string) {
	switch path {
	case "context.tool_result_threshold_chars":
		partial.Context.ToolResultsThresholdChars = Optional[int64]{Set: true, Value: 4 * mebibyte}
	case "context.tool_results_threshold_chars":
		partial.Context.ToolResultThresholdChars = Optional[int64]{Set: true, Value: 1}
	case "context.model_window_tokens":
		partial.Context.AutoMarginTokens = Optional[int64]{Set: true, Value: 1}
		partial.Context.ManualMarginTokens = Optional[int64]{Set: true, Value: 1}
		partial.Context.RecentKeepTokens = Optional[int64]{Set: true, Value: 1}
	case "context.auto_margin_tokens", "context.manual_margin_tokens", "context.recent_keep_tokens":
		partial.Context.ModelWindowTokens = Optional[int64]{Set: true, Value: 10_000_000}
	case "session.max_session_bytes":
		partial.Session.MaxRecordBytes = Optional[int64]{Set: true, Value: 1}
	}
}

func TestContextWindowMinimumAndThreeStrictRelations(t *testing.T) {
	valid := PartialContextConfig{
		ModelWindowTokens:  Optional[int64]{Set: true, Value: 2},
		AutoMarginTokens:   Optional[int64]{Set: true, Value: 1},
		ManualMarginTokens: Optional[int64]{Set: true, Value: 1},
		RecentKeepTokens:   Optional[int64]{Set: true, Value: 1},
	}
	if _, err := ResolveConfig(MergeResult{Value: PartialAppConfig{Context: valid}}, LoadOptions{}); err != nil {
		t.Fatal("minimum Context window with minimum token reserves was rejected")
	}

	tooSmall := valid
	tooSmall.ModelWindowTokens = Optional[int64]{Set: true, Value: 1}
	if _, err := ResolveConfig(MergeResult{Value: PartialAppConfig{Context: tooSmall}}, LoadOptions{}); err == nil {
		t.Fatal("Context model window below its independent minimum was accepted")
	}

	for _, path := range []string{
		"context.auto_margin_tokens",
		"context.manual_margin_tokens",
		"context.recent_keep_tokens",
	} {
		t.Run(path, func(t *testing.T) {
			partial := PartialAppConfig{Context: valid}
			setPartialNumericValue(t, &partial, path, Optional[int64]{Set: true, Value: 2})
			if _, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{}); err == nil {
				t.Fatal("Context token reserve equal to the model window was accepted")
			}
		})
	}
}

func TestSessionRecordCannotExceedSession(t *testing.T) {
	valid := PartialSessionConfig{
		MaxRecordBytes:  Optional[int64]{Set: true, Value: 1},
		MaxSessionBytes: Optional[int64]{Set: true, Value: 1},
	}
	if _, err := ResolveConfig(MergeResult{Value: PartialAppConfig{Session: valid}}, LoadOptions{}); err != nil {
		t.Fatal("Session record equal to the session limit was rejected")
	}

	invalid := valid
	invalid.MaxRecordBytes = Optional[int64]{Set: true, Value: 2}
	if _, err := ResolveConfig(MergeResult{Value: PartialAppConfig{Session: invalid}}, LoadOptions{}); err == nil {
		t.Fatal("Session record above the session limit was accepted")
	}
}

func TestResolveMemoryDiagnosticsAndLifecycleNumericMatrix(t *testing.T) {
	for _, spec := range memoryDiagnosticsLifecycleNumericSpecs {
		for _, testCase := range numericBoundaryCases(spec.defaultVal, spec.minimum, spec.hardCap) {
			if testCase.encoded != "" {
				continue
			}
			t.Run(spec.path+"/"+testCase.name, func(t *testing.T) {
				var partial PartialAppConfig
				prepareMemoryDiagnosticsLifecycleBoundary(&partial, spec.path)
				setPartialNumericValue(t, &partial, spec.path, testCase.candidate)
				loaded, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{})
				if testCase.wantValid {
					if err != nil || resolvedNumericValue(t, loaded.Config, spec.path) != testCase.want {
						t.Fatal("valid Memory, Diagnostics, or Lifecycle boundary did not resolve")
					}
				} else if err == nil {
					t.Fatal("invalid Memory, Diagnostics, or Lifecycle boundary was accepted")
				}
			})
		}
	}

	_, err := decodePartial("overflow.yaml", []byte("memory:\n  max_index_lines: 9223372036854775808\n"))
	if err == nil {
		t.Fatal("overflowing Memory numeric configuration was accepted")
	}
}

func prepareMemoryDiagnosticsLifecycleBoundary(partial *PartialAppConfig, path string) {
	switch path {
	case "memory.update_queue_size":
		partial.Memory.UpdateConcurrency = Optional[int64]{Set: true, Value: 1}
	case "memory.update_concurrency":
		partial.Memory.UpdateQueueSize = Optional[int64]{Set: true, Value: 1_024}
	case "diagnostics.max_total_bytes":
		partial.Diagnostics.MaxItemBytes = Optional[int64]{Set: true, Value: 1}
	}
}

func TestMemoryQueueAndDiagnosticTotalRelations(t *testing.T) {
	memory := PartialMemoryConfig{
		UpdateQueueSize:   Optional[int64]{Set: true, Value: 1},
		UpdateConcurrency: Optional[int64]{Set: true, Value: 1},
	}
	if _, err := ResolveConfig(MergeResult{Value: PartialAppConfig{Memory: memory}}, LoadOptions{}); err != nil {
		t.Fatal("Memory concurrency equal to queue size was rejected")
	}
	memory.UpdateConcurrency = Optional[int64]{Set: true, Value: 2}
	if _, err := ResolveConfig(MergeResult{Value: PartialAppConfig{Memory: memory}}, LoadOptions{}); err == nil {
		t.Fatal("Memory concurrency above queue size was accepted")
	}

	diagnostics := PartialDiagnosticsConfig{
		MaxItemBytes:  Optional[int64]{Set: true, Value: 1},
		MaxTotalBytes: Optional[int64]{Set: true, Value: 1},
	}
	if _, err := ResolveConfig(MergeResult{Value: PartialAppConfig{Diagnostics: diagnostics}}, LoadOptions{}); err != nil {
		t.Fatal("Diagnostics item equal to total limit was rejected")
	}
	diagnostics.MaxItemBytes = Optional[int64]{Set: true, Value: 2}
	if _, err := ResolveConfig(MergeResult{Value: PartialAppConfig{Diagnostics: diagnostics}}, LoadOptions{}); err == nil {
		t.Fatal("Diagnostics item above total limit was accepted")
	}
}

func TestCleanupTimeoutCannotBeDisabledOrRaised(t *testing.T) {
	for _, value := range []int64{1, 2_000} {
		partial := PartialAppConfig{Lifecycle: PartialLifecycleConfig{
			CleanupTimeoutMS: Optional[int64]{Set: true, Value: value},
		}}
		if _, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{}); err != nil {
			t.Fatal("valid cleanup timeout was rejected")
		}
	}
	for _, value := range []int64{0, -1, 2_001} {
		partial := PartialAppConfig{Lifecycle: PartialLifecycleConfig{
			CleanupTimeoutMS: Optional[int64]{Set: true, Value: value},
		}}
		if _, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{}); err == nil {
			t.Fatal("cleanup timeout outside the closed range was accepted")
		}
	}
}

func TestResolveRejectsEveryApprovedNumericCombination(t *testing.T) {
	optional := func(value int64) Optional[int64] { return Optional[int64]{Set: true, Value: value} }
	streamBase := PartialStreamConfig{
		MaxResponseBytes:      optional(1),
		MaxEventBytes:         optional(1),
		MaxTextBytes:          optional(1),
		MaxThinkingBytes:      optional(1),
		MaxToolArgumentsBytes: optional(1),
	}
	contextBase := PartialContextConfig{
		ModelWindowTokens:  optional(2),
		AutoMarginTokens:   optional(1),
		ManualMarginTokens: optional(1),
		RecentKeepTokens:   optional(1),
	}

	cases := []struct {
		name    string
		partial PartialAppConfig
	}{
		{name: "inline_above_capture", partial: PartialAppConfig{Tool: PartialToolConfig{InlineOutputBytes: optional(2), CaptureBytes: optional(1)}}},
		{name: "capture_above_artifact_file", partial: PartialAppConfig{Tool: PartialToolConfig{InlineOutputBytes: optional(1), CaptureBytes: optional(2)}, Artifact: PartialArtifactConfig{MaxFileBytes: optional(1)}}},
		{name: "artifact_file_above_total", partial: PartialAppConfig{Tool: PartialToolConfig{InlineOutputBytes: optional(1), CaptureBytes: optional(1)}, Artifact: PartialArtifactConfig{MaxFileBytes: optional(2), MaxTotalBytes: optional(1)}}},
		{name: "instruction_file_above_total", partial: PartialAppConfig{Instructions: PartialInstructionsConfig{MaxFileBytes: optional(2), MaxTotalBytes: optional(1)}}},
		{name: "instruction_file_above_expanded", partial: PartialAppConfig{Instructions: PartialInstructionsConfig{MaxFileBytes: optional(2), MaxExpandedBytes: optional(1)}}},
		{name: "stream_event_above_response", partial: PartialAppConfig{LLM: PartialLLMConfig{Stream: func() PartialStreamConfig { value := streamBase; value.MaxEventBytes = optional(2); return value }()}}},
		{name: "stream_text_above_response", partial: PartialAppConfig{LLM: PartialLLMConfig{Stream: func() PartialStreamConfig { value := streamBase; value.MaxTextBytes = optional(2); return value }()}}},
		{name: "stream_thinking_above_response", partial: PartialAppConfig{LLM: PartialLLMConfig{Stream: func() PartialStreamConfig { value := streamBase; value.MaxThinkingBytes = optional(2); return value }()}}},
		{name: "stream_arguments_above_response", partial: PartialAppConfig{LLM: PartialLLMConfig{Stream: func() PartialStreamConfig {
			value := streamBase
			value.MaxToolArgumentsBytes = optional(2)
			return value
		}()}}},
		{name: "session_record_above_session", partial: PartialAppConfig{Session: PartialSessionConfig{MaxRecordBytes: optional(2), MaxSessionBytes: optional(1)}}},
		{name: "diagnostic_item_above_total", partial: PartialAppConfig{Diagnostics: PartialDiagnosticsConfig{MaxItemBytes: optional(2), MaxTotalBytes: optional(1)}}},
		{name: "single_tool_result_above_multiple", partial: PartialAppConfig{Context: PartialContextConfig{ToolResultThresholdChars: optional(2), ToolResultsThresholdChars: optional(1)}}},
		{name: "auto_margin_equal_to_window", partial: PartialAppConfig{Context: func() PartialContextConfig { value := contextBase; value.AutoMarginTokens = optional(2); return value }()}},
		{name: "manual_margin_equal_to_window", partial: PartialAppConfig{Context: func() PartialContextConfig {
			value := contextBase
			value.ManualMarginTokens = optional(2)
			return value
		}()}},
		{name: "recent_keep_equal_to_window", partial: PartialAppConfig{Context: func() PartialContextConfig { value := contextBase; value.RecentKeepTokens = optional(2); return value }()}},
		{name: "memory_concurrency_above_queue", partial: PartialAppConfig{Memory: PartialMemoryConfig{UpdateQueueSize: optional(1), UpdateConcurrency: optional(2)}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ResolveConfig(MergeResult{Value: testCase.partial}, LoadOptions{}); err == nil {
				t.Fatal("invalid approved numeric combination was accepted")
			}
		})
	}
}

func TestEveryAppConfigNumericKeyHasCompleteBoundaryMatrix(t *testing.T) {
	specs := allAppConfigNumericSpecs()
	seen := make(map[string]int, len(specs))
	for _, spec := range specs {
		seen[spec.path]++
		for _, testCase := range numericBoundaryCases(spec.defaultVal, spec.minimum, spec.hardCap) {
			t.Run(spec.path+"/"+testCase.name, func(t *testing.T) {
				if testCase.encoded != "" {
					if _, err := decodePartial("overflow.yaml", numericValueYAML(spec.path, testCase.encoded)); err == nil {
						t.Fatal("overflowing numeric configuration was accepted")
					}
					return
				}
				var partial PartialAppConfig
				prepareCompleteNumericBoundary(&partial, spec.path)
				setCompleteNumericValue(t, &partial, spec.path, testCase.candidate)
				loaded, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{})
				if testCase.wantValid {
					if err != nil || resolvedNumericValue(t, loaded.Config, spec.path) != testCase.want {
						t.Fatal("valid complete-matrix boundary did not resolve")
					}
				} else if err == nil {
					t.Fatal("invalid complete-matrix boundary was accepted")
				}
			})
		}
	}
	if len(seen) != len(approvedAppConfigNumericKeys) {
		t.Fatal("complete boundary matrix does not cover the numeric manifest exactly")
	}
	for _, path := range approvedAppConfigNumericKeys {
		if seen[path] != 1 {
			t.Fatal("numeric key does not have exactly one complete boundary matrix")
		}
	}
}

func TestResolveErrorsContainOnlyPathAndAllowedRange(t *testing.T) {
	const rejected = int64(-7_777_777)
	for _, spec := range allAppConfigNumericSpecs() {
		t.Run(spec.path, func(t *testing.T) {
			var partial PartialAppConfig
			prepareCompleteNumericBoundary(&partial, spec.path)
			setCompleteNumericValue(t, &partial, spec.path, Optional[int64]{Set: true, Value: rejected})
			_, err := ResolveConfig(MergeResult{Value: partial}, LoadOptions{})
			if err == nil {
				t.Fatal("invalid numeric value did not produce an error")
			}
			expected := fmt.Sprintf("config field %q must be in range %d..%d", spec.path, spec.minimum, spec.hardCap)
			if err.Error() != expected || strings.Contains(err.Error(), "7777777") {
				t.Fatal("numeric error contains content beyond the path and allowed range")
			}
		})
	}
}

func TestResolveRegistersOnlyEffectiveExpandedSecrets(t *testing.T) {
	for name, value := range map[string]string{
		"WINNER_API_KEY": "winner-api-canary",
		"MCP_HOST":       "mcp.example.invalid",
		"MCP_TOKEN":      "winner-header-canary",
		"MCP_COMMAND":    "winner-command",
		"MCP_ARG":        "winner-argument",
		"SERVICE_SECRET": "winner-env-canary",
		"LOSER_API_KEY":  "loser-api-canary",
	} {
		t.Setenv(name, value)
	}
	optionalString := func(value string) Optional[string] { return Optional[string]{Set: true, Value: value} }
	optionalBool := func(value bool) Optional[bool] { return Optional[bool]{Set: true, Value: value} }
	optionalStrings := func(value []string) Optional[[]string] { return Optional[[]string]{Set: true, Value: value} }
	optionalMap := func(value map[string]string) Optional[map[string]string] {
		return Optional[map[string]string]{Set: true, Value: value}
	}

	user := PartialAppConfig{
		LLM: PartialLLMConfig{APIKey: optionalString("${LOSER_API_KEY}")},
		MCP: PartialMCPConfig{Servers: map[string]PartialMCPServerConfig{
			"remote": {
				Type:    optionalString(MCPTransportHTTP),
				URL:     optionalString("https://loser.invalid/mcp"),
				Headers: optionalMap(map[string]string{"Authorization": "Bearer ${LOSER_MISSING}"}),
			},
		}},
	}
	project := PartialAppConfig{
		LLM: PartialLLMConfig{APIKey: optionalString("${WINNER_API_KEY}")},
		MCP: PartialMCPConfig{Servers: map[string]PartialMCPServerConfig{
			"remote": {
				Type:    optionalString(MCPTransportHTTP),
				URL:     optionalString("https://${MCP_HOST}/mcp"),
				Headers: optionalMap(map[string]string{"Authorization": "Bearer ${MCP_TOKEN}"}),
			},
			"local": {
				Type:    optionalString(MCPTransportStdio),
				Command: optionalString("${MCP_COMMAND}"),
				Args:    optionalStrings([]string{"--value=${MCP_ARG}"}),
				Env:     optionalMap(map[string]string{"SERVICE_SECRET": "${SERVICE_SECRET}"}),
			},
			"disabled": {
				Disabled: optionalBool(true),
				Type:     optionalString(MCPTransportHTTP),
				URL:      optionalString("https://${DISABLED_MISSING}/mcp"),
			},
		}},
	}
	merged, err := MergeLayers(
		ConfigLayer{Source: SourceUser, Value: user},
		ConfigLayer{Source: SourceProject, Value: project},
	)
	if err != nil {
		t.Fatal("merge effective secret fixtures failed")
	}
	runtimeRedactor := redact.NewRuntimeRedactor()
	loaded, err := ResolveConfig(merged, LoadOptions{Redactor: runtimeRedactor})
	if err != nil {
		t.Fatal("resolve effective expanded secrets failed")
	}
	if loaded.Config.LLM.APIKey != "winner-api-canary" ||
		loaded.Config.MCP.Servers["remote"].URL != "https://mcp.example.invalid/mcp" ||
		loaded.Config.MCP.Servers["remote"].Headers["Authorization"] != "Bearer winner-header-canary" ||
		loaded.Config.MCP.Servers["local"].Command != "winner-command" ||
		loaded.Config.MCP.Servers["local"].Args[0] != "--value=winner-argument" ||
		loaded.Config.MCP.Servers["local"].Env["SERVICE_SECRET"] != "winner-env-canary" {
		t.Fatal("effective environment references were not fully expanded")
	}
	for _, secret := range []string{"winner-api-canary", "winner-header-canary", "winner-env-canary"} {
		if strings.Contains(runtimeRedactor.Text(secret), secret) {
			t.Fatal("effective secret was not registered")
		}
	}
	if runtimeRedactor.Text("loser-api-canary") != "loser-api-canary" {
		t.Fatal("overridden secret was expanded or registered")
	}

	failing := PartialAppConfig{
		LLM: PartialLLMConfig{APIKey: optionalString("literal-before-failure-canary")},
		MCP: PartialMCPConfig{Servers: map[string]PartialMCPServerConfig{
			"remote": {
				Type:    optionalString(MCPTransportHTTP),
				Headers: optionalMap(map[string]string{"Authorization": "Bearer ${EFFECTIVE_MISSING}"}),
			},
		}},
	}
	failingRedactor := redact.NewRuntimeRedactor()
	_, err = ResolveConfig(MergeResult{Value: failing}, LoadOptions{Redactor: failingRedactor})
	if err == nil || !strings.Contains(err.Error(), "mcp.servers.remote.headers.Authorization") ||
		!strings.Contains(err.Error(), "EFFECTIVE_MISSING") || strings.Contains(err.Error(), "literal-before-failure-canary") {
		t.Fatal("effective missing environment reference did not return a safe path error")
	}
	if failingRedactor.Text("literal-before-failure-canary") != "literal-before-failure-canary" {
		t.Fatal("failed resolution partially registered a secret")
	}
}

func TestLegacyConfigCompatibilityAndConflict(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	base := `
llm:
  protocol: anthropic
  model: claude-test
  base_url: http://127.0.0.1:1
  api_key: test-key
ui:
  show_response_timer: false
`

	legacyPath := writeConfig(t, base+`
tool:
  max_output_bytes: 12345
`)
	legacy, err := Load(legacyPath)
	if err != nil {
		t.Fatal("legacy tool output configuration no longer loads")
	}
	if legacy.Tool.InlineOutputBytes != 12_345 || legacy.Tool.MaxOutputBytes != 12_345 ||
		legacy.Tool.CaptureBytes != 64*mebibyte || legacy.UI.ShowResponseTimer {
		t.Fatal("legacy tool output did not map only to inline output or explicit false was lost")
	}

	currentPath := writeConfig(t, base+`
tool:
  inline_output_bytes: 54321
`)
	current, err := Load(currentPath)
	if err != nil {
		t.Fatal("current inline output configuration did not load")
	}
	if current.Tool.InlineOutputBytes != 54_321 || current.Tool.MaxOutputBytes != 54_321 {
		t.Fatal("current inline output did not materialize through the compatibility field")
	}

	conflictPath := writeConfig(t, base+`
tool:
  max_output_bytes: 12345
  inline_output_bytes: 54321
`)
	before, err := os.ReadFile(conflictPath)
	if err != nil {
		t.Fatal("read conflict fixture failed")
	}
	_, err = Load(conflictPath)
	if err == nil || !strings.Contains(err.Error(), "tool.inline_output_bytes") ||
		!strings.Contains(err.Error(), "tool.max_output_bytes") {
		t.Fatal("legacy and current tool output keys did not report an explicit conflict")
	}
	after, err := os.ReadFile(conflictPath)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("compatibility conflict rewrote the source configuration")
	}

	zeroPath := writeConfig(t, base+`
tool:
  max_output_bytes: 0
`)
	if _, err := Load(zeroPath); err == nil || !strings.Contains(err.Error(), "tool.inline_output_bytes") {
		t.Fatal("legacy explicit zero bypassed inline output validation")
	}
}

func allAppConfigNumericSpecs() []numericSpec {
	result := make([]numericSpec, 0, len(appConfigNumericManifest))
	result = append(result, toolArtifactFilesNumericSpecs[:]...)
	result = append(result, instructionMCPNumericSpecs[:]...)
	serverTimeout := mcpServerTimeoutSpec
	serverTimeout.defaultVal = 30_000
	result = append(result, serverTimeout)
	result = append(result, llmAgentStreamNumericSpecs[:]...)
	result = append(result, contextSessionNumericSpecs[:]...)
	result = append(result, memoryDiagnosticsLifecycleNumericSpecs[:]...)
	return result
}

func prepareCompleteNumericBoundary(partial *PartialAppConfig, path string) {
	prepareLLMAgentStreamBoundary(partial, path)
	prepareContextSessionBoundary(partial, path)
	prepareMemoryDiagnosticsLifecycleBoundary(partial, path)
	switch path {
	case "tool.capture_bytes":
		partial.Tool.InlineOutputBytes = Optional[int64]{Set: true, Value: 1}
		partial.Artifact.MaxFileBytes = Optional[int64]{Set: true, Value: 512 * mebibyte}
		partial.Artifact.MaxTotalBytes = Optional[int64]{Set: true, Value: 8 * gibibyte}
	case "artifact.max_file_bytes":
		partial.Tool.InlineOutputBytes = Optional[int64]{Set: true, Value: 1}
		partial.Tool.CaptureBytes = Optional[int64]{Set: true, Value: 1}
	case "artifact.max_total_bytes":
		partial.Tool.InlineOutputBytes = Optional[int64]{Set: true, Value: 1}
		partial.Tool.CaptureBytes = Optional[int64]{Set: true, Value: 1}
		partial.Artifact.MaxFileBytes = Optional[int64]{Set: true, Value: 1}
	case "instructions.max_total_bytes", "instructions.max_expanded_bytes":
		partial.Instructions.MaxFileBytes = Optional[int64]{Set: true, Value: 1}
	case "context.tool_result_threshold_chars":
		partial.Context.ToolResultsThresholdChars = Optional[int64]{Set: true, Value: 4 * mebibyte}
	case "context.tool_results_threshold_chars":
		partial.Context.ToolResultThresholdChars = Optional[int64]{Set: true, Value: 1}
	case "mcp.servers.<name>.timeout_ms":
		partial.MCP.Servers = map[string]PartialMCPServerConfig{"local": {}}
	}
}

func setCompleteNumericValue(t *testing.T, partial *PartialAppConfig, path string, value Optional[int64]) {
	t.Helper()
	if path != "mcp.servers.<name>.timeout_ms" {
		setPartialNumericValue(t, partial, path, value)
		return
	}
	server := partial.MCP.Servers["local"]
	server.TimeoutMS = value
	partial.MCP.Servers["local"] = server
}

func numericValueYAML(path string, encoded string) []byte {
	parts := strings.Split(strings.Replace(path, "<name>", "local", 1), ".")
	var result strings.Builder
	for index, part := range parts {
		result.WriteString(strings.Repeat("  ", index))
		result.WriteString(part)
		result.WriteByte(':')
		if index == len(parts)-1 {
			result.WriteByte(' ')
			result.WriteString(encoded)
		}
		result.WriteByte('\n')
	}
	return []byte(result.String())
}

type numericBoundaryCase struct {
	name      string
	candidate Optional[int64]
	encoded   string
	want      int64
	wantValid bool
}

func numericBoundaryCases(defaultValue int64, minimum int64, hardCap int64) []numericBoundaryCase {
	return []numericBoundaryCase{
		{name: "absent_default", want: defaultValue, wantValid: true},
		{name: "minimum", candidate: Optional[int64]{Set: true, Value: minimum}, want: minimum, wantValid: true},
		{name: "hard_cap", candidate: Optional[int64]{Set: true, Value: hardCap}, want: hardCap, wantValid: true},
		{name: "hard_cap_plus_one", candidate: Optional[int64]{Set: true, Value: hardCap + 1}},
		{name: "explicit_zero", candidate: Optional[int64]{Set: true, Value: 0}},
		{name: "negative", candidate: Optional[int64]{Set: true, Value: -1}},
		{name: "overflow", encoded: "9223372036854775808"},
	}
}

func setPartialNumericValue(t *testing.T, partial *PartialAppConfig, path string, value Optional[int64]) {
	t.Helper()
	current := reflect.ValueOf(partial).Elem()
	parts := strings.Split(path, ".")
	for index, part := range parts {
		fieldIndex := -1
		for candidate := 0; candidate < current.NumField(); candidate++ {
			if strings.Split(current.Type().Field(candidate).Tag.Get("yaml"), ",")[0] == part {
				fieldIndex = candidate
				break
			}
		}
		if fieldIndex < 0 {
			t.Fatal("numeric test path is absent from PartialAppConfig")
		}
		current = current.Field(fieldIndex)
		if index == len(parts)-1 {
			current.Set(reflect.ValueOf(value))
			return
		}
	}
}

func resolvedNumericValue(t *testing.T, config AppConfig, path string) int64 {
	t.Helper()
	switch path {
	case "tool.inline_output_bytes":
		return config.Tool.InlineOutputBytes
	case "tool.capture_bytes":
		return config.Tool.CaptureBytes
	case "tool.timeout_ms":
		return int64(config.Tool.TimeoutMS)
	case "artifact.max_file_bytes":
		return config.Artifact.MaxFileBytes
	case "artifact.max_total_bytes":
		return config.Artifact.MaxTotalBytes
	case "artifact.retention_days":
		return config.Artifact.RetentionDays
	case "files.read_max_bytes":
		return config.Files.ReadMaxBytes
	case "files.scan_max_bytes":
		return config.Files.ScanMaxBytes
	case "files.scan_max_files":
		return config.Files.ScanMaxFiles
	case "files.scan_max_directories":
		return config.Files.ScanMaxDirectories
	case "files.scan_max_lines":
		return config.Files.ScanMaxLines
	case "instructions.max_file_bytes":
		return config.Instructions.MaxFileBytes
	case "instructions.max_total_bytes":
		return config.Instructions.MaxTotalBytes
	case "instructions.max_files":
		return config.Instructions.MaxFiles
	case "instructions.max_expanded_bytes":
		return config.Instructions.MaxExpandedBytes
	case "instructions.max_include_depth":
		return int64(config.Instructions.MaxIncludeDepth)
	case "mcp.max_response_bytes":
		return config.MCP.MaxResponseBytes
	case "mcp.max_tools":
		return config.MCP.MaxTools
	case "mcp.max_pages":
		return config.MCP.MaxPages
	case "mcp.max_protocol_errors":
		return config.MCP.MaxProtocolErrors
	case "mcp.default_timeout_ms":
		return config.MCP.DefaultTimeoutMS
	case "mcp.servers.<name>.timeout_ms":
		return config.MCP.Servers["local"].TimeoutMS
	case "llm.request_timeout_ms":
		return int64(config.LLM.RequestTimeoutMS)
	case "llm.thinking.budget_tokens":
		return int64(config.LLM.Thinking.BudgetTokens)
	case "llm.stream.max_response_bytes":
		return config.LLM.Stream.MaxResponseBytes
	case "llm.stream.max_event_bytes":
		return config.LLM.Stream.MaxEventBytes
	case "llm.stream.max_events":
		return config.LLM.Stream.MaxEvents
	case "llm.stream.max_text_bytes":
		return config.LLM.Stream.MaxTextBytes
	case "llm.stream.max_thinking_bytes":
		return config.LLM.Stream.MaxThinkingBytes
	case "llm.stream.max_tool_arguments_bytes":
		return config.LLM.Stream.MaxToolArgumentsBytes
	case "agent.max_iterations":
		return int64(config.Agent.MaxIterations)
	case "agent.max_unknown_tool_calls":
		return int64(config.Agent.MaxUnknownToolCalls)
	case "context.tool_result_threshold_chars":
		return int64(config.Context.ToolResultThresholdChars)
	case "context.tool_results_threshold_chars":
		return int64(config.Context.ToolResultsThresholdChars)
	case "context.model_window_tokens":
		return config.Context.ModelWindowTokens
	case "context.auto_margin_tokens":
		return config.Context.AutoMarginTokens
	case "context.manual_margin_tokens":
		return config.Context.ManualMarginTokens
	case "context.recent_keep_tokens":
		return config.Context.RecentKeepTokens
	case "context.recent_keep_messages":
		return int64(config.Context.RecentKeepMessages)
	case "context.summary_failure_limit":
		return int64(config.Context.SummaryFailureLimit)
	case "context.preview_chars":
		return int64(config.Context.PreviewChars)
	case "session.max_record_bytes":
		return config.Session.MaxRecordBytes
	case "session.max_session_bytes":
		return config.Session.MaxSessionBytes
	case "session.max_scan_files":
		return int64(config.Session.MaxScanFiles)
	case "session.max_scan_bytes":
		return config.Session.MaxScanBytes
	case "session.retention_days":
		return int64(config.Session.RetentionDays)
	case "session.gap_reminder_days":
		return int64(config.Session.GapReminderDays)
	case "memory.max_index_lines":
		return int64(config.Memory.MaxIndexLines)
	case "memory.max_index_bytes":
		return int64(config.Memory.MaxIndexBytes)
	case "memory.update_queue_size":
		return int64(config.Memory.UpdateQueueSize)
	case "memory.update_concurrency":
		return int64(config.Memory.UpdateConcurrency)
	case "memory.update_timeout_ms":
		return int64(config.Memory.UpdateTimeoutMS)
	case "memory.max_candidate_bytes":
		return int64(config.Memory.MaxCandidateBytes)
	case "diagnostics.max_items":
		return config.Diagnostics.MaxItems
	case "diagnostics.max_item_bytes":
		return config.Diagnostics.MaxItemBytes
	case "diagnostics.max_total_bytes":
		return config.Diagnostics.MaxTotalBytes
	case "lifecycle.cleanup_timeout_ms":
		return config.Lifecycle.CleanupTimeoutMS
	default:
		t.Fatal("resolved numeric test path is unknown")
		return 0
	}
}

func partialNumericKeys(t *testing.T) map[string]struct{} {
	t.Helper()
	result := make(map[string]struct{})
	collectPartialNumericKeys(t, reflect.TypeOf(PartialAppConfig{}), "", result)
	return result
}

func collectPartialNumericKeys(t *testing.T, typeOf reflect.Type, path string, result map[string]struct{}) {
	t.Helper()
	if optionalValueType, ok := optionalTypeValue(typeOf); ok {
		if optionalValueType.Kind() >= reflect.Int && optionalValueType.Kind() <= reflect.Int64 {
			result[path] = struct{}{}
		}
		return
	}
	if typeOf.Kind() == reflect.Map {
		collectPartialNumericKeys(t, typeOf.Elem(), joinConfigPath(path, "<name>"), result)
		return
	}
	if typeOf.Kind() != reflect.Struct {
		return
	}
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		collectPartialNumericKeys(t, field.Type, joinConfigPath(path, name), result)
	}
}

func optionalTypeValue(typeOf reflect.Type) (reflect.Type, bool) {
	if typeOf.Kind() != reflect.Struct || typeOf.PkgPath() != reflect.TypeOf(Optional[int]{}).PkgPath() ||
		!strings.HasPrefix(typeOf.Name(), "Optional[") || typeOf.NumField() != 2 ||
		typeOf.Field(0).Name != "Set" || typeOf.Field(1).Name != "Value" {
		return nil, false
	}
	return typeOf.Field(1).Type, true
}

func yamlField(typeOf reflect.Type, name string) (reflect.StructField, bool) {
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		if strings.Split(field.Tag.Get("yaml"), ",")[0] == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

func partialContainsYAMLName(typeOf reflect.Type, name string) bool {
	if _, ok := optionalTypeValue(typeOf); ok {
		return false
	}
	if typeOf.Kind() == reflect.Map || typeOf.Kind() == reflect.Slice {
		return partialContainsYAMLName(typeOf.Elem(), name)
	}
	if typeOf.Kind() != reflect.Struct {
		return false
	}
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		if strings.Split(field.Tag.Get("yaml"), ",")[0] == name || partialContainsYAMLName(field.Type, name) {
			return true
		}
	}
	return false
}
