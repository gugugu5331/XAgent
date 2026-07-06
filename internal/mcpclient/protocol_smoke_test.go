package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProtocolClientWithFakeStdioServer(t *testing.T) {
	server := buildFakeStdioServer(t)
	transport := NewStdioTransport(StdioConfig{Command: server, Args: []string{"mcp"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := transport.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer closeTransport(t, transport)

	client := NewProtocolClient(transport)
	client.Start(ctx)
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(ctx, 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "echo" || tools[1].Name != "fail" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	result, err := client.CallTool(ctx, "echo", map[string]any{"message": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hello" {
		t.Fatalf("unexpected call result: %#v", result)
	}
	result, err = client.CallTool(ctx, "fail", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("expected isError result: %#v", result)
	}
	if _, err := client.CallTool(ctx, "rpc_error", nil); err == nil {
		t.Fatal("expected fake stdio JSON-RPC error")
	}
}

func TestProtocolClientWithFakeHTTPServer(t *testing.T) {
	var initialized bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Fatal(err)
		}
		w.Header().Set(headerContentType, contentTypeJSON)
		switch message.Method {
		case "initialize":
			writeRPCResult(t, w, message.ID, map[string]any{"protocolVersion": SupportedProtocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake-http"}})
		case "notifications/initialized":
			initialized = true
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if !initialized {
				t.Fatal("tools/list before initialized")
			}
			writeRPCResult(t, w, message.ID, map[string]any{"tools": []map[string]any{{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}})
		case "tools/call":
			writeRPCResult(t, w, message.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}})
		default:
			t.Fatalf("unexpected method %s", message.Method)
		}
	}))
	defer server.Close()

	transport, err := NewHTTPTransport(HTTPConfig{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	client := NewProtocolClient(transport)
	client.Start(context.Background())
	if _, err := client.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(context.Background(), 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	result, err := client.CallTool(context.Background(), "echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "ok" {
		t.Fatalf("unexpected call result: %#v", result)
	}
}

func writeRPCResult(t *testing.T, w http.ResponseWriter, id any, result any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
		t.Fatal(err)
	}
}
