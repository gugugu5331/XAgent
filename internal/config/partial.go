package config

// PartialAppConfig is the presence-aware representation used only while
// decoding and merging configuration layers. Runtime owners receive the
// resolved, deterministic AppConfig instead.
type PartialAppConfig struct {
	LLM          PartialLLMConfig          `yaml:"llm"`
	UI           PartialUIConfig           `yaml:"ui"`
	Storage      PartialStorageConfig      `yaml:"storage"`
	Permission   PartialPermissionConfig   `yaml:"permission"`
	MCP          PartialMCPConfig          `yaml:"mcp"`
	Agent        PartialAgentConfig        `yaml:"agent"`
	Tool         PartialToolConfig         `yaml:"tool"`
	Artifact     PartialArtifactConfig     `yaml:"artifact"`
	Files        PartialFilesConfig        `yaml:"files"`
	Context      PartialContextConfig      `yaml:"context"`
	Instructions PartialInstructionsConfig `yaml:"instructions"`
	Session      PartialSessionConfig      `yaml:"session"`
	Memory       PartialMemoryConfig       `yaml:"memory"`
	Diagnostics  PartialDiagnosticsConfig  `yaml:"diagnostics"`
	Lifecycle    PartialLifecycleConfig    `yaml:"lifecycle"`
	Subagent     PartialSubagentConfig     `yaml:"subagent"`
}

type PartialLLMConfig struct {
	Protocol         Optional[string]      `yaml:"protocol"`
	Model            Optional[string]      `yaml:"model"`
	BaseURL          Optional[string]      `yaml:"base_url"`
	APIKey           Optional[string]      `yaml:"api_key"`
	RequestTimeoutMS Optional[int64]       `yaml:"request_timeout_ms"`
	Thinking         PartialThinkingConfig `yaml:"thinking"`
	Stream           PartialStreamConfig   `yaml:"stream"`
	ModelAliases     PartialModelAliases   `yaml:"model_aliases"`
}

type PartialModelAliases struct {
	Haiku  Optional[string] `yaml:"haiku"`
	Sonnet Optional[string] `yaml:"sonnet"`
	Opus   Optional[string] `yaml:"opus"`
}

type PartialThinkingConfig struct {
	Enabled      Optional[bool]  `yaml:"enabled"`
	Show         Optional[bool]  `yaml:"show"`
	BudgetTokens Optional[int64] `yaml:"budget_tokens"`
}

type PartialStreamConfig struct {
	MaxResponseBytes      Optional[int64] `yaml:"max_response_bytes"`
	MaxEventBytes         Optional[int64] `yaml:"max_event_bytes"`
	MaxEvents             Optional[int64] `yaml:"max_events"`
	MaxTextBytes          Optional[int64] `yaml:"max_text_bytes"`
	MaxThinkingBytes      Optional[int64] `yaml:"max_thinking_bytes"`
	MaxToolArgumentsBytes Optional[int64] `yaml:"max_tool_arguments_bytes"`
}

type PartialUIConfig struct {
	ShowResponseTimer Optional[bool]   `yaml:"show_response_timer"`
	StartMode         Optional[string] `yaml:"start_mode"`
}

type PartialStorageConfig struct {
	DataDir Optional[string] `yaml:"data_dir"`
}

type PartialPermissionConfig struct {
	Mode Optional[string] `yaml:"mode"`
}

type PartialAgentConfig struct {
	MaxIterations       Optional[int64] `yaml:"max_iterations"`
	MaxUnknownToolCalls Optional[int64] `yaml:"max_unknown_tool_calls"`
}

type PartialToolConfig struct {
	TimeoutMS         Optional[int64] `yaml:"timeout_ms"`
	InlineOutputBytes Optional[int64] `yaml:"inline_output_bytes"`
	CaptureBytes      Optional[int64] `yaml:"capture_bytes"`
	MaxOutputBytes    Optional[int64] `yaml:"max_output_bytes"`
}

type PartialArtifactConfig struct {
	Root          Optional[string] `yaml:"root"`
	MaxFileBytes  Optional[int64]  `yaml:"max_file_bytes"`
	MaxTotalBytes Optional[int64]  `yaml:"max_total_bytes"`
	RetentionDays Optional[int64]  `yaml:"retention_days"`
}

type PartialFilesConfig struct {
	ReadMaxBytes       Optional[int64] `yaml:"read_max_bytes"`
	ScanMaxBytes       Optional[int64] `yaml:"scan_max_bytes"`
	ScanMaxFiles       Optional[int64] `yaml:"scan_max_files"`
	ScanMaxDirectories Optional[int64] `yaml:"scan_max_directories"`
	ScanMaxLines       Optional[int64] `yaml:"scan_max_lines"`
}

type PartialInstructionsConfig struct {
	Enabled          Optional[bool]   `yaml:"enabled"`
	ProjectFile      Optional[string] `yaml:"project_file"`
	ProjectDir       Optional[string] `yaml:"project_dir"`
	UserDir          Optional[string] `yaml:"user_dir"`
	MaxIncludeDepth  Optional[int64]  `yaml:"max_include_depth"`
	MaxFileBytes     Optional[int64]  `yaml:"max_file_bytes"`
	MaxTotalBytes    Optional[int64]  `yaml:"max_total_bytes"`
	MaxFiles         Optional[int64]  `yaml:"max_files"`
	MaxExpandedBytes Optional[int64]  `yaml:"max_expanded_bytes"`
}

type PartialMCPConfig struct {
	DefaultTimeoutMS  Optional[int64]                   `yaml:"default_timeout_ms"`
	MaxResponseBytes  Optional[int64]                   `yaml:"max_response_bytes"`
	MaxTools          Optional[int64]                   `yaml:"max_tools"`
	MaxPages          Optional[int64]                   `yaml:"max_pages"`
	MaxProtocolErrors Optional[int64]                   `yaml:"max_protocol_errors"`
	Servers           map[string]PartialMCPServerConfig `yaml:"servers"`
}

type PartialMCPServerConfig struct {
	Disabled  Optional[bool]              `yaml:"disabled"`
	Type      Optional[string]            `yaml:"type"`
	Command   Optional[string]            `yaml:"command"`
	Args      Optional[[]string]          `yaml:"args"`
	Env       Optional[map[string]string] `yaml:"env"`
	URL       Optional[string]            `yaml:"url"`
	Headers   Optional[map[string]string] `yaml:"headers"`
	TimeoutMS Optional[int64]             `yaml:"timeout_ms"`
}

type PartialContextConfig struct {
	Enabled                   Optional[bool]  `yaml:"enabled"`
	ToolResultThresholdChars  Optional[int64] `yaml:"tool_result_threshold_chars"`
	ToolResultsThresholdChars Optional[int64] `yaml:"tool_results_threshold_chars"`
	ModelWindowTokens         Optional[int64] `yaml:"model_window_tokens"`
	AutoMarginTokens          Optional[int64] `yaml:"auto_margin_tokens"`
	ManualMarginTokens        Optional[int64] `yaml:"manual_margin_tokens"`
	RecentKeepTokens          Optional[int64] `yaml:"recent_keep_tokens"`
	RecentKeepMessages        Optional[int64] `yaml:"recent_keep_messages"`
	SummaryFailureLimit       Optional[int64] `yaml:"summary_failure_limit"`
	PreviewChars              Optional[int64] `yaml:"preview_chars"`
}

type PartialSessionConfig struct {
	Dir             Optional[string] `yaml:"dir"`
	MaxRecordBytes  Optional[int64]  `yaml:"max_record_bytes"`
	MaxSessionBytes Optional[int64]  `yaml:"max_session_bytes"`
	MaxScanFiles    Optional[int64]  `yaml:"max_scan_files"`
	MaxScanBytes    Optional[int64]  `yaml:"max_scan_bytes"`
	RetentionDays   Optional[int64]  `yaml:"retention_days"`
	GapReminderDays Optional[int64]  `yaml:"gap_reminder_days"`
}

type PartialMemoryConfig struct {
	Enabled           Optional[bool]   `yaml:"enabled"`
	UserDir           Optional[string] `yaml:"user_dir"`
	ProjectDir        Optional[string] `yaml:"project_dir"`
	MaxIndexLines     Optional[int64]  `yaml:"max_index_lines"`
	MaxIndexBytes     Optional[int64]  `yaml:"max_index_bytes"`
	UpdateQueueSize   Optional[int64]  `yaml:"update_queue_size"`
	UpdateConcurrency Optional[int64]  `yaml:"update_concurrency"`
	UpdateTimeoutMS   Optional[int64]  `yaml:"update_timeout_ms"`
	MaxCandidateBytes Optional[int64]  `yaml:"max_candidate_bytes"`
}

type PartialDiagnosticsConfig struct {
	MaxItems      Optional[int64] `yaml:"max_items"`
	MaxItemBytes  Optional[int64] `yaml:"max_item_bytes"`
	MaxTotalBytes Optional[int64] `yaml:"max_total_bytes"`
}

type PartialLifecycleConfig struct {
	CleanupTimeoutMS Optional[int64] `yaml:"cleanup_timeout_ms"`
}

type PartialSubagentConfig struct {
	RoleLimits                       PartialRoleLimits     `yaml:"role_limits"`
	Worktree                         PartialWorktreeConfig `yaml:"worktree"`
	MaxTaskBytes                     Optional[int64]       `yaml:"max_task_bytes"`
	MaxConcurrent                    Optional[int64]       `yaml:"max_concurrent"`
	MaxQueued                        Optional[int64]       `yaml:"max_queued"`
	MaxRetainedTasks                 Optional[int64]       `yaml:"max_retained_tasks"`
	MaxTaskTombstones                Optional[int64]       `yaml:"max_task_tombstones"`
	MaxGlobalEvents                  Optional[int64]       `yaml:"max_global_events"`
	MaxEventsPerTask                 Optional[int64]       `yaml:"max_events_per_task"`
	MaxEventBytes                    Optional[int64]       `yaml:"max_event_bytes"`
	MaxSubscriberBuffer              Optional[int64]       `yaml:"max_subscriber_buffer"`
	MaxResultBytes                   Optional[int64]       `yaml:"max_result_bytes"`
	MaxPendingResults                Optional[int64]       `yaml:"max_pending_results"`
	MaxResultTotalBytes              Optional[int64]       `yaml:"max_result_total_bytes"`
	MaxResultsPerClaim               Optional[int64]       `yaml:"max_results_per_claim"`
	ReadCacheMaxEntries              Optional[int64]       `yaml:"read_cache_max_entries"`
	ReadCacheMaxBytes                Optional[int64]       `yaml:"read_cache_max_bytes"`
	ReadCacheMaxValueBytes           Optional[int64]       `yaml:"read_cache_max_value_bytes"`
	ReadCacheMaxDependenciesPerEntry Optional[int64]       `yaml:"read_cache_max_dependencies_per_entry"`
	MaxRoleNameBytes                 Optional[int64]       `yaml:"max_role_name_bytes"`
	AutoBackgroundAfterMS            Optional[int64]       `yaml:"auto_background_after_ms"`
	MaxTaskDurationMS                Optional[int64]       `yaml:"max_task_duration_ms"`
	BackgroundTools                  Optional[[]string]    `yaml:"background_tools"`
}

type PartialWorktreeConfig struct {
	Lifecycle PartialWorktreeLifecycleConfig `yaml:"lifecycle"`
	Limits    PartialWorktreeLimits          `yaml:"limits"`
	Init      PartialWorktreeInitConfig      `yaml:"init"`
}

type PartialWorktreeLifecycleConfig struct {
	RetentionTTLMS    Optional[int64] `yaml:"retention_ttl_ms"`
	JanitorIntervalMS Optional[int64] `yaml:"janitor_interval_ms"`
	GitTimeoutMS      Optional[int64] `yaml:"git_timeout_ms"`
	LockTimeoutMS     Optional[int64] `yaml:"lock_timeout_ms"`
	InitTimeoutMS     Optional[int64] `yaml:"init_timeout_ms"`
	RecoveryTimeoutMS Optional[int64] `yaml:"recovery_timeout_ms"`
	SettleTimeoutMS   Optional[int64] `yaml:"settle_timeout_ms"`
	JanitorTimeoutMS  Optional[int64] `yaml:"janitor_timeout_ms"`
}

type PartialWorktreeLimits struct {
	MaxActive             Optional[int64] `yaml:"max_active"`
	MaxRetained           Optional[int64] `yaml:"max_retained"`
	MaxNameBytes          Optional[int64] `yaml:"max_name_bytes"`
	MaxSegmentBytes       Optional[int64] `yaml:"max_segment_bytes"`
	MaxDepth              Optional[int64] `yaml:"max_depth"`
	MaxInitFiles          Optional[int64] `yaml:"max_init_files"`
	MaxInitBytes          Optional[int64] `yaml:"max_init_bytes"`
	MaxInitDepth          Optional[int64] `yaml:"max_init_depth"`
	MaxJanitorCandidates  Optional[int64] `yaml:"max_janitor_candidates"`
	MaxJanitorConcurrency Optional[int64] `yaml:"max_janitor_concurrency"`
}

type PartialWorktreeInitConfig struct {
	Copy        Optional[[]PartialWorktreeCopyRule] `yaml:"copy"`
	Link        Optional[[]PartialWorktreeLinkRule] `yaml:"link"`
	IgnoredCopy Optional[[]PartialWorktreeCopyRule] `yaml:"ignored_copy"`
	GitHooks    PartialWorktreeGitHooksRule         `yaml:"git_hooks"`
}

type PartialWorktreeCopyRule struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

type PartialWorktreeLinkRule struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

type PartialWorktreeGitHooksRule struct {
	Enabled Optional[bool]   `yaml:"enabled"`
	Path    Optional[string] `yaml:"path"`
}

type PartialRoleLimits struct {
	MaxFiles            Optional[int64] `yaml:"max_files"`
	MaxEntryBytes       Optional[int64] `yaml:"max_entry_bytes"`
	MaxFrontmatterBytes Optional[int64] `yaml:"max_frontmatter_bytes"`
	MaxBodyBytes        Optional[int64] `yaml:"max_body_bytes"`
	MaxNameBytes        Optional[int64] `yaml:"max_name_bytes"`
	MaxDescriptionBytes Optional[int64] `yaml:"max_description_bytes"`
	MaxInstructionBytes Optional[int64] `yaml:"max_instruction_bytes"`
	MaxToolNameBytes    Optional[int64] `yaml:"max_tool_name_bytes"`
	MaxToolListBytes    Optional[int64] `yaml:"max_tool_list_bytes"`
	MaxOriginBytes      Optional[int64] `yaml:"max_origin_bytes"`
	MaxSourceIDBytes    Optional[int64] `yaml:"max_source_id_bytes"`
	MaxProviderIDBytes  Optional[int64] `yaml:"max_provider_id_bytes"`
	MaxRootBytes        Optional[int64] `yaml:"max_root_bytes"`
	MaxModelBytes       Optional[int64] `yaml:"max_model_bytes"`
	MaxTotalBytes       Optional[int64] `yaml:"max_total_bytes"`
	MaxToolNames        Optional[int64] `yaml:"max_tool_names"`
	MaxProviders        Optional[int64] `yaml:"max_providers"`
	MaxCandidates       Optional[int64] `yaml:"max_candidates"`
	MaxDiagnostics      Optional[int64] `yaml:"max_diagnostics"`
}
