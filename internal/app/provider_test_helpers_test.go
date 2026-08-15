package app

import (
	"context"
	"sync"

	"xagent/internal/diagnostics"
	"xagent/internal/provider"
	"xagent/internal/redact"
)

var appTestRedactor = redact.NewRuntimeRedactor()

func appSafeText(value string) redact.SafeText {
	return appTestRedactor.Redact(value)
}

func appSafeError(code string, message string) *diagnostics.SafeError {
	return &diagnostics.SafeError{
		Code:    code,
		Source:  "app.test",
		Message: appSafeText(message),
	}
}

type appTestStreamTracker struct {
	mu         sync.Mutex
	closeCalls int
}

func (tracker *appTestStreamTracker) recordClose() {
	if tracker == nil {
		return
	}
	tracker.mu.Lock()
	tracker.closeCalls++
	tracker.mu.Unlock()
}

func (tracker *appTestStreamTracker) CloseCalls() int {
	if tracker == nil {
		return 0
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.closeCalls
}

type appTestChatStream struct {
	events  <-chan provider.StreamEvent
	tracker *appTestStreamTracker
	once    sync.Once
}

func newAppTestChatStream(script []provider.StreamEvent, tracker *appTestStreamTracker) *appTestChatStream {
	events := make(chan provider.StreamEvent, len(script))
	for _, event := range script {
		events <- event
	}
	close(events)
	return newAppTestChatStreamChannel(events, tracker)
}

func newAppTestChatStreamChannel(events <-chan provider.StreamEvent, tracker *appTestStreamTracker) *appTestChatStream {
	return &appTestChatStream{events: events, tracker: tracker}
}

func (stream *appTestChatStream) Events() <-chan provider.StreamEvent {
	if stream == nil {
		return nil
	}
	return stream.events
}

func (stream *appTestChatStream) Close(context.Context) error {
	if stream == nil {
		return nil
	}
	stream.once.Do(stream.tracker.recordClose)
	return nil
}
