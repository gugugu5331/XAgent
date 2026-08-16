package agentrole

import (
	"fmt"
	"io/fs"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

type Source string

const (
	SourcePlugin  Source = "plugin"
	SourceBuiltin Source = "builtin"
	SourceUser    Source = "user"
	SourceProject Source = "project"
)

type ModelAlias string

const (
	ModelInherit ModelAlias = "inherit"
	ModelHaiku   ModelAlias = "haiku"
	ModelSonnet  ModelAlias = "sonnet"
	ModelOpus    ModelAlias = "opus"
)

type ModelAliases struct {
	Haiku  string
	Sonnet string
	Opus   string
}

type PermissionMode string

const (
	PermissionInherit    PermissionMode = "inherit"
	PermissionStrict     PermissionMode = "strict"
	PermissionDefault    PermissionMode = "default"
	PermissionPermissive PermissionMode = "permissive"
)

// IsolationMode 描述角色需要的项目工作区隔离方式。
type IsolationMode string

const (
	IsolationNone     IsolationMode = ""
	IsolationWorktree IsolationMode = "worktree"
)

type Metadata struct {
	Name           string
	Description    redact.SafeText
	ToolAllow      []string
	ToolDeny       []string
	Model          ModelAlias
	MaxIterations  *int
	PermissionMode PermissionMode
	Isolation      IsolationMode
}

type Provenance struct {
	Source     Source
	SourceID   string
	ProviderID string
	Origin     redact.SafeText
}

type Definition struct {
	Metadata
	Instructions redact.SafeText
	Provenance
	Fingerprint string
}

type Candidate struct {
	Metadata
	Instructions redact.SafeText
	Origin       redact.SafeText
	Valid        bool
	Diagnostics  []diagnostics.SafeDiagnostic
}

type FileSource struct {
	Source Source
	ID     string
	FS     fs.FS
	Root   string
}

type CatalogItem struct {
	Name        string
	Description redact.SafeText
	Source      Source
	SourceID    string
	ProviderID  string
}

type ToolMetadata struct {
	Name           string
	ReadOnly       bool
	SideEffectFree bool
	ConcurrentSafe bool
}

type ModelMetadata struct {
	Alias     ModelAlias
	Concrete  string
	Provider  string
	Available bool
	Dynamic   bool
}

type Limits struct {
	MaxFiles            int
	MaxEntryBytes       int64
	MaxFrontmatterBytes int64
	MaxBodyBytes        int64
	MaxNameBytes        int64
	MaxDescriptionBytes int64
	MaxInstructionBytes int64
	MaxToolNameBytes    int64
	MaxToolListBytes    int64
	MaxOriginBytes      int64
	MaxSourceIDBytes    int64
	MaxProviderIDBytes  int64
	MaxRootBytes        int64
	MaxModelBytes       int64
	MaxTotalBytes       int64
	MaxToolNames        int
	MaxProviders        int
	MaxCandidates       int
	MaxDiagnostics      int
}

func DefaultLimits() Limits {
	return Limits{
		MaxFiles:            256,
		MaxEntryBytes:       256 << 10,
		MaxFrontmatterBytes: 32 << 10,
		MaxBodyBytes:        128 << 10,
		MaxNameBytes:        64,
		MaxDescriptionBytes: 8 << 10,
		MaxInstructionBytes: 128 << 10,
		MaxToolNameBytes:    128,
		MaxToolListBytes:    8 << 10,
		MaxOriginBytes:      256,
		MaxSourceIDBytes:    256,
		MaxProviderIDBytes:  256,
		MaxRootBytes:        4 << 10,
		MaxModelBytes:       256,
		MaxTotalBytes:       64 << 20,
		MaxToolNames:        128,
		MaxProviders:        64,
		MaxCandidates:       512,
		MaxDiagnostics:      256,
	}
}

func hardLimits() Limits {
	return Limits{
		MaxFiles:            4_096,
		MaxEntryBytes:       4 << 20,
		MaxFrontmatterBytes: 256 << 10,
		MaxBodyBytes:        2 << 20,
		MaxNameBytes:        64,
		MaxDescriptionBytes: 64 << 10,
		MaxInstructionBytes: 2 << 20,
		MaxToolNameBytes:    512,
		MaxToolListBytes:    128 << 10,
		MaxOriginBytes:      4 << 10,
		MaxSourceIDBytes:    4 << 10,
		MaxProviderIDBytes:  4 << 10,
		MaxRootBytes:        64 << 10,
		MaxModelBytes:       4 << 10,
		MaxTotalBytes:       512 << 20,
		MaxToolNames:        1_024,
		MaxProviders:        256,
		MaxCandidates:       4_096,
		MaxDiagnostics:      4_096,
	}
}

func (l Limits) Validate() error {
	hard := hardLimits()
	intFields := []struct {
		name       string
		value, cap int
	}{
		{"max_files", l.MaxFiles, hard.MaxFiles},
		{"max_tool_names", l.MaxToolNames, hard.MaxToolNames},
		{"max_providers", l.MaxProviders, hard.MaxProviders},
		{"max_candidates", l.MaxCandidates, hard.MaxCandidates},
		{"max_diagnostics", l.MaxDiagnostics, hard.MaxDiagnostics},
	}
	for _, field := range intFields {
		if field.value <= 0 || field.value > field.cap {
			return fmt.Errorf("role limit %s is out of range", field.name)
		}
	}
	byteFields := []struct {
		name       string
		value, cap int64
	}{
		{"max_entry_bytes", l.MaxEntryBytes, hard.MaxEntryBytes},
		{"max_frontmatter_bytes", l.MaxFrontmatterBytes, hard.MaxFrontmatterBytes},
		{"max_body_bytes", l.MaxBodyBytes, hard.MaxBodyBytes},
		{"max_name_bytes", l.MaxNameBytes, hard.MaxNameBytes},
		{"max_description_bytes", l.MaxDescriptionBytes, hard.MaxDescriptionBytes},
		{"max_instruction_bytes", l.MaxInstructionBytes, hard.MaxInstructionBytes},
		{"max_tool_name_bytes", l.MaxToolNameBytes, hard.MaxToolNameBytes},
		{"max_tool_list_bytes", l.MaxToolListBytes, hard.MaxToolListBytes},
		{"max_origin_bytes", l.MaxOriginBytes, hard.MaxOriginBytes},
		{"max_source_id_bytes", l.MaxSourceIDBytes, hard.MaxSourceIDBytes},
		{"max_provider_id_bytes", l.MaxProviderIDBytes, hard.MaxProviderIDBytes},
		{"max_root_bytes", l.MaxRootBytes, hard.MaxRootBytes},
		{"max_model_bytes", l.MaxModelBytes, hard.MaxModelBytes},
		{"max_total_bytes", l.MaxTotalBytes, hard.MaxTotalBytes},
	}
	for _, field := range byteFields {
		if field.value <= 0 || field.value > field.cap {
			return fmt.Errorf("role limit %s is out of range", field.name)
		}
	}
	if l.MaxFrontmatterBytes > l.MaxEntryBytes || l.MaxBodyBytes > l.MaxEntryBytes || l.MaxInstructionBytes > l.MaxBodyBytes {
		return fmt.Errorf("role byte limits have an unsafe relation")
	}
	if l.MaxEntryBytes > l.MaxTotalBytes {
		return fmt.Errorf("role entry limit exceeds total limit")
	}
	return nil
}
