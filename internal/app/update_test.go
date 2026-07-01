package app

import (
	"testing"

	"xagent/internal/events"
)

func TestUsageUpdatesAccumulateCacheFields(t *testing.T) {
	model := Model{}
	msg := eventMsg{event: events.Event{Type: events.UsageUpdated, Usage: &events.UsageDisplay{
		InputTokens:              1,
		OutputTokens:             2,
		CacheCreationInputTokens: 3,
		CacheReadInputTokens:     4,
	}}, events: closedEvents()}
	updated, _ := model.Update(msg)
	model = updated.(Model)
	msg = eventMsg{event: events.Event{Type: events.UsageUpdated, Usage: &events.UsageDisplay{
		InputTokens:              10,
		OutputTokens:             20,
		CacheCreationInputTokens: 30,
		CacheReadInputTokens:     40,
	}}, events: closedEvents()}
	updated, _ = model.Update(msg)
	model = updated.(Model)
	if model.status.InputTokens != 11 || model.status.OutputTokens != 22 || model.status.CacheCreationInputTokens != 33 || model.status.CacheReadInputTokens != 44 {
		t.Fatalf("usage was not accumulated: %#v", model.status)
	}
}

func closedEvents() <-chan Event {
	ch := make(chan Event)
	close(ch)
	return ch
}
