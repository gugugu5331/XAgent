package provider

import (
	"encoding/json"
	"errors"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

type StreamEventType string

const (
	StreamEventTextDelta     StreamEventType = "text_delta"
	StreamEventThinkingDelta StreamEventType = "thinking_delta"
	StreamEventToolCall      StreamEventType = "tool_call"
	StreamEventUsage         StreamEventType = "usage"
	StreamEventDone          StreamEventType = "done"
	StreamEventError         StreamEventType = "error"
)

type SafeToolCall struct {
	ID            string
	Name          string
	ArgumentsJSON redact.SafeText
}

type StreamEvent struct {
	Type     StreamEventType
	Delta    redact.SafeText
	Error    *diagnostics.SafeError
	Usage    *Usage
	ToolCall *SafeToolCall
}

func safeProviderError(redactor *redact.RuntimeRedactor, code string, source string, err error, recoverable bool) *diagnostics.SafeError {
	if err == nil {
		err = errors.New(code)
	}
	return &diagnostics.SafeError{
		Code:        code,
		Source:      source,
		Message:     redactor.Redact(err.Error()),
		Recoverable: recoverable,
	}
}

func safeToolCalls(redactor *redact.RuntimeRedactor, source string, raw []rawToolCall) ([]SafeToolCall, *diagnostics.SafeError) {
	result := make([]SafeToolCall, 0, len(raw))
	for _, call := range raw {
		arguments := redactor.Redact(call.arguments)
		if !json.Valid([]byte(arguments.Text())) {
			return nil, safeProviderError(redactor, "unsafe_tool_arguments", source, errors.New("工具参数脱敏后不是有效 JSON，已拒绝工具调用"), false)
		}
		result = append(result, SafeToolCall{ID: call.id, Name: call.name, ArgumentsJSON: arguments})
	}
	return result, nil
}

type rawToolCall struct {
	id        string
	name      string
	arguments string
}
