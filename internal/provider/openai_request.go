package provider

import "xagent/internal/tool"

type openAIRequest struct {
	Model         string                  `json:"model"`
	Stream        bool                    `json:"stream"`
	StreamOptions *openAIStreamOptions    `json:"stream_options,omitempty"`
	Messages      []openAIMessage         `json:"messages"`
	Tools         []tool.OpenAIDefinition `json:"tools,omitempty"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// newOpenAIRequest is the only mapping from the Provider-owned safe ChatRequest
// DTO to OpenAI wire types. CachePolicy is deliberately not serialized because
// the OpenAI-compatible request format has no approved equivalent for the
// Anthropic cache_control fields.
func newOpenAIRequest(req ChatRequest, defaultModel string) openAIRequest {
	return openAIRequest{
		Model:         requestModel(req.Model, defaultModel),
		Stream:        true,
		StreamOptions: &openAIStreamOptions{IncludeUsage: true},
		Messages:      toOpenAIMessages(req),
		Tools:         openAITools(req),
	}
}

func openAITools(req ChatRequest) []tool.OpenAIDefinition {
	definitions := toolDefinitions(req)
	if len(definitions) == 0 {
		return nil
	}
	tools := make([]tool.OpenAIDefinition, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, tool.OpenAIDefinition{
			Type: "function",
			Function: tool.OpenAIFunction{
				Name:        definition.Name,
				Description: definition.Description,
				Parameters:  definition.Schema,
			},
		})
	}
	return tools
}

func toOpenAIMessages(req ChatRequest) []openAIMessage {
	systemCount := 1
	if usesOrderedSystem(req) {
		systemCount = len(req.System)
	}
	messages := make([]openAIMessage, 0, len(req.Messages)+systemCount)
	if usesOrderedSystem(req) {
		for _, block := range systemBlocks(req) {
			messages = append(messages, openAIMessage{Role: "system", Content: block.Content.Text()})
		}
	} else if system := joinedSystemBlocks(req); system != "" {
		messages = append(messages, openAIMessage{Role: "system", Content: system})
	}
	for _, message := range req.Messages {
		switch message.Role {
		case ModelMessageRoleUser, ModelMessageRoleContextSummary, ModelMessageRoleContextBoundary:
			messages = append(messages, openAIMessage{Role: "user", Content: message.Content.Text()})
		case ModelMessageRoleAssistant:
			messages = append(messages, openAIMessage{Role: "assistant", Content: message.Content.Text()})
		case ModelMessageRoleToolCall:
			messages = append(messages, openAIMessage{
				Role: "assistant",
				ToolCalls: []openAIToolCall{{
					ID:   message.ToolCallID,
					Type: "function",
					Function: openAIToolFunction{
						Name:      message.ToolName,
						Arguments: message.ArgumentsJSON.Text(),
					},
				}},
			})
		case ModelMessageRoleToolResult:
			messages = append(messages, openAIMessage{
				Role:       "tool",
				ToolCallID: message.ToolCallID,
				Content:    message.ToolResult.Text(),
			})
		}
	}
	return messages
}
