package config

import (
	"xagent/internal/agentrole"
	"xagent/internal/redact"
	"xagent/internal/subagent"
)

type AppConfig struct {
	LLM          LLMConfig          `yaml:"llm"`
	UI           UIConfig           `yaml:"ui"`
	Storage      StorageConfig      `yaml:"storage"`
	Permission   PermissionConfig   `yaml:"permission"`
	MCP          MCPConfig          `yaml:"mcp"`
	Agent        AgentConfig        `yaml:"agent"`
	Tool         ToolConfig         `yaml:"tool"`
	Artifact     ArtifactConfig     `yaml:"-"`
	Files        FilesConfig        `yaml:"-"`
	Context      ContextConfig      `yaml:"context"`
	Instructions InstructionsConfig `yaml:"instructions"`
	Session      SessionConfig      `yaml:"session"`
	Memory       MemoryConfig       `yaml:"memory"`
	Diagnostics  DiagnosticsConfig  `yaml:"-"`
	Lifecycle    LifecycleConfig    `yaml:"-"`
	Subagent     SubagentConfig     `yaml:"-"`
}

type LLMConfig struct {
	Protocol         string               `yaml:"protocol"`
	Model            string               `yaml:"model"`
	BaseURL          string               `yaml:"base_url"`
	APIKey           string               `yaml:"api_key"`
	RequestTimeoutMS int                  `yaml:"request_timeout_ms"`
	Thinking         ThinkingConfig       `yaml:"thinking"`
	Stream           StreamConfig         `yaml:"-"`
	ModelAliases     ResolvedModelAliases `yaml:"-"`
}

type ResolvedModelAliases struct {
	Haiku  string
	Sonnet string
	Opus   string
}

func (a ResolvedModelAliases) ToAgentRole() agentrole.ModelAliases {
	return agentrole.ModelAliases{Haiku: a.Haiku, Sonnet: a.Sonnet, Opus: a.Opus}
}

type SubagentConfig struct {
	RoleLimits      agentrole.Limits
	Limits          subagent.Limits
	BackgroundTools []string
}

type StreamConfig struct {
	MaxResponseBytes      int64
	MaxEventBytes         int64
	MaxEvents             int64
	MaxTextBytes          int64
	MaxThinkingBytes      int64
	MaxToolArgumentsBytes int64
}

type AgentConfig struct {
	MaxIterations       int `yaml:"max_iterations"`
	MaxUnknownToolCalls int `yaml:"max_unknown_tool_calls"`
}

type ToolConfig struct {
	TimeoutMS         int   `yaml:"timeout_ms"`
	MaxOutputBytes    int   `yaml:"max_output_bytes"`
	InlineOutputBytes int64 `yaml:"-"`
	CaptureBytes      int64 `yaml:"-"`
}

type ArtifactConfig struct {
	Root          string
	MaxFileBytes  int64
	MaxTotalBytes int64
	RetentionDays int64
}

type FilesConfig struct {
	ReadMaxBytes       int64
	ScanMaxBytes       int64
	ScanMaxFiles       int64
	ScanMaxDirectories int64
	ScanMaxLines       int64
}

type LoadOptions struct {
	Redactor *redact.RuntimeRedactor
}

type ThinkingConfig struct {
	Enabled      bool `yaml:"enabled"`
	Show         bool `yaml:"show"`
	BudgetTokens int  `yaml:"budget_tokens"`
}

type UIConfig struct {
	ShowResponseTimer bool   `yaml:"show_response_timer"`
	StartMode         string `yaml:"start_mode"`
}

type StorageConfig struct {
	DataDir string `yaml:"data_dir"`
}

type PermissionConfig struct {
	Mode string `yaml:"mode"`
}

type ContextConfig struct {
	Enabled                   *bool `yaml:"enabled"`
	ToolResultThresholdChars  int   `yaml:"tool_result_threshold_chars"`
	ToolResultsThresholdChars int   `yaml:"tool_results_threshold_chars"`
	ModelWindowTokens         int64 `yaml:"model_window_tokens"`
	AutoMarginTokens          int64 `yaml:"auto_margin_tokens"`
	ManualMarginTokens        int64 `yaml:"manual_margin_tokens"`
	RecentKeepTokens          int64 `yaml:"recent_keep_tokens"`
	RecentKeepMessages        int   `yaml:"recent_keep_messages"`
	SummaryFailureLimit       int   `yaml:"summary_failure_limit"`
	PreviewChars              int   `yaml:"preview_chars"`
}

type InstructionsConfig struct {
	Enabled          *bool  `yaml:"enabled"`
	ProjectFile      string `yaml:"project_file"`
	ProjectDir       string `yaml:"project_dir"`
	UserDir          string `yaml:"user_dir"`
	MaxIncludeDepth  int    `yaml:"max_include_depth"`
	MaxFileBytes     int64  `yaml:"max_file_bytes"`
	MaxTotalBytes    int64  `yaml:"-"`
	MaxFiles         int64  `yaml:"-"`
	MaxExpandedBytes int64  `yaml:"-"`
}

type SessionConfig struct {
	Dir             string `yaml:"dir"`
	RetentionDays   int    `yaml:"retention_days"`
	MaxRecordBytes  int64  `yaml:"-"`
	MaxSessionBytes int64  `yaml:"-"`
	MaxScanFiles    int    `yaml:"max_scan_files"`
	MaxScanBytes    int64  `yaml:"max_scan_bytes"`
	GapReminderDays int    `yaml:"gap_reminder_days"`
}

type MemoryConfig struct {
	Enabled           *bool  `yaml:"enabled"`
	UserDir           string `yaml:"user_dir"`
	ProjectDir        string `yaml:"project_dir"`
	MaxIndexLines     int    `yaml:"max_index_lines"`
	MaxIndexBytes     int    `yaml:"max_index_bytes"`
	UpdateQueueSize   int    `yaml:"update_queue_size"`
	UpdateConcurrency int    `yaml:"update_concurrency"`
	UpdateTimeoutMS   int    `yaml:"update_timeout_ms"`
	MaxCandidateBytes int    `yaml:"max_candidate_bytes"`
}

type DiagnosticsConfig struct {
	MaxItems      int64
	MaxItemBytes  int64
	MaxTotalBytes int64
}

type LifecycleConfig struct {
	CleanupTimeoutMS int64
}
