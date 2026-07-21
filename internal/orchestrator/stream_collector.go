package orchestrator

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"xagent/internal/events"
	"xagent/internal/provider"
	"xagent/internal/tool"
)

const maxPendingStreamBytes = 256 * 1024

var sensitiveStreamField = regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|refresh[_-]?token|token|secret|password|cookie|set-cookie|x-api-key|authorization|credential|AWS_SECRET_ACCESS_KEY|GITHUB_TOKEN|ANTHROPIC_API_KEY|OPENAI_API_KEY)\s*[:=]\s*`)
var streamURLStart = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://`)

type StreamCollector struct {
	AssistantText strings.Builder
	ThinkingText  strings.Builder
	ToolCalls     []tool.Call
	Usage         *provider.Usage
	Done          bool
}

func collectProviderStream(ctx context.Context, stream <-chan provider.StreamEvent, out chan<- events.Event) (StreamCollector, StopReason, error) {
	return collectProviderStreamWithRedactor(ctx, stream, out, nil, 64)
}

func collectProviderStreamWithRedactor(ctx context.Context, stream <-chan provider.StreamEvent, out chan<- events.Event, redactor func(string) string, lookbehind int) (StreamCollector, StopReason, error) {
	var collector StreamCollector
	if redactor == nil {
		redactor = func(value string) string { return value }
	}
	type pendingSegment struct {
		eventType events.Type
		text      string
	}
	pending := []pendingSegment{}
	pendingBytes := 0
	appendPending := func(eventType events.Type, value string) {
		if value == "" {
			return
		}
		if len(pending) > 0 && pending[len(pending)-1].eventType == eventType {
			pending[len(pending)-1].text += value
		} else {
			pending = append(pending, pendingSegment{eventType: eventType, text: value})
		}
		pendingBytes += len(value)
	}
	pendingValue := func() string {
		var value strings.Builder
		value.Grow(pendingBytes)
		for _, segment := range pending {
			value.WriteString(segment.text)
		}
		return value.String()
	}
	consumePending := func(count int) []pendingSegment {
		consumed := make([]pendingSegment, 0, len(pending))
		for count > 0 && len(pending) > 0 {
			segment := pending[0]
			if count < len(segment.text) {
				consumed = append(consumed, pendingSegment{eventType: segment.eventType, text: segment.text[:count]})
				pending[0].text = segment.text[count:]
				pendingBytes -= count
				count = 0
				break
			}
			consumed = append(consumed, segment)
			count -= len(segment.text)
			pendingBytes -= len(segment.text)
			pending = pending[1:]
		}
		return consumed
	}
	emitText := func(eventType events.Type, value string) bool {
		if value == "" {
			return true
		}
		switch eventType {
		case events.TextDelta:
			collector.AssistantText.WriteString(value)
		case events.ThinkingDelta:
			collector.ThinkingText.WriteString(value)
		}
		return emitEvent(ctx, out, events.Event{Type: eventType, Text: value})
	}
	flushPending := func(final bool) bool {
		if pendingBytes == 0 {
			pending = nil
			return true
		}
		raw := pendingValue()
		prefix := raw
		if !final {
			prefix, _ = splitRedactionSafePrefix(raw, lookbehind, redactor)
		}
		if prefix == "" {
			return true
		}
		safe := redactor(prefix)
		if final {
			safe = redactIncompletePrivateKey(prefix, redactor)
		}
		consumed := consumePending(len(prefix))
		if safe == prefix {
			for _, segment := range consumed {
				if !emitText(segment.eventType, segment.text) {
					return false
				}
			}
			return true
		}
		eventType := events.ThinkingDelta
		for _, segment := range consumed {
			if segment.eventType == events.TextDelta {
				eventType = events.TextDelta
				break
			}
		}
		return emitText(eventType, safe)
	}
	for {
		select {
		case <-ctx.Done():
			return collector, StopReasonCancelled, ctx.Err()
		case event, ok := <-stream:
			if !ok {
				return collector, StopReasonProviderError, fmt.Errorf("provider 流异常结束")
			}
			switch event.Type {
			case provider.StreamEventTextDelta:
				appendPending(events.TextDelta, event.Delta)
				if !flushPending(false) {
					return collector, StopReasonCancelled, ctx.Err()
				}
				if pendingBytes > maxPendingStreamBytes {
					return collector, StopReasonProviderError, fmt.Errorf("provider 流超过安全脱敏缓冲上限")
				}
			case provider.StreamEventThinkingDelta:
				appendPending(events.ThinkingDelta, event.Delta)
				if !flushPending(false) {
					return collector, StopReasonCancelled, ctx.Err()
				}
				if pendingBytes > maxPendingStreamBytes {
					return collector, StopReasonProviderError, fmt.Errorf("provider 流超过安全脱敏缓冲上限")
				}
			case provider.StreamEventToolCall:
				if !flushPending(true) {
					return collector, StopReasonCancelled, ctx.Err()
				}
				collector.ToolCalls = streamToolCalls(event)
				return collector, "", nil
			case provider.StreamEventUsage:
				if !flushPending(false) {
					return collector, StopReasonCancelled, ctx.Err()
				}
				collector.Usage = event.Usage
				if event.Usage != nil {
					if !emitEvent(ctx, out, events.Event{Type: events.UsageUpdated, Usage: usageDisplay(event.Usage)}) {
						return collector, StopReasonCancelled, ctx.Err()
					}
				}
			case provider.StreamEventDone:
				if !flushPending(true) {
					return collector, StopReasonCancelled, ctx.Err()
				}
				if event.Usage != nil {
					collector.Usage = event.Usage
					if !emitEvent(ctx, out, events.Event{Type: events.UsageUpdated, Usage: usageDisplay(event.Usage)}) {
						return collector, StopReasonCancelled, ctx.Err()
					}
				}
				collector.Done = true
				return collector, StopReasonCompleted, nil
			case provider.StreamEventError:
				if !flushPending(true) {
					return collector, StopReasonCancelled, ctx.Err()
				}
				if event.Err == nil {
					return collector, StopReasonProviderError, fmt.Errorf("provider 返回空错误事件")
				}
				safeError := redactor(event.Err.Error())
				if safeError == event.Err.Error() {
					return collector, StopReasonProviderError, event.Err
				}
				return collector, StopReasonProviderError, fmt.Errorf("%s", safeError)
			}
		}
	}
}

func splitRedactionSafePrefix(value string, lookbehindBytes int, redactor func(string) string) (string, string) {
	if lookbehindBytes < 64 {
		lookbehindBytes = 64
	}
	if len(value) <= lookbehindBytes {
		return "", value
	}
	minimumBoundary := 0
	if begin := strings.LastIndex(value, "-----BEGIN "); begin >= 0 {
		privateKey := strings.Index(value[begin:], "PRIVATE KEY-----")
		if privateKey >= 0 {
			end := strings.Index(value[begin+privateKey:], "-----END ")
			if end < 0 {
				return "", value
			}
			end += begin + privateKey
			terminator := strings.Index(value[end:], "PRIVATE KEY-----")
			if terminator < 0 {
				return "", value
			}
			minimumBoundary = end + terminator + len("PRIVATE KEY-----")
		}
	}
	limit := len(value) - lookbehindBytes
	unsafeRanges := sensitiveStreamRanges(value)
	boundary := -1
	for index, r := range value {
		end := index + len(string(r))
		if end > limit {
			break
		}
		if (unicode.IsSpace(r) || r == ',' || r == ';') && streamBoundaryIsSafe(end, unsafeRanges) {
			boundary = end
		}
	}
	if boundary <= 0 || boundary < minimumBoundary {
		if minimumBoundary > 0 || len(unsafeRanges) > 0 || redactor == nil || redactor(value) != value {
			return "", value
		}
		boundary = limit
		for boundary > 0 && !utf8.RuneStart(value[boundary]) {
			boundary--
		}
		if boundary <= 0 {
			return "", value
		}
	}
	if redactor != nil {
		windowStart := boundary - lookbehindBytes
		if windowStart < 0 {
			windowStart = 0
		}
		windowEnd := boundary + lookbehindBytes
		if windowEnd > len(value) {
			windowEnd = len(value)
		}
		window := value[windowStart:windowEnd]
		if redactor(window) != window {
			return "", value
		}
	}
	return value[:boundary], value[boundary:]
}

type streamRange struct {
	start int
	end   int
}

func sensitiveStreamRanges(value string) []streamRange {
	matches := sensitiveStreamField.FindAllStringIndex(value, -1)
	ranges := make([]streamRange, 0, len(matches)+1)
	for _, match := range matches {
		end := len(value) + 1
		if offset := strings.IndexAny(value[match[1]:], "\r\n,;"); offset >= 0 {
			end = match[1] + offset + 1
		}
		ranges = append(ranges, streamRange{start: match[0], end: end})
	}
	for _, match := range streamURLStart.FindAllStringIndex(value, -1) {
		end := len(value) + 1
		if offset := strings.IndexAny(value[match[1]:], " \t\r\n,;"); offset >= 0 {
			end = match[1] + offset + 1
		}
		ranges = append(ranges, streamRange{start: match[0], end: end})
	}
	return ranges
}

func streamBoundaryIsSafe(boundary int, ranges []streamRange) bool {
	for _, item := range ranges {
		if boundary > item.start && boundary < item.end {
			return false
		}
	}
	return true
}

func redactIncompletePrivateKey(value string, redactor func(string) string) string {
	begin := strings.LastIndex(value, "-----BEGIN ")
	if begin < 0 {
		return redactor(value)
	}
	remainder := value[begin:]
	if !strings.Contains(remainder, "PRIVATE KEY-----") {
		return redactor(value)
	}
	if end := strings.Index(remainder, "-----END "); end >= 0 && strings.Contains(remainder[end:], "PRIVATE KEY-----") {
		return redactor(value)
	}
	return redactor(value[:begin]) + "[redacted]"
}

func usageDisplay(usage *provider.Usage) *events.UsageDisplay {
	if usage == nil {
		return nil
	}
	return &events.UsageDisplay{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
	}
}
