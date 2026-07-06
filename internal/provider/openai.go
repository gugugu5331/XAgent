package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/tool"
)

type OpenAIProvider struct {
	cfg    config.LLMConfig
	client *http.Client
}

func NewOpenAI(cfg config.LLMConfig, client *http.Client) *OpenAIProvider {
	if client == nil {
		client = http.DefaultClient
	}
	return &OpenAIProvider{cfg: cfg, client: client}
}

func (p *OpenAIProvider) Name() string {
	return "OpenAI"
}

func (p *OpenAIProvider) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	out := make(chan StreamEvent)
	body, err := json.Marshal(openAIRequest{
		Model:         p.cfg.Model,
		Stream:        true,
		StreamOptions: &openAIStreamOptions{IncludeUsage: true},
		Messages:      toOpenAIMessages(req),
		Tools:         openAITools(req),
	})
	if err != nil {
		return nil, fmt.Errorf("构造 OpenAI 请求失败: %w", err)
	}

	endpoint := strings.TrimRight(p.cfg.BaseURL, "/")
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建 OpenAI 请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("OpenAI 请求失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("OpenAI 返回错误状态 %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	go func() {
		defer close(out)
		defer resp.Body.Close()
		calls := map[int]*openAIToolCallState{}
		for event := range ReadSSE(resp.Body) {
			if event.Data == "[DONE]" {
				if len(calls) > 0 {
					out <- newToolCallsEvent(openAIToolCalls(calls))
					return
				}
				out <- StreamEvent{Type: StreamEventDone}
				return
			}
			var chunk openAIChunk
			if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
				out <- StreamEvent{Type: StreamEventError, Err: fmt.Errorf("解析 OpenAI 流式响应失败: %w", err)}
				return
			}
			if chunk.Usage != nil {
				out <- StreamEvent{Type: StreamEventUsage, Usage: &Usage{InputTokens: int64(chunk.Usage.PromptTokens), OutputTokens: int64(chunk.Usage.CompletionTokens)}}
			}
			for _, choice := range chunk.Choices {
				if choice.Delta.Content != "" {
					out <- StreamEvent{Type: StreamEventTextDelta, Delta: choice.Delta.Content}
				}
				for _, delta := range choice.Delta.ToolCalls {
					state := calls[delta.Index]
					if state == nil {
						state = &openAIToolCallState{}
						calls[delta.Index] = state
					}
					if delta.ID != "" {
						state.ID = delta.ID
					}
					if delta.Function.Name != "" {
						state.Name = delta.Function.Name
					}
					if delta.Function.Arguments != "" {
						state.Arguments.WriteString(delta.Function.Arguments)
					}
				}
				if choice.FinishReason != "" && choice.FinishReason != "tool_calls" {
					out <- StreamEvent{Type: StreamEventDone}
					return
				}
			}
		}
	}()
	return out, nil
}

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

type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type openAIToolCallState struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

func openAIToolCalls(calls map[int]*openAIToolCallState) []tool.Call {
	indexes := make([]int, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

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

func newToolCallsEvent(toolCalls []tool.Call) StreamEvent {
	event := StreamEvent{Type: StreamEventToolCall, ToolCalls: toolCalls}
	if len(toolCalls) == 1 {
		event.ToolCall = &event.ToolCalls[0]
	}
	return event
}

func openAITools(req ChatRequest) []tool.OpenAIDefinition {
	definitions := toolDefinitions(req)
	if len(definitions) == 0 {
		return nil
	}
	tools := make([]tool.OpenAIDefinition, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, tool.OpenAIDefinition{
			Type:     "function",
			Function: tool.OpenAIFunction{Name: definition.Name, Description: definition.Description, Parameters: definition.Schema},
		})
	}
	return tools
}

func toOpenAIMessages(req ChatRequest) []openAIMessage {
	messages := make([]openAIMessage, 0, len(req.Messages)+1)
	system := joinedSystemBlocks(req)
	if system != "" {
		messages = append(messages, openAIMessage{Role: "system", Content: system})
	}
	for _, message := range req.Messages {
		switch message.Role {
		case conversation.RoleUser, conversation.RoleContextSummary, conversation.RoleContextBoundary:
			messages = append(messages, openAIMessage{Role: "user", Content: message.Content})
		case conversation.RoleAssistant:
			messages = append(messages, openAIMessage{Role: "assistant", Content: message.Content})
		case conversation.RoleToolCall:
			messages = append(messages, openAIMessage{
				Role: "assistant",
				ToolCalls: []openAIToolCall{{
					ID:   message.ToolCallID,
					Type: "function",
					Function: openAIToolFunction{
						Name:      message.ToolName,
						Arguments: message.RawToolArguments,
					},
				}},
			})
		case conversation.RoleToolResult:
			messages = append(messages, openAIMessage{Role: "tool", ToolCallID: message.ToolCallID, Content: message.ToolResultContent})
		}
	}
	return messages
}
