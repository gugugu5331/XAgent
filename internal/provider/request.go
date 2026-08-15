package provider

import (
	"strings"

	"xagent/internal/config"
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
)

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

func systemBlocks(req ChatRequest) []SystemBlock {
	if len(req.System) > 0 {
		return nonEmptySystemBlocks(req.System)
	}
	blocks := make([]SystemBlock, 0, len(req.StableSystem)+len(req.DynamicSystem))
	blocks = append(blocks, nonEmptySystemBlocks(req.StableSystem)...)
	blocks = append(blocks, nonEmptySystemBlocks(req.DynamicSystem)...)
	return blocks
}

func usesOrderedSystem(req ChatRequest) bool {
	return len(req.System) > 0
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
	if len(req.Tools) > 0 {
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
