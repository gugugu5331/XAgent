package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPTransportJSONResponseAndSessionReuse(t *testing.T) {
	var sawSession string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.Header.Get(headerContentType) != contentTypeJSON {
			t.Fatalf("unexpected content-type: %q", r.Header.Get(headerContentType))
		}
		if r.Header.Get(headerAccept) != acceptStreamableHTTPMCP {
			t.Fatalf("unexpected accept: %q", r.Header.Get(headerAccept))
		}
		sawSession = r.Header.Get(headerSessionID)
		w.Header().Set(headerContentType, contentTypeJSON)
		w.Header().Set(headerSessionID, "session-1")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer server.Close()

	transport, err := NewHTTPTransport(HTTPConfig{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Send(context.Background(), RPCRequest{JSONRPC: "2.0", ID: NumberID(1), Method: "ping"}); err != nil {
		t.Fatal(err)
	}
	response := receiveHTTPResponse(t, transport)
	if response.ID.key() != NumberID(1).key() || transport.SessionID() != "session-1" {
		t.Fatalf("unexpected response/session: %#v %q", response, transport.SessionID())
	}
	if err := transport.Send(context.Background(), RPCRequest{JSONRPC: "2.0", ID: NumberID(2), Method: "ping"}); err != nil {
		t.Fatal(err)
	}
	if sawSession != "session-1" {
		t.Fatalf("expected reused session id, got %q", sawSession)
	}
}

func TestHTTPTransportSSEResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentType, contentTypeEventStream)
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n"))
	}))
	defer server.Close()
	transport, err := NewHTTPTransport(HTTPConfig{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Send(context.Background(), RPCRequest{JSONRPC: "2.0", ID: NumberID(1), Method: "ping"}); err != nil {
		t.Fatal(err)
	}
	if response := receiveHTTPResponse(t, transport); response.ID.key() != NumberID(1).key() {
		t.Fatalf("unexpected SSE response: %#v", response)
	}
}

func TestHTTPTransportAcceptedNotificationHasNoResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	transport, err := NewHTTPTransport(HTTPConfig{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Send(context.Background(), RPCNotification{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-transport.Recv():
		t.Fatalf("unexpected response for 202: %#v", response)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestHTTPTransportStatusErrorsAndCancel(t *testing.T) {
	errorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer errorServer.Close()
	transport, err := NewHTTPTransport(HTTPConfig{URL: errorServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Send(context.Background(), RPCRequest{JSONRPC: "2.0", ID: NumberID(1), Method: "ping"}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
	}

	serverError := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer serverError.Close()
	transport, err = NewHTTPTransport(HTTPConfig{URL: serverError.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Send(context.Background(), RPCRequest{JSONRPC: "2.0", ID: NumberID(1), Method: "ping"}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected 500 error, got %v", err)
	}

	transport, err = NewHTTPTransport(HTTPConfig{
		URL: "http://localhost/mcp",
		Client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := transport.Send(ctx, RPCRequest{JSONRPC: "2.0", ID: NumberID(1), Method: "slow"}); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestHTTPTransportRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"text":"too-large"}}`))
	}))
	defer server.Close()
	transport, err := NewHTTPTransport(HTTPConfig{URL: server.URL, MaxResponseBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Send(context.Background(), RPCRequest{JSONRPC: "2.0", ID: NumberID(1), Method: "ping"}); err == nil {
		t.Fatal("expected oversized response error")
	}
}

func TestHTTPTransportRejectsUnsafeURLAndHeaders(t *testing.T) {
	if _, err := NewHTTPTransport(HTTPConfig{URL: "http://example.invalid/mcp"}); err == nil {
		t.Fatal("expected non-localhost http URL to be rejected")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer server.Close()
	transport, err := NewHTTPTransport(HTTPConfig{URL: server.URL, Headers: map[string]string{"Host": "evil"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := transport.Send(context.Background(), RPCRequest{JSONRPC: "2.0", ID: NumberID(1), Method: "ping"}); err == nil {
		t.Fatal("expected blocked Host header error")
	}
}

func TestHTTPTransportDoesNotLeakAuthorizationAcrossRedirect(t *testing.T) {
	authorizationSeen := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizationSeen <- r.Header.Get("Authorization")
		w.Header().Set(headerContentType, contentTypeJSON)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	transport, err := NewHTTPTransport(HTTPConfig{URL: redirector.URL, Headers: map[string]string{"Authorization": "Bearer secret"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = transport.Send(context.Background(), RPCRequest{JSONRPC: "2.0", ID: NumberID(1), Method: "ping"})
	select {
	case got := <-authorizationSeen:
		if got != "" {
			t.Fatalf("authorization leaked across redirect: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("redirect target was not reached")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func receiveHTTPResponse(t *testing.T, transport *HTTPTransport) RPCResponse {
	t.Helper()
	select {
	case response := <-transport.Recv():
		data, _ := json.Marshal(response)
		if len(data) == 0 {
			t.Fatal("empty response")
		}
		return response
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for http response")
	}
	return RPCResponse{}
}
