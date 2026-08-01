package config

import (
	"errors"
	"fmt"
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
	{path: "context.model_window_tokens", defaultVal: 200_000, minimum: 2, hardCap: 10_000_000},
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
	_ = options
	var config AppConfig
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
	provenance := make(map[string]ConfigSource, len(result.Provenance))
	for path, source := range result.Provenance {
		provenance[path] = source
	}
	return LoadedConfig{Config: config, Provenance: provenance}, nil
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
	config.Tool.InlineOutputBytes = values[0]
	config.Tool.MaxOutputBytes = int(values[0])
	config.Tool.CaptureBytes = values[1]
	config.Tool.TimeoutMS = int(values[2])
	config.Artifact = ArtifactConfig{MaxFileBytes: values[3], MaxTotalBytes: values[4], RetentionDays: values[5]}
	config.Files = FilesConfig{
		ReadMaxBytes:       values[6],
		ScanMaxBytes:       values[7],
		ScanMaxFiles:       values[8],
		ScanMaxDirectories: values[9],
		ScanMaxLines:       values[10],
	}
	return nil
}
