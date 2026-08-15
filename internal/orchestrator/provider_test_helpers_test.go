package orchestrator

import (
	"context"
	"sync"
	"sync/atomic"

	"xagent/internal/diagnostics"
	"xagent/internal/provider"
	"xagent/internal/redact"
	"xagent/internal/testutil"
)

var orchestratorTestRedactor = redact.NewRuntimeRedactor()

func newConversationTestStore(string) (*testutil.ScriptedStore, error) {
	return &testutil.ScriptedStore{}, nil
}

func testSafeText(value string) redact.SafeText {
	return orchestratorTestRedactor.Redact(value)
}

func testSafeProviderError(err error) *diagnostics.SafeError {
	if err == nil {
		return nil
	}
	return &diagnostics.SafeError{
		Code:        "test_provider_error",
		Source:      "orchestrator.test",
		Message:     testSafeText(err.Error()),
		Recoverable: true,
	}
}

func testSafeToolCall(id string, name string, argumentsJSON string) *provider.SafeToolCall {
	return &provider.SafeToolCall{
		ID:            id,
		Name:          name,
		ArgumentsJSON: testSafeText(argumentsJSON),
	}
}

type orchestratorTestChatStream struct {
	events     <-chan provider.StreamEvent
	closeFn    func()
	closeOnce  sync.Once
	closeCalls atomic.Int64
}

func newOrchestratorTestChatStream(events ...provider.StreamEvent) *orchestratorTestChatStream {
	streamEvents := make(chan provider.StreamEvent, len(events))
	for _, event := range events {
		streamEvents <- event
	}
	close(streamEvents)
	return newOrchestratorTestChatStreamFromChannel(streamEvents, nil)
}

func newOrchestratorTestChatStreamFromChannel(events <-chan provider.StreamEvent, closeFn func()) *orchestratorTestChatStream {
	return &orchestratorTestChatStream{events: events, closeFn: closeFn}
}

func (stream *orchestratorTestChatStream) Events() <-chan provider.StreamEvent {
	if stream == nil {
		return nil
	}
	return stream.events
}

func (stream *orchestratorTestChatStream) Close(context.Context) error {
	if stream == nil {
		return nil
	}
	stream.closeCalls.Add(1)
	stream.closeOnce.Do(func() {
		if stream.closeFn != nil {
			stream.closeFn()
		}
	})
	return nil
}

func (stream *orchestratorTestChatStream) CloseCalls() int64 {
	if stream == nil {
		return 0
	}
	return stream.closeCalls.Load()
}

var _ provider.ChatStream = (*orchestratorTestChatStream)(nil)
