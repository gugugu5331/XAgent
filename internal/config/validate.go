package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	ProtocolAnthropic    = "anthropic"
	ProtocolOpenAI       = "openai"
	StartModeList        = "list"
	StartModeNew         = "new"
	PermissionStrict     = "strict"
	PermissionDefault    = "default"
	PermissionPermissive = "permissive"
)

func Validate(config *AppConfig) error {
	if config == nil {
		return errors.New("配置为空")
	}

	protocol := strings.TrimSpace(config.LLM.Protocol)
	if protocol != ProtocolAnthropic && protocol != ProtocolOpenAI {
		return fmt.Errorf("llm.protocol 必须是 %q 或 %q；请在 config.yaml 的 llm.protocol 中配置 provider 协议", ProtocolAnthropic, ProtocolOpenAI)
	}
	config.LLM.Protocol = protocol

	config.LLM.Model = strings.TrimSpace(config.LLM.Model)
	if config.LLM.Model == "" {
		return errors.New("llm.model 不能为空；请配置要使用的模型名")
	}

	config.LLM.BaseURL = strings.TrimSpace(config.LLM.BaseURL)
	if config.LLM.BaseURL == "" {
		return errors.New("llm.base_url 不能为空；请配置 provider API 地址")
	}

	config.LLM.APIKey = strings.TrimSpace(config.LLM.APIKey)
	if config.LLM.APIKey == "" {
		return errors.New("llm.api_key 不能为空；建议使用 ${XAGENT_TEST_API_KEY} 或对应 provider 的环境变量")
	}

	config.UI.StartMode = strings.TrimSpace(config.UI.StartMode)
	if config.UI.StartMode != StartModeList && config.UI.StartMode != StartModeNew {
		return fmt.Errorf("ui.start_mode 必须是 %q 或 %q", StartModeList, StartModeNew)
	}

	if config.LLM.Thinking.BudgetTokens <= 0 {
		config.LLM.Thinking.BudgetTokens = DefaultThinkingBudget
	}
	if config.LLM.RequestTimeoutMS == 0 {
		config.LLM.RequestTimeoutMS = DefaultLLMRequestTimeoutMS
	}
	if config.LLM.RequestTimeoutMS < 0 {
		return errors.New("llm.request_timeout_ms 必须大于 0")
	}
	applyAgentDefaults(&config.Agent)
	if err := validateAgent(&config.Agent); err != nil {
		return err
	}
	applyToolDefaults(&config.Tool)
	if err := validateTool(&config.Tool); err != nil {
		return err
	}

	config.Permission.Mode = strings.TrimSpace(config.Permission.Mode)
	if config.Permission.Mode == "" {
		config.Permission.Mode = PermissionDefault
	}
	if config.Permission.Mode != PermissionStrict && config.Permission.Mode != PermissionDefault && config.Permission.Mode != PermissionPermissive {
		return fmt.Errorf("permission.mode 必须是 %q、%q 或 %q", PermissionStrict, PermissionDefault, PermissionPermissive)
	}

	if err := validateMCP(&config.MCP); err != nil {
		return err
	}
	applyContextDefaults(&config.Context)
	if err := validateContext(&config.Context); err != nil {
		return err
	}
	applyInstructionsDefaults(&config.Instructions)
	if err := validateInstructions(&config.Instructions); err != nil {
		return err
	}
	applySessionDefaults(&config.Session)
	if err := validateSession(&config.Session); err != nil {
		return err
	}
	applyMemoryDefaults(&config.Memory)
	if err := validateMemory(&config.Memory); err != nil {
		return err
	}

	return nil
}

func validateAgent(agent *AgentConfig) error {
	if agent.MaxIterations <= 0 {
		return errors.New("agent.max_iterations 必须大于 0")
	}
	if agent.MaxUnknownToolCalls <= 0 {
		return errors.New("agent.max_unknown_tool_calls 必须大于 0")
	}
	return nil
}

func validateTool(tool *ToolConfig) error {
	if tool.TimeoutMS <= 0 {
		return errors.New("tool.timeout_ms 必须大于 0")
	}
	if tool.MaxOutputBytes <= 0 {
		return errors.New("tool.max_output_bytes 必须大于 0")
	}
	return nil
}

func validateContext(context *ContextConfig) error {
	if context.ToolResultThresholdChars <= 0 {
		return errors.New("context.tool_result_threshold_chars 必须大于 0")
	}
	if context.ToolResultsThresholdChars <= 0 {
		return errors.New("context.tool_results_threshold_chars 必须大于 0")
	}
	if context.ModelWindowTokens <= 0 {
		return errors.New("context.model_window_tokens 必须大于 0")
	}
	if context.AutoMarginTokens <= 0 {
		return errors.New("context.auto_margin_tokens 必须大于 0")
	}
	if context.ManualMarginTokens <= 0 {
		return errors.New("context.manual_margin_tokens 必须大于 0")
	}
	if context.RecentKeepTokens <= 0 {
		return errors.New("context.recent_keep_tokens 必须大于 0")
	}
	if context.RecentKeepMessages <= 0 {
		return errors.New("context.recent_keep_messages 必须大于 0")
	}
	if context.SummaryFailureLimit <= 0 {
		return errors.New("context.summary_failure_limit 必须大于 0")
	}
	if context.PreviewChars <= 0 {
		return errors.New("context.preview_chars 必须大于 0")
	}
	if context.AutoMarginTokens >= context.ModelWindowTokens {
		return errors.New("context.auto_margin_tokens 必须小于 context.model_window_tokens")
	}
	if context.ManualMarginTokens >= context.ModelWindowTokens {
		return errors.New("context.manual_margin_tokens 必须小于 context.model_window_tokens")
	}
	return nil
}

func validateInstructions(instructions *InstructionsConfig) error {
	instructions.ProjectFile = strings.TrimSpace(instructions.ProjectFile)
	instructions.ProjectDir = strings.TrimSpace(instructions.ProjectDir)
	instructions.UserDir = strings.TrimSpace(instructions.UserDir)
	if instructions.ProjectFile == "" {
		return errors.New("instructions.project_file 不能为空")
	}
	if instructions.ProjectDir == "" {
		return errors.New("instructions.project_dir 不能为空")
	}
	if instructions.UserDir == "" {
		return errors.New("instructions.user_dir 不能为空")
	}
	if instructions.MaxIncludeDepth <= 0 {
		return errors.New("instructions.max_include_depth 必须大于 0")
	}
	if instructions.MaxFileBytes <= 0 {
		return errors.New("instructions.max_file_bytes 必须大于 0")
	}
	return nil
}

func validateSession(session *SessionConfig) error {
	session.Dir = strings.TrimSpace(session.Dir)
	if session.Dir == "" {
		return errors.New("session.dir 不能为空")
	}
	if session.RetentionDays <= 0 {
		return errors.New("session.retention_days 必须大于 0")
	}
	if session.MaxScanFiles <= 0 {
		return errors.New("session.max_scan_files 必须大于 0")
	}
	if session.MaxScanBytes <= 0 {
		return errors.New("session.max_scan_bytes 必须大于 0")
	}
	if session.GapReminderDays <= 0 {
		return errors.New("session.gap_reminder_days 必须大于 0")
	}
	return nil
}

func validateMemory(memory *MemoryConfig) error {
	memory.UserDir = strings.TrimSpace(memory.UserDir)
	memory.ProjectDir = strings.TrimSpace(memory.ProjectDir)
	if memory.UserDir == "" {
		return errors.New("memory.user_dir 不能为空")
	}
	if memory.ProjectDir == "" {
		return errors.New("memory.project_dir 不能为空")
	}
	if memory.MaxIndexLines <= 0 {
		return errors.New("memory.max_index_lines 必须大于 0")
	}
	if memory.MaxIndexBytes <= 0 {
		return errors.New("memory.max_index_bytes 必须大于 0")
	}
	if memory.UpdateQueueSize <= 0 {
		return errors.New("memory.update_queue_size 必须大于 0")
	}
	if memory.UpdateConcurrency <= 0 {
		return errors.New("memory.update_concurrency 必须大于 0")
	}
	if memory.UpdateTimeoutMS <= 0 {
		return errors.New("memory.update_timeout_ms 必须大于 0")
	}
	if memory.MaxCandidateBytes <= 0 {
		return errors.New("memory.max_candidate_bytes 必须大于 0")
	}
	return nil
}

func validateMCP(mcp *MCPConfig) error {
	if mcp.DefaultTimeoutMS < 0 {
		return errors.New("mcp.default_timeout_ms 不能为负数")
	}
	if mcp.Servers == nil {
		return nil
	}
	valid := map[string]MCPServerConfig{}
	for name, server := range mcp.Servers {
		serverName := strings.TrimSpace(name)
		if serverName == "" {
			mcp.addDiagnostic(name, server.Source, "server 名不能为空")
			continue
		}
		server.Source = sourceOrDefault(server.Source, "project")
		if server.Disabled {
			valid[serverName] = server
			continue
		}
		if err := validateMCPServer(serverName, &server); err != nil {
			mcp.addDiagnostic(serverName, server.Source, "%v", err)
			continue
		}
		valid[serverName] = server
	}
	mcp.Servers = valid
	return nil
}

func validateMCPServer(name string, server *MCPServerConfig) error {
	server.Type = strings.TrimSpace(server.Type)
	if server.TimeoutMS < 0 {
		return fmt.Errorf("server %s timeout_ms 不能为负数", name)
	}
	switch server.Type {
	case MCPTransportStdio:
		command, err := expandConfigValue(server.Command)
		if err != nil {
			return fmt.Errorf("server %s command: %w", name, err)
		}
		command = strings.TrimSpace(command)
		if command == "" {
			return fmt.Errorf("server %s stdio command 不能为空", name)
		}
		server.Command = command
		for index, arg := range server.Args {
			expanded, err := expandConfigValue(arg)
			if err != nil {
				return fmt.Errorf("server %s args[%d]: %w", name, index, err)
			}
			server.Args[index] = expanded
		}
		if err := expandMapValues(server.Env, true); err != nil {
			return fmt.Errorf("server %s env: %w", name, err)
		}
	case MCPTransportHTTP:
		url, err := expandConfigValue(server.URL)
		if err != nil {
			return fmt.Errorf("server %s url: %w", name, err)
		}
		url = strings.TrimSpace(url)
		if url == "" {
			return fmt.Errorf("server %s http url 不能为空", name)
		}
		server.URL = url
		if err := expandMapValues(server.Headers, true); err != nil {
			return fmt.Errorf("server %s headers: %w", name, err)
		}
	case "":
		return fmt.Errorf("server %s type 不能为空", name)
	default:
		return fmt.Errorf("server %s type 必须是 %q 或 %q", name, MCPTransportStdio, MCPTransportHTTP)
	}
	return nil
}

func expandMapValues(values map[string]string, redactValue bool) error {
	for key, value := range values {
		expanded, err := expandConfigValue(value)
		if err != nil {
			if redactValue || isSensitiveKey(key) {
				return fmt.Errorf("%s: %w", key, err)
			}
			return fmt.Errorf("%s=%q: %w", key, value, err)
		}
		values[key] = expanded
	}
	return nil
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandConfigValue(value string) (string, error) {
	placeholder := "\x00XAGENT_LITERAL_ENV\x00"
	value = strings.ReplaceAll(value, `$${`, placeholder+`{`)
	var missing string
	expanded := envPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := envPattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		if env, ok := os.LookupEnv(parts[1]); ok {
			return env
		}
		missing = parts[1]
		return ""
	})
	if missing != "" {
		return "", fmt.Errorf("环境变量 %s 未定义", missing)
	}
	return strings.ReplaceAll(expanded, placeholder+`{`, `${`), nil
}

func isSensitiveKey(key string) bool {
	key = strings.ToLower(key)
	for _, marker := range []string{"token", "key", "secret", "password", "authorization", "credential"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}
