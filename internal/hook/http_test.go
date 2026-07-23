package hook

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"xagent/internal/redact"
)

func TestHTTPURLValidation(t *testing.T) {
	valid := []string{"https://example.invalid/path", "http://localhost/path", "http://127.0.0.1:8080/path", "http://[::1]/path"}
	for _, raw := range valid {
		if _, err := compileHTTPAction(ActionConfig{Type: ActionHTTP, URL: raw, present: map[string]bool{"type": true, "url": true}}, DefaultLimits(), func(string) (string, bool) { return "", false }, nil); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	invalid := []string{"http://example.com", "ftp://example.com", "/relative", "https://user:pass@example.com", "https://example.com/#fragment", "http://LOCALHOST", "https://example.com/${TOKEN}", "https://example.com/{{event}}"}
	for _, raw := range invalid {
		if _, err := compileHTTPAction(ActionConfig{Type: ActionHTTP, URL: raw, present: map[string]bool{"type": true, "url": true}}, DefaultLimits(), func(string) (string, bool) { return "", false }, nil); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestHTTPURLLengthExactAndPlusOne(t *testing.T) {
	limits := DefaultLimits()
	prefix := "https://example.invalid/"
	atLimit := prefix + strings.Repeat("a", limits.HTTPURLBytes-len(prefix))
	if len(atLimit) != limits.HTTPURLBytes {
		t.Fatalf("URL is %d bytes, want %d", len(atLimit), limits.HTTPURLBytes)
	}
	for _, item := range []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "exact", raw: atLimit},
		{name: "plus one", raw: atLimit + "a", wantErr: true},
	} {
		t.Run(item.name, func(t *testing.T) {
			_, err := compileHTTPAction(ActionConfig{Type: ActionHTTP, URL: item.raw, present: map[string]bool{"type": true, "url": true}}, limits, func(string) (string, bool) { return "", false }, nil)
			if (err != nil) != item.wantErr {
				t.Fatalf("compile error = %v, wantErr %v", err, item.wantErr)
			}
		})
	}
}

func TestHTTPHeaderValidation(t *testing.T) {
	config := ActionConfig{Type: ActionHTTP, URL: "https://example.invalid", Headers: map[string]string{"Authorization": "Bearer ${API_TOKEN}"}, present: map[string]bool{"type": true, "url": true, "headers": true}}
	runtimeRedactor := redact.NewRuntimeRedactor()
	const secret = "http-header-component-canary-12345"
	action, err := compileHTTPAction(config, DefaultLimits(), func(name string) (string, bool) { return secret, name == "API_TOKEN" }, runtimeRedactor)
	if err != nil {
		t.Fatal(err)
	}
	if action.headers.Get("Authorization") != "Bearer "+secret {
		t.Fatalf("header expansion = %q", action.headers.Get("Authorization"))
	}
	if got := runtimeRedactor.Text(secret); got != "[redacted]" {
		t.Fatalf("bare header component was not registered: %q", got)
	}
	if got := runtimeRedactor.Text("Bearer " + secret); got != "Bearer [redacted]" {
		t.Fatalf("registered composite header instead of component: %q", got)
	}
	for _, headers := range []map[string]string{{"Host": "x"}, {"Content-Length": "1"}, {"Bad\nName": "x"}, {"X": "bad\rvalue"}, {"x-a": "1", "X-A": "2"}, {"Content-Type": "application/json"}} {
		config.Headers = headers
		if _, err := compileHTTPAction(config, DefaultLimits(), func(string) (string, bool) { return "", false }, nil); err == nil {
			t.Fatalf("accepted headers %#v", headers)
		}
	}
}

func TestHTTPExpandedHeaderSecretRedactedFromDecisionHandoff(t *testing.T) {
	const secret = "http-response-component-canary-12345"
	authorization := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization <- r.Header.Get("Authorization")
		_, _ = fmt.Fprintf(w, `{"decision":"deny","reason":%q}`, secret)
	}))
	defer server.Close()

	runtimeRedactor := redact.NewRuntimeRedactor()
	config := ActionConfig{
		Type:     ActionHTTP,
		URL:      server.URL,
		Headers:  map[string]string{"Authorization": "Bearer ${API_TOKEN}"},
		Decision: true,
		present:  map[string]bool{"type": true, "url": true, "headers": true, "decision": true},
	}
	action, err := compileAction(EventToolBefore, false, Duration{}, config, DefaultLimits(), func(name string) (string, bool) {
		return secret, name == "API_TOKEN"
	}, runtimeRedactor)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(newSnapshot([]Rule{{
		Event:   EventToolBefore,
		Source:  Source{Path: "header-decision.yaml", Ordinal: 1, EffectiveOrdinal: 1},
		Timeout: action.timeout,
		action:  action,
	}}), EngineOptions{ProjectRoot: t.TempDir(), Redactor: runtimeRedactor})
	if err != nil {
		t.Fatal(err)
	}

	decision := engine.BeforeTool(context.Background(), ExecutionRef{SessionID: "session", ExecutionID: "execution", TurnID: "turn"}, ToolInput{CallID: "call", Name: "Read", Arguments: map[string]any{}})
	if got := <-authorization; got != "Bearer "+secret {
		t.Fatalf("Authorization header = %q", got)
	}
	if !decision.IsDeny() || decision.Reason != "[redacted]" || strings.Contains(decision.Reason, secret) {
		t.Fatalf("unsafe Provider-facing decision = %#v", decision)
	}
}

func TestHTTPSendEventDefaultAndFalse(t *testing.T) {
	falseValue := false
	defaultAction, err := compileHTTPAction(ActionConfig{
		Type:    ActionHTTP,
		URL:     "https://hook.test/default",
		present: map[string]bool{"type": true, "url": true},
	}, DefaultLimits(), func(string) (string, bool) { return "", false }, nil)
	if err != nil {
		t.Fatal(err)
	}
	falseAction, err := compileHTTPAction(ActionConfig{
		Type:      ActionHTTP,
		URL:       "https://hook.test/false",
		SendEvent: &falseValue,
		Headers:   map[string]string{"Content-Type": "text/plain"},
		present:   map[string]bool{"type": true, "url": true, "send_event": true, "headers": true},
	}, DefaultLimits(), func(string) (string, bool) { return "", false }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !defaultAction.sendEvent || falseAction.sendEvent {
		t.Fatalf("send_event defaults: default=%v false=%v", defaultAction.sendEvent, falseAction.sendEvent)
	}

	type observedRequest struct {
		path        string
		contentType string
		body        string
	}
	var observed []observedRequest
	runner := &DefaultHTTPRunner{transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body []byte
		if request.Body != nil {
			var readErr error
			body, readErr = io.ReadAll(request.Body)
			if readErr != nil {
				return nil, readErr
			}
		}
		observed = append(observed, observedRequest{path: request.URL.Path, contentType: request.Header.Get("Content-Type"), body: string(body)})
		return testHTTPResponse(request, http.StatusOK, nil, "ok"), nil
	})}
	payload := []byte(`{"sequence":1}`)
	for _, action := range []*httpAction{defaultAction, falseAction} {
		_, runErr := runner.Run(context.Background(), HTTPRequest{
			URL:       action.url,
			Method:    action.method,
			Headers:   action.headers,
			SendEvent: action.sendEvent,
			EventJSON: payload,
		})
		if runErr != nil {
			t.Fatal(runErr)
		}
	}
	if len(observed) != 2 {
		t.Fatalf("observed %d requests", len(observed))
	}
	if got := observed[0]; got.path != "/default" || got.contentType != "application/json" || got.body != string(payload) {
		t.Fatalf("default request = %#v", got)
	}
	if got := observed[1]; got.path != "/false" || got.contentType != "text/plain" || got.body != "" {
		t.Fatalf("send_event=false request = %#v", got)
	}
}

func TestHTTPRequestAndResponseBodyLimitsExactAndPlusOne(t *testing.T) {
	limits := DefaultLimits()
	var calls []string
	runner := &DefaultHTTPRunner{transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, request.URL.Path)
		switch request.URL.Path {
		case "/request-exact":
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			if len(body) != limits.HTTPRequestBytes {
				return nil, fmt.Errorf("request body = %d bytes", len(body))
			}
			return testHTTPResponse(request, http.StatusOK, nil, "ok"), nil
		case "/response-exact":
			return testHTTPResponse(request, http.StatusOK, nil, strings.Repeat("r", limits.HTTPResponseBytes)), nil
		case "/response-over":
			return testHTTPResponse(request, http.StatusOK, nil, strings.Repeat("r", limits.HTTPResponseBytes+1)), nil
		default:
			return nil, fmt.Errorf("unexpected request path %q", request.URL.Path)
		}
	})}

	requestExactURL := mustURL(t, "https://hook.test/request-exact")
	if _, err := runner.Run(context.Background(), HTTPRequest{URL: requestExactURL, Method: http.MethodPost, SendEvent: true, EventJSON: bytes.Repeat([]byte("q"), limits.HTTPRequestBytes)}); err != nil {
		t.Fatalf("request exact: %v", err)
	}
	callsBeforeOver := len(calls)
	if _, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "https://hook.test/request-over"), Method: http.MethodPost, SendEvent: true, EventJSON: bytes.Repeat([]byte("q"), limits.HTTPRequestBytes+1)}); err == nil {
		t.Fatal("request limit+1 accepted")
	}
	if len(calls) != callsBeforeOver {
		t.Fatal("oversize request reached the transport")
	}

	result, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "https://hook.test/response-exact"), Method: http.MethodGet})
	if err != nil || len(result.Body) != limits.HTTPResponseBytes {
		t.Fatalf("response exact = %d bytes, %v", len(result.Body), err)
	}
	if _, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "https://hook.test/response-over"), Method: http.MethodGet}); err == nil {
		t.Fatal("response limit+1 accepted")
	}
}

func TestHTTPResponseHeaderLimitExactAndPlusOne(t *testing.T) {
	limits := DefaultLimits()
	limits.HTTPResponseBytes = 256
	const name = "X-Boundary"
	baseBytes := 2 + len(name) + 2 + 2
	if baseBytes >= limits.HTTPResponseBytes {
		t.Fatal("invalid test limit")
	}
	for _, item := range []struct {
		name       string
		valueBytes int
		wantErr    bool
	}{
		{name: "exact", valueBytes: limits.HTTPResponseBytes - baseBytes},
		{name: "plus one", valueBytes: limits.HTTPResponseBytes - baseBytes + 1, wantErr: true},
	} {
		t.Run(item.name, func(t *testing.T) {
			runner := &DefaultHTTPRunner{
				Limits: limits,
				transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					return testHTTPResponse(request, http.StatusOK, http.Header{name: []string{strings.Repeat("h", item.valueBytes)}}, "ok"), nil
				}),
			}
			_, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "https://hook.test/header"), Method: http.MethodGet})
			if (err != nil) != item.wantErr {
				t.Fatalf("run error = %v, wantErr %v", err, item.wantErr)
			}
		})
	}
}

func TestHTTPGzipLimitAppliesAfterDecompression(t *testing.T) {
	limits := DefaultLimits()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		size := limits.HTTPResponseBytes
		if request.URL.Path == "/over" {
			size++
		}
		writer.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(writer)
		_, _ = compressed.Write(bytes.Repeat([]byte("g"), size))
		_ = compressed.Close()
	}))
	defer server.Close()

	exact, err := (&DefaultHTTPRunner{}).Run(context.Background(), HTTPRequest{URL: mustURL(t, server.URL+"/exact"), Method: http.MethodGet})
	if err != nil || len(exact.Body) != limits.HTTPResponseBytes {
		t.Fatalf("gzip exact = %d bytes, %v", len(exact.Body), err)
	}
	if _, err := (&DefaultHTTPRunner{}).Run(context.Background(), HTTPRequest{URL: mustURL(t, server.URL+"/over"), Method: http.MethodGet}); err == nil {
		t.Fatal("gzip decompressed limit+1 accepted")
	}
}

func TestHTTPFailures(t *testing.T) {
	sentinel := errors.New("network unavailable")
	expiredContext, cancelExpiredContext := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpiredContext()
	cases := []struct {
		name      string
		ctx       context.Context
		transport roundTripFunc
	}{
		{
			name: "non-2xx",
			ctx:  context.Background(),
			transport: func(request *http.Request) (*http.Response, error) {
				return testHTTPResponse(request, http.StatusServiceUnavailable, nil, "unavailable"), nil
			},
		},
		{
			name: "network",
			ctx:  context.Background(),
			transport: func(*http.Request) (*http.Response, error) {
				return nil, sentinel
			},
		},
		{
			name: "timeout",
			ctx:  expiredContext,
			transport: func(request *http.Request) (*http.Response, error) {
				return nil, request.Context().Err()
			},
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			runner := &DefaultHTTPRunner{transport: item.transport}
			if _, err := runner.Run(item.ctx, HTTPRequest{URL: mustURL(t, "https://hook.test/failure"), Method: http.MethodGet}); err == nil {
				t.Fatal("failure accepted")
			}
		})
	}
}

func TestHTTPTransportDisablesProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:8080")
	transport := newHTTPTransport(DefaultLimits(), fakeResolver{}, &recordingDialer{})
	if transport.Proxy != nil {
		t.Fatal("HTTP transport can use an environment proxy")
	}
}

func TestHTTPRedirectPreservesMethodAndBody(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			type seenRequest struct {
				method string
				body   string
			}
			var seen []seenRequest
			runner := &DefaultHTTPRunner{transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				seen = append(seen, seenRequest{method: request.Method, body: string(body)})
				if request.URL.Path == "/start" {
					return testHTTPResponse(request, status, http.Header{"Location": []string{"/final"}}, "redirect"), nil
				}
				return testHTTPResponse(request, http.StatusOK, nil, "done"), nil
			})}
			result, err := runner.Run(context.Background(), HTTPRequest{
				URL:       mustURL(t, "https://hook.test/start"),
				Method:    http.MethodPatch,
				SendEvent: true,
				EventJSON: []byte("payload"),
			})
			if err != nil || string(result.Body) != "done" {
				t.Fatalf("redirect result = %q, %v", result.Body, err)
			}
			if len(seen) != 2 {
				t.Fatalf("saw %d requests", len(seen))
			}
			for index, request := range seen {
				if request.method != http.MethodPatch || request.body != "payload" {
					t.Fatalf("request %d = %#v", index, request)
				}
			}
		})
	}
}

func TestHTTPRedirectCountBoundary(t *testing.T) {
	for _, item := range []struct {
		name          string
		redirectCount int
		wantErr       bool
		wantCalls     int
	}{
		{name: "three redirects succeed", redirectCount: 3, wantCalls: 4},
		{name: "fourth redirect fails", redirectCount: 4, wantErr: true, wantCalls: 4},
	} {
		t.Run(item.name, func(t *testing.T) {
			calls := 0
			runner := &DefaultHTTPRunner{transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if calls <= item.redirectCount {
					location := fmt.Sprintf("/step-%d", calls)
					return testHTTPResponse(request, http.StatusTemporaryRedirect, http.Header{"Location": []string{location}}, "redirect"), nil
				}
				return testHTTPResponse(request, http.StatusOK, nil, "done"), nil
			})}
			result, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "https://hook.test/start"), Method: http.MethodPost, SendEvent: true, EventJSON: []byte("payload")})
			if (err != nil) != item.wantErr {
				t.Fatalf("run result = %q, %v", result.Body, err)
			}
			if calls != item.wantCalls {
				t.Fatalf("transport calls = %d, want %d", calls, item.wantCalls)
			}
		})
	}
}

func TestHTTPRedirectRejectsCrossOriginAndHTTPSDowngrade(t *testing.T) {
	for _, item := range []struct {
		name     string
		location string
	}{
		{name: "cross origin", location: "https://other.test/final"},
		{name: "HTTPS downgrade", location: "http://hook.test:443/final"},
	} {
		t.Run(item.name, func(t *testing.T) {
			calls := 0
			runner := &DefaultHTTPRunner{transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return testHTTPResponse(request, http.StatusFound, http.Header{"Location": []string{item.location}}, "redirect"), nil
			})}
			if _, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "https://hook.test/start"), Method: http.MethodGet}); err == nil {
				t.Fatal("unsafe redirect accepted")
			}
			if calls != 1 {
				t.Fatalf("unsafe redirect made %d requests", calls)
			}
		})
	}
}

func TestHTTPRedirectRejectsUnsafeURLStructure(t *testing.T) {
	for _, item := range []struct {
		name     string
		location string
	}{
		{name: "same origin userinfo", location: "https://user:secret@hook.test/final"},
		{name: "same origin fragment", location: "/final#fragment"},
		{name: "raw control", location: "/final\x7f"},
		{name: "escaped control", location: "/final%0a"},
		{name: "opaque", location: "https:opaque"},
	} {
		t.Run(item.name, func(t *testing.T) {
			calls := 0
			runner := &DefaultHTTPRunner{transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return testHTTPResponse(request, http.StatusFound, http.Header{"Location": []string{item.location}}, "redirect"), nil
			})}
			if _, err := runner.Run(context.Background(), HTTPRequest{URL: mustURL(t, "https://hook.test/start"), Method: http.MethodGet}); err == nil {
				t.Fatal("unsafe redirect accepted")
			}
			if calls != 1 {
				t.Fatalf("unsafe redirect made %d requests", calls)
			}
		})
	}
}

func TestHTTPLoopbackDialerPositiveAndRebinding(t *testing.T) {
	resolver := &sequenceResolver{responses: [][]string{
		{"127.0.0.1", "::1"},
		{"127.0.0.1", "192.0.2.1"},
	}}
	dialer := &recordingDialer{}
	dial := secureDialer(resolver, dialer)
	connection, err := dial(context.Background(), "tcp", "localhost:443")
	if err != nil {
		t.Fatalf("pure loopback resolution: %v", err)
	}
	_ = connection.Close()
	if len(dialer.addresses) != 1 || dialer.addresses[0] != "127.0.0.1:443" {
		t.Fatalf("dialed addresses = %#v", dialer.addresses)
	}
	if _, err := dial(context.Background(), "tcp", "localhost:443"); err == nil {
		t.Fatal("mixed rebinding resolution accepted")
	}
	if len(dialer.addresses) != 1 {
		t.Fatalf("mixed DNS reached dialer: %#v", dialer.addresses)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func testHTTPResponse(request *http.Request, status int, headers http.Header, body string) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{
		StatusCode: status,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

type fakeResolver struct{ addresses []string }

func (f fakeResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return ipAddresses(f.addresses), nil
}

type sequenceResolver struct {
	responses [][]string
	calls     int
}

func (r *sequenceResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	if r.calls >= len(r.responses) {
		return nil, errors.New("unexpected resolution")
	}
	addresses := ipAddresses(r.responses[r.calls])
	r.calls++
	return addresses, nil
}

func ipAddresses(rawAddresses []string) []net.IPAddr {
	result := make([]net.IPAddr, 0, len(rawAddresses))
	for _, raw := range rawAddresses {
		result = append(result, net.IPAddr{IP: net.ParseIP(raw)})
	}
	return result
}

type recordingDialer struct{ addresses []string }

func (d *recordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.addresses = append(d.addresses, address)
	local, remote := net.Pipe()
	_ = remote.Close()
	return local, nil
}
