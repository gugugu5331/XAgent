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

func TestOpenAIFinishThenUsageThenDone(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"final text"},"finish_reason":"stop"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20}}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":34}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	client := &http.Client{Transport: openAIStreamRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	provider := NewOpenAI(config.LLMConfig{
		Model:   "test",
		BaseURL: "https://example.test",
		APIKey:  "test",
	}, borrowProviderTestClient(client), providerTestRuntimeRedactor())
	stream, err := provider.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}

	var events []StreamEvent
	for event := range stream.Events() {
		events = append(events, event)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close completed stream: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %#v, want text, one final usage, done", events)
	}
	if events[0].Type != StreamEventTextDelta || events[0].Delta.Text() != "final text" {
		t.Fatalf("first event = %#v, want final text before finish_reason", events[0])
	}
	if events[1].Type != StreamEventUsage || events[1].Usage == nil || events[1].Usage.InputTokens != 12 || events[1].Usage.OutputTokens != 34 {
		t.Fatalf("second event = %#v, want final usage", events[1])
	}
	if events[2].Type != StreamEventDone {
		t.Fatalf("third event = %#v, want done only after [DONE]", events[2])
	}
	usageEvents := 0
	for _, event := range events {
		if event.Type == StreamEventUsage {
			usageEvents++
		}
		if event.Type == StreamEventError {
			t.Fatalf("standard terminal sequence emitted error: %#v", event.Error)
		}
	}
	if usageEvents != 1 {
		t.Fatalf("usage events = %d, want exactly one", usageEvents)
	}
}

func TestOpenAIUnexpectedEOFIsError(t *testing.T) {
	body := newTrackingOpenAIBody(strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":34}}`,
		"",
	}, "\n")))
	provider := newTrackingOpenAIProvider(body)
	stream, err := provider.StreamChat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}

	var safeErrorCode string
	for event := range stream.Events() {
		switch event.Type {
		case StreamEventError:
			if event.Error != nil {
				safeErrorCode = event.Error.Code
			}
		case StreamEventDone:
			t.Fatal("unexpected EOF was reported as normal Done")
		case StreamEventUsage:
			t.Fatal("usage was committed before protocol termination")
		}
	}
	if safeErrorCode != "openai_stream_unexpected_eof" {
		t.Fatalf("safe error code = %q, want openai_stream_unexpected_eof", safeErrorCode)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close truncated stream: %v", err)
	}
	body.assertClosedOnce(t)
}

func TestOpenAIClosesBodyOnEveryExit(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		body := newTrackingOpenAIBody(strings.NewReader("data: [DONE]\n\n"))
		stream := startTrackingOpenAIStream(t, context.Background(), body)
		seenDone := false
		for event := range stream.Events() {
			assertProviderCanaryAbsent(t, "OpenAI normal exit event", event)
			if event.Type == StreamEventDone {
				seenDone = true
			}
			if event.Type == StreamEventError {
				t.Fatalf("normal stream error: %#v", event.Error)
			}
		}
		if !seenDone {
			t.Fatal("normal stream did not publish Done")
		}
		closeTrackingOpenAIStream(t, stream)
		body.assertClosedOnce(t)
	})

	t.Run("parse error", func(t *testing.T) {
		body := newTrackingOpenAIBody(strings.NewReader("data: not-json " + providerSecretCanary + "\n\n"))
		stream := startTrackingOpenAIStream(t, context.Background(), body)
		assertOpenAIStreamErrorWithoutDone(t, stream)
		closeTrackingOpenAIStream(t, stream)
		body.assertClosedOnce(t)
	})

	t.Run("network error", func(t *testing.T) {
		readErr := errors.New("injected provider network failure " + providerSecretCanary)
		body := newTrackingOpenAIBody(&openAIReadErrorReader{
			data: []byte("data: {\"choices\":[]}\n\n"),
			err:  readErr,
		})
		stream := startTrackingOpenAIStream(t, context.Background(), body)
		assertOpenAIStreamErrorWithoutDone(t, stream)
		closeTrackingOpenAIStream(t, stream)
		body.assertClosedOnce(t)
	})

	t.Run("cancellation", func(t *testing.T) {
		body := newBlockingOpenAIBody()
		ctx, cancel := context.WithCancel(context.Background())
		stream := startTrackingOpenAIStream(t, ctx, body)
		select {
		case <-body.started:
		case <-time.After(time.Second):
			t.Fatal("provider did not start reading response body")
		}
		cancel()
		closeTrackingOpenAIStream(t, stream)
		body.assertClosedOnce(t)
	})

	t.Run("consumer exits early", func(t *testing.T) {
		body := newTrackingOpenAIBody(strings.NewReader(strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"first ` + providerSecretCanary + `"}}]}`,
			"",
			`data: {"choices":[{"delta":{"content":"second"}}]}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")))
		stream := startTrackingOpenAIStream(t, context.Background(), body)
		select {
		case event := <-stream.Events():
			assertProviderCanaryAbsent(t, "OpenAI consumer early exit event", event)
			if event.Type != StreamEventTextDelta || event.Delta.Text() != "first [redacted]" {
				t.Fatalf("first event = %#v", event)
			}
		case <-time.After(time.Second):
			t.Fatal("provider did not publish first event")
		}
		closeTrackingOpenAIStream(t, stream)
		body.assertClosedOnce(t)
	})
}

func newTrackingOpenAIProvider(body io.ReadCloser) *OpenAIProvider {
	client := &http.Client{Transport: openAIStreamRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    request,
		}, nil
	})}
	return NewOpenAI(config.LLMConfig{
		Model:   "test",
		BaseURL: "https://example.test",
		APIKey:  providerSecretCanary,
	}, borrowProviderTestClient(client), providerTestRuntimeRedactor())
}

func startTrackingOpenAIStream(t *testing.T, ctx context.Context, body io.ReadCloser) ChatStream {
	t.Helper()
	stream, err := newTrackingOpenAIProvider(body).StreamChat(ctx, ChatRequest{})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	return stream
}

func closeTrackingOpenAIStream(t *testing.T, stream ChatStream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := stream.Close(ctx); err != nil {
		assertProviderCanaryAbsent(t, "OpenAI stream close error", err)
		t.Fatalf("close stream: %v", err)
	}
}

func assertOpenAIStreamErrorWithoutDone(t *testing.T, stream ChatStream) {
	t.Helper()
	seenError := false
	for event := range stream.Events() {
		assertProviderCanaryAbsent(t, "OpenAI failed exit event", event)
		if event.Type == StreamEventDone {
			t.Fatal("failed stream published normal Done")
		}
		if event.Type == StreamEventError {
			seenError = true
		}
	}
	if !seenError {
		t.Fatal("failed stream did not publish SafeError")
	}
}

type trackingOpenAIBody struct {
	reader     io.Reader
	closeCount atomic.Int32
	closeOnce  sync.Once
	closed     chan struct{}
}

func newTrackingOpenAIBody(reader io.Reader) *trackingOpenAIBody {
	return &trackingOpenAIBody{reader: reader, closed: make(chan struct{})}
}

func (b *trackingOpenAIBody) Read(buffer []byte) (int, error) {
	return b.reader.Read(buffer)
}

func (b *trackingOpenAIBody) Close() error {
	b.closeCount.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func (b *trackingOpenAIBody) assertClosedOnce(t *testing.T) {
	t.Helper()
	if got := b.closeCount.Load(); got != 1 {
		t.Fatalf("response body close count = %d, want 1", got)
	}
}

type blockingOpenAIBody struct {
	startedOnce sync.Once
	closeOnce   sync.Once
	closeCount  atomic.Int32
	started     chan struct{}
	closed      chan struct{}
}

func newBlockingOpenAIBody() *blockingOpenAIBody {
	return &blockingOpenAIBody{started: make(chan struct{}), closed: make(chan struct{})}
}

func (b *blockingOpenAIBody) Read([]byte) (int, error) {
	b.startedOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("tracking response body closed " + providerSecretCanary)
}

func (b *blockingOpenAIBody) Close() error {
	b.closeCount.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func (b *blockingOpenAIBody) assertClosedOnce(t *testing.T) {
	t.Helper()
	if got := b.closeCount.Load(); got != 1 {
		t.Fatalf("response body close count = %d, want 1", got)
	}
}

type openAIReadErrorReader struct {
	data []byte
	err  error
}

func (r *openAIReadErrorReader) Read(buffer []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(buffer, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

type openAIStreamRoundTripFunc func(*http.Request) (*http.Response, error)

func (f openAIStreamRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
