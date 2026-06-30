package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/tool"
)

type AnthropicProvider struct {
	cfg    config.LLMConfig
	client anthropic.Client
}

func NewAnthropic(cfg config.LLMConfig, _ *http.Client) *AnthropicProvider {
	options := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if strings.TrimRight(cfg.BaseURL, "/") != "https://api.anthropic.com" {
		options = append(options, option.WithBaseURL(strings.TrimRight(cfg.BaseURL, "/")))
	}
	return &AnthropicProvider{cfg: cfg, client: anthropic.NewClient(options...)}
}

func (p *AnthropicProvider) Name() string {
	return "Anthropic Claude"
}

func (p *AnthropicProvider) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	out := make(chan StreamEvent)
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(reqModel(p.cfg.Model)),
		MaxTokens: 64000,
		Messages:  toAnthropicMessages(req.Messages),
		Tools:     toAnthropicTools(req.ToolDefs),
	}
	if req.SystemPrompt != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.SystemPrompt}}
	}
	if req.Thinking.Enabled {
		adaptive := anthropic.ThinkingConfigAdaptiveParam{}
		if req.Thinking.Show {
			adaptive.Display = anthropic.ThinkingConfigAdaptiveDisplaySummarized
		}
		params.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive}
	}

	go func() {
		defer close(out)
		stream := p.client.Messages.NewStreaming(ctx, params)
		toolCalls := map[int64]*anthropicToolCallState{}
		for stream.Next() {
			event := stream.Current()
			switch value := event.AsAny().(type) {
			case anthropic.ContentBlockStartEvent:
				if block, ok := value.ContentBlock.AsAny().(anthropic.ToolUseBlock); ok {
					toolCalls[value.Index] = &anthropicToolCallState{ID: block.ID, Name: block.Name}
				}
			case anthropic.ContentBlockDeltaEvent:
				switch delta := value.Delta.AsAny().(type) {
				case anthropic.TextDelta:
					out <- StreamEvent{Type: StreamEventTextDelta, Delta: delta.Text}
				case anthropic.ThinkingDelta:
					out <- StreamEvent{Type: StreamEventThinkingDelta, Delta: delta.Thinking}
				case anthropic.InputJSONDelta:
					if state := toolCalls[value.Index]; state != nil {
						state.Arguments.WriteString(delta.PartialJSON)
					}
				}
			case anthropic.MessageStopEvent:
				if len(toolCalls) > 1 {
					out <- StreamEvent{Type: StreamEventError, Err: fmt.Errorf("%s", tool.ErrMultipleToolCallsUnsupported)}
					return
				}
				for _, call := range toolCalls {
					out <- StreamEvent{Type: StreamEventToolCall, ToolCall: &tool.Call{ID: call.ID, Name: call.Name, ArgumentsJSON: call.Arguments.String()}}
					return
				}
				out <- StreamEvent{Type: StreamEventDone}
				return
			}
		}
		if err := stream.Err(); err != nil {
			out <- StreamEvent{Type: StreamEventError, Err: err}
		}
	}()
	return out, nil
}

type anthropicToolCallState struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

func reqModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return "claude-opus-4-7"
	}
	return model
}

func toAnthropicMessages(messages []conversation.Message) []anthropic.MessageParam {
	result := make([]anthropic.MessageParam, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case conversation.RoleUser:
			result = append(result, anthropic.NewUserMessage(anthropic.NewTextBlock(message.Content)))
		case conversation.RoleAssistant:
			result = append(result, anthropic.NewAssistantMessage(anthropic.NewTextBlock(message.Content)))
		case conversation.RoleToolCall:
			var input any = map[string]any{}
			if strings.TrimSpace(message.RawToolArguments) != "" {
				_ = json.Unmarshal([]byte(message.RawToolArguments), &input)
			}
			result = append(result, anthropic.NewAssistantMessage(anthropic.NewToolUseBlock(message.ToolCallID, input, message.ToolName)))
		case conversation.RoleToolResult:
			isError := message.ToolResultStatus != string(tool.StatusSuccess)
			result = append(result, anthropic.NewUserMessage(anthropic.NewToolResultBlock(message.ToolCallID, message.ToolResultContent, isError)))
		}
	}
	return result
}

func toAnthropicTools(registry *tool.Registry) []anthropic.ToolUnionParam {
	if registry == nil {
		return nil
	}
	definitions := registry.AnthropicDefinitions()
	tools := make([]anthropic.ToolUnionParam, 0, len(definitions))
	for _, definition := range definitions {
		param := anthropic.ToolParam{
			Name:        definition.Name,
			Description: anthropic.String(definition.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: definition.InputSchema.Properties,
				Required:   definition.InputSchema.Required,
			},
		}
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &param})
	}
	return tools
}
