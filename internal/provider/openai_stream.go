package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"xagent/internal/redact"
)

type openAIStreamState uint8

const (
	openAIStreamReading openAIStreamState = iota
	openAIStreamFinishReasonSeen
	openAIStreamUsageSeen
	openAIStreamDoneSeen
	openAIStreamCompleted
)

type openAIStreamFailure struct {
	code        string
	err         error
	recoverable bool
}

type openAIStreamProcessor struct {
	ctx      context.Context
	stream   *chatStream
	limits   *streamLimits
	redactor *redact.RuntimeRedactor
	attempt  *requestAttempt

	state          openAIStreamState
	finalUsage     *Usage
	usagePublished bool
	stopped        bool
	calls          map[int]*openAIToolCallState
}

func newOpenAIStreamProcessor(
	ctx context.Context,
	stream *chatStream,
	limits *streamLimits,
	redactor *redact.RuntimeRedactor,
	attempt *requestAttempt,
) *openAIStreamProcessor {
	return &openAIStreamProcessor{
		ctx:      ctx,
		stream:   stream,
		limits:   limits,
		redactor: redactor,
		attempt:  attempt,
		state:    openAIStreamReading,
		calls:    make(map[int]*openAIToolCallState),
	}
}

func (p *openAIStreamProcessor) run(reader io.Reader) *openAIStreamFailure {
	decoder, err := newSSEDecoder(reader, p.limits)
	if err != nil {
		return streamFailure("openai_stream_decoder_failed", err, false)
	}
	for decoder.Next() {
		event := decoder.Event()
		if event.Data == "[DONE]" {
			if !p.complete() {
				return nil
			}
			return nil
		}
		if failure := p.consumeChunk(event.Data); failure != nil {
			return failure
		}
		if p.stopped {
			return nil
		}
	}
	if err := decoder.Err(); err != nil {
		switch decoder.Termination() {
		case sseTerminationUnexpectedEOF:
			return streamFailure("openai_stream_unexpected_eof", err, true)
		case sseTerminationLimitExceeded:
			return streamFailure("openai_stream_limit_exceeded", err, false)
		default:
			return streamFailure("openai_stream_read_failed", err, true)
		}
	}
	return streamFailure("openai_stream_incomplete", io.ErrUnexpectedEOF, true)
}

func (p *openAIStreamProcessor) consumeChunk(data string) *openAIStreamFailure {
	var chunk openAIChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return streamFailure("openai_stream_decode_failed", fmt.Errorf("解析 OpenAI 流式响应失败: %w", err), false)
	}

	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" {
			if err := p.limits.consumeText(int64(len(choice.Delta.Content))); err != nil {
				return streamFailure("openai_stream_text_limit", err, false)
			}
			if !p.stream.emit(p.ctx, StreamEvent{Type: StreamEventTextDelta, Delta: p.redactor.Redact(choice.Delta.Content)}) {
				p.stopped = true
				return nil
			}
		}
		for _, delta := range choice.Delta.ToolCalls {
			if failure := p.consumeToolCallDelta(delta); failure != nil {
				return failure
			}
		}
		if choice.FinishReason != "" {
			p.markFinishReasonSeen()
		}
	}

	if chunk.Usage != nil {
		p.finalUsage = &Usage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
		}
		p.state = openAIStreamUsageSeen
	}
	return nil
}

func (p *openAIStreamProcessor) consumeToolCallDelta(delta openAIToolCallDelta) *openAIStreamFailure {
	state := p.calls[delta.Index]
	if state == nil {
		state = &openAIToolCallState{}
		p.calls[delta.Index] = state
	}
	if delta.ID != "" {
		state.ID = delta.ID
	}
	if delta.Function.Name != "" {
		state.Name = delta.Function.Name
	}
	if delta.Function.Arguments == "" {
		return nil
	}
	if err := p.limits.consumeToolArguments(int64(len(delta.Function.Arguments))); err != nil {
		return streamFailure("openai_stream_tool_arguments_limit", err, false)
	}
	state.Arguments.WriteString(delta.Function.Arguments)
	return nil
}

func (p *openAIStreamProcessor) markFinishReasonSeen() {
	if p.state == openAIStreamReading {
		p.state = openAIStreamFinishReasonSeen
	}
}

func (p *openAIStreamProcessor) complete() bool {
	p.state = openAIStreamDoneSeen
	p.attempt.finish()
	if p.finalUsage != nil && !p.usagePublished {
		usage := *p.finalUsage
		if !p.stream.emit(p.ctx, StreamEvent{Type: StreamEventUsage, Usage: &usage}) {
			return false
		}
		p.usagePublished = true
	}

	if len(p.calls) > 0 {
		toolCalls, safeErr := safeToolCalls(p.redactor, "openai", openAIToolCalls(p.calls))
		if safeErr != nil {
			p.stream.emit(p.ctx, StreamEvent{Type: StreamEventError, Error: safeErr})
			return false
		}
		for index := range toolCalls {
			if !p.stream.emit(p.ctx, StreamEvent{Type: StreamEventToolCall, ToolCall: &toolCalls[index]}) {
				return false
			}
		}
	} else if !p.stream.emit(p.ctx, StreamEvent{Type: StreamEventDone}) {
		return false
	}
	p.state = openAIStreamCompleted
	return true
}

func streamFailure(code string, err error, recoverable bool) *openAIStreamFailure {
	if err == nil {
		err = errors.New(code)
	}
	return &openAIStreamFailure{code: code, err: err, recoverable: recoverable}
}

type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content   string                `json:"content"`
			ToolCalls []openAIToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIToolCallState struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

func openAIToolCalls(calls map[int]*openAIToolCallState) []rawToolCall {
	indexes := make([]int, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	toolCalls := make([]rawToolCall, 0, len(indexes))
	for _, index := range indexes {
		call := calls[index]
		if call == nil {
			continue
		}
		toolCalls = append(toolCalls, rawToolCall{id: call.ID, name: call.Name, arguments: call.Arguments.String()})
	}
	return toolCalls
}
