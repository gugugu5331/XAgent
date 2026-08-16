package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"xagent/internal/agentrole"
	"xagent/internal/redact"
	"xagent/internal/subagent"
	"xagent/internal/worktree"
)

// appConfigNumericManifest is the closed set of numeric values owned by the
// AppConfig resolve stage. Compatibility inputs and numeric values owned by
// Hook, Permission, Skill, or other loaders are deliberately absent.
var appConfigNumericManifest = [...]string{
	"tool.inline_output_bytes",
	"tool.capture_bytes",
	"tool.timeout_ms",
	"artifact.max_file_bytes",
	"artifact.max_total_bytes",
	"artifact.retention_days",
	"files.read_max_bytes",
	"files.scan_max_bytes",
	"files.scan_max_files",
	"files.scan_max_directories",
	"files.scan_max_lines",
	"instructions.max_file_bytes",
	"instructions.max_total_bytes",
	"instructions.max_files",
	"instructions.max_expanded_bytes",
	"instructions.max_include_depth",
	"mcp.max_response_bytes",
	"mcp.max_tools",
	"mcp.max_pages",
	"mcp.max_protocol_errors",
	"mcp.default_timeout_ms",
	"mcp.servers.<name>.timeout_ms",
	"llm.request_timeout_ms",
	"llm.thinking.budget_tokens",
	"llm.stream.max_response_bytes",
	"llm.stream.max_event_bytes",
	"llm.stream.max_events",
	"llm.stream.max_text_bytes",
	"llm.stream.max_thinking_bytes",
	"llm.stream.max_tool_arguments_bytes",
	"agent.max_iterations",
	"agent.max_unknown_tool_calls",
	"context.tool_result_threshold_chars",
	"context.tool_results_threshold_chars",
	"context.model_window_tokens",
	"context.auto_margin_tokens",
	"context.manual_margin_tokens",
	"context.recent_keep_tokens",
	"context.recent_keep_messages",
	"context.summary_failure_limit",
	"context.preview_chars",
	"session.max_record_bytes",
	"session.max_session_bytes",
	"session.max_scan_files",
	"session.max_scan_bytes",
	"session.retention_days",
	"session.gap_reminder_days",
	"memory.max_index_lines",
	"memory.max_index_bytes",
	"memory.update_queue_size",
	"memory.update_concurrency",
	"memory.update_timeout_ms",
	"memory.max_candidate_bytes",
	"diagnostics.max_items",
	"diagnostics.max_item_bytes",
	"diagnostics.max_total_bytes",
	"lifecycle.cleanup_timeout_ms",
	"subagent.auto_background_after_ms",
	"subagent.max_concurrent",
	"subagent.max_event_bytes",
	"subagent.max_events_per_task",
	"subagent.max_global_events",
	"subagent.max_pending_results",
	"subagent.max_queued",
	"subagent.max_result_bytes",
	"subagent.max_result_total_bytes",
	"subagent.max_results_per_claim",
	"subagent.max_retained_tasks",
	"subagent.max_role_name_bytes",
	"subagent.max_subscriber_buffer",
	"subagent.max_task_bytes",
	"subagent.max_task_duration_ms",
	"subagent.max_task_tombstones",
	"subagent.read_cache_max_bytes",
	"subagent.read_cache_max_dependencies_per_entry",
	"subagent.read_cache_max_entries",
	"subagent.read_cache_max_value_bytes",
	"subagent.role_limits.max_body_bytes",
	"subagent.role_limits.max_candidates",
	"subagent.role_limits.max_description_bytes",
	"subagent.role_limits.max_diagnostics",
	"subagent.role_limits.max_entry_bytes",
	"subagent.role_limits.max_files",
	"subagent.role_limits.max_frontmatter_bytes",
	"subagent.role_limits.max_instruction_bytes",
	"subagent.role_limits.max_model_bytes",
	"subagent.role_limits.max_name_bytes",
	"subagent.role_limits.max_origin_bytes",
	"subagent.role_limits.max_provider_id_bytes",
	"subagent.role_limits.max_providers",
	"subagent.role_limits.max_root_bytes",
	"subagent.role_limits.max_source_id_bytes",
	"subagent.role_limits.max_tool_list_bytes",
	"subagent.role_limits.max_tool_name_bytes",
	"subagent.role_limits.max_tool_names",
	"subagent.role_limits.max_total_bytes",
	"subagent.worktree.lifecycle.git_timeout_ms",
	"subagent.worktree.lifecycle.init_timeout_ms",
	"subagent.worktree.lifecycle.janitor_interval_ms",
	"subagent.worktree.lifecycle.janitor_timeout_ms",
	"subagent.worktree.lifecycle.lock_timeout_ms",
	"subagent.worktree.lifecycle.recovery_timeout_ms",
	"subagent.worktree.lifecycle.retention_ttl_ms",
	"subagent.worktree.lifecycle.settle_timeout_ms",
	"subagent.worktree.limits.max_active",
	"subagent.worktree.limits.max_depth",
	"subagent.worktree.limits.max_init_bytes",
	"subagent.worktree.limits.max_init_depth",
	"subagent.worktree.limits.max_init_files",
	"subagent.worktree.limits.max_janitor_candidates",
	"subagent.worktree.limits.max_janitor_concurrency",
	"subagent.worktree.limits.max_name_bytes",
	"subagent.worktree.limits.max_retained",
	"subagent.worktree.limits.max_segment_bytes",
}

func appConfigNumericKeys() []string {
	result := make([]string, len(appConfigNumericManifest))
	copy(result, appConfigNumericManifest[:])
	return result
}

type LoadedConfig struct {
	Config     AppConfig
	Provenance map[string]ConfigSource
}

type numericSpec struct {
	path       string
	defaultVal int64
	minimum    int64
	hardCap    int64
}

const (
	kibibyte int64 = 1 << 10
	mebibyte       = 1 << 20
	gibibyte       = 1 << 30
)

var toolArtifactFilesNumericSpecs = [...]numericSpec{
	{path: "tool.inline_output_bytes", defaultVal: 32 * kibibyte, minimum: 1, hardCap: 1 * mebibyte},
	{path: "tool.capture_bytes", defaultVal: 64 * mebibyte, minimum: 1, hardCap: 512 * mebibyte},
	{path: "tool.timeout_ms", defaultVal: 30_000, minimum: 1, hardCap: 86_400_000},
	{path: "artifact.max_file_bytes", defaultVal: 64 * mebibyte, minimum: 1, hardCap: 512 * mebibyte},
	{path: "artifact.max_total_bytes", defaultVal: 1 * gibibyte, minimum: 1, hardCap: 8 * gibibyte},
	{path: "artifact.retention_days", defaultVal: 7, minimum: 1, hardCap: 365},
	{path: "files.read_max_bytes", defaultVal: 16 * mebibyte, minimum: 1, hardCap: 256 * mebibyte},
	{path: "files.scan_max_bytes", defaultVal: 256 * mebibyte, minimum: 1, hardCap: 2 * gibibyte},
	{path: "files.scan_max_files", defaultVal: 100_000, minimum: 1, hardCap: 1_000_000},
	{path: "files.scan_max_directories", defaultVal: 25_000, minimum: 1, hardCap: 250_000},
	{path: "files.scan_max_lines", defaultVal: 1_000_000, minimum: 1, hardCap: 10_000_000},
}

var instructionMCPNumericSpecs = [...]numericSpec{
	{path: "instructions.max_file_bytes", defaultVal: 64 * kibibyte, minimum: 1, hardCap: 1 * mebibyte},
	{path: "instructions.max_total_bytes", defaultVal: 1 * mebibyte, minimum: 1, hardCap: 16 * mebibyte},
	{path: "instructions.max_files", defaultVal: 64, minimum: 1, hardCap: 1_024},
	{path: "instructions.max_expanded_bytes", defaultVal: 2 * mebibyte, minimum: 1, hardCap: 32 * mebibyte},
	{path: "instructions.max_include_depth", defaultVal: 5, minimum: 1, hardCap: 32},
	{path: "mcp.max_response_bytes", defaultVal: 1 * mebibyte, minimum: 1, hardCap: 16 * mebibyte},
	{path: "mcp.max_tools", defaultVal: 128, minimum: 1, hardCap: 1_024},
	{path: "mcp.max_pages", defaultVal: 32, minimum: 1, hardCap: 128},
	{path: "mcp.max_protocol_errors", defaultVal: 32, minimum: 1, hardCap: 256},
	{path: "mcp.default_timeout_ms", defaultVal: 30_000, minimum: 1, hardCap: 86_400_000},
}

var mcpServerTimeoutSpec = numericSpec{
	path: "mcp.servers.<name>.timeout_ms", minimum: 1, hardCap: 86_400_000,
}

var llmAgentStreamNumericSpecs = [...]numericSpec{
	{path: "llm.request_timeout_ms", defaultVal: 120_000, minimum: 1, hardCap: 86_400_000},
	{path: "llm.thinking.budget_tokens", defaultVal: 4_096, minimum: 1, hardCap: 1_000_000},
	{path: "agent.max_iterations", defaultVal: 10, minimum: 1, hardCap: 1_000},
	{path: "agent.max_unknown_tool_calls", defaultVal: 2, minimum: 1, hardCap: 100},
	{path: "llm.stream.max_response_bytes", defaultVal: 16 * mebibyte, minimum: 1, hardCap: 64 * mebibyte},
	{path: "llm.stream.max_event_bytes", defaultVal: 1 * mebibyte, minimum: 1, hardCap: 4 * mebibyte},
	{path: "llm.stream.max_events", defaultVal: 100_000, minimum: 1, hardCap: 1_000_000},
	{path: "llm.stream.max_text_bytes", defaultVal: 8 * mebibyte, minimum: 1, hardCap: 32 * mebibyte},
	{path: "llm.stream.max_thinking_bytes", defaultVal: 8 * mebibyte, minimum: 1, hardCap: 32 * mebibyte},
	{path: "llm.stream.max_tool_arguments_bytes", defaultVal: 1 * mebibyte, minimum: 1, hardCap: 8 * mebibyte},
}

var contextSessionNumericSpecs = [...]numericSpec{
	{path: "context.tool_result_threshold_chars", defaultVal: 32 * kibibyte, minimum: 1, hardCap: 1 * mebibyte},
	{path: "context.tool_results_threshold_chars", defaultVal: 64 * kibibyte, minimum: 1, hardCap: 4 * mebibyte},
	{path: "context.model_window_tokens", defaultVal: 200_000, minimum: 131, hardCap: 10_000_000},
	{path: "context.auto_margin_tokens", defaultVal: 13_000, minimum: 1, hardCap: 1_000_000},
	{path: "context.manual_margin_tokens", defaultVal: 3_000, minimum: 1, hardCap: 1_000_000},
	{path: "context.recent_keep_tokens", defaultVal: 10_000, minimum: 1, hardCap: 1_000_000},
	{path: "context.recent_keep_messages", defaultVal: 5, minimum: 1, hardCap: 10_000},
	{path: "context.summary_failure_limit", defaultVal: 3, minimum: 1, hardCap: 100},
	{path: "context.preview_chars", defaultVal: 2_000, minimum: 1, hardCap: 1 * mebibyte},
	{path: "session.max_record_bytes", defaultVal: 16 * mebibyte, minimum: 1, hardCap: 64 * mebibyte},
	{path: "session.max_session_bytes", defaultVal: 256 * mebibyte, minimum: 1, hardCap: 1 * gibibyte},
	{path: "session.max_scan_files", defaultVal: 1_000, minimum: 1, hardCap: 100_000},
	{path: "session.max_scan_bytes", defaultVal: 10 * mebibyte, minimum: 1, hardCap: 1 * gibibyte},
	{path: "session.retention_days", defaultVal: 30, minimum: 1, hardCap: 3_650},
	{path: "session.gap_reminder_days", defaultVal: 7, minimum: 1, hardCap: 3_650},
}

var memoryDiagnosticsLifecycleNumericSpecs = [...]numericSpec{
	{path: "memory.max_index_lines", defaultVal: 200, minimum: 1, hardCap: 100_000},
	{path: "memory.max_index_bytes", defaultVal: 25 * kibibyte, minimum: 1, hardCap: 16 * mebibyte},
	{path: "memory.update_queue_size", defaultVal: 8, minimum: 1, hardCap: 1_024},
	{path: "memory.update_concurrency", defaultVal: 1, minimum: 1, hardCap: 64},
	{path: "memory.update_timeout_ms", defaultVal: 30_000, minimum: 1, hardCap: 86_400_000},
	{path: "memory.max_candidate_bytes", defaultVal: 64 * kibibyte, minimum: 1, hardCap: 16 * mebibyte},
	{path: "diagnostics.max_items", defaultVal: 100, minimum: 1, hardCap: 1_000},
	{path: "diagnostics.max_item_bytes", defaultVal: 2 * kibibyte, minimum: 1, hardCap: 64 * kibibyte},
	{path: "diagnostics.max_total_bytes", defaultVal: 2 * mebibyte, minimum: 1, hardCap: 16 * mebibyte},
	{path: "lifecycle.cleanup_timeout_ms", defaultVal: 2_000, minimum: 1, hardCap: 2_000},
}

var roleLimitNumericSpecs = [...]numericSpec{
	{path: "subagent.role_limits.max_files", defaultVal: 256, minimum: 1, hardCap: 4_096},
	{path: "subagent.role_limits.max_entry_bytes", defaultVal: 256 * kibibyte, minimum: 1, hardCap: 4 * mebibyte},
	{path: "subagent.role_limits.max_frontmatter_bytes", defaultVal: 32 * kibibyte, minimum: 1, hardCap: 256 * kibibyte},
	{path: "subagent.role_limits.max_body_bytes", defaultVal: 128 * kibibyte, minimum: 1, hardCap: 2 * mebibyte},
	{path: "subagent.role_limits.max_name_bytes", defaultVal: 64, minimum: 1, hardCap: 64},
	{path: "subagent.role_limits.max_description_bytes", defaultVal: 8 * kibibyte, minimum: 1, hardCap: 64 * kibibyte},
	{path: "subagent.role_limits.max_instruction_bytes", defaultVal: 128 * kibibyte, minimum: 1, hardCap: 2 * mebibyte},
	{path: "subagent.role_limits.max_tool_name_bytes", defaultVal: 128, minimum: 1, hardCap: 512},
	{path: "subagent.role_limits.max_tool_list_bytes", defaultVal: 8 * kibibyte, minimum: 1, hardCap: 128 * kibibyte},
	{path: "subagent.role_limits.max_origin_bytes", defaultVal: 256, minimum: 1, hardCap: 4 * kibibyte},
	{path: "subagent.role_limits.max_source_id_bytes", defaultVal: 256, minimum: 1, hardCap: 4 * kibibyte},
	{path: "subagent.role_limits.max_provider_id_bytes", defaultVal: 256, minimum: 1, hardCap: 4 * kibibyte},
	{path: "subagent.role_limits.max_root_bytes", defaultVal: 4 * kibibyte, minimum: 1, hardCap: 64 * kibibyte},
	{path: "subagent.role_limits.max_model_bytes", defaultVal: 256, minimum: 1, hardCap: 4 * kibibyte},
	{path: "subagent.role_limits.max_total_bytes", defaultVal: 64 * mebibyte, minimum: 1, hardCap: 512 * mebibyte},
	{path: "subagent.role_limits.max_tool_names", defaultVal: 128, minimum: 1, hardCap: 1_024},
	{path: "subagent.role_limits.max_providers", defaultVal: 64, minimum: 1, hardCap: 256},
	{path: "subagent.role_limits.max_candidates", defaultVal: 512, minimum: 1, hardCap: 4_096},
	{path: "subagent.role_limits.max_diagnostics", defaultVal: 256, minimum: 1, hardCap: 4_096},
}

var subagentRuntimeNumericSpecs = [...]numericSpec{
	{path: "subagent.max_task_bytes", defaultVal: 32 * kibibyte, minimum: 1, hardCap: 1 * mebibyte},
	{path: "subagent.max_concurrent", defaultVal: 4, minimum: 1, hardCap: 64},
	{path: "subagent.max_queued", defaultVal: 32, minimum: 1, hardCap: 4_096},
	{path: "subagent.max_retained_tasks", defaultVal: 256, minimum: 2, hardCap: 65_536},
	{path: "subagent.max_task_tombstones", defaultVal: 1_024, minimum: 1, hardCap: 65_536},
	{path: "subagent.max_global_events", defaultVal: 8_192, minimum: 1, hardCap: 1_000_000},
	{path: "subagent.max_events_per_task", defaultVal: 1_024, minimum: 1, hardCap: 100_000},
	{path: "subagent.max_event_bytes", defaultVal: 64 * kibibyte, minimum: 1, hardCap: 1 * mebibyte},
	{path: "subagent.max_subscriber_buffer", defaultVal: 128, minimum: 1, hardCap: 65_536},
	{path: "subagent.max_result_bytes", defaultVal: 64 * kibibyte, minimum: 1, hardCap: 1 * mebibyte},
	{path: "subagent.max_pending_results", defaultVal: 256, minimum: 1, hardCap: 65_536},
	{path: "subagent.max_result_total_bytes", defaultVal: 8 * mebibyte, minimum: 1, hardCap: 512 * mebibyte},
	{path: "subagent.max_results_per_claim", defaultVal: 32, minimum: 1, hardCap: 1_024},
	{path: "subagent.read_cache_max_entries", defaultVal: 128, minimum: 1, hardCap: 65_536},
	{path: "subagent.read_cache_max_bytes", defaultVal: 8 * mebibyte, minimum: 1, hardCap: 512 * mebibyte},
	{path: "subagent.read_cache_max_value_bytes", defaultVal: 1 * mebibyte, minimum: 1, hardCap: 64 * mebibyte},
	{path: "subagent.read_cache_max_dependencies_per_entry", defaultVal: 4_096, minimum: 1, hardCap: 1_000_000},
	{path: "subagent.max_role_name_bytes", defaultVal: 64, minimum: 1, hardCap: 64},
	{path: "subagent.auto_background_after_ms", defaultVal: 10_000, minimum: 1, hardCap: 3_600_000},
	{path: "subagent.max_task_duration_ms", defaultVal: 0, minimum: 0, hardCap: 86_400_000},
}

var worktreeNumericSpecs = [...]numericSpec{
	{path: "subagent.worktree.lifecycle.retention_ttl_ms", defaultVal: 86_400_000, minimum: 60_000, hardCap: 2_592_000_000},
	{path: "subagent.worktree.lifecycle.janitor_interval_ms", defaultVal: 1_800_000, minimum: 60_000, hardCap: 86_400_000},
	{path: "subagent.worktree.lifecycle.git_timeout_ms", defaultVal: 30_000, minimum: 100, hardCap: 300_000},
	{path: "subagent.worktree.lifecycle.lock_timeout_ms", defaultVal: 10_000, minimum: 100, hardCap: 60_000},
	{path: "subagent.worktree.lifecycle.init_timeout_ms", defaultVal: 120_000, minimum: 1_000, hardCap: 300_000},
	{path: "subagent.worktree.lifecycle.recovery_timeout_ms", defaultVal: 30_000, minimum: 100, hardCap: 300_000},
	{path: "subagent.worktree.lifecycle.settle_timeout_ms", defaultVal: 120_000, minimum: 1_000, hardCap: 1_800_000},
	{path: "subagent.worktree.lifecycle.janitor_timeout_ms", defaultVal: 60_000, minimum: 1_000, hardCap: 1_800_000},
	{path: "subagent.worktree.limits.max_active", defaultVal: 8, minimum: 1, hardCap: 64},
	{path: "subagent.worktree.limits.max_retained", defaultVal: 32, minimum: 1, hardCap: 4_096},
	{path: "subagent.worktree.limits.max_name_bytes", defaultVal: 192, minimum: 1, hardCap: 1_024},
	{path: "subagent.worktree.limits.max_segment_bytes", defaultVal: 64, minimum: 1, hardCap: 255},
	{path: "subagent.worktree.limits.max_depth", defaultVal: 8, minimum: 1, hardCap: 32},
	{path: "subagent.worktree.limits.max_init_files", defaultVal: 10_000, minimum: 1, hardCap: 10_000},
	{path: "subagent.worktree.limits.max_init_bytes", defaultVal: 1 * gibibyte, minimum: 1, hardCap: 1 * gibibyte},
	{path: "subagent.worktree.limits.max_init_depth", defaultVal: 32, minimum: 1, hardCap: 64},
	{path: "subagent.worktree.limits.max_janitor_candidates", defaultVal: 256, minimum: 1, hardCap: 10_000},
	{path: "subagent.worktree.limits.max_janitor_concurrency", defaultVal: 4, minimum: 1, hardCap: 64},
}

func (s numericSpec) resolve(candidate Optional[int64]) (int64, error) {
	if !candidate.Set {
		return s.defaultVal, nil
	}
	if candidate.Value < s.minimum || candidate.Value > s.hardCap {
		return 0, fmt.Errorf("config field %q must be in range %d..%d", s.path, s.minimum, s.hardCap)
	}
	return candidate.Value, nil
}

func ResolveConfig(result MergeResult, options LoadOptions) (LoadedConfig, error) {
	var config AppConfig
	resolveNonNumeric(&config, result.Value)
	resolvedSubagent, err := ResolveSubagentConfig(result.Value.Subagent)
	if err != nil {
		return LoadedConfig{}, err
	}
	config.Subagent = resolvedSubagent
	config.LLM.ModelAliases, err = ResolveModelAliases(result.Value.LLM.ModelAliases, resolvedSubagent.RoleLimits.MaxModelBytes)
	if err != nil {
		return LoadedConfig{}, err
	}
	if err := resolveToolArtifactFiles(&config, result.Value); err != nil {
		return LoadedConfig{}, err
	}
	if err := resolveInstructionsMCP(&config, result); err != nil {
		return LoadedConfig{}, err
	}
	if err := resolveLLMAgentStream(&config, result.Value); err != nil {
		return LoadedConfig{}, err
	}
	if err := resolveContextSession(&config, result.Value); err != nil {
		return LoadedConfig{}, err
	}
	if err := validateSubagentResultPlanningReserve(config); err != nil {
		return LoadedConfig{}, err
	}
	if err := resolveMemoryDiagnosticsLifecycle(&config, result.Value); err != nil {
		return LoadedConfig{}, err
	}
	if err := resolveEffectiveExpandedSecrets(&config, result.Value, options.Redactor); err != nil {
		return LoadedConfig{}, err
	}
	provenance := make(map[string]ConfigSource, len(result.Provenance))
	for path, source := range result.Provenance {
		provenance[path] = source
	}
	return LoadedConfig{Config: config, Provenance: provenance}, nil
}

func validateSubagentResultPlanningReserve(config AppConfig) error {
	reserve, err := subagent.ResultPlanningReserveTokens(config.Subagent.Limits.MaxResultBytes)
	if err != nil {
		return errors.New("config field \"subagent.max_result_bytes\" has an invalid planning reserve")
	}
	planningWindow := config.Context.ModelWindowTokens - config.Context.AutoMarginTokens
	if planningWindow <= 0 || reserve >= planningWindow {
		return errors.New("config field \"subagent.max_result_bytes\" cannot fit inside the automatic context planning window")
	}
	return nil
}

func ResolveModelAliases(partial PartialModelAliases, maxModelBytes int64) (ResolvedModelAliases, error) {
	if maxModelBytes <= 0 {
		return ResolvedModelAliases{}, errors.New("config field \"llm.model_aliases\" has an invalid size limit")
	}
	resolve := func(path string, value Optional[string]) (string, error) {
		if !value.Set {
			return "", nil
		}
		trimmed := strings.TrimSpace(value.Value)
		if trimmed == "" || int64(len(trimmed)) > maxModelBytes {
			return "", fmt.Errorf("config field %q must be non-empty and at most %d bytes", path, maxModelBytes)
		}
		return trimmed, nil
	}
	var resolved ResolvedModelAliases
	var err error
	if resolved.Haiku, err = resolve("llm.model_aliases.haiku", partial.Haiku); err != nil {
		return ResolvedModelAliases{}, err
	}
	if resolved.Sonnet, err = resolve("llm.model_aliases.sonnet", partial.Sonnet); err != nil {
		return ResolvedModelAliases{}, err
	}
	if resolved.Opus, err = resolve("llm.model_aliases.opus", partial.Opus); err != nil {
		return ResolvedModelAliases{}, err
	}
	return resolved, nil
}

func ResolveSubagentConfig(partial PartialSubagentConfig) (SubagentConfig, error) {
	roleLimits := agentrole.DefaultLimits()
	if err := applyRoleLimits(&roleLimits, partial.RoleLimits); err != nil {
		return SubagentConfig{}, err
	}
	if err := roleLimits.Validate(); err != nil {
		return SubagentConfig{}, fmt.Errorf("config field \"subagent.role_limits\": %w", err)
	}

	runtimeLimits := subagent.DefaultLimits()
	if err := applySubagentLimits(&runtimeLimits, partial); err != nil {
		return SubagentConfig{}, err
	}
	if err := runtimeLimits.Validate(); err != nil {
		return SubagentConfig{}, fmt.Errorf("config field \"subagent\": %w", err)
	}

	var backgroundTools []string
	if partial.BackgroundTools.Set {
		backgroundTools = append([]string{}, partial.BackgroundTools.Value...)
	}
	worktreeConfig, err := resolveWorktreeConfig(partial.Worktree)
	if err != nil {
		return SubagentConfig{}, err
	}
	return SubagentConfig{RoleLimits: roleLimits, Limits: runtimeLimits, BackgroundTools: backgroundTools, Worktree: worktreeConfig}, nil
}

func resolveWorktreeConfig(partial PartialWorktreeConfig) (worktree.Config, error) {
	var config worktree.Config
	durationFields := []struct {
		path  string
		value Optional[int64]
		set   func(time.Duration)
	}{
		{"subagent.worktree.lifecycle.retention_ttl_ms", partial.Lifecycle.RetentionTTLMS, func(value time.Duration) { config.Lifecycle.RetentionTTL = value }},
		{"subagent.worktree.lifecycle.janitor_interval_ms", partial.Lifecycle.JanitorIntervalMS, func(value time.Duration) { config.Lifecycle.JanitorInterval = value }},
		{"subagent.worktree.lifecycle.git_timeout_ms", partial.Lifecycle.GitTimeoutMS, func(value time.Duration) { config.Lifecycle.GitTimeout = value }},
		{"subagent.worktree.lifecycle.lock_timeout_ms", partial.Lifecycle.LockTimeoutMS, func(value time.Duration) { config.Lifecycle.LockTimeout = value }},
		{"subagent.worktree.lifecycle.init_timeout_ms", partial.Lifecycle.InitTimeoutMS, func(value time.Duration) { config.Lifecycle.InitTimeout = value }},
		{"subagent.worktree.lifecycle.recovery_timeout_ms", partial.Lifecycle.RecoveryTimeoutMS, func(value time.Duration) { config.Lifecycle.RecoveryTimeout = value }},
		{"subagent.worktree.lifecycle.settle_timeout_ms", partial.Lifecycle.SettleTimeoutMS, func(value time.Duration) { config.Lifecycle.SettleTimeout = value }},
		{"subagent.worktree.lifecycle.janitor_timeout_ms", partial.Lifecycle.JanitorTimeoutMS, func(value time.Duration) { config.Lifecycle.JanitorTimeout = value }},
	}
	for _, field := range durationFields {
		value, err := resolveSubagentNumeric(field.path, field.value)
		if err != nil {
			return worktree.Config{}, err
		}
		field.set(time.Duration(value) * time.Millisecond)
	}
	intFields := []struct {
		path  string
		value Optional[int64]
		set   func(int)
	}{
		{"subagent.worktree.limits.max_active", partial.Limits.MaxActive, func(value int) { config.Limits.MaxActive = value }},
		{"subagent.worktree.limits.max_retained", partial.Limits.MaxRetained, func(value int) { config.Limits.MaxRetained = value }},
		{"subagent.worktree.limits.max_name_bytes", partial.Limits.MaxNameBytes, func(value int) { config.Limits.MaxNameBytes = value }},
		{"subagent.worktree.limits.max_segment_bytes", partial.Limits.MaxSegmentBytes, func(value int) { config.Limits.MaxSegmentBytes = value }},
		{"subagent.worktree.limits.max_depth", partial.Limits.MaxDepth, func(value int) { config.Limits.MaxDepth = value }},
		{"subagent.worktree.limits.max_init_files", partial.Limits.MaxInitFiles, func(value int) { config.Limits.MaxInitFiles = value }},
		{"subagent.worktree.limits.max_init_depth", partial.Limits.MaxInitDepth, func(value int) { config.Limits.MaxInitDepth = value }},
		{"subagent.worktree.limits.max_janitor_candidates", partial.Limits.MaxJanitorCandidates, func(value int) { config.Limits.MaxJanitorCandidates = value }},
		{"subagent.worktree.limits.max_janitor_concurrency", partial.Limits.MaxJanitorConcurrency, func(value int) { config.Limits.MaxJanitorConcurrency = value }},
	}
	for _, field := range intFields {
		value, err := resolveSubagentNumeric(field.path, field.value)
		if err != nil {
			return worktree.Config{}, err
		}
		resolved, err := positiveInt(field.path, value)
		if err != nil {
			return worktree.Config{}, err
		}
		field.set(resolved)
	}
	maxInitBytes, err := resolveSubagentNumeric("subagent.worktree.limits.max_init_bytes", partial.Limits.MaxInitBytes)
	if err != nil {
		return worktree.Config{}, err
	}
	config.Limits.MaxInitBytes = maxInitBytes

	if partial.Init.Copy.Set {
		config.Init.Copy = make([]worktree.CopyRule, len(partial.Init.Copy.Value))
		for index, rule := range partial.Init.Copy.Value {
			config.Init.Copy[index] = worktree.CopyRule{Source: strings.TrimSpace(rule.Source), Target: strings.TrimSpace(rule.Target)}
		}
	}
	if partial.Init.Link.Set {
		config.Init.Link = make([]worktree.LinkRule, len(partial.Init.Link.Value))
		for index, rule := range partial.Init.Link.Value {
			config.Init.Link[index] = worktree.LinkRule{Source: strings.TrimSpace(rule.Source), Target: strings.TrimSpace(rule.Target)}
		}
	}
	if partial.Init.IgnoredCopy.Set {
		config.Init.IgnoredCopy = make([]worktree.CopyRule, len(partial.Init.IgnoredCopy.Value))
		for index, rule := range partial.Init.IgnoredCopy.Value {
			config.Init.IgnoredCopy[index] = worktree.CopyRule{Source: strings.TrimSpace(rule.Source), Target: strings.TrimSpace(rule.Target)}
		}
	}
	if partial.Init.GitHooks.Enabled.Set {
		config.Init.GitHooks.Enabled = partial.Init.GitHooks.Enabled.Value
	}
	if partial.Init.GitHooks.Path.Set {
		config.Init.GitHooks.Path = strings.TrimSpace(partial.Init.GitHooks.Path.Value)
	}
	if config.Limits.MaxRetained < config.Limits.MaxActive {
		return worktree.Config{}, errors.New("config field \"subagent.worktree.limits.max_retained\" must be at least max_active")
	}
	if config.Limits.MaxSegmentBytes > config.Limits.MaxNameBytes {
		return worktree.Config{}, errors.New("config field \"subagent.worktree.limits.max_segment_bytes\" must not exceed max_name_bytes")
	}
	if config.Limits.MaxJanitorConcurrency > config.Limits.MaxJanitorCandidates {
		return worktree.Config{}, errors.New("config field \"subagent.worktree.limits.max_janitor_concurrency\" must not exceed max_janitor_candidates")
	}
	if err := config.Validate(); err != nil {
		return worktree.Config{}, fmt.Errorf("config field \"subagent.worktree\": %w", err)
	}
	return config, nil
}

func applyRoleLimits(target *agentrole.Limits, partial PartialRoleLimits) error {
	if target == nil {
		return errors.New("config field \"subagent.role_limits\" target is nil")
	}
	intFields := []struct {
		path  string
		value Optional[int64]
		set   func(int)
	}{
		{"subagent.role_limits.max_files", partial.MaxFiles, func(value int) { target.MaxFiles = value }},
		{"subagent.role_limits.max_tool_names", partial.MaxToolNames, func(value int) { target.MaxToolNames = value }},
		{"subagent.role_limits.max_providers", partial.MaxProviders, func(value int) { target.MaxProviders = value }},
		{"subagent.role_limits.max_candidates", partial.MaxCandidates, func(value int) { target.MaxCandidates = value }},
		{"subagent.role_limits.max_diagnostics", partial.MaxDiagnostics, func(value int) { target.MaxDiagnostics = value }},
	}
	for _, field := range intFields {
		if !field.value.Set {
			continue
		}
		resolved, err := resolveSubagentNumeric(field.path, field.value)
		if err != nil {
			return err
		}
		value, err := positiveInt(field.path, resolved)
		if err != nil {
			return err
		}
		field.set(value)
	}
	byteFields := []struct {
		path  string
		value Optional[int64]
		set   func(int64)
	}{
		{"subagent.role_limits.max_entry_bytes", partial.MaxEntryBytes, func(value int64) { target.MaxEntryBytes = value }},
		{"subagent.role_limits.max_frontmatter_bytes", partial.MaxFrontmatterBytes, func(value int64) { target.MaxFrontmatterBytes = value }},
		{"subagent.role_limits.max_body_bytes", partial.MaxBodyBytes, func(value int64) { target.MaxBodyBytes = value }},
		{"subagent.role_limits.max_name_bytes", partial.MaxNameBytes, func(value int64) { target.MaxNameBytes = value }},
		{"subagent.role_limits.max_description_bytes", partial.MaxDescriptionBytes, func(value int64) { target.MaxDescriptionBytes = value }},
		{"subagent.role_limits.max_instruction_bytes", partial.MaxInstructionBytes, func(value int64) { target.MaxInstructionBytes = value }},
		{"subagent.role_limits.max_tool_name_bytes", partial.MaxToolNameBytes, func(value int64) { target.MaxToolNameBytes = value }},
		{"subagent.role_limits.max_tool_list_bytes", partial.MaxToolListBytes, func(value int64) { target.MaxToolListBytes = value }},
		{"subagent.role_limits.max_origin_bytes", partial.MaxOriginBytes, func(value int64) { target.MaxOriginBytes = value }},
		{"subagent.role_limits.max_source_id_bytes", partial.MaxSourceIDBytes, func(value int64) { target.MaxSourceIDBytes = value }},
		{"subagent.role_limits.max_provider_id_bytes", partial.MaxProviderIDBytes, func(value int64) { target.MaxProviderIDBytes = value }},
		{"subagent.role_limits.max_root_bytes", partial.MaxRootBytes, func(value int64) { target.MaxRootBytes = value }},
		{"subagent.role_limits.max_model_bytes", partial.MaxModelBytes, func(value int64) { target.MaxModelBytes = value }},
		{"subagent.role_limits.max_total_bytes", partial.MaxTotalBytes, func(value int64) { target.MaxTotalBytes = value }},
	}
	for _, field := range byteFields {
		if !field.value.Set {
			continue
		}
		value, err := resolveSubagentNumeric(field.path, field.value)
		if err != nil {
			return err
		}
		field.set(value)
	}
	return nil
}

func applySubagentLimits(target *subagent.Limits, partial PartialSubagentConfig) error {
	if target == nil {
		return errors.New("config field \"subagent\" target is nil")
	}
	intFields := []struct {
		path  string
		value Optional[int64]
		set   func(int)
	}{
		{"subagent.max_concurrent", partial.MaxConcurrent, func(value int) { target.MaxConcurrent = value }},
		{"subagent.max_queued", partial.MaxQueued, func(value int) { target.MaxQueued = value }},
		{"subagent.max_retained_tasks", partial.MaxRetainedTasks, func(value int) { target.MaxRetainedTasks = value }},
		{"subagent.max_task_tombstones", partial.MaxTaskTombstones, func(value int) { target.MaxTaskTombstones = value }},
		{"subagent.max_global_events", partial.MaxGlobalEvents, func(value int) { target.MaxGlobalEvents = value }},
		{"subagent.max_events_per_task", partial.MaxEventsPerTask, func(value int) { target.MaxEventsPerTask = value }},
		{"subagent.max_subscriber_buffer", partial.MaxSubscriberBuffer, func(value int) { target.MaxSubscriberBuffer = value }},
		{"subagent.max_pending_results", partial.MaxPendingResults, func(value int) { target.MaxPendingResults = value }},
		{"subagent.max_results_per_claim", partial.MaxResultsPerClaim, func(value int) { target.MaxResultsPerClaim = value }},
		{"subagent.read_cache_max_entries", partial.ReadCacheMaxEntries, func(value int) { target.ReadCacheMaxEntries = value }},
		{"subagent.read_cache_max_dependencies_per_entry", partial.ReadCacheMaxDependenciesPerEntry, func(value int) { target.ReadCacheMaxDependenciesPerEntry = value }},
	}
	for _, field := range intFields {
		if !field.value.Set {
			continue
		}
		resolved, err := resolveSubagentNumeric(field.path, field.value)
		if err != nil {
			return err
		}
		value, err := positiveInt(field.path, resolved)
		if err != nil {
			return err
		}
		field.set(value)
	}
	byteFields := []struct {
		path  string
		value Optional[int64]
		set   func(int64)
	}{
		{"subagent.max_task_bytes", partial.MaxTaskBytes, func(value int64) { target.MaxTaskBytes = value }},
		{"subagent.max_event_bytes", partial.MaxEventBytes, func(value int64) { target.MaxEventBytes = value }},
		{"subagent.max_result_bytes", partial.MaxResultBytes, func(value int64) { target.MaxResultBytes = value }},
		{"subagent.max_result_total_bytes", partial.MaxResultTotalBytes, func(value int64) { target.MaxResultTotalBytes = value }},
		{"subagent.read_cache_max_bytes", partial.ReadCacheMaxBytes, func(value int64) { target.ReadCacheMaxBytes = value }},
		{"subagent.read_cache_max_value_bytes", partial.ReadCacheMaxValueBytes, func(value int64) { target.ReadCacheMaxValueBytes = value }},
		{"subagent.max_role_name_bytes", partial.MaxRoleNameBytes, func(value int64) { target.MaxRoleNameBytes = value }},
	}
	for _, field := range byteFields {
		if !field.value.Set {
			continue
		}
		value, err := resolveSubagentNumeric(field.path, field.value)
		if err != nil {
			return err
		}
		field.set(value)
	}
	if partial.AutoBackgroundAfterMS.Set {
		value, err := resolveSubagentNumeric("subagent.auto_background_after_ms", partial.AutoBackgroundAfterMS)
		if err != nil {
			return err
		}
		target.AutoBackgroundAfter = time.Duration(value) * time.Millisecond
	}
	if partial.MaxTaskDurationMS.Set {
		value, err := resolveSubagentNumeric("subagent.max_task_duration_ms", partial.MaxTaskDurationMS)
		if err != nil {
			return err
		}
		target.MaxTaskDuration = time.Duration(value) * time.Millisecond
	}
	return nil
}

func resolveSubagentNumeric(path string, candidate Optional[int64]) (int64, error) {
	for _, specs := range [][]numericSpec{roleLimitNumericSpecs[:], subagentRuntimeNumericSpecs[:], worktreeNumericSpecs[:]} {
		for _, spec := range specs {
			if spec.path == path {
				return spec.resolve(candidate)
			}
		}
	}
	return 0, fmt.Errorf("config field %q is not a registered subagent limit", path)
}

func positiveInt(path string, value int64) (int, error) {
	maxInt := int64(^uint(0) >> 1)
	if value <= 0 || value > maxInt {
		return 0, fmt.Errorf("config field %q must be a positive platform integer", path)
	}
	return int(value), nil
}

func resolveNonNumeric(config *AppConfig, partial PartialAppConfig) {
	config.LLM.Protocol = partial.LLM.Protocol.Value
	config.LLM.Model = partial.LLM.Model.Value
	config.LLM.BaseURL = partial.LLM.BaseURL.Value
	config.LLM.Thinking.Enabled = partial.LLM.Thinking.Enabled.Value
	config.LLM.Thinking.Show = partial.LLM.Thinking.Show.Value

	config.UI.ShowResponseTimer = true
	if partial.UI.ShowResponseTimer.Set {
		config.UI.ShowResponseTimer = partial.UI.ShowResponseTimer.Value
	}
	config.UI.StartMode = DefaultStartMode
	if partial.UI.StartMode.Set {
		config.UI.StartMode = partial.UI.StartMode.Value
	}
	config.Storage.DataDir = DefaultDataDir
	if partial.Storage.DataDir.Set {
		config.Storage.DataDir = partial.Storage.DataDir.Value
	}
	config.Permission.Mode = PermissionDefault
	if partial.Permission.Mode.Set {
		config.Permission.Mode = partial.Permission.Mode.Value
	}

	config.Context.Enabled = boolPtr(true)
	if partial.Context.Enabled.Set {
		config.Context.Enabled = boolPtr(partial.Context.Enabled.Value)
	}
	config.Instructions.Enabled = boolPtr(true)
	if partial.Instructions.Enabled.Set {
		config.Instructions.Enabled = boolPtr(partial.Instructions.Enabled.Value)
	}
	config.Instructions.ProjectFile = optionalStringOrDefault(partial.Instructions.ProjectFile, DefaultInstructionsProjectFile)
	config.Instructions.ProjectDir = optionalStringOrDefault(partial.Instructions.ProjectDir, DefaultInstructionsProjectDir)
	config.Instructions.UserDir = optionalStringOrDefault(partial.Instructions.UserDir, DefaultInstructionsUserDir)
	config.Session.Dir = optionalStringOrDefault(partial.Session.Dir, DefaultSessionDir)
	config.Memory.Enabled = boolPtr(true)
	if partial.Memory.Enabled.Set {
		config.Memory.Enabled = boolPtr(partial.Memory.Enabled.Value)
	}
	config.Memory.UserDir = optionalStringOrDefault(partial.Memory.UserDir, DefaultMemoryUserDir)
	config.Memory.ProjectDir = optionalStringOrDefault(partial.Memory.ProjectDir, DefaultMemoryProjectDir)
}

func optionalStringOrDefault(value Optional[string], defaultValue string) string {
	if value.Set {
		return value.Value
	}
	return defaultValue
}

func resolveEffectiveExpandedSecrets(config *AppConfig, partial PartialAppConfig, runtimeRedactor *redact.RuntimeRedactor) error {
	secrets := make([]string, 0)
	expand := func(path string, key string, value string) (string, error) {
		expanded, expansions, err := expandConfigValueDetailed(value)
		if err != nil {
			return "", fmt.Errorf("config field %q: %w", path, err)
		}
		fieldSensitive := redact.IsSensitiveKey(key)
		if fieldSensitive && expanded != "" {
			secrets = append(secrets, expanded)
		}
		for _, expansion := range expansions {
			if expansion.value != "" && (fieldSensitive || redact.IsSensitiveKey(expansion.name)) {
				secrets = append(secrets, expansion.value)
			}
		}
		return expanded, nil
	}

	apiKey, err := expand("llm.api_key", "api_key", partial.LLM.APIKey.Value)
	if err != nil {
		return err
	}
	config.LLM.APIKey = apiKey

	serverNames := make([]string, 0, len(config.MCP.Servers))
	for name := range config.MCP.Servers {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)
	for _, name := range serverNames {
		server := config.MCP.Servers[name]
		if server.Disabled {
			continue
		}
		prefix := joinConfigPath("mcp.servers", name)
		server.Command, err = expand(joinConfigPath(prefix, "command"), "command", server.Command)
		if err != nil {
			return err
		}
		for index, argument := range server.Args {
			server.Args[index], err = expand(fmt.Sprintf("%s.args[%d]", prefix, index), "args", argument)
			if err != nil {
				return err
			}
		}
		if err := expandEffectiveStringMap(prefix, "env", server.Env, expand); err != nil {
			return err
		}
		server.URL, err = expand(joinConfigPath(prefix, "url"), "url", server.URL)
		if err != nil {
			return err
		}
		if err := expandEffectiveStringMap(prefix, "headers", server.Headers, expand); err != nil {
			return err
		}
		config.MCP.Servers[name] = server
	}

	if runtimeRedactor != nil {
		for _, secret := range secrets {
			runtimeRedactor.RegisterSecret(secret)
		}
	}
	return nil
}

func expandEffectiveStringMap(
	prefix string,
	field string,
	values map[string]string,
	expand func(path string, key string, value string) (string, error),
) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		expanded, err := expand(joinConfigPath(joinConfigPath(prefix, field), key), key, values[key])
		if err != nil {
			return err
		}
		values[key] = expanded
	}
	return nil
}

func resolveMemoryDiagnosticsLifecycle(config *AppConfig, partial PartialAppConfig) error {
	candidates := [...]Optional[int64]{
		partial.Memory.MaxIndexLines,
		partial.Memory.MaxIndexBytes,
		partial.Memory.UpdateQueueSize,
		partial.Memory.UpdateConcurrency,
		partial.Memory.UpdateTimeoutMS,
		partial.Memory.MaxCandidateBytes,
		partial.Diagnostics.MaxItems,
		partial.Diagnostics.MaxItemBytes,
		partial.Diagnostics.MaxTotalBytes,
		partial.Lifecycle.CleanupTimeoutMS,
	}
	values := make([]int64, len(candidates))
	for index, spec := range memoryDiagnosticsLifecycleNumericSpecs {
		value, err := spec.resolve(candidates[index])
		if err != nil {
			return err
		}
		values[index] = value
	}

	if values[3] > values[2] {
		return fmt.Errorf("config field %q must not exceed %q", memoryDiagnosticsLifecycleNumericSpecs[3].path, memoryDiagnosticsLifecycleNumericSpecs[2].path)
	}
	if values[7] > values[8] {
		return fmt.Errorf("config field %q must not exceed %q", memoryDiagnosticsLifecycleNumericSpecs[7].path, memoryDiagnosticsLifecycleNumericSpecs[8].path)
	}

	config.Memory.MaxIndexLines = int(values[0])
	config.Memory.MaxIndexBytes = int(values[1])
	config.Memory.UpdateQueueSize = int(values[2])
	config.Memory.UpdateConcurrency = int(values[3])
	config.Memory.UpdateTimeoutMS = int(values[4])
	config.Memory.MaxCandidateBytes = int(values[5])
	config.Diagnostics = DiagnosticsConfig{MaxItems: values[6], MaxItemBytes: values[7], MaxTotalBytes: values[8]}
	config.Lifecycle = LifecycleConfig{CleanupTimeoutMS: values[9]}
	return nil
}

func resolveContextSession(config *AppConfig, partial PartialAppConfig) error {
	candidates := [...]Optional[int64]{
		partial.Context.ToolResultThresholdChars,
		partial.Context.ToolResultsThresholdChars,
		partial.Context.ModelWindowTokens,
		partial.Context.AutoMarginTokens,
		partial.Context.ManualMarginTokens,
		partial.Context.RecentKeepTokens,
		partial.Context.RecentKeepMessages,
		partial.Context.SummaryFailureLimit,
		partial.Context.PreviewChars,
		partial.Session.MaxRecordBytes,
		partial.Session.MaxSessionBytes,
		partial.Session.MaxScanFiles,
		partial.Session.MaxScanBytes,
		partial.Session.RetentionDays,
		partial.Session.GapReminderDays,
	}
	values := make([]int64, len(candidates))
	for index, spec := range contextSessionNumericSpecs {
		value, err := spec.resolve(candidates[index])
		if err != nil {
			return err
		}
		values[index] = value
	}

	if values[0] > values[1] {
		return fmt.Errorf("config field %q must not exceed %q", contextSessionNumericSpecs[0].path, contextSessionNumericSpecs[1].path)
	}
	for _, index := range []int{3, 4, 5} {
		if values[index] >= values[2] {
			return fmt.Errorf("config field %q must be less than %q", contextSessionNumericSpecs[index].path, contextSessionNumericSpecs[2].path)
		}
	}
	if values[9] > values[10] {
		return fmt.Errorf("config field %q must not exceed %q", contextSessionNumericSpecs[9].path, contextSessionNumericSpecs[10].path)
	}

	config.Context.ToolResultThresholdChars = int(values[0])
	config.Context.ToolResultsThresholdChars = int(values[1])
	config.Context.ModelWindowTokens = values[2]
	config.Context.AutoMarginTokens = values[3]
	config.Context.ManualMarginTokens = values[4]
	config.Context.RecentKeepTokens = values[5]
	config.Context.RecentKeepMessages = int(values[6])
	config.Context.SummaryFailureLimit = int(values[7])
	config.Context.PreviewChars = int(values[8])
	config.Session.MaxRecordBytes = values[9]
	config.Session.MaxSessionBytes = values[10]
	config.Session.MaxScanFiles = int(values[11])
	config.Session.MaxScanBytes = values[12]
	config.Session.RetentionDays = int(values[13])
	config.Session.GapReminderDays = int(values[14])
	return nil
}

func resolveLLMAgentStream(config *AppConfig, partial PartialAppConfig) error {
	candidates := [...]Optional[int64]{
		partial.LLM.RequestTimeoutMS,
		partial.LLM.Thinking.BudgetTokens,
		partial.Agent.MaxIterations,
		partial.Agent.MaxUnknownToolCalls,
		partial.LLM.Stream.MaxResponseBytes,
		partial.LLM.Stream.MaxEventBytes,
		partial.LLM.Stream.MaxEvents,
		partial.LLM.Stream.MaxTextBytes,
		partial.LLM.Stream.MaxThinkingBytes,
		partial.LLM.Stream.MaxToolArgumentsBytes,
	}
	values := make([]int64, len(candidates))
	for index, spec := range llmAgentStreamNumericSpecs {
		value, err := spec.resolve(candidates[index])
		if err != nil {
			return err
		}
		values[index] = value
	}

	responseLimit := values[4]
	for _, index := range []int{5, 7, 8, 9} {
		if values[index] > responseLimit {
			return fmt.Errorf("config field %q must not exceed %q", llmAgentStreamNumericSpecs[index].path, llmAgentStreamNumericSpecs[4].path)
		}
	}

	config.LLM.RequestTimeoutMS = int(values[0])
	config.LLM.Thinking.BudgetTokens = int(values[1])
	config.Agent.MaxIterations = int(values[2])
	config.Agent.MaxUnknownToolCalls = int(values[3])
	config.LLM.Stream = StreamConfig{
		MaxResponseBytes:      values[4],
		MaxEventBytes:         values[5],
		MaxEvents:             values[6],
		MaxTextBytes:          values[7],
		MaxThinkingBytes:      values[8],
		MaxToolArgumentsBytes: values[9],
	}
	return nil
}

func resolveInstructionsMCP(config *AppConfig, result MergeResult) error {
	partial := result.Value
	candidates := [...]Optional[int64]{
		partial.Instructions.MaxFileBytes,
		partial.Instructions.MaxTotalBytes,
		partial.Instructions.MaxFiles,
		partial.Instructions.MaxExpandedBytes,
		partial.Instructions.MaxIncludeDepth,
		partial.MCP.MaxResponseBytes,
		partial.MCP.MaxTools,
		partial.MCP.MaxPages,
		partial.MCP.MaxProtocolErrors,
		partial.MCP.DefaultTimeoutMS,
	}
	values := make([]int64, len(candidates))
	for index, spec := range instructionMCPNumericSpecs {
		value, err := spec.resolve(candidates[index])
		if err != nil {
			return err
		}
		values[index] = value
	}
	if values[0] > values[1] {
		return fmt.Errorf("config field %q must not exceed %q", instructionMCPNumericSpecs[0].path, instructionMCPNumericSpecs[1].path)
	}
	if values[0] > values[3] {
		return fmt.Errorf("config field %q must not exceed %q", instructionMCPNumericSpecs[0].path, instructionMCPNumericSpecs[3].path)
	}
	config.Instructions.MaxFileBytes = values[0]
	config.Instructions.MaxTotalBytes = values[1]
	config.Instructions.MaxFiles = values[2]
	config.Instructions.MaxExpandedBytes = values[3]
	config.Instructions.MaxIncludeDepth = int(values[4])
	config.MCP.DefaultTimeoutMS = values[9]
	config.MCP.MaxResponseBytes = values[5]
	config.MCP.MaxTools = values[6]
	config.MCP.MaxPages = values[7]
	config.MCP.MaxProtocolErrors = values[8]
	config.MCP.Servers = make(map[string]MCPServerConfig, len(partial.MCP.Servers))
	for name, server := range partial.MCP.Servers {
		timeoutCandidate := server.TimeoutMS
		if !timeoutCandidate.Set {
			timeoutCandidate = Optional[int64]{Set: true, Value: config.MCP.DefaultTimeoutMS}
		}
		timeout, err := mcpServerTimeoutSpec.resolve(timeoutCandidate)
		if err != nil {
			return err
		}
		resolved := MCPServerConfig{
			Disabled:  server.Disabled.Value,
			Type:      server.Type.Value,
			Command:   server.Command.Value,
			Args:      append([]string(nil), server.Args.Value...),
			Env:       cloneStringMap(server.Env.Value),
			URL:       server.URL.Value,
			Headers:   cloneStringMap(server.Headers.Value),
			TimeoutMS: timeout,
			Source:    string(result.Provenance[joinConfigPath("mcp.servers", name)]),
		}
		config.MCP.Servers[name] = resolved
	}
	return nil
}

func cloneStringMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func resolveToolArtifactFiles(config *AppConfig, partial PartialAppConfig) error {
	if config == nil {
		return errors.New("resolved config target is nil")
	}
	if partial.Artifact.Root.Set && partial.Artifact.Root.Value == "" {
		return errors.New("config field \"artifact.root\" must not be empty")
	}
	inlineCandidate := partial.Tool.InlineOutputBytes
	if partial.Tool.MaxOutputBytes.Set {
		if inlineCandidate.Set {
			return errors.New("config fields \"tool.inline_output_bytes\" and \"tool.max_output_bytes\" conflict")
		}
		inlineCandidate = partial.Tool.MaxOutputBytes
	}
	candidates := [...]Optional[int64]{
		inlineCandidate,
		partial.Tool.CaptureBytes,
		partial.Tool.TimeoutMS,
		partial.Artifact.MaxFileBytes,
		partial.Artifact.MaxTotalBytes,
		partial.Artifact.RetentionDays,
		partial.Files.ReadMaxBytes,
		partial.Files.ScanMaxBytes,
		partial.Files.ScanMaxFiles,
		partial.Files.ScanMaxDirectories,
		partial.Files.ScanMaxLines,
	}
	values := make([]int64, len(candidates))
	for index, spec := range toolArtifactFilesNumericSpecs {
		value, err := spec.resolve(candidates[index])
		if err != nil {
			return err
		}
		values[index] = value
	}
	for _, relation := range [][2]int{{0, 1}, {1, 3}, {3, 4}} {
		if values[relation[0]] > values[relation[1]] {
			return fmt.Errorf(
				"config field %q must not exceed %q",
				toolArtifactFilesNumericSpecs[relation[0]].path,
				toolArtifactFilesNumericSpecs[relation[1]].path,
			)
		}
	}
	config.Tool.InlineOutputBytes = values[0]
	config.Tool.MaxOutputBytes = int(values[0])
	config.Tool.CaptureBytes = values[1]
	config.Tool.TimeoutMS = int(values[2])
	config.Artifact = ArtifactConfig{
		Root:          partial.Artifact.Root.Value,
		MaxFileBytes:  values[3],
		MaxTotalBytes: values[4],
		RetentionDays: values[5],
	}
	config.Files = FilesConfig{
		ReadMaxBytes:       values[6],
		ScanMaxBytes:       values[7],
		ScanMaxFiles:       values[8],
		ScanMaxDirectories: values[9],
		ScanMaxLines:       values[10],
	}
	return nil
}
