package mcpclient

import (
	"context"
	"errors"
	"strings"

	"xagent/internal/tool"
)

type ToolCaller interface {
	CallTool(ctx context.Context, registeredName string, arguments map[string]any) (CallToolResult, error)
}

type ToolAdapter struct {
	registeredName string
	serverName     string
	remoteName     string
	description    string
	schema         tool.Schema
	caller         ToolCaller
}

func NewToolAdapter(registeredName string, serverName string, remoteName string, description string, schema tool.Schema, caller ToolCaller) ToolAdapter {
	return ToolAdapter{
		registeredName: registeredName,
		serverName:     serverName,
		remoteName:     remoteName,
		description:    SanitizeMetadata(description, 1024),
		schema:         schema,
		caller:         caller,
	}
}

func (a ToolAdapter) Name() string {
	return a.registeredName
}

func (a ToolAdapter) Description() string {
	if a.description == "" {
		return "MCP tool from " + a.serverName + ": " + a.remoteName
	}
	return a.description
}

func (a ToolAdapter) Schema() tool.Schema {
	return a.schema
}

func (a ToolAdapter) Risk() tool.Risk {
	return tool.RiskDangerous
}

func (a ToolAdapter) Execute(ctx context.Context, input tool.Input) tool.Result {
	arguments := map[string]any{}
	for key, value := range input.Arguments {
		arguments[key] = value
	}
	result, err := a.caller.CallTool(ctx, a.registeredName, arguments)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return tool.Result{CallID: input.CallID, Name: input.Name, Status: tool.StatusTimeout, Summary: "MCP 工具调用超时", Error: &tool.Error{Code: tool.ErrTimeout, Message: "MCP tool call timed out", Recoverable: true}}
		}
		return tool.Failure(input, tool.ErrCommandFailed, "MCP 工具调用失败: "+RedactText(err.Error()), true)
	}
	content, data := convertMCPResult(result)
	if result.IsError {
		return tool.Result{CallID: input.CallID, Name: input.Name, Status: tool.StatusError, Summary: "MCP 工具返回错误", Content: content, Data: data, Error: &tool.Error{Code: tool.ErrCommandFailed, Message: contentOrDefault(content, "MCP tool returned isError=true"), Recoverable: true}}
	}
	return tool.Success(input, "MCP 工具调用完成", content, data)
}

func convertMCPResult(result CallToolResult) (string, map[string]any) {
	textBlocks := []string{}
	nonTextBlocks := []any{}
	for _, block := range result.Content {
		if block.Type == "text" {
			textBlocks = append(textBlocks, RedactText(block.Text))
			continue
		}
		nonTextBlocks = append(nonTextBlocks, map[string]any{"type": block.Type, "raw": RedactText(string(block.Raw))})
	}
	data := map[string]any{}
	if len(nonTextBlocks) > 0 {
		data["content"] = nonTextBlocks
	}
	if result.StructuredContent != nil {
		data["structuredContent"] = RedactAny(result.StructuredContent)
	}
	content := strings.Join(textBlocks, "\n")
	if len(nonTextBlocks) > 0 {
		if content != "" {
			content += "\n"
		}
		content += "[non-text MCP content omitted]"
	}
	return content, data
}

func contentOrDefault(content string, fallback string) string {
	if strings.TrimSpace(content) == "" {
		return fallback
	}
	return content
}
