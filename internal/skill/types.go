package skill

import (
	"xagent/internal/diagnostics"
)

const (
	MaxSkillNameLength         = 64
	DefaultMaxFiles            = 256
	DefaultMaxEntryBytes int64 = 256 * 1024
	DefaultMaxBodyBytes        = 128 * 1024
	DefaultMaxArgsBytes        = 32 * 1024
	LoadSkillToolName          = "load_skill"
)

type Source string

const (
	SourceBuiltin Source = "builtin"
	SourceUser    Source = "user"
	SourceProject Source = "project"
)

type Mode string

const (
	ModeShared   Mode = "shared"
	ModeIsolated Mode = "isolated"
)

type Metadata struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	AllowedTools []string `yaml:"allowed_tools,omitempty"`
	Mode         Mode     `yaml:"mode"`
	History      int      `yaml:"history,omitempty"`
	Model        string   `yaml:"model,omitempty"`
	historySet   bool
}

type Definition struct {
	Metadata
	Body        string
	EntryPath   string
	PackageRoot string
	Source      Source
	Fingerprint string
}

type CatalogItem struct {
	Name         string
	Description  string
	Mode         Mode
	SlashEnabled bool
}

type Snapshot struct {
	Generation  uint64
	Fingerprint string
	Catalog     []CatalogItem
	Definitions map[string]Definition
	Diagnostics []diagnostics.Diagnostic
}

type Invocation struct {
	Name   string
	Args   string
	Raw    string
	Origin InvocationOrigin
}

type InvocationOrigin string

const (
	OriginSlash     InvocationOrigin = "slash"
	OriginAgentTool InvocationOrigin = "agent_tool"
)

type PreparedInvocation struct {
	Definition Definition
	Activated  Activated
	Activity   *Activity
	Mode       Mode
	History    int
}

type Limits struct {
	MaxFiles      int
	MaxEntryBytes int64
	MaxBodyBytes  int
	MaxArgsBytes  int
}

func DefaultLimits() Limits {
	return Limits{
		MaxFiles:      DefaultMaxFiles,
		MaxEntryBytes: DefaultMaxEntryBytes,
		MaxBodyBytes:  DefaultMaxBodyBytes,
		MaxArgsBytes:  DefaultMaxArgsBytes,
	}
}

func normalizeLimits(limits Limits) Limits {
	defaults := DefaultLimits()
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = defaults.MaxFiles
	}
	if limits.MaxEntryBytes <= 0 {
		limits.MaxEntryBytes = defaults.MaxEntryBytes
	}
	if limits.MaxBodyBytes <= 0 {
		limits.MaxBodyBytes = defaults.MaxBodyBytes
	}
	if limits.MaxArgsBytes <= 0 {
		limits.MaxArgsBytes = defaults.MaxArgsBytes
	}
	return limits
}

func (s Snapshot) Clone() Snapshot {
	clone := Snapshot{
		Generation:  s.Generation,
		Fingerprint: s.Fingerprint,
		Catalog:     append([]CatalogItem(nil), s.Catalog...),
		Definitions: make(map[string]Definition, len(s.Definitions)),
		Diagnostics: append([]diagnostics.Diagnostic(nil), s.Diagnostics...),
	}
	for name, definition := range s.Definitions {
		clone.Definitions[name] = cloneDefinition(definition)
	}
	return clone
}

func cloneDefinition(definition Definition) Definition {
	definition.AllowedTools = append([]string(nil), definition.AllowedTools...)
	return definition
}
