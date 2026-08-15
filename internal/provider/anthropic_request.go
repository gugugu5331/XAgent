package provider

import (
	"encoding/json"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	"xagent/internal/tool"
)

func anthropicMessageParams(defaultModel string, req ChatRequest) (anthropic.MessageNewParams, error) {
	if err := req.Validate(); err != nil {
		return anthropic.MessageNewParams{}, err
	}
	messages, err := toAnthropicMessages(req.Messages)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(reqModel(requestModel(req.Model, defaultModel))),
		MaxTokens: 64000,
		Messages:  messages,
		Tools:     toAnthropicTools(req),
		System:    toAnthropicSystemBlocks(req),
	}
	if req.Thinking.Enabled {
		adaptive := anthropic.ThinkingConfigAdaptiveParam{}
		if req.Thinking.Show {
			adaptive.Display = anthropic.ThinkingConfigAdaptiveDisplaySummarized
		}
		params.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive}
	}

	return params, nil
}

func reqModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return "claude-opus-4-7"
	}
	return model
}

func toAnthropicMessages(messages []ModelMessage) ([]anthropic.MessageParam, error) {
	result := make([]anthropic.MessageParam, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case ModelMessageRoleUser, ModelMessageRoleContextSummary, ModelMessageRoleContextBoundary:
			result = append(result, anthropic.NewUserMessage(anthropic.NewTextBlock(message.Content.Text())))
		case ModelMessageRoleAssistant:
			result = append(result, anthropic.NewAssistantMessage(anthropic.NewTextBlock(message.Content.Text())))
		case ModelMessageRoleToolCall:
			var input any = map[string]any{}
			if strings.TrimSpace(message.ArgumentsJSON.Text()) != "" {
				_ = json.Unmarshal([]byte(message.ArgumentsJSON.Text()), &input)
			}
			result = append(result, anthropic.NewAssistantMessage(anthropic.NewToolUseBlock(message.ToolCallID, input, message.ToolName)))
		case ModelMessageRoleToolResult:
			isError := message.ToolResultStatus != string(tool.StatusSuccess)
			result = append(result, anthropic.NewUserMessage(anthropic.NewToolResultBlock(message.ToolCallID, message.ToolResult.Text(), isError)))
		case ModelMessageRoleSubagentResult:
			result = append(result, anthropic.NewUserMessage(anthropic.NewTextBlock(markedSubagentResult(message.Content.Text()))))
		default:
			return nil, ErrInvalidChatRequest
		}
	}
	return result, nil
}

func toAnthropicSystemBlocks(req ChatRequest) []anthropic.TextBlockParam {
	blocks := systemBlocks(req)
	if len(blocks) == 0 {
		return nil
	}
	params := make([]anthropic.TextBlockParam, 0, len(blocks))
	breakpoint := -1
	targetName := strings.TrimSpace(req.Cache.SystemBreakpointName)
	for _, block := range blocks {
		param := anthropic.TextBlockParam{Text: block.Content.Text()}
		if usesOrderedSystem(req) {
			if breakpoint < 0 && targetName != "" && block.Cacheable && block.Name == targetName {
				breakpoint = len(params)
			}
		} else if block.Cacheable {
			breakpoint = len(params)
		}
		params = append(params, param)
	}
	if req.Cache.EnablePromptCache && breakpoint >= 0 {
		params[breakpoint].CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	return params
}

func toAnthropicTools(req ChatRequest) []anthropic.ToolUnionParam {
	definitions := toolDefinitions(req)
	if len(definitions) == 0 {
		return nil
	}
	tools := make([]anthropic.ToolUnionParam, 0, len(definitions))
	for _, definition := range definitions {
		param := anthropic.ToolParam{
			Name:        definition.Name,
			Description: anthropic.String(definition.Description),
			InputSchema: toAnthropicInputSchema(definition.Schema),
		}
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &param})
	}
	if req.Cache.EnablePromptCache && req.Cache.CacheTools {
		last := len(tools) - 1
		tools[last].OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	return tools
}

func toAnthropicInputSchema(schema tool.Schema) anthropic.ToolInputSchemaParam {
	if len(schema.Raw) == 0 {
		return anthropic.ToolInputSchemaParam{
			Properties: schema.Properties,
			Required:   schema.Required,
		}
	}

	var raw map[string]any
	if err := json.Unmarshal(schema.Raw, &raw); err != nil {
		return anthropic.ToolInputSchemaParam{
			Properties: schema.Properties,
			Required:   schema.Required,
		}
	}

	param := anthropic.ToolInputSchemaParam{ExtraFields: map[string]any{}}
	for key, value := range raw {
		switch key {
		case "properties":
			param.Properties = value
		case "required":
			if required, ok := value.([]any); ok {
				param.Required = make([]string, 0, len(required))
				for _, item := range required {
					if name, ok := item.(string); ok {
						param.Required = append(param.Required, name)
					}
				}
			}
		case "type":
			if value == "object" {
				param.Type = constant.Object("object")
			}
		default:
			param.ExtraFields[key] = value
		}
	}
	if len(param.ExtraFields) == 0 {
		param.ExtraFields = nil
	}
	return param
}
