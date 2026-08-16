package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"xagent/internal/config"
	"xagent/internal/prompt"
	"xagent/internal/redact"
	"xagent/internal/tool"
)

type ModelMessageRole string

const (
	ModelMessageRoleUser            ModelMessageRole = "user"
	ModelMessageRoleAssistant       ModelMessageRole = "assistant"
	ModelMessageRoleToolCall        ModelMessageRole = "tool_call"
	ModelMessageRoleToolResult      ModelMessageRole = "tool_result"
	ModelMessageRoleContextSummary  ModelMessageRole = "context_summary"
	ModelMessageRoleContextBoundary ModelMessageRole = "context_boundary"
	ModelMessageRoleSubagentResult  ModelMessageRole = "subagent_result"
)

const SubagentResultMarker = "[subagent_result]"

var ErrInvalidChatRequest = errors.New("provider chat request invalid")

// ModelMessage is the Provider-owned request DTO. All message payloads have
// already crossed the redaction boundary; string fields are stable metadata.
type ModelMessage struct {
	Role             ModelMessageRole
	Content          redact.SafeText
	ToolCallID       string
	ToolName         string
	ArgumentsJSON    redact.SafeText
	ToolResult       redact.SafeText
	ToolResultStatus string
}

type ChatRequest struct {
	Model         string
	System        []SystemBlock
	StableSystem  []SystemBlock
	DynamicSystem []SystemBlock
	Messages      []ModelMessage
	Thinking      config.ThinkingConfig
	Tools         []ToolDefinition
	ToolDefs      *tool.Registry
	Cache         CachePolicy
	Observer      RequestObserver
}

type SystemBlock struct {
	Name      string
	Content   redact.SafeText
	Cacheable bool
	Scope     prompt.Scope
}

type ToolDefinition struct {
	Name        string
	Description string
	Schema      tool.Schema
}

type CachePolicy struct {
	EnablePromptCache    bool
	SystemBreakpointName string
	CacheTools           bool
}

// Validate is the shared fail-closed request boundary used before any wire
// adapter. It prevents adapters from silently dropping unknown internal roles
// and enforces the mutually exclusive ordered/split system representations.
func (request ChatRequest) Validate() error {
	if request.System != nil && (request.StableSystem != nil || request.DynamicSystem != nil) {
		return invalidChatRequest("system layout is ambiguous")
	}
	if !utf8.ValidString(request.Model) || request.Thinking.BudgetTokens < 0 ||
		!utf8.ValidString(request.Cache.SystemBreakpointName) {
		return invalidChatRequest("request metadata is invalid")
	}
	for _, blocks := range [][]SystemBlock{request.System, request.StableSystem, request.DynamicSystem} {
		for _, block := range blocks {
			if !utf8.ValidString(block.Name) || !utf8.ValidString(block.Content.Text()) {
				return invalidChatRequest("system block is invalid")
			}
		}
	}
	for index := range request.Messages {
		if err := validateModelMessage(request.Messages[index]); err != nil {
			return fmt.Errorf("%w: message %d", err, index)
		}
	}
	for index, definition := range toolDefinitions(request) {
		if strings.TrimSpace(definition.Name) == "" || !utf8.ValidString(definition.Name) ||
			!utf8.ValidString(definition.Description) {
			return invalidChatRequest(fmt.Sprintf("tool %d metadata is invalid", index))
		}
		if _, err := json.Marshal(definition.Schema); err != nil {
			return invalidChatRequest(fmt.Sprintf("tool %d schema is invalid", index))
		}
	}
	return nil
}

func invalidChatRequest(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidChatRequest, reason)
}

func validateModelMessage(message ModelMessage) error {
	if !utf8.ValidString(message.Content.Text()) || !utf8.ValidString(message.ToolCallID) ||
		!utf8.ValidString(message.ToolName) || !utf8.ValidString(message.ArgumentsJSON.Text()) ||
		!utf8.ValidString(message.ToolResult.Text()) || !utf8.ValidString(message.ToolResultStatus) {
		return invalidChatRequest("message text is invalid")
	}
	toolFieldsEmpty := message.ToolCallID == "" && message.ToolName == "" && message.ArgumentsJSON.Text() == "" &&
		message.ToolResult.Text() == "" && message.ToolResultStatus == ""
	switch message.Role {
	case ModelMessageRoleUser, ModelMessageRoleAssistant, ModelMessageRoleContextSummary, ModelMessageRoleContextBoundary:
		if !toolFieldsEmpty {
			return invalidChatRequest("base message contains tool fields")
		}
		return nil
	case ModelMessageRoleSubagentResult:
		if !toolFieldsEmpty || !validSubagentResultJSON(message.Content.Text()) {
			return invalidChatRequest("subagent result is invalid")
		}
		return nil
	case ModelMessageRoleToolCall:
		if strings.TrimSpace(message.ToolCallID) == "" || strings.TrimSpace(message.ToolName) == "" ||
			message.ToolResult.Text() != "" || message.ToolResultStatus != "" {
			return invalidChatRequest("tool call is invalid")
		}
		arguments := strings.TrimSpace(message.ArgumentsJSON.Text())
		if arguments != "" && !json.Valid([]byte(arguments)) {
			return invalidChatRequest("tool arguments are invalid")
		}
		return nil
	case ModelMessageRoleToolResult:
		if strings.TrimSpace(message.ToolCallID) == "" || strings.TrimSpace(message.ToolName) == "" ||
			message.ArgumentsJSON.Text() != "" || message.ToolResult.Text() == "" || !validToolResultStatus(message.ToolResultStatus) {
			return invalidChatRequest("tool result is invalid")
		}
		return nil
	default:
		return invalidChatRequest("message role is unknown")
	}
}

func validToolResultStatus(status string) bool {
	switch tool.ResultStatus(status) {
	case tool.StatusSuccess, tool.StatusError, tool.StatusDenied, tool.StatusTimeout:
		return true
	default:
		return false
	}
}

func validSubagentResultJSON(payload string) bool {
	if payload == "" || len(payload) > 64<<10 || !utf8.ValidString(payload) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewBufferString(payload))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var value subagentResultPayload
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false
	}
	if value.SchemaVersion != 1 || !validSubagentResultID(value.TaskID) ||
		len(value.Summary) > 64<<10 || !utf8.ValidString(value.Summary) ||
		len(value.TruncationReason) > 256 || !utf8.ValidString(value.TruncationReason) ||
		value.SummaryTruncated != (value.TruncationReason != "") ||
		!validSubagentResultTerminalPair(value.Status, value.StopReason) || value.Usage == nil || !value.Usage.valid() {
		return false
	}
	if value.Error != nil && !value.Error.valid() {
		return false
	}
	return len(value.Workspace) == 0 || validSubagentResultWorkspace(value.Workspace)
}

type subagentResultPayload struct {
	SchemaVersion    int                         `json:"schema_version"`
	TaskID           string                      `json:"task_id"`
	Status           string                      `json:"status"`
	Summary          string                      `json:"summary"`
	SummaryTruncated bool                        `json:"summary_truncated"`
	TruncationReason string                      `json:"truncation_reason"`
	StopReason       string                      `json:"stop_reason"`
	Usage            *subagentResultUsagePayload `json:"usage"`
	Error            *subagentResultErrorPayload `json:"error"`
	Workspace        json.RawMessage             `json:"workspace"`
}

type subagentResultWorkspacePayload struct {
	WorkspaceID    string `json:"workspace_id"`
	Isolation      string `json:"isolation"`
	State          string `json:"state"`
	BaseOID        string `json:"base_oid"`
	Branch         string `json:"branch"`
	Dirty          *bool  `json:"dirty"`
	Unpushed       *bool  `json:"unpushed"`
	Cleanup        string `json:"cleanup"`
	RetentionCause string `json:"retention_cause,omitempty"`
}

func validSubagentResultWorkspace(raw json.RawMessage) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var workspace subagentResultWorkspacePayload
	if err := decoder.Decode(&workspace); err != nil {
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false
	}
	return workspace.Isolation == "worktree" && validSubagentLowerHex(workspace.WorkspaceID, 32) &&
		validSubagentGitOID(workspace.BaseOID) && workspace.Branch == "xagent/worktree/"+workspace.WorkspaceID &&
		validSubagentWorkspaceTerminal(workspace.State) && validSubagentWorkspaceTerminal(workspace.Cleanup) &&
		workspace.Dirty != nil && workspace.Unpushed != nil &&
		(workspace.RetentionCause == "" || validSubagentWorkspaceReason(workspace.RetentionCause))
}

func validSubagentGitOID(value string) bool {
	return validSubagentLowerHex(value, 40) || validSubagentLowerHex(value, 64)
}

func validSubagentLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validSubagentWorkspaceTerminal(value string) bool {
	switch value {
	case "deleted", "retained", "partial", "manual_attention":
		return true
	default:
		return false
	}
}

func validSubagentWorkspaceReason(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		if index > 0 && (character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}

type subagentResultUsagePayload struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (usage subagentResultUsagePayload) valid() bool {
	return usage.InputTokens >= 0 && usage.OutputTokens >= 0 && usage.CacheCreationInputTokens >= 0 && usage.CacheReadInputTokens >= 0
}

type subagentResultErrorPayload struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable,omitempty"`
}

func (safeError subagentResultErrorPayload) valid() bool {
	return validSubagentResultID(safeError.Code) && len(safeError.Message) <= 64<<10 && utf8.ValidString(safeError.Message)
}

func validSubagentResultID(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validSubagentResultTerminalPair(status, stopReason string) bool {
	switch status {
	case "completed":
		return stopReason == "completed"
	case "failed":
		return stopReason == "provider_error" || stopReason == "tool_error" || stopReason == "internal_error"
	case "cancelled":
		return stopReason == "cancelled" || stopReason == "application_closed"
	case "timed_out":
		return stopReason == "task_timeout"
	case "limit_reached":
		return stopReason == "max_iterations" || stopReason == "unknown_tool_limit"
	default:
		return false
	}
}

func systemBlocks(req ChatRequest) []SystemBlock {
	if req.System != nil {
		return nonEmptySystemBlocks(req.System)
	}
	blocks := make([]SystemBlock, 0, len(req.StableSystem)+len(req.DynamicSystem))
	blocks = append(blocks, nonEmptySystemBlocks(req.StableSystem)...)
	blocks = append(blocks, nonEmptySystemBlocks(req.DynamicSystem)...)
	return blocks
}

func usesOrderedSystem(req ChatRequest) bool {
	return req.System != nil
}

func joinedSystemBlocks(req ChatRequest) string {
	blocks := systemBlocks(req)
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.Content.Text())
	}
	return strings.Join(parts, "\n\n")
}

func nonEmptySystemBlocks(blocks []SystemBlock) []SystemBlock {
	result := make([]SystemBlock, 0, len(blocks))
	for _, block := range blocks {
		block.Name = strings.TrimSpace(block.Name)
		if strings.TrimSpace(block.Content.Text()) == "" {
			continue
		}
		result = append(result, block)
	}
	return result
}

func toolDefinitions(req ChatRequest) []ToolDefinition {
	if req.Tools != nil {
		return nonEmptyToolDefinitions(req.Tools)
	}
	if req.ToolDefs == nil {
		return nil
	}
	definitions := req.ToolDefs.OpenAIDefinitions()
	tools := make([]ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, ToolDefinition{Name: definition.Function.Name, Description: definition.Function.Description, Schema: definition.Function.Parameters})
	}
	return nonEmptyToolDefinitions(tools)
}

func nonEmptyToolDefinitions(definitions []ToolDefinition) []ToolDefinition {
	result := make([]ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		definition.Name = strings.TrimSpace(definition.Name)
		definition.Description = strings.TrimSpace(definition.Description)
		if definition.Name == "" {
			continue
		}
		result = append(result, definition)
	}
	return result
}

func requestModel(override string, fallback string) string {
	if model := strings.TrimSpace(override); model != "" {
		return model
	}
	return fallback
}
