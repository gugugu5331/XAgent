package testutil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

type FakeProviderServer struct {
	Server *httptest.Server
	mu     sync.Mutex
	Seen   []FakeProviderRequest
	Mode   FakeProviderMode
	Reply  string
	Delay  time.Duration
}

type FakeProviderMode string

const (
	FakeProviderSuccess      FakeProviderMode = "success"
	FakeProviderBlock        FakeProviderMode = "block"
	FakeProviderHTTP401      FakeProviderMode = "http_401"
	FakeProviderToolBashFail FakeProviderMode = "tool_bash_fail"
)

type FakeProviderRequest struct {
	Authorization string
	Body          string
}

func NewFakeProviderServer(mode FakeProviderMode) *FakeProviderServer {
	fake := &FakeProviderServer{Mode: mode, Reply: "OK from fake provider"}
	fake.Server = httptest.NewServer(http.HandlerFunc(fake.handle))
	return fake
}

func (f *FakeProviderServer) URL() string {
	if f == nil || f.Server == nil {
		return ""
	}
	return f.Server.URL
}

func (f *FakeProviderServer) Close() {
	if f != nil && f.Server != nil {
		f.Server.Close()
	}
}

func (f *FakeProviderServer) Requests() []FakeProviderRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := make([]FakeProviderRequest, len(f.Seen))
	copy(items, f.Seen)
	return items
}

func (f *FakeProviderServer) handle(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	data, _ := json.Marshal(body)
	f.mu.Lock()
	f.Seen = append(f.Seen, FakeProviderRequest{Authorization: r.Header.Get("Authorization"), Body: string(data)})
	f.mu.Unlock()

	switch f.Mode {
	case FakeProviderBlock:
		delay := f.Delay
		if delay <= 0 {
			delay = 10 * time.Second
		}
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
		}
		return
	case FakeProviderHTTP401:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Authorization: Bearer abc123 sk-test-secret"}`))
		return
	case FakeProviderToolBashFail:
		f.handleToolBashFail(w, body)
		return
	default:
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, f.Reply))
		writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		writeSSE(w, "[DONE]")
	}
}

func (f *FakeProviderServer) handleToolBashFail(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	if hasToolResult(body) {
		writeSSE(w, `{"choices":[{"delta":{"content":"我看到了 Bash 失败摘要。"}}]}`)
		writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2}}`)
		writeSSE(w, "[DONE]")
		return
	}
	writeSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_bash_fail","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"printf 'api_key=secret-key'; printf 'Authorization: Bearer abc123' >&2; exit 7\"}"}}]},"finish_reason":"tool_calls"}]}`)
	writeSSE(w, "[DONE]")
}

func hasToolResult(body map[string]any) bool {
	messages, _ := body["messages"].([]any)
	for _, item := range messages {
		message, _ := item.(map[string]any)
		if role, _ := message["role"].(string); role == "tool" {
			return true
		}
	}
	return false
}

func writeSSE(w http.ResponseWriter, data string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
