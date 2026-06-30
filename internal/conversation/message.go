package conversation

import (
	"encoding/json"
	"time"
)

type MessageRole string

const (
	RoleUser       MessageRole = "user"
	RoleAssistant  MessageRole = "assistant"
	RoleThinking   MessageRole = "thinking"
	RoleToolCall   MessageRole = "tool_call"
	RoleToolResult MessageRole = "tool_result"
)

type Message struct {
	Role                MessageRole     `json:"role"`
	Content             string          `json:"content"`
	CreatedAt           time.Time       `json:"created_at"`
	ToolCallID          string          `json:"tool_call_id,omitempty"`
	ToolName            string          `json:"tool_name,omitempty"`
	RawToolArguments    string          `json:"raw_tool_arguments,omitempty"`
	ToolResultContent   string          `json:"tool_result_content,omitempty"`
	ToolResultStatus    string          `json:"tool_result_status,omitempty"`
	ToolResultSummary   string          `json:"tool_result_summary,omitempty"`
	ToolResultTruncated bool            `json:"tool_result_truncated,omitempty"`
	ToolResultData      json.RawMessage `json:"tool_result_data,omitempty"`
	ToolErrorCode       string          `json:"tool_error_code,omitempty"`
}
