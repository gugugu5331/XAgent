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
		Model:    p.cfg.Model,
		Stream:   true,
		Messages: toOpenAIMessages(req),
		Tools:    openAITools(req.ToolDefs),
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
	Model    string                  `json:"model"`
	Stream   bool                    `json:"stream"`
	Messages []openAIMessage         `json:"messages"`
	Tools    []tool.OpenAIDefinition `json:"tools,omitempty"`
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

func openAITools(registry *tool.Registry) []tool.OpenAIDefinition {
	if registry == nil {
		return nil
	}
	return registry.OpenAIDefinitions()
}

func toOpenAIMessages(req ChatRequest) []openAIMessage {
	messages := make([]openAIMessage, 0, len(req.Messages)+1)
	if strings.TrimSpace(req.SystemPrompt) != "" {
		messages = append(messages, openAIMessage{Role: "system", Content: req.SystemPrompt})
	}
	for _, message := range req.Messages {
		switch message.Role {
		case conversation.RoleUser:
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
