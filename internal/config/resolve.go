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
	provenance := make(map[string]ConfigSource, len(result.Provenance))
	for path, source := range result.Provenance {
		provenance[path] = source
	}
	return LoadedConfig{Config: config, Provenance: provenance}, nil
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
