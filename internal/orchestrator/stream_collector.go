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
	"xagent/internal/redact"
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

func collectProviderStream(ctx context.Context, stream provider.ChatStream, out chan<- events.Event) (StreamCollector, StopReason, error) {
	return collectProviderStreamWithRedactor(ctx, stream, out, nil, 64)
}

// collectAndCloseProviderStreamWithRedactor is the single Orchestrator owner
// for a successfully created Provider stream. Close is registered before the
// event channel is inspected, so invalid streams, normal completion, Provider
// errors, cancellation, and an output consumer exiting early all converge on
// the same exactly-once close path.
func collectAndCloseProviderStreamWithRedactor(
	ctx context.Context,
	stream provider.ChatStream,
	out chan<- events.Event,
	redactor func(string) string,
	lookbehind int,
) (collector StreamCollector, reason StopReason, err error) {
	return ownProviderStreamWithRedactor(ctx, stream, nil, out, redactor, lookbehind)
}

// ownProviderStreamWithRedactor also owns partially initialized streams
// returned together with a StreamChat error. A non-nil stream is registered
// for Close before the start error is inspected.
func ownProviderStreamWithRedactor(
	ctx context.Context,
	stream provider.ChatStream,
	startErr error,
	out chan<- events.Event,
	redactor func(string) string,
	lookbehind int,
) (collector StreamCollector, reason StopReason, err error) {
	if stream == nil {
		if startErr != nil {
			return collector, StopReasonProviderError, startErr
		}
		return collector, StopReasonProviderError, fmt.Errorf("provider 流不可用")
	}
	defer func() {
		closeErr := closeProviderStream(stream)
		if err == nil && closeErr != nil {
			err = closeErr
			reason = StopReasonProviderError
		}
	}()
	if startErr != nil {
		return collector, StopReasonProviderError, startErr
	}
	return collectProviderStreamWithRedactor(ctx, stream, out, redactor, lookbehind)
}

func collectProviderStreamWithRedactor(ctx context.Context, stream provider.ChatStream, out chan<- events.Event, redactor func(string) string, lookbehind int) (StreamCollector, StopReason, error) {
	var collector StreamCollector
	usageSeen := false
	if stream == nil {
		return collector, StopReasonProviderError, fmt.Errorf("provider 流不可用")
	}
	streamEvents := stream.Events()
	if streamEvents == nil {
		return collector, StopReasonProviderError, fmt.Errorf("provider 流不可用")
	}
	if redactor == nil {
		redactor = redact.Text
	}
	safeRedactor := redact.NewRuntimeRedactor()
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
		return emitEvent(ctx, out, events.Event{Type: eventType, Text: safeRedactor.Redact(redactor(value))})
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
		if err := requestContextError(ctx); err != nil {
			return collector, StopReasonCancelled, err
		}
		select {
		case <-ctx.Done():
			return collector, StopReasonCancelled, ctx.Err()
		case event, ok := <-streamEvents:
			// Cancellation owns the turn terminal state when it races a Provider
			// error or channel close. Re-check after the receive so selecting the
			// stream arm cannot nondeterministically turn the same cancellation
			// into a provider_error result.
			if err := requestContextError(ctx); err != nil {
				return collector, StopReasonCancelled, err
			}
			if !ok {
				if len(collector.ToolCalls) > 0 {
					return collector, "", nil
				}
				return collector, StopReasonProviderError, fmt.Errorf("provider 流异常结束")
			}
			switch event.Type {
			case provider.StreamEventTextDelta:
				appendPending(events.TextDelta, event.Delta.Text())
				if !flushPending(false) {
					return collector, StopReasonCancelled, ctx.Err()
				}
				if pendingBytes > maxPendingStreamBytes {
					return collector, StopReasonProviderError, fmt.Errorf("provider 流超过安全脱敏缓冲上限")
				}
			case provider.StreamEventThinkingDelta:
				appendPending(events.ThinkingDelta, event.Delta.Text())
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
				collector.ToolCalls = append(collector.ToolCalls, streamToolCalls(event)...)
			case provider.StreamEventUsage:
				if !flushPending(false) {
					return collector, StopReasonCancelled, ctx.Err()
				}
				if usageSeen || event.Usage == nil {
					return collector, StopReasonProviderError, fmt.Errorf("provider usage 快照无效或重复")
				}
				collector.Usage = cloneUsage(event.Usage)
				usageSeen = true
			case provider.StreamEventDone:
				if !flushPending(true) {
					return collector, StopReasonCancelled, ctx.Err()
				}
				if event.Usage != nil {
					if usageSeen {
						return collector, StopReasonProviderError, fmt.Errorf("provider usage 快照重复")
					}
					collector.Usage = cloneUsage(event.Usage)
					usageSeen = true
				}
				collector.Done = true
				select {
				case _, open := <-streamEvents:
					if open {
						return collector, StopReasonProviderError, fmt.Errorf("provider 流在终态后发布事件")
					}
				default:
				}
				return collector, StopReasonCompleted, nil
			case provider.StreamEventError:
				if !flushPending(true) {
					return collector, StopReasonCancelled, ctx.Err()
				}
				if event.Error == nil {
					return collector, StopReasonProviderError, fmt.Errorf("provider 返回空错误事件")
				}
				safeError := redactor(event.Error.Error())
				if safeError == event.Error.Error() {
					return collector, StopReasonProviderError, event.Error
				}
				return collector, StopReasonProviderError, fmt.Errorf("%s", safeError)
			}
		}
	}
}

func cloneUsage(usage *provider.Usage) *provider.Usage {
	if usage == nil {
		return nil
	}
	snapshot := *usage
	return &snapshot
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
