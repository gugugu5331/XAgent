package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xagent/internal/config"
)

func TestAnthropicClosesSDKStreamOnEveryExit(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		body := newTrackingAnthropicBody(strings.NewReader(
			anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"visible `+providerSecretCanary+`"}}`) +
				anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"thinking `+providerSecretCanary+`"}}`) +
				anthropicSSE("message_stop", `{"type":"message_stop"}`),
		))
		stream := startTrackingAnthropicStream(t, context.Background(), body, config.StreamConfig{})

		var events []StreamEvent
		for event := range stream.Events() {
			assertProviderCanaryAbsent(t, "Anthropic normal exit event", event)
			events = append(events, event)
		}
		if len(events) != 3 {
			t.Fatalf("events = %#v, want text, thinking, done", events)
		}
		if events[0].Type != StreamEventTextDelta || events[0].Delta.Text() != "visible [redacted]" {
			t.Fatalf("text event = %#v", events[0])
		}
		if events[1].Type != StreamEventThinkingDelta || events[1].Delta.Text() != "thinking [redacted]" {
			t.Fatalf("thinking event = %#v", events[1])
		}
		if events[2].Type != StreamEventDone {
			t.Fatalf("terminal event = %#v, want Done", events[2])
		}
		closeTrackingAnthropicStream(t, stream)
		body.assertClosedOnce(t)
	})

	t.Run("SDK error", func(t *testing.T) {
		body := newTrackingAnthropicBody(strings.NewReader(anthropicSSE(
			"error",
			`{"type":"error","error":{"type":"api_error","message":"injected SDK stream failure `+providerSecretCanary+`"}}`,
		)))
		stream := startTrackingAnthropicStream(t, context.Background(), body, config.StreamConfig{})
		assertAnthropicStreamErrorWithoutDone(t, stream, "anthropic_stream_failed")
		closeTrackingAnthropicStream(t, stream)
		body.assertClosedOnce(t)
	})

	t.Run("cancellation", func(t *testing.T) {
		body := newBlockingAnthropicBody()
		ctx, cancel := context.WithCancel(context.Background())
		stream := startTrackingAnthropicStream(t, ctx, body, config.StreamConfig{})
		select {
		case <-body.started:
		case <-time.After(time.Second):
			t.Fatal("Anthropic SDK did not start reading the response body")
		}
		cancel()
		closeTrackingAnthropicStream(t, stream)
		body.assertClosedOnce(t)
	})

	t.Run("consumer exits early", func(t *testing.T) {
		body := newTrackingAnthropicBody(strings.NewReader(
			anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"first `+providerSecretCanary+`"}}`) +
				anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"second"}}`) +
				anthropicSSE("message_stop", `{"type":"message_stop"}`),
		))
		stream := startTrackingAnthropicStream(t, context.Background(), body, config.StreamConfig{})
		select {
		case event := <-stream.Events():
			assertProviderCanaryAbsent(t, "Anthropic consumer early exit event", event)
			if event.Type != StreamEventTextDelta || event.Delta.Text() != "first [redacted]" {
				t.Fatalf("first event = %#v", event)
			}
		case <-time.After(time.Second):
			t.Fatal("Anthropic provider did not publish the first event")
		}
		closeTrackingAnthropicStream(t, stream)
		body.assertClosedOnce(t)
	})

	t.Run("text budget", func(t *testing.T) {
		body := newTrackingAnthropicBody(strings.NewReader(
			anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"four `+providerSecretCanary+`"}}`) +
				anthropicSSE("message_stop", `{"type":"message_stop"}`),
		))
		stream := startTrackingAnthropicStream(t, context.Background(), body, config.StreamConfig{MaxTextBytes: 3})
		assertAnthropicStreamErrorWithoutDone(t, stream, "anthropic_stream_text_limit")
		closeTrackingAnthropicStream(t, stream)
		body.assertClosedOnce(t)
	})

	t.Run("tool arguments budget", func(t *testing.T) {
		body := newTrackingAnthropicBody(strings.NewReader(
			anthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-1","name":"Read","input":{}}}`) +
				anthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"four `+providerSecretCanary+`"}}`) +
				anthropicSSE("message_stop", `{"type":"message_stop"}`),
		))
		stream := startTrackingAnthropicStream(t, context.Background(), body, config.StreamConfig{MaxToolArgumentsBytes: 3})
		assertAnthropicStreamErrorWithoutDone(t, stream, "anthropic_stream_tool_arguments_limit")
		closeTrackingAnthropicStream(t, stream)
		body.assertClosedOnce(t)
	})
}

func startTrackingAnthropicStream(t *testing.T, ctx context.Context, body io.ReadCloser, streamConfig config.StreamConfig) ChatStream {
	t.Helper()
	client := &http.Client{Transport: anthropicStreamRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    request,
		}, nil
	})}
	provider := NewAnthropic(config.LLMConfig{
		Model:   "test",
		BaseURL: "https://example.test",
		APIKey:  providerSecretCanary,
		Stream:  streamConfig,
	}, borrowProviderTestClient(client), providerTestRuntimeRedactor())
	stream, err := provider.StreamChat(ctx, ChatRequest{})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	return stream
}

func closeTrackingAnthropicStream(t *testing.T, stream ChatStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := stream.Close(ctx); err != nil {
		assertProviderCanaryAbsent(t, "Anthropic stream close error", err)
		t.Fatalf("close Anthropic stream: %v", err)
	}
	select {
	case _, open := <-stream.Events():
		if open {
			t.Fatal("Anthropic producer remained active after Close returned")
		}
	default:
		t.Fatal("Anthropic event channel remained open after Close returned")
	}
}

func assertAnthropicStreamErrorWithoutDone(t *testing.T, stream ChatStream, wantCode string) {
	t.Helper()
	seenCode := ""
	for event := range stream.Events() {
		assertProviderCanaryAbsent(t, "Anthropic failed exit event", event)
		switch event.Type {
		case StreamEventDone:
			t.Fatal("failed Anthropic stream published normal Done")
		case StreamEventTextDelta, StreamEventThinkingDelta, StreamEventToolCall:
			t.Fatalf("failed Anthropic stream published payload event: %#v", event)
		case StreamEventError:
			if event.Error != nil {
				seenCode = event.Error.Code
			}
		}
	}
	if seenCode != wantCode {
		t.Fatalf("safe error code = %q, want %q", seenCode, wantCode)
	}
}

func anthropicSSE(eventType, data string) string {
	return "event: " + eventType + "\ndata: " + data + "\n\n"
}

type anthropicStreamRoundTripFunc func(*http.Request) (*http.Response, error)

func (f anthropicStreamRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type trackingAnthropicBody struct {
	reader     io.Reader
	closeCount atomic.Int32
	closeOnce  sync.Once
	closed     chan struct{}
}

func newTrackingAnthropicBody(reader io.Reader) *trackingAnthropicBody {
	return &trackingAnthropicBody{reader: reader, closed: make(chan struct{})}
}

func (b *trackingAnthropicBody) Read(buffer []byte) (int, error) {
	return b.reader.Read(buffer)
}

func (b *trackingAnthropicBody) Close() error {
	b.closeCount.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func (b *trackingAnthropicBody) assertClosedOnce(t *testing.T) {
	t.Helper()
	if got := b.closeCount.Load(); got != 1 {
		t.Fatalf("Anthropic SDK body close count = %d, want 1", got)
	}
}

type blockingAnthropicBody struct {
	startedOnce sync.Once
	closeOnce   sync.Once
	closeCount  atomic.Int32
	started     chan struct{}
	closed      chan struct{}
}

func newBlockingAnthropicBody() *blockingAnthropicBody {
	return &blockingAnthropicBody{started: make(chan struct{}), closed: make(chan struct{})}
}

func (b *blockingAnthropicBody) Read([]byte) (int, error) {
	b.startedOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("tracking Anthropic SDK body closed " + providerSecretCanary)
}

func (b *blockingAnthropicBody) Close() error {
	b.closeCount.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func (b *blockingAnthropicBody) assertClosedOnce(t *testing.T) {
	t.Helper()
	if got := b.closeCount.Load(); got != 1 {
		t.Fatalf("Anthropic SDK body close count = %d, want 1", got)
	}
}
