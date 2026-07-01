package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
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
			case anthropic.MessageDeltaEvent:
				out <- StreamEvent{Type: StreamEventUsage, Usage: &Usage{
					InputTokens:              value.Usage.InputTokens,
					OutputTokens:             value.Usage.OutputTokens,
					CacheCreationInputTokens: value.Usage.CacheCreationInputTokens,
					CacheReadInputTokens:     value.Usage.CacheReadInputTokens,
				}}
			case anthropic.MessageStopEvent:
				if len(toolCalls) > 0 {
					out <- newToolCallsEvent(anthropicToolCalls(toolCalls))
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

func anthropicToolCalls(calls map[int64]*anthropicToolCallState) []tool.Call {
	indexes := make([]int64, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })

	toolCalls := make([]tool.Call, 0, len(indexes))
	for _, index := range indexes {
		call := calls[index]
		if call == nil {
			continue
		}
		toolCalls = append(toolCalls, tool.Call{ID: call.ID, Name: call.Name, ArgumentsJSON: call.Arguments.String()})
	}
	return toolCalls
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

func toAnthropicSystemBlocks(req ChatRequest) []anthropic.TextBlockParam {
	blocks := systemBlocks(req)
	if len(blocks) == 0 {
		return nil
	}
	params := make([]anthropic.TextBlockParam, 0, len(blocks))
	lastCacheable := -1
	for _, block := range blocks {
		param := anthropic.TextBlockParam{Text: block.Content}
		if block.Cacheable {
			lastCacheable = len(params)
		}
		params = append(params, param)
	}
	if req.Cache.EnablePromptCache && lastCacheable >= 0 {
		params[lastCacheable].CacheControl = anthropic.NewCacheControlEphemeralParam()
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
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: definition.Schema.Properties,
				Required:   definition.Schema.Required,
			},
		}
		tools = append(tools, anthropic.ToolUnionParam{OfTool: &param})
	}
	if req.Cache.EnablePromptCache {
		last := len(tools) - 1
		tools[last].OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	return tools
}
