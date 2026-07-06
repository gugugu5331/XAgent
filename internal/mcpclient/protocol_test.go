package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestProtocolInitializeSendsInitializedBeforeList(t *testing.T) {
	transport := newMemoryTransport()
	client := NewProtocolClient(transport)
	client.Start(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := client.Initialize(context.Background())
		done <- err
	}()
	request := transport.waitForRequests(t, 1)[0]
	if request.Method != "initialize" {
		t.Fatalf("expected initialize, got %s", request.Method)
	}
	var initialize InitializeRequest
	if err := json.Unmarshal(request.Params, &initialize); err != nil {
		t.Fatal(err)
	}
	if initialize.ProtocolVersion != SupportedProtocolVersion {
		t.Fatalf("unexpected initialize request: %#v", initialize)
	}
	transport.deliver(RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{"protocolVersion":"` + SupportedProtocolVersion + `","capabilities":{},"serverInfo":{"name":"fake"}}`)})
	message := transport.waitForMessages(t, 1)[0]
	if notification, ok := message.(RPCNotification); !ok || notification.Method != "notifications/initialized" {
		t.Fatalf("expected initialized notification, got %#v", message)
	}
	if err := <-done; err != nil {
		t.Fatalf("initialize failed: %v", err)
	}
}

func TestProtocolRejectsUnsupportedVersion(t *testing.T) {
	transport := newMemoryTransport()
	client := NewProtocolClient(transport)
	client.Start(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Initialize(context.Background())
		done <- err
	}()
	request := transport.waitForRequests(t, 1)[0]
	transport.deliver(RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{"protocolVersion":"old"}`)})
	if err := <-done; err == nil {
		t.Fatal("expected unsupported version error")
	}
}

func TestProtocolListToolsPaginationAndRepeatedCursor(t *testing.T) {
	transport := newMemoryTransport()
	client := NewProtocolClient(transport)
	client.Start(context.Background())
	done := make(chan []RemoteTool, 1)
	errCh := make(chan error, 1)
	go func() {
		tools, err := client.ListTools(context.Background(), 4, 4)
		if err != nil {
			errCh <- err
			return
		}
		done <- tools
	}()
	first := transport.waitForRequests(t, 1)[0]
	if first.Method != "tools/list" {
		t.Fatalf("expected tools/list, got %s", first.Method)
	}
	transport.deliver(RPCResponse{JSONRPC: "2.0", ID: first.ID, Result: json.RawMessage(`{"tools":[{"name":"a","inputSchema":{"type":"object"}}],"nextCursor":"next"}`)})
	second := transport.waitForRequests(t, 1)[0]
	var params ListToolsRequest
	if err := json.Unmarshal(second.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.Cursor != "next" {
		t.Fatalf("expected next cursor, got %#v", params)
	}
	transport.deliver(RPCResponse{JSONRPC: "2.0", ID: second.ID, Result: json.RawMessage(`{"tools":[{"name":"b","inputSchema":{"type":"object"}}]}`)})
	select {
	case tools := <-done:
		if len(tools) != 2 || tools[0].Name != "a" || tools[1].Name != "b" {
			t.Fatalf("unexpected tools: %#v", tools)
		}
	case err := <-errCh:
		t.Fatalf("list tools failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	go func() {
		_, err := client.ListTools(context.Background(), 4, 10)
		errCh <- err
	}()
	repeatFirst := transport.waitForRequests(t, 1)[0]
	transport.deliver(RPCResponse{JSONRPC: "2.0", ID: repeatFirst.ID, Result: json.RawMessage(`{"tools":[],"nextCursor":"same"}`)})
	repeatSecond := transport.waitForRequests(t, 1)[0]
	transport.deliver(RPCResponse{JSONRPC: "2.0", ID: repeatSecond.ID, Result: json.RawMessage(`{"tools":[],"nextCursor":"same"}`)})
	if err := <-errCh; err == nil {
		t.Fatal("expected repeated cursor error")
	}
}

func TestProtocolCallToolSuccessIsErrorAndRPCError(t *testing.T) {
	transport := newMemoryTransport()
	client := NewProtocolClient(transport)
	client.Start(context.Background())
	done := make(chan CallToolResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := client.CallTool(context.Background(), "remote", map[string]any{"q": "x"})
		if err != nil {
			errCh <- err
			return
		}
		done <- result
	}()
	request := transport.waitForRequests(t, 1)[0]
	if request.Method != "tools/call" {
		t.Fatalf("expected tools/call, got %s", request.Method)
	}
	var params CallToolRequest
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.Name != "remote" || params.Arguments["q"] != "x" {
		t.Fatalf("unexpected call params: %#v", params)
	}
	transport.deliver(RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)})
	if result := <-done; len(result.Content) != 1 || result.Content[0].Text != "ok" {
		t.Fatalf("unexpected call result: %#v", result)
	}

	go func() {
		result, err := client.CallTool(context.Background(), "remote", nil)
		if err != nil {
			errCh <- err
			return
		}
		done <- result
	}()
	request = transport.waitForRequests(t, 1)[0]
	transport.deliver(RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"tool failed"}]}`)})
	if result := <-done; !result.IsError {
		t.Fatalf("expected isError result, got %#v", result)
	}

	go func() {
		_, err := client.CallTool(context.Background(), "remote", nil)
		errCh <- err
	}()
	request = transport.waitForRequests(t, 1)[0]
	transport.deliver(RPCResponse{JSONRPC: "2.0", ID: request.ID, Error: &RPCError{Code: -32000, Message: "boom"}})
	var rpcErr *RPCError
	if err := <-errCh; !errors.As(err, &rpcErr) {
		t.Fatalf("expected RPC error, got %v", err)
	}
}
