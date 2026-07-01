package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"xagent/internal/events"
	"xagent/internal/provider"
	"xagent/internal/tool"
)

type StreamCollector struct {
	AssistantText strings.Builder
	ThinkingText  strings.Builder
	ToolCalls     []tool.Call
	Usage         *provider.Usage
	Done          bool
}

func collectProviderStream(ctx context.Context, stream <-chan provider.StreamEvent, out chan<- events.Event) (StreamCollector, StopReason, error) {
	var collector StreamCollector
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
				collector.AssistantText.WriteString(event.Delta)
				out <- events.Event{Type: events.TextDelta, Text: event.Delta}
			case provider.StreamEventThinkingDelta:
				collector.ThinkingText.WriteString(event.Delta)
				out <- events.Event{Type: events.ThinkingDelta, Text: event.Delta}
			case provider.StreamEventToolCall:
				collector.ToolCalls = streamToolCalls(event)
				return collector, "", nil
			case provider.StreamEventUsage:
				collector.Usage = event.Usage
				if event.Usage != nil {
					out <- events.Event{Type: events.UsageUpdated, Usage: usageDisplay(event.Usage)}
				}
			case provider.StreamEventDone:
				if event.Usage != nil {
					collector.Usage = event.Usage
					out <- events.Event{Type: events.UsageUpdated, Usage: usageDisplay(event.Usage)}
				}
				collector.Done = true
				return collector, StopReasonCompleted, nil
			case provider.StreamEventError:
				return collector, StopReasonProviderError, event.Err
			}
		}
	}
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
