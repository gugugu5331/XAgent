package provider

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"

	"xagent/internal/redact"
)

type anthropicSDKStream interface {
	Next() bool
	Current() anthropic.MessageStreamEventUnion
	Err() error
	Close() error
}

type anthropicSDKStreamOwner struct {
	stream anthropicSDKStream

	closeOnce sync.Once
	closeErr  error
}

func newAnthropicSDKStreamOwner(stream anthropicSDKStream) *anthropicSDKStreamOwner {
	return &anthropicSDKStreamOwner{stream: stream}
}

func (s *anthropicSDKStreamOwner) next() bool {
	return s != nil && s.stream != nil && s.stream.Next()
}

func (s *anthropicSDKStreamOwner) current() anthropic.MessageStreamEventUnion {
	if s == nil || s.stream == nil {
		return anthropic.MessageStreamEventUnion{}
	}
	return s.stream.Current()
}

func (s *anthropicSDKStreamOwner) err() error {
	if s == nil || s.stream == nil {
		return errors.New("anthropic SDK stream is unavailable")
	}
	return s.stream.Err()
}

func (s *anthropicSDKStreamOwner) close() error {
	if s == nil || s.stream == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.stream.Close()
	})
	return s.closeErr
}

type anthropicStreamFailure struct {
	code        string
	err         error
	recoverable bool
}

type anthropicStreamProcessor struct {
	ctx      context.Context
	stream   *chatStream
	limits   *streamLimits
	redactor *redact.RuntimeRedactor
	attempt  *requestAttempt

	stopped   bool
	toolCalls map[int64]*anthropicToolCallState
}

func newAnthropicStreamProcessor(
	ctx context.Context,
	stream *chatStream,
	limits *streamLimits,
	redactor *redact.RuntimeRedactor,
	attempt *requestAttempt,
) *anthropicStreamProcessor {
	return &anthropicStreamProcessor{
		ctx:       ctx,
		stream:    stream,
		limits:    limits,
		redactor:  redactor,
		attempt:   attempt,
		toolCalls: make(map[int64]*anthropicToolCallState),
	}
}

func (p *anthropicStreamProcessor) run(source *anthropicSDKStreamOwner) *anthropicStreamFailure {
	for source.next() {
		event := source.current()
		if failure := p.consumeEnvelope(event.RawJSON()); failure != nil {
			return failure
		}
		if failure := p.consumeEvent(event); failure != nil {
			return failure
		}
		if p.stopped {
			return nil
		}
	}
	if err := source.err(); err != nil {
		return anthropicFailure("anthropic_stream_failed", err, true)
	}
	return nil
}

func (p *anthropicStreamProcessor) consumeEnvelope(raw string) *anthropicStreamFailure {
	size := int64(len(raw))
	if err := p.limits.consumeResponse(size); err != nil {
		return anthropicFailure("anthropic_stream_response_limit", err, false)
	}
	if err := p.limits.consumeEvent(size); err != nil {
		return anthropicFailure("anthropic_stream_event_limit", err, false)
	}
	if err := p.limits.consumeEventCount(); err != nil {
		return anthropicFailure("anthropic_stream_event_count_limit", err, false)
	}
	return nil
}

func (p *anthropicStreamProcessor) consumeEvent(event anthropic.MessageStreamEventUnion) *anthropicStreamFailure {
	switch value := event.AsAny().(type) {
	case anthropic.ContentBlockStartEvent:
		if block, ok := value.ContentBlock.AsAny().(anthropic.ToolUseBlock); ok {
			p.toolCalls[value.Index] = &anthropicToolCallState{ID: block.ID, Name: block.Name}
		}
	case anthropic.ContentBlockDeltaEvent:
		return p.consumeContentDelta(value)
	case anthropic.MessageDeltaEvent:
		p.emit(StreamEvent{Type: StreamEventUsage, Usage: &Usage{
			InputTokens:              value.Usage.InputTokens,
			OutputTokens:             value.Usage.OutputTokens,
			CacheCreationInputTokens: value.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     value.Usage.CacheReadInputTokens,
		}})
	case anthropic.MessageStopEvent:
		p.complete()
	}
	return nil
}

func (p *anthropicStreamProcessor) consumeContentDelta(event anthropic.ContentBlockDeltaEvent) *anthropicStreamFailure {
	switch delta := event.Delta.AsAny().(type) {
	case anthropic.TextDelta:
		if err := p.limits.consumeText(int64(len(delta.Text))); err != nil {
			return anthropicFailure("anthropic_stream_text_limit", err, false)
		}
		p.emit(StreamEvent{Type: StreamEventTextDelta, Delta: p.redactor.Redact(delta.Text)})
	case anthropic.ThinkingDelta:
		if err := p.limits.consumeThinking(int64(len(delta.Thinking))); err != nil {
			return anthropicFailure("anthropic_stream_thinking_limit", err, false)
		}
		p.emit(StreamEvent{Type: StreamEventThinkingDelta, Delta: p.redactor.Redact(delta.Thinking)})
	case anthropic.InputJSONDelta:
		if err := p.limits.consumeToolArguments(int64(len(delta.PartialJSON))); err != nil {
			return anthropicFailure("anthropic_stream_tool_arguments_limit", err, false)
		}
		if state := p.toolCalls[event.Index]; state != nil {
			state.Arguments.WriteString(delta.PartialJSON)
		}
	}
	return nil
}

func (p *anthropicStreamProcessor) complete() {
	p.attempt.finish()
	if len(p.toolCalls) > 0 {
		calls, safeErr := safeToolCalls(p.redactor, "anthropic", anthropicToolCalls(p.toolCalls))
		if safeErr != nil {
			p.emit(StreamEvent{Type: StreamEventError, Error: safeErr})
			return
		}
		for index := range calls {
			if !p.emit(StreamEvent{Type: StreamEventToolCall, ToolCall: &calls[index]}) {
				return
			}
		}
		return
	}
	p.emit(StreamEvent{Type: StreamEventDone})
}

func (p *anthropicStreamProcessor) emit(event StreamEvent) bool {
	if p.stopped {
		return false
	}
	if !p.stream.emit(p.ctx, event) {
		p.stopped = true
		return false
	}
	return true
}

func anthropicFailure(code string, err error, recoverable bool) *anthropicStreamFailure {
	if err == nil {
		err = errors.New(code)
	}
	return &anthropicStreamFailure{code: code, err: err, recoverable: recoverable}
}

type anthropicToolCallState struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

func anthropicToolCalls(calls map[int64]*anthropicToolCallState) []rawToolCall {
	indexes := make([]int64, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })

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
