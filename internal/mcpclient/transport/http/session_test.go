package http

import (
	"context"
	"io"
	stdhttp "net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"xagent/internal/mcpclient/transport"
	"xagent/internal/netpolicy"
)

func TestHTTPRemoteSessionTracksAndDeletesLatestID(t *testing.T) {
	client := &remoteSessionClient{responses: []*stdhttp.Response{
		remoteSessionResponse(stdhttp.StatusAccepted, "session-one"),
		remoteSessionResponse(stdhttp.StatusAccepted, "session-two"),
		remoteSessionResponse(stdhttp.StatusNoContent, ""),
	}}
	created, err := New(Config{
		Endpoint:        testMCPEndpoint(t),
		ClientFactory:   &recordingClientFactory{client: client},
		Headers:         map[string]string{"Authorization": "Bearer test-secret"},
		ProtocolVersion: "2025-06-18",
		Lifecycle:       transport.Options{Diagnostics: &discardSink{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	frame := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if err := created.Send(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	if err := created.Send(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	if err := created.CloseRemote(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := created.CloseRemote(context.Background()); err != nil {
		t.Fatalf("repeat remote close after successful delete: %v", err)
	}

	requests := client.snapshot()
	if len(requests) != 3 {
		t.Fatalf("HTTP requests = %d, want 3", len(requests))
	}
	if got := requests[0].Header.Get(headerSessionID); got != "" {
		t.Fatalf("first POST session id = %q, want empty", got)
	}
	if got := requests[1].Header.Get(headerSessionID); got != "session-one" {
		t.Fatalf("second POST session id = %q, want session-one", got)
	}
	deleted := requests[2]
	if deleted.Method != stdhttp.MethodDelete || deleted.Header.Get(headerSessionID) != "session-two" {
		t.Fatalf("remote cleanup request = %s session=%q", deleted.Method, deleted.Header.Get(headerSessionID))
	}
	if deleted.Header.Get(headerProtocolVersion) != "2025-06-18" ||
		deleted.Header.Get("Authorization") != "Bearer test-secret" {
		t.Fatalf("remote cleanup omitted frozen headers: %#v", deleted.Header)
	}
	if client.closeCalls.Load() != 0 {
		t.Fatal("CloseRemote closed the owned client")
	}
	if err := created.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.closeCalls.Load() != 1 {
		t.Fatalf("local client closes = %d, want 1", client.closeCalls.Load())
	}
}

func TestHTTPRemoteSessionRejectsUnsafeIDsAndBoundsErrors(t *testing.T) {
	for _, value := range []string{"", " leading", "trailing ", "contains\tspace", "contains\nnewline", strings.Repeat("x", maxSessionIDBytes+1)} {
		if validSessionID(value) {
			t.Fatalf("unsafe session id was accepted: %q", value)
		}
	}
	if !validSessionID("opaque-session_123.~") {
		t.Fatal("visible ASCII session id was rejected")
	}

	const canary = "remote-session-secret-canary"
	client := &remoteSessionClient{responses: []*stdhttp.Response{
		remoteSessionResponse(stdhttp.StatusAccepted, "safe-session"),
		remoteSessionResponse(stdhttp.StatusInternalServerError, canary),
	}}
	created, err := New(Config{
		Endpoint:      testMCPEndpoint(t),
		ClientFactory: &recordingClientFactory{client: client},
		Lifecycle:     transport.Options{Diagnostics: &discardSink{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := created.Send(context.Background(), []byte(`{"jsonrpc":"2.0","method":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	if err := created.CloseRemote(context.Background()); err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("unsafe remote cleanup error = %v", err)
	}
	if err := created.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type remoteSessionClient struct {
	mu         sync.Mutex
	responses  []*stdhttp.Response
	requests   []*stdhttp.Request
	closeCalls atomic.Int64
}

var _ netpolicy.Client = (*remoteSessionClient)(nil)

func (client *remoteSessionClient) Do(request *stdhttp.Request) (*stdhttp.Response, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.requests = append(client.requests, request.Clone(request.Context()))
	if len(client.responses) == 0 {
		return nil, ErrResponseUnavailable
	}
	response := client.responses[0]
	client.responses = client.responses[1:]
	response.Request = request
	return response, nil
}

func (*remoteSessionClient) SDKHTTPClient() *stdhttp.Client { return nil }

func (client *remoteSessionClient) CloseIdleConnections() { client.closeCalls.Add(1) }

func (client *remoteSessionClient) snapshot() []*stdhttp.Request {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]*stdhttp.Request(nil), client.requests...)
}

func remoteSessionResponse(status int, sessionID string) *stdhttp.Response {
	header := make(stdhttp.Header)
	if sessionID != "" {
		header.Set(headerSessionID, sessionID)
	}
	return &stdhttp.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}
