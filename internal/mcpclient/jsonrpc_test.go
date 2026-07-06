package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRPCIDDifferentiatesStringAndNumber(t *testing.T) {
	var stringID RPCID
	if err := json.Unmarshal([]byte(`"1"`), &stringID); err != nil {
		t.Fatal(err)
	}
	var numberID RPCID
	if err := json.Unmarshal([]byte(`1`), &numberID); err != nil {
		t.Fatal(err)
	}
	if stringID.key() == numberID.key() {
		t.Fatalf("expected distinct keys for string and number ids: %q", stringID.key())
	}
}

func TestConnectionMatchesOutOfOrderResponses(t *testing.T) {
	transport := newMemoryTransport()
	connection := NewConnection(transport)
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([]int, 3)
	for index := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			var result struct {
				Value int `json:"value"`
			}
			if err := connection.Request(ctx, "test/method", map[string]int{"index": index}, &result); err != nil {
				t.Errorf("request %d failed: %v", index, err)
				return
			}
			results[index] = result.Value
		}(index)
	}

	requests := transport.waitForRequests(t, 3)
	for index := len(requests) - 1; index >= 0; index-- {
		request := requests[index]
		var params struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(map[string]int{"value": params.Index})
		if err != nil {
			t.Fatal(err)
		}
		connection.HandleResponse(RPCResponse{
			JSONRPC: "2.0",
			ID:      request.ID,
			Result:  data,
		})
	}
	wg.Wait()

	for index, result := range results {
		if result != index {
			t.Fatalf("request %d got result %d", index, result)
		}
	}
	if connection.PendingCount() != 0 {
		t.Fatalf("pending map not cleaned up: %d", connection.PendingCount())
	}
}

func TestConnectionHandlesSuccessAndErrorMix(t *testing.T) {
	transport := newMemoryTransport()
	connection := NewConnection(transport)

	var successErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		var result struct {
			OK bool `json:"ok"`
		}
		successErr = connection.Request(context.Background(), "ok", nil, &result)
		if !result.OK {
			t.Errorf("expected ok result")
		}
	}()
	request := transport.waitForRequests(t, 1)[0]
	connection.HandleResponse(RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{"ok":true}`)})
	<-done
	if successErr != nil {
		t.Fatalf("success request failed: %v", successErr)
	}

	errorDone := make(chan error, 1)
	go func() {
		errorDone <- connection.Request(context.Background(), "fail", nil, nil)
	}()
	request = transport.waitForRequests(t, 1)[0]
	connection.HandleResponse(RPCResponse{JSONRPC: "2.0", ID: request.ID, Error: &RPCError{Code: -32000, Message: "boom"}})
	var rpcErr *RPCError
	if err := <-errorDone; !errors.As(err, &rpcErr) || rpcErr.Code != -32000 {
		t.Fatalf("expected rpc error, got %v", err)
	}
}

func TestConnectionRecordsUnknownAndInvalidResponses(t *testing.T) {
	connection := NewConnection(newMemoryTransport())
	connection.HandleResponse(RPCResponse{JSONRPC: "2.0", ID: StringID("missing"), Result: json.RawMessage(`{}`)})
	connection.HandleResponse(RPCResponse{JSONRPC: "1.0", ID: NumberID(1), Result: json.RawMessage(`{}`)})
	if len(connection.ProtocolErrors()) != 2 {
		t.Fatalf("expected two protocol errors, got %#v", connection.ProtocolErrors())
	}
}

func TestConnectionTimeoutCleansPending(t *testing.T) {
	transport := newMemoryTransport()
	connection := NewConnection(transport)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := connection.Request(ctx, "slow", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if connection.PendingCount() != 0 {
		t.Fatalf("pending not cleaned after timeout: %d", connection.PendingCount())
	}
	request := transport.waitForRequests(t, 1)[0]
	connection.HandleResponse(RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)})
	if len(connection.ProtocolErrors()) != 1 {
		t.Fatalf("expected late response to be recorded, got %#v", connection.ProtocolErrors())
	}
}

func TestConnectionNotifySendsNotificationWithoutPending(t *testing.T) {
	transport := newMemoryTransport()
	connection := NewConnection(transport)
	if err := connection.Notify(context.Background(), "notifications/initialized", map[string]string{"x": "y"}); err != nil {
		t.Fatal(err)
	}
	if connection.PendingCount() != 0 {
		t.Fatalf("notification created pending request")
	}
	message := transport.waitForMessages(t, 1)[0]
	if notification, ok := message.(RPCNotification); !ok || notification.Method != "notifications/initialized" {
		t.Fatalf("unexpected notification message: %#v", message)
	}
}

type memoryTransport struct {
	ch   chan any
	recv chan RPCResponse
}

func newMemoryTransport() *memoryTransport {
	return &memoryTransport{ch: make(chan any, 16), recv: make(chan RPCResponse, 16)}
}

func (t *memoryTransport) Send(ctx context.Context, msg any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case t.ch <- msg:
		return nil
	}
}

func (t *memoryTransport) Recv() <-chan RPCResponse {
	return t.recv
}

func (t *memoryTransport) deliver(response RPCResponse) {
	t.recv <- response
}

func (t *memoryTransport) waitForRequests(tb testing.TB, count int) []RPCRequest {
	tb.Helper()
	messages := t.waitForMessages(tb, count)
	requests := make([]RPCRequest, 0, len(messages))
	for _, message := range messages {
		request, ok := message.(RPCRequest)
		if !ok {
			tb.Fatalf("expected request, got %#v", message)
		}
		requests = append(requests, request)
	}
	return requests
}

func (t *memoryTransport) waitForMessages(tb testing.TB, count int) []any {
	tb.Helper()
	messages := make([]any, 0, count)
	for len(messages) < count {
		select {
		case message := <-t.ch:
			messages = append(messages, message)
		case <-time.After(time.Second):
			tb.Fatalf("timed out waiting for %d messages, got %d", count, len(messages))
		}
	}
	return messages
}
