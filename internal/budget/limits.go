package budget

import (
	"errors"
	"math"
)

const (
	kibibyte int64 = 1 << 10
	mebibyte       = 1 << 20
	gibibyte       = 1 << 30
)

// Dimension identifies one independently accumulated resource.
type Dimension uint8

const (
	Bytes Dimension = iota
	Files
	Directories
	Lines
	Items
	ExpandedBytes
	ProtocolErrors
	dimensionCount
)

func (d Dimension) Valid() bool {
	return d < dimensionCount
}

func (d Dimension) String() string {
	switch d {
	case Bytes:
		return "bytes"
	case Files:
		return "files"
	case Directories:
		return "directories"
	case Lines:
		return "lines"
	case Items:
		return "items"
	case ExpandedBytes:
		return "expanded_bytes"
	case ProtocolErrors:
		return "protocol_errors"
	default:
		return "unknown"
	}
}

// Scope identifies a configured cumulative budget without conflating other
// budgets that happen to use the same Dimension.
type Scope string

const (
	ToolInlineOutputBytes         Scope = "tool.inline_output_bytes"
	ToolCaptureBytes              Scope = "tool.capture_bytes"
	ArtifactMaxFileBytes          Scope = "artifact.max_file_bytes"
	ArtifactMaxTotalBytes         Scope = "artifact.max_total_bytes"
	FilesReadMaxBytes             Scope = "files.read_max_bytes"
	FilesScanMaxBytes             Scope = "files.scan_max_bytes"
	FilesScanMaxFiles             Scope = "files.scan_max_files"
	FilesScanMaxDirectories       Scope = "files.scan_max_directories"
	FilesScanMaxLines             Scope = "files.scan_max_lines"
	InstructionsMaxFileBytes      Scope = "instructions.max_file_bytes"
	InstructionsMaxTotalBytes     Scope = "instructions.max_total_bytes"
	InstructionsMaxFiles          Scope = "instructions.max_files"
	InstructionsMaxExpandedBytes  Scope = "instructions.max_expanded_bytes"
	MCPMaxResponseBytes           Scope = "mcp.max_response_bytes"
	MCPMaxTools                   Scope = "mcp.max_tools"
	MCPMaxPages                   Scope = "mcp.max_pages"
	MCPMaxProtocolErrors          Scope = "mcp.max_protocol_errors"
	ProviderMaxResponseBytes      Scope = "llm.stream.max_response_bytes"
	ProviderMaxEventBytes         Scope = "llm.stream.max_event_bytes"
	ProviderMaxEvents             Scope = "llm.stream.max_events"
	ProviderMaxTextBytes          Scope = "llm.stream.max_text_bytes"
	ProviderMaxThinkingBytes      Scope = "llm.stream.max_thinking_bytes"
	ProviderMaxToolArgumentsBytes Scope = "llm.stream.max_tool_arguments_bytes"
	SessionMaxRecordBytes         Scope = "session.max_record_bytes"
	SessionMaxSessionBytes        Scope = "session.max_session_bytes"
	SessionMaxScanFiles           Scope = "session.max_scan_files"
	SessionMaxScanBytes           Scope = "session.max_scan_bytes"
	DiagnosticsMaxItems           Scope = "diagnostics.max_items"
	DiagnosticsMaxItemBytes       Scope = "diagnostics.max_item_bytes"
	DiagnosticsMaxTotalBytes      Scope = "diagnostics.max_total_bytes"
)

// Spec fixes the approved default, minimum, and non-configurable hard cap for
// one cumulative budget scope.
type Spec struct {
	Scope     Scope
	Dimension Dimension
	Default   int64
	Minimum   int64
	HardCap   int64
}

// Resolve applies the default only when candidate is nil. Explicit zero,
// negative values, and values above the hard cap fail closed.
func (s Spec) Resolve(candidate *int64) (int64, error) {
	if candidate == nil {
		return s.Default, nil
	}
	if *candidate < s.Minimum {
		return 0, errors.New("budget value is below the minimum")
	}
	if *candidate > s.HardCap {
		return 0, &LimitError{
			Scope:     string(s.Scope),
			Dimension: s.Dimension,
			Limit:     s.HardCap,
			Observed:  *candidate,
		}
	}
	return *candidate, nil
}

// ResolveUnsigned rejects values that would overflow the signed counter
// representation before applying this scope's hard cap.
func (s Spec) ResolveUnsigned(candidate uint64) (int64, error) {
	if candidate > math.MaxInt64 {
		return 0, ErrIntegerOverflow
	}
	value := int64(candidate)
	return s.Resolve(&value)
}

var specs = [...]Spec{
	{ToolInlineOutputBytes, Bytes, 32 * kibibyte, 1, 1 * mebibyte},
	{ToolCaptureBytes, Bytes, 64 * mebibyte, 1, 512 * mebibyte},
	{ArtifactMaxFileBytes, Bytes, 64 * mebibyte, 1, 512 * mebibyte},
	{ArtifactMaxTotalBytes, Bytes, 1 * gibibyte, 1, 8 * gibibyte},
	{FilesReadMaxBytes, Bytes, 16 * mebibyte, 1, 256 * mebibyte},
	{FilesScanMaxBytes, Bytes, 256 * mebibyte, 1, 2 * gibibyte},
	{FilesScanMaxFiles, Files, 100_000, 1, 1_000_000},
	{FilesScanMaxDirectories, Directories, 25_000, 1, 250_000},
	{FilesScanMaxLines, Lines, 1_000_000, 1, 10_000_000},
	{InstructionsMaxFileBytes, Bytes, 64 * kibibyte, 1, 1 * mebibyte},
	{InstructionsMaxTotalBytes, Bytes, 1 * mebibyte, 1, 16 * mebibyte},
	{InstructionsMaxFiles, Files, 64, 1, 1_024},
	{InstructionsMaxExpandedBytes, ExpandedBytes, 2 * mebibyte, 1, 32 * mebibyte},
	{MCPMaxResponseBytes, Bytes, 1 * mebibyte, 1, 16 * mebibyte},
	{MCPMaxTools, Items, 128, 1, 1_024},
	{MCPMaxPages, Items, 32, 1, 128},
	{MCPMaxProtocolErrors, ProtocolErrors, 32, 1, 256},
	{ProviderMaxResponseBytes, Bytes, 16 * mebibyte, 1, 64 * mebibyte},
	{ProviderMaxEventBytes, Bytes, 1 * mebibyte, 1, 4 * mebibyte},
	{ProviderMaxEvents, Items, 100_000, 1, 1_000_000},
	{ProviderMaxTextBytes, Bytes, 8 * mebibyte, 1, 32 * mebibyte},
	{ProviderMaxThinkingBytes, Bytes, 8 * mebibyte, 1, 32 * mebibyte},
	{ProviderMaxToolArgumentsBytes, Bytes, 1 * mebibyte, 1, 8 * mebibyte},
	{SessionMaxRecordBytes, Bytes, 16 * mebibyte, 1, 64 * mebibyte},
	{SessionMaxSessionBytes, Bytes, 256 * mebibyte, 1, 1 * gibibyte},
	{SessionMaxScanFiles, Files, 1_000, 1, 100_000},
	{SessionMaxScanBytes, Bytes, 10 * mebibyte, 1, 1 * gibibyte},
	{DiagnosticsMaxItems, Items, 100, 1, 1_000},
	{DiagnosticsMaxItemBytes, Bytes, 2 * kibibyte, 1, 64 * kibibyte},
	{DiagnosticsMaxTotalBytes, Bytes, 2 * mebibyte, 1, 16 * mebibyte},
}

// AllSpecs returns a copy so callers cannot mutate the process-wide contract.
func AllSpecs() []Spec {
	result := make([]Spec, len(specs))
	copy(result, specs[:])
	return result
}

// Limit is one effective or hard limit for a Counter dimension.
type Limit struct {
	Dimension Dimension
	Value     int64
}

// Limits is immutable after construction.
type Limits struct {
	values  [dimensionCount]int64
	present uint16
}

func NewLimits(entries ...Limit) (Limits, error) {
	var result Limits
	for _, entry := range entries {
		if !entry.Dimension.Valid() {
			return Limits{}, errors.New("budget limits contain an unknown dimension")
		}
		if entry.Value <= 0 {
			return Limits{}, errors.New("budget limits must be positive")
		}
		mask := uint16(1) << entry.Dimension
		if result.present&mask != 0 {
			return Limits{}, errors.New("budget limits contain a duplicate dimension")
		}
		result.values[entry.Dimension] = entry.Value
		result.present |= mask
	}
	return result, nil
}

func (l Limits) Value(dimension Dimension) (int64, bool) {
	if !dimension.Valid() {
		return 0, false
	}
	mask := uint16(1) << dimension
	if l.present&mask == 0 {
		return 0, false
	}
	return l.values[dimension], true
}

// ValidateLimits rejects an effective limit that has no hard limit or exceeds
// its immutable hard boundary.
func ValidateLimits(scope string, effective, hard Limits) error {
	for dimension := Bytes; dimension < dimensionCount; dimension++ {
		observed, configured := effective.Value(dimension)
		if !configured {
			continue
		}
		limit, bounded := hard.Value(dimension)
		if !bounded {
			return errors.New("effective budget has no hard limit")
		}
		if observed > limit {
			return &LimitError{
				Scope:     scope,
				Dimension: dimension,
				Limit:     limit,
				Observed:  observed,
			}
		}
	}
	return nil
}
