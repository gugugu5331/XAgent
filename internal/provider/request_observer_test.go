package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/config"
)

type recordingRequestObserver struct {
	mu     sync.Mutex
	events []string
}

func (o *recordingRequestObserver) MarkSent() {
	o.mu.Lock()
	o.events = append(o.events, "sent")
	o.mu.Unlock()
}

func (o *recordingRequestObserver) Finish(sent bool) {
	o.mu.Lock()
	if sent {
		o.events = append(o.events, "finish:true")
	} else {
		o.events = append(o.events, "finish:false")
	}
	o.mu.Unlock()
}

func (o *recordingRequestObserver) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

func TestRequestObserverContract(t *testing.T) {
	attempt := newRequestAttempt(nil)
	attempt.markSent()
	attempt.finish()
	attempt.finish()

	recorder := &recordingRequestObserver{}
	attempt = newRequestAttempt(recorder)
	attempt.markSent()
	attempt.markSent()
	attempt.finish()
	attempt.finish()
	assertObserverEvents(t, recorder, "sent", "finish:true")

	recorder = &recordingRequestObserver{}
	newRequestAttempt(recorder).finish()
	assertObserverEvents(t, recorder, "finish:false")

	// A strict observer can detect a Provider contract violation instead of
	// requestAttempt silently hiding a late transport callback.
	recorder = &recordingRequestObserver{}
	attempt = newRequestAttempt(recorder)
	attempt.finish()
	attempt.markSent()
	assertObserverEvents(t, recorder, "finish:false", "sent")
}

func TestOpenAIRequestAttempt(t *testing.T) {
	t.Run("pre-send failure", func(t *testing.T) {
		recorder := &recordingRequestObserver{}
		provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: "://bad", APIKey: "test"}, nil)
		if _, err := provider.StreamChat(context.Background(), ChatRequest{Observer: recorder}); err == nil {
			t.Fatal("invalid endpoint unexpectedly succeeded")
		}
		assertObserverEvents(t, recorder, "finish:false")
	})

	t.Run("post-write success", func(t *testing.T) {
		recorder := &recordingRequestObserver{}
		client := &http.Client{Transport: observerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteRequest != nil {
				trace.WroteRequest(httptrace.WroteRequestInfo{})
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
				Request:    request,
			}, nil
		})}
		provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: "https://example.test", APIKey: "test"}, client)
		stream, err := provider.StreamChat(context.Background(), ChatRequest{Observer: recorder})
		if err != nil {
			t.Fatal(err)
		}
		seenTerminal := false
		for event := range stream {
			if event.Type == StreamEventDone {
				seenTerminal = true
				assertObserverEvents(t, recorder, "sent", "finish:true")
			}
		}
		if !seenTerminal {
			t.Fatal("OpenAI stream closed without terminal event")
		}
		assertObserverEvents(t, recorder, "sent", "finish:true")
	})

	t.Run("http status after write", func(t *testing.T) {
		recorder := &recordingRequestObserver{}
		client := &http.Client{Transport: observerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("denied")),
				Request:    request,
			}, nil
		})}
		provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: "https://example.test", APIKey: "test"}, client)
		if _, err := provider.StreamChat(context.Background(), ChatRequest{Observer: recorder}); err == nil {
			t.Fatal("HTTP status failure unexpectedly succeeded")
		}
		assertObserverEvents(t, recorder, "sent", "finish:true")
	})

	t.Run("stream error after write", func(t *testing.T) {
		recorder := &recordingRequestObserver{}
		client := &http.Client{Transport: observerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: not-json\n\n")),
				Request:    request,
			}, nil
		})}
		provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: "https://example.test", APIKey: "test"}, client)
		stream, err := provider.StreamChat(context.Background(), ChatRequest{Observer: recorder})
		if err != nil {
			t.Fatal(err)
		}
		seenError := false
		for event := range stream {
			if event.Type == StreamEventError {
				seenError = true
				assertObserverEvents(t, recorder, "sent", "finish:true")
			}
		}
		if !seenError {
			t.Fatal("invalid SSE payload did not emit a stream error")
		}
		assertObserverEvents(t, recorder, "sent", "finish:true")
	})
}

func TestAnthropicRequestAttempt(t *testing.T) {
	recorder := &recordingRequestObserver{}
	client := &http.Client{Transport: observerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","message":"test"}}`)),
			Request:    request,
		}, nil
	})}
	provider := NewAnthropic(config.LLMConfig{Model: "test", BaseURL: "https://example.test", APIKey: "test"}, client)
	stream, err := provider.StreamChat(context.Background(), ChatRequest{Observer: recorder})
	if err != nil {
		t.Fatal(err)
	}
	seenTerminal := false
	for event := range stream {
		if event.Type == StreamEventError {
			seenTerminal = true
			assertObserverEvents(t, recorder, "sent", "finish:true")
		}
	}
	if !seenTerminal {
		t.Fatal("Anthropic stream closed without terminal error event")
	}
	assertObserverEvents(t, recorder, "sent", "finish:true")
}

func TestRequestObserverBarrier(t *testing.T) {
	t.Run("cancel before write", func(t *testing.T) {
		entered := make(chan struct{})
		client := &http.Client{Transport: observerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			close(entered)
			<-request.Context().Done()
			return nil, request.Context().Err()
		})}
		recorder := &recordingRequestObserver{}
		ctx, cancel := context.WithCancel(context.Background())
		provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: "https://example.test", APIKey: "test"}, client)
		done := make(chan error, 1)
		go func() {
			_, err := provider.StreamChat(ctx, ChatRequest{Observer: recorder})
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("transport did not start")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) && (err == nil || !strings.Contains(err.Error(), context.Canceled.Error())) {
			t.Fatalf("StreamChat error = %v, want cancellation", err)
		}
		assertObserverEvents(t, recorder, "finish:false")
	})

	t.Run("write callback finishes before terminal", func(t *testing.T) {
		entered := make(chan struct{})
		releaseWrite := make(chan struct{})
		callbackDone := make(chan struct{})
		client := &http.Client{Transport: observerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(request.Context())
			close(entered)
			<-releaseWrite
			trace.WroteRequest(httptrace.WroteRequestInfo{})
			close(callbackDone)
			return nil, context.Canceled
		})}
		recorder := &recordingRequestObserver{}
		provider := NewOpenAI(config.LLMConfig{Model: "test", BaseURL: "https://example.test", APIKey: "test"}, client)
		done := make(chan error, 1)
		go func() {
			_, err := provider.StreamChat(context.Background(), ChatRequest{Observer: recorder})
			done <- err
		}()
		<-entered
		close(releaseWrite)
		if err := <-done; err == nil {
			t.Fatal("transport error unexpectedly succeeded")
		}
		select {
		case <-callbackDone:
		default:
			t.Fatal("Finish returned before write callback became quiescent")
		}
		assertObserverEvents(t, recorder, "sent", "finish:true")
	})
}

func assertObserverEvents(t *testing.T, observer *recordingRequestObserver, wants ...string) {
	t.Helper()
	got := observer.snapshot()
	if len(got) != len(wants) {
		t.Fatalf("observer events = %#v, want %#v", got, wants)
	}
	for index := range wants {
		if got[index] != wants[index] {
			t.Fatalf("observer events = %#v, want %#v", got, wants)
		}
	}
}

type observerRoundTripFunc func(*http.Request) (*http.Response, error)

func (f observerRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
